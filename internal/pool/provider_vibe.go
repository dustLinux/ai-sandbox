package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// vibeProvider provisions machines through the Vibe Hosting API.
type vibeProvider struct {
	cfg  Config
	pool *Pool
	vibe *VibeClient
}

func (p *vibeProvider) Name() string { return "vibe" }

func (p *vibeProvider) Create(ctx context.Context, slug, ownerID string) (*Machine, error) {
	acc := p.pool.pickAccount()
	if acc == nil {
		return nil, fmt.Errorf("no Vibe account registered")
	}
	created, err := p.vibe.CreateServer(acc.APIKey, []string{slug}, false)
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}
	host, port := ParseSSH(created.SSH)
	if host == "" {
		return nil, fmt.Errorf("cannot parse ssh command %q", created.SSH)
	}
	keyPath := filepath.Join(p.cfg.KeysDir, created.ID+".key")
	if err := os.WriteFile(keyPath, []byte(created.SSHPrivateKey), 0600); err != nil {
		return nil, err
	}
	os.Chmod(keyPath, 0600)
	return &Machine{
		ID:         created.ID,
		Host:       host,
		Port:       port,
		SSHKeyFile: keyPath,
		URL:        "https://" + created.Subdomain,
		Provider:   "vibe",
		Slug:       created.Subdomain,
	}, nil
}

func (p *vibeProvider) Deploy(ctx context.Context, m *Machine, env Env) error {
	return deployOverSSH(m, env, p.cfg)
}

func (p *vibeProvider) Healthy(ctx context.Context, m *Machine) (bool, error) {
	return probeHealth(ctx, m.URL)
}

func (p *vibeProvider) Delete(ctx context.Context, m *Machine) error {
	acc := p.pool.accountForMachine(m.ID)
	if acc == nil {
		return fmt.Errorf("no account for machine %s", m.ID)
	}
	return p.vibe.DeleteServer(acc.APIKey, m.ID)
}

// sshProvider deploys to machines that were added by hand: any VPS with root
// SSH works, the pool never creates or deletes them.
type sshProvider struct {
	cfg Config
}

func (p *sshProvider) Name() string { return "ssh" }

func (p *sshProvider) Create(ctx context.Context, slug, ownerID string) (*Machine, error) {
	return nil, errManualProvider
}

func (p *sshProvider) Deploy(ctx context.Context, m *Machine, env Env) error {
	return deployOverSSH(m, env, p.cfg)
}

func (p *sshProvider) Healthy(ctx context.Context, m *Machine) (bool, error) {
	return probeHealth(ctx, m.URL)
}

func (p *sshProvider) Delete(ctx context.Context, m *Machine) error {
	return nil // hand managed machines are never destroyed by the pool
}

// localProvider runs sandboxes as processes on the control box. It is meant for
// development: no remote machine, no SSH, everything on one host.
type localProvider struct {
	cfg    Config
	procs  map[string]*localProc
	procsM chan struct{}
}

type localProc struct {
	cmd    *execCmd
	root   string
	binary string
}

func (p *localProvider) Name() string { return "local" }

func (p *localProvider) Create(ctx context.Context, slug, ownerID string) (*Machine, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(p.cfg.StateDir, "sandboxes", slug)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Machine{
		ID:       "local_" + randomHex(4),
		Host:     "127.0.0.1",
		Port:     port,
		URL:      fmt.Sprintf("http://127.0.0.1:%d", port),
		Provider: "local",
		Slug:     slug,
		Extra:    map[string]string{"root": root},
	}, nil
}

func (p *localProvider) Deploy(ctx context.Context, m *Machine, env Env) error {
	root := m.Extra["root"]
	binary := filepath.Join(p.cfg.BinDir, "sandboxd")
	if !fileExists(binary) {
		return fmt.Errorf("sandbox binary %s is missing", binary)
	}
	if env.Root == "" {
		env.Root = root
	}
	if env.StateFile == "" {
		env.StateFile = filepath.Join(p.cfg.StateDir, "state", m.ID+".json")
	}
	if err := os.MkdirAll(filepath.Dir(env.StateFile), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(env.Root, 0700); err != nil {
		return err
	}
	cmd := startLocal(binary, env, m.Port)
	p.procs[m.ID] = &localProc{cmd: cmd, root: root, binary: binary}
	return nil
}

func (p *localProvider) Healthy(ctx context.Context, m *Machine) (bool, error) {
	return probeHealth(ctx, m.URL)
}

func (p *localProvider) Delete(ctx context.Context, m *Machine) error {
	if proc, ok := p.procs[m.ID]; ok && proc.cmd != nil {
		proc.cmd.kill()
		delete(p.procs, m.ID)
	}
	if m.Extra["root"] != "" {
		os.RemoveAll(m.Extra["root"])
	}
	return nil
}

// deployOverSSH installs the sandbox on a remote machine over SSH.
func deployOverSSH(m *Machine, env Env, cfg Config) error {
	if err := WaitForSSH(m.Host, m.Port, m.SSHKeyFile, 3*time.Minute); err != nil {
		return err
	}
	out, err := SSH(m.Host, m.Port, m.SSHKeyFile, "uname -m", nil, 30*time.Second)
	if err != nil {
		return fmt.Errorf("detect arch: %w", err)
	}
	arch := "amd64"
	if containsAny(string(out), "aarch64", "arm64") {
		arch = "arm64"
	}
	binary := filepath.Join(cfg.BinDir, "sandboxd-linux-"+arch)
	if !fileExists(binary) {
		return fmt.Errorf("sandbox binary %s is missing", binary)
	}
	if err := SCP(binary, "/root/sandboxd", m.Host, m.Port, m.SSHKeyFile, 3*time.Minute); err != nil {
		return err
	}
	if err := WriteFile(m.Host, m.Port, m.SSHKeyFile, "/root/sandbox.env", env.Lines(), "600", 60*time.Second); err != nil {
		return err
	}
	run := "#!/bin/sh\nset -a\n. /root/sandbox.env\nset +a\nexec /root/sandboxd\n"
	if err := WriteFile(m.Host, m.Port, m.SSHKeyFile, "/root/run.sh", run, "755", 60*time.Second); err != nil {
		return err
	}
	_, err = SSH(m.Host, m.Port, m.SSHKeyFile,
		"mkdir -p "+env.Root+" $(dirname "+env.StateFile+") && chmod 700 "+env.Root+" && chmod +x /root/sandboxd", nil, 60*time.Second)
	return err
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
