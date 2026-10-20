package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Account struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	APIKey    string    `json:"api_key"`
	Verified  bool      `json:"verified"`
	Disabled  bool      `json:"disabled"`
	CreditUSD float64   `json:"credit_usd"`
	Servers   int       `json:"servers"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"last_used"`
	Note      string    `json:"note,omitempty"`
}

type Server struct {
	ID           string     `json:"id"`
	VibeID       string     `json:"vibe_id"`
	AccountID    string     `json:"account_id"`
	Subdomain    string     `json:"subdomain"`
	URL          string     `json:"url"`
	SSHHost      string     `json:"ssh_host"`
	SSHPort      int        `json:"ssh_port"`
	SSHKeyFile   string     `json:"ssh_key_file"`
	OwnerID      string     `json:"owner_id"`
	SandboxToken string     `json:"sandbox_token"`
	ArchiveToken string     `json:"archive_token"`
	State        string     `json:"state"`
	Phase        string     `json:"phase"`
	Provider     string     `json:"provider,omitempty"`
	Root         string     `json:"root,omitempty"`
	LeaseID      string     `json:"lease_id,omitempty"`
	Role         string     `json:"role,omitempty"`
	Created      time.Time  `json:"created"`
	LastSeen     time.Time  `json:"last_seen"`
	HealthFails  int        `json:"health_fails,omitempty"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
	Error        string     `json:"error,omitempty"`
}

type Lease struct {
	ID       string     `json:"id"`
	ServerID string     `json:"server_id"`
	OwnerID  string     `json:"owner_id"`
	ChatID   int64      `json:"chat_id,omitempty"`
	State    string     `json:"state"`
	Created  time.Time  `json:"created"`
	Ended    *time.Time `json:"ended,omitempty"`
}

type QueueItem struct {
	ID       string    `json:"id"`
	OwnerID  string    `json:"owner_id"`
	ChatID   int64     `json:"chat_id,omitempty"`
	Task     string    `json:"task,omitempty"`
	State    string    `json:"state"`
	Created  time.Time `json:"created"`
	ServerID string    `json:"server_id,omitempty"`
	LeaseID  string    `json:"lease_id,omitempty"`
}

type ArchiveEntry struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	SandboxID string    `json:"sandbox_id"`
	Remote    string    `json:"remote"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	Reason    string    `json:"reason"`
	Created   time.Time `json:"created"`
}

type Event struct {
	ID      int64     `json:"id"`
	Kind    string    `json:"kind"`
	OwnerID string    `json:"owner_id,omitempty"`
	ChatID  int64     `json:"chat_id,omitempty"`
	Message string    `json:"message"`
	Created time.Time `json:"created"`
}

type data struct {
	Accounts []*Account      `json:"accounts"`
	Servers  []*Server       `json:"servers"`
	Leases   []*Lease        `json:"leases"`
	Queue    []*QueueItem    `json:"queue"`
	Archives []*ArchiveEntry `json:"archives"`
	Events   []*Event        `json:"events"`
	Seq      int64           `json:"seq"`
}

// Store keeps the pool state in a single JSON file.
type Store struct {
	mu   sync.Mutex
	path string
	d    data
}

func NewStore(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "state.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	s.d = data{}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	return nil
}

func (s *Store) save() {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return
	}
	os.Chmod(tmp, 0600)
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
	}
}

// Update runs fn under the store lock and persists the result.
func (s *Store) Update(fn func(d *data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
	s.save()
}

// View runs fn under the store lock without persisting.
func (s *Store) View(fn func(d *data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}

func (s *Store) nextID(prefix string) string {
	s.d.Seq++
	return fmt.Sprintf("%s_%d_%s", prefix, s.d.Seq, randomHex(4))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "00000000"
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}
