package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StatusFree = "free"
	StatusBusy = "busy"

	PhaseIdle         = "idle"
	PhaseRunning      = "running"
	PhaseArchiving    = "archiving"
	PhaseArchiveError = "archive_error"
)

// ArchiveInfo describes the last successful archive of this sandbox.
type ArchiveInfo struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Remote string    `json:"remote"`
	Bytes  int64     `json:"bytes"`
	SHA256 string    `json:"sha256"`
}

// State is the persisted lifecycle of one sandbox.
type State struct {
	ServerID       string       `json:"server_id"`
	OwnerID        string       `json:"owner_id"`
	Status         string       `json:"status"`
	Phase          string       `json:"phase"`
	BusySince      time.Time    `json:"busy_since,omitempty"`
	LastActivity   time.Time    `json:"last_activity,omitempty"`
	LastTool       string       `json:"last_tool,omitempty"`
	ArchiveReason  string       `json:"archive_reason,omitempty"`
	ArchiveError   string       `json:"archive_error,omitempty"`
	ArchiveStarted time.Time    `json:"archive_started,omitempty"`
	NextRetry      time.Time    `json:"next_archive_retry,omitempty"`
	ReleaseRequested bool       `json:"release_requested,omitempty"`
	LastArchive    *ArchiveInfo `json:"last_archive,omitempty"`
	Updated        time.Time    `json:"updated"`
}

type Server struct {
	cfg Config

	mu         sync.Mutex
	st         State
	callMu     sync.Mutex
	activeOps  int32
	started    time.Time
	stateDir   string
	oauth      *oauthStore
	archiveJob chan struct{}
}

func New(cfg Config) (*Server, error) {
	s := &Server{
		cfg:        cfg,
		started:    time.Now().UTC(),
		oauth:      newOAuthStore(),
		archiveJob: make(chan struct{}, 1),
	}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	s.reconcile()
	go s.idleLoop()
	return s, nil
}

func (s *Server) Config() Config { return s.cfg }

func (s *Server) loadState() error {
	s.st = State{
		ServerID: s.cfg.SandboxID,
		OwnerID:  s.cfg.OwnerID,
		Status:   StatusFree,
		Phase:    PhaseIdle,
	}
	b, err := os.ReadFile(s.cfg.StateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read state: %w", err)
		}
		return nil
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	if st.ServerID != "" && st.ServerID != s.cfg.SandboxID {
		return fmt.Errorf("state file belongs to sandbox %q", st.ServerID)
	}
	st.ServerID = s.cfg.SandboxID
	if st.OwnerID == "" {
		st.OwnerID = s.cfg.OwnerID
	}
	if st.Status == "" {
		st.Status = StatusFree
	}
	if st.Phase == "" {
		st.Phase = PhaseIdle
	}
	s.st = st
	return nil
}

// reconcile aligns the persisted state with what is actually on disk.
func (s *Server) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	empty, err := dirEmpty(s.cfg.Root)
	if err != nil {
		empty = false
	}
	switch s.st.Status {
	case StatusBusy:
		if s.st.LastActivity.IsZero() {
			s.st.LastActivity = time.Now().UTC()
		}
	default:
		s.st.Status = StatusFree
		if !empty {
			// Leftovers from a crash: treat the sandbox as busy so the files
			// are archived instead of leaking into the next lease.
			s.st.Status = StatusBusy
			s.st.BusySince = time.Now().UTC()
			s.st.LastActivity = time.Now().UTC()
		} else {
			s.st.BusySince = time.Time{}
			s.st.LastActivity = time.Time{}
		}
	}
	if s.st.Phase == PhaseArchiving {
		s.st.ArchiveStarted = time.Time{}
		s.st.NextRetry = time.Now().UTC()
	}
	s.saveLocked()
	if s.st.Status == StatusBusy {
		go s.tryArchive("startup")
	}
}

func (s *Server) saveLocked() {
	s.st.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.cfg.StateFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	os.Chmod(name, 0600)
	if err := os.Rename(name, s.cfg.StateFile); err != nil {
		os.Remove(name)
	}
}

func (s *Server) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// touch marks the sandbox busy and refreshes the idle timer.
func (s *Server) touch(tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if s.st.Status != StatusBusy {
		s.st.Status = StatusBusy
		s.st.BusySince = now
	}
	s.st.LastActivity = now
	if tool != "" {
		s.st.LastTool = tool
	}
	s.st.Phase = PhaseIdle
	s.st.ArchiveError = ""
	s.saveLocked()
}

func (s *Server) beginOperation() int32 {
	return atomic.AddInt32(&s.activeOps, 1)
}

func (s *Server) endOperation() {
	atomic.AddInt32(&s.activeOps, -1)
}

func (s *Server) ActiveOperations() int32 { return atomic.LoadInt32(&s.activeOps) }

// archiving reports whether the sandbox is being released.
func (s *Server) archiving() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Phase == PhaseArchiving
}

// releasing reports whether the sandbox was handed back and is closing down.
func (s *Server) releasing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.ReleaseRequested || s.st.Phase == PhaseArchiving
}

func (s *Server) idleLoop() {
	interval := 20 * time.Second
	if s.cfg.IdleTimeout > 0 {
		if quarter := s.cfg.IdleTimeout / 4; quarter < interval {
			interval = quarter
		}
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		s.tryArchive("idle")
	}
}

// tryArchive releases the sandbox once it has been silent for the idle timeout
// or once a release was explicitly requested.
func (s *Server) tryArchive(reason string) {
	s.mu.Lock()
	if s.st.Status != StatusBusy || s.st.Phase == PhaseArchiving {
		s.mu.Unlock()
		return
	}
	if s.st.Phase == PhaseArchiveError {
		if time.Now().UTC().Before(s.st.NextRetry) {
			s.mu.Unlock()
			return
		}
		s.st.Phase = PhaseIdle
	}
	release := s.st.ReleaseRequested
	if !release {
		if s.cfg.IdleTimeout <= 0 || time.Since(s.st.LastActivity) < s.cfg.IdleTimeout {
			s.mu.Unlock()
			return
		}
	}
	if atomic.LoadInt32(&s.activeOps) > 0 {
		s.mu.Unlock()
		return
	}
	if release && s.st.ArchiveReason != "" {
		reason = s.st.ArchiveReason
	}
	s.st.Phase = PhaseArchiving
	s.st.ReleaseRequested = true
	s.st.ArchiveReason = reason
	s.st.ArchiveStarted = time.Now().UTC()
	s.st.ArchiveError = ""
	s.saveLocked()
	s.mu.Unlock()
	go s.runArchive(reason)
}

// StartRelease begins the archive-and-free cycle; it returns immediately.
func (s *Server) StartRelease(reason string) State {
	s.mu.Lock()
	if s.st.Status == StatusFree {
		s.st.Phase = PhaseIdle
		s.saveLocked()
		s.mu.Unlock()
		return s.st
	}
	if s.st.Phase == PhaseArchiving {
		s.mu.Unlock()
		return s.st
	}
	s.st.Phase = PhaseArchiving
	s.st.ReleaseRequested = true
	s.st.ArchiveReason = reason
	s.st.ArchiveStarted = time.Now().UTC()
	s.st.ArchiveError = ""
	s.saveLocked()
	s.mu.Unlock()
	go s.runArchive(reason)
	return s.Snapshot()
}

func (s *Server) runArchive(reason string) {
	info, err := s.archive(reason)
	if err != nil {
		s.mu.Lock()
		s.st.Phase = PhaseArchiveError
		s.st.ArchiveError = err.Error()
		s.st.NextRetry = time.Now().UTC().Add(s.cfg.ArchiveRetry)
		s.saveLocked()
		s.mu.Unlock()
		logf("archive failed: %v", err)
		return
	}
	if err := wipeDir(s.cfg.Root); err != nil {
		s.mu.Lock()
		s.st.Phase = PhaseArchiveError
		s.st.ArchiveError = "wipe failed: " + err.Error()
		s.st.NextRetry = time.Now().UTC().Add(s.cfg.ArchiveRetry)
		s.saveLocked()
		s.mu.Unlock()
		logf("wipe failed: %v", err)
		return
	}
	s.mu.Lock()
	s.st.Status = StatusFree
	s.st.Phase = PhaseIdle
	s.st.BusySince = time.Time{}
	s.st.LastActivity = time.Time{}
	s.st.LastTool = ""
	s.st.ArchiveError = ""
	s.st.NextRetry = time.Time{}
	s.st.ReleaseRequested = false
	s.st.LastArchive = info
	s.saveLocked()
	s.mu.Unlock()
	logf("archived (%s) -> free: %s", reason, info.Remote)
}

func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// wipeDir removes everything inside dir but keeps dir itself.
func wipeDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

var errNotReady = errors.New("sandbox is not ready")
