package pool

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler exposes the pool API used by the Telegram bot and the sandboxes.
func (p *Pool) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"pool":   p.cfg.PoolName,
			"public": p.cfg.PublicURL,
			"stats":  p.Stats(),
			"status": p.Status(),
		})
	})

	mux.HandleFunc("/v1/archive", p.handleArchive)
	mux.HandleFunc("/v1/acquire", p.authed(p.handleAcquire))
	mux.HandleFunc("/v1/release", p.authed(p.handleRelease))
	mux.HandleFunc("/v1/status", p.authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Status())
	}))
	mux.HandleFunc("/v1/events", p.authed(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		writeJSON(w, http.StatusOK, map[string]any{"events": p.Events(after)})
	}))
	mux.HandleFunc("/v1/lease/", p.authed(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/lease/")
		lease, ok := p.Lease(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown lease"})
			return
		}
		writeJSON(w, http.StatusOK, lease)
	}))
	mux.HandleFunc("/v1/accounts", p.authed(p.handleAddAccount))
	mux.HandleFunc("/v1/accounts/phone", p.authed(p.handlePhoneSignup))
	mux.HandleFunc("/v1/accounts/phone/confirm", p.authed(p.handlePhoneConfirm))
	mux.HandleFunc("/v1/servers/delete", p.authed(p.handleDeleteServer))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

// authed protects the bot facing endpoints with the pool token.
func (p *Pool) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		token := strings.TrimSpace(header[7:])
		if token != p.cfg.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (p *Pool) handleAcquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		OwnerID string `json:"owner_id"`
		ChatID  int64  `json:"chat_id"`
		Task    string `json:"task"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if in.OwnerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner_id is required"})
		return
	}
	res := p.Acquire(in.OwnerID, in.ChatID, in.Task)
	status := http.StatusOK
	if res.Queued {
		status = http.StatusAccepted
	}
	writeJSON(w, status, res)
}

func (p *Pool) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if in.LeaseID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lease_id is required"})
		return
	}
	if err := p.Release(in.LeaseID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleArchive receives the workspace archive from a sandbox.
func (p *Pool) handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	token := strings.TrimSpace(header[7:])
	var srv *Server
	p.store.View(func(d *data) {
		for _, s := range d.Servers {
			if s.ArchiveToken != "" && s.ArchiveToken == token {
				srv = s
				return
			}
		}
	})
	if srv == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown sandbox"})
		return
	}
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		reason = "unknown"
	}
	filename := r.Header.Get("X-Filename")
	owner := r.Header.Get("X-Owner-Id")
	if owner != "" && srv.OwnerID != "" && owner != srv.OwnerID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner mismatch"})
		return
	}
	res, err := p.IngestArchive(srv, reason, filename, r.Body)
	if err != nil {
		logf("archive from %s failed: %v", srv.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.notify("archived", srv.OwnerID, 0, fmtID(srv.ID)+" archived "+humanBytes(res.Bytes))
	writeJSON(w, http.StatusOK, res)
}

func (p *Pool) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	acc, err := p.AddAccount(in.Label, in.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	redacted := *acc
	redacted.APIKey = ""
	writeJSON(w, http.StatusOK, redacted)
}

func (p *Pool) handlePhoneSignup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if err := p.StartPhoneSignup(in.Phone); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "code sent"})
}

func (p *Pool) handlePhoneConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Phone      string `json:"phone"`
		Code       string `json:"code"`
		NeverSleep bool   `json:"never_sleep"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	srv, err := p.FinishPhoneSignup(in.Phone, in.Code, in.NeverSleep)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        srv.ID,
		"subdomain": srv.Subdomain,
		"ssh":       srv.SSH,
		"api_key":   srv.APIKey,
	})
}

func (p *Pool) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ServerID string `json:"server_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	var target *Server
	p.store.View(func(d *data) {
		for _, s := range d.Servers {
			if s.ID == in.ServerID || s.VibeID == in.ServerID {
				target = s
				return
			}
		}
	})
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown server"})
		return
	}
	go p.destroyServer(target)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var _ = io.Discard
var _ = time.Now
