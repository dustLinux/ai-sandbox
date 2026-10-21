package pool

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Machine is a sandbox host, whatever it actually is.
type Machine struct {
	ID         string // provider specific id
	Host       string
	Port       int
	SSHKeyFile string
	URL        string // public base URL of the sandbox (MCP endpoint)
	Provider   string
	Slug       string
	Extra      map[string]string
}

// Env is the configuration handed to a sandbox machine.
type Env struct {
	ID           string
	OwnerID      string
	PublicURL    string
	Token        string
	ArchiveToken string
	ArchiveURL   string
	IdleTimeout  string
	Root         string
	StateFile    string
}

// Lines renders the environment as a shell env file.
func (e Env) Lines() string {
	values := []string{
		"SANDBOX_ID=" + e.ID,
		"OWNER_ID=" + sanitizeID(e.OwnerID),
		"PUBLIC_URL=" + e.PublicURL,
		"SANDBOX_TOKEN=" + e.Token,
		"SANDBOX_ROOT=" + e.Root,
		"STATE_FILE=" + e.StateFile,
		"IDLE_TIMEOUT=" + e.IdleTimeout,
		"ARCHIVE_MODE=push",
		"ARCHIVE_URL=" + e.ArchiveURL,
		"ARCHIVE_TOKEN=" + e.ArchiveToken,
		"MIN_FREE_DISK_MB=128",
		"LOG_COMMANDS=0",
	}
	return strings.Join(values, "\n") + "\n"
}

// Provider knows how to create, deploy and destroy sandbox machines.
//
// The Vibe Hosting API is one implementation; any other VPS can be used
// through the ssh provider, and the local provider runs sandboxes as processes
// on the control box itself (development).
type Provider interface {
	Name() string
	// Create provisions a new machine for an owner. It may fail with
	// "manual" when the provider cannot create machines on its own.
	Create(ctx context.Context, slug, ownerID string) (*Machine, error)
	// Deploy installs the sandbox binary and configuration on a machine.
	Deploy(ctx context.Context, m *Machine, env Env) error
	// Healthy reports whether the sandbox answers on its health endpoint.
	Healthy(ctx context.Context, m *Machine) (bool, error)
	// Delete tears the machine down.
	Delete(ctx context.Context, m *Machine) error
}

var errManualProvider = fmt.Errorf("this provider does not create machines: add them by hand")

// providerFor builds the provider named in the configuration.
func providerFor(cfg Config, p *Pool) (Provider, error) {
	switch cfg.Provider {
	case "", "vibe":
		return &vibeProvider{cfg: cfg, pool: p, vibe: NewVibeClient(cfg.VibeAPI)}, nil
	case "ssh":
		return &sshProvider{cfg: cfg}, nil
	case "github":
		return &githubProvider{cfg: cfg, pool: p, gh: NewGitHubClientAt(cfg.GitHubAPI, cfg.GitHubToken)}, nil
	case "local":
		return &localProvider{cfg: cfg, procs: map[string]*localProc{}}, nil
	default:
		return nil, fmt.Errorf("unknown POOL_PROVIDER %q (vibe|ssh|local)", cfg.Provider)
	}
}

// machineFromServer rebuilds a provider machine from a stored server.
func machineFromServer(s *Server, provider string) *Machine {
	m := &Machine{
		ID:         s.VibeID,
		Host:       s.SSHHost,
		Port:       s.SSHPort,
		SSHKeyFile: s.SSHKeyFile,
		URL:        s.URL,
		Provider:   provider,
		Extra:      map[string]string{},
	}
	if s.Root != "" {
		m.Extra["root"] = s.Root
	}
	return m
}

func waitFor(ctx context.Context, d time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err == nil && ok {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if last != nil {
		return fmt.Errorf("timeout: %w", last)
	}
	return fmt.Errorf("timeout")
}
