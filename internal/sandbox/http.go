package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler builds the HTTP surface of a sandbox.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/f/", s.handleDownload)

	// The pool claims a fresh sandbox through this endpoint: a sandbox that
	// was never used still reports "free", and the pool would otherwise think
	// the work is already finished.
	mux.HandleFunc("/internal/lease", s.handleInternalLease)

	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("/oauth/register", s.handleRegister)
	mux.HandleFunc("/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/token", s.handleToken)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return s.securityMiddleware(mux)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, mcp-protocol-version")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", fmtErr(
			"Bearer resource_metadata=%q, scope=%q", s.issuerURL()+"/.well-known/oauth-protected-resource", "mcp",
		).Error())
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if s.releasing() {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "sandbox is being archived and released",
		})
		return
	}
	// The handshake itself leases the sandbox: an agent that only initializes
	// still holds it until the idle timeout.
	s.touch("initialize")

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequest)
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, rpcError(nil, -32700, "Parse error"))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, rpcError(req.ID, -32600, "Invalid Request"))
		return
	}
	start := time.Now()
	resp, notify := s.rpcResponse(req)
	if notify {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "tools/call" {
		name, _ := req.Params["name"].(string)
		logf("tool %s took %dms", name, time.Since(start).Milliseconds())
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleInternalLease marks the sandbox as leased by the pool.
func (s *Server) handleInternalLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	header := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(header), "bearer "))
	if token == "" || (token != s.cfg.ArchiveToken && token != s.cfg.Token) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	s.touch("lease")
	st := s.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"state":    st.Status,
		"phase":    st.Phase,
		"lease_at": st.BusySince,
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/f/")
	full, rel, err := s.verifyDownload(token)
	if err != nil {
		http.Error(w, "invalid link", http.StatusNotFound)
		return
	}
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", detectMime(full))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(rel)+"\"")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filepath.Base(rel), st.ModTime(), f)
}

// healthResponse is the public view of a sandbox.
type healthResponse struct {
	OK            bool           `json:"ok"`
	ServerID      string         `json:"server_id"`
	OwnerID       string         `json:"owner_id,omitempty"`
	State         string         `json:"state"`
	Phase         string         `json:"phase"`
	BusySince     *time.Time     `json:"busy_since,omitempty"`
	LastActivity  *time.Time     `json:"last_activity,omitempty"`
	IdleSeconds   int64          `json:"idle_seconds"`
	FreeInSeconds int64          `json:"free_in_seconds"`
	IdleTimeout   int64          `json:"idle_timeout_seconds"`
	ActiveCalls   int32          `json:"active_calls"`
	LastTool      string         `json:"last_tool,omitempty"`
	Workspace     workspaceStats `json:"workspace"`
	Disk          diskHealth     `json:"disk"`
	Archive       archiveHealth  `json:"archive"`
	Version       string         `json:"version"`
	Uptime        int64          `json:"uptime_seconds"`
}

type diskHealth struct {
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	Low        bool   `json:"low"`
	Reserved   int64  `json:"reserved_bytes"`
}

type archiveHealth struct {
	Mode      string       `json:"mode"`
	Status    string       `json:"status"`
	Reason    string       `json:"reason,omitempty"`
	Error     string       `json:"error,omitempty"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	Last      *ArchiveInfo `json:"last,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.Snapshot()
	resp := healthResponse{
		OK:          true,
		ServerID:    st.ServerID,
		OwnerID:     st.OwnerID,
		State:       st.Status,
		Phase:       st.Phase,
		IdleTimeout: int64(s.cfg.IdleTimeout.Seconds()),
		ActiveCalls: s.ActiveOperations(),
		LastTool:    st.LastTool,
		Workspace:   s.workspaceStats(),
		Version:     s.version(),
		Uptime:      int64(time.Since(s.started).Seconds()),
	}
	if !st.BusySince.IsZero() {
		t := st.BusySince
		resp.BusySince = &t
	}
	if !st.LastActivity.IsZero() {
		t := st.LastActivity
		resp.LastActivity = &t
		idle := int64(time.Since(st.LastActivity).Seconds())
		if idle < 0 {
			idle = 0
		}
		resp.IdleSeconds = idle
		if st.Status == StatusBusy {
			free := int64(s.cfg.IdleTimeout.Seconds()) - idle
			if free < 0 {
				free = 0
			}
			resp.FreeInSeconds = free
		}
	}
	if free, total, err := diskUsage(s.cfg.Root); err == nil {
		resp.Disk = diskHealth{
			TotalBytes: total,
			FreeBytes:  free,
			Low:        s.cfg.MinFreeDisk > 0 && free < s.cfg.MinFreeDisk,
			Reserved:   s.cfg.MinFreeDisk,
		}
	}
	status := "idle"
	switch st.Phase {
	case PhaseArchiving:
		status = "running"
		if !st.ArchiveStarted.IsZero() {
			t := st.ArchiveStarted
			resp.Archive.StartedAt = &t
		}
	case PhaseArchiveError:
		status = "error"
	}
	resp.Archive = archiveHealth{
		Mode:   s.cfg.ArchiveMode,
		Status: status,
		Reason: st.ArchiveReason,
		Error:  st.ArchiveError,
		Last:   st.LastArchive,
	}
	writeJSON(w, http.StatusOK, resp)
}
