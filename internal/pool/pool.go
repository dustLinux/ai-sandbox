package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StateProvisioning = "provisioning"
	StateFree         = "free"
	StateBusy         = "busy"
	StateDeleting     = "deleting"
	StateError        = "error"
)

type Pool struct {
	cfg      Config
	store    *Store
	vibe     *VibeClient
	provider Provider
	client   *http.Client
	mu       sync.Mutex
	busy     map[string]bool // provisionings in flight

	observed     atomic.Value // string: the public base URL seen in a request

	statsMu      sync.Mutex
	lastHealth   time.Time
	lastQueue    time.Time
	healthChecks int
	healthErrors int
	lastError    string
}

// Stats is a small snapshot of the background workers, exposed over HTTPS so
// the pool can be inspected without SSH.
func (p *Pool) Stats() map[string]any {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	return map[string]any{
		"last_health_check": p.lastHealth,
		"last_queue_tick":   p.lastQueue,
		"health_checks":     p.healthChecks,
		"health_errors":     p.healthErrors,
		"last_error":        p.lastError,
	}
}

func (p *Pool) noteHealth(err error) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	p.lastHealth = time.Now().UTC()
	p.healthChecks++
	if err != nil {
		p.healthErrors++
		p.lastError = err.Error()
	}
}

func (p *Pool) noteQueue() {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	p.lastQueue = time.Now().UTC()
}

func New(cfg Config) (*Pool, error) {
	store, err := NewStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	p := &Pool{
		cfg:    cfg,
		store:  store,
		vibe:   NewVibeClient(cfg.VibeAPI),
		client: &http.Client{Timeout: 30 * time.Second},
		busy:   map[string]bool{},
	}
	provider, err := providerFor(cfg, p)
	if err != nil {
		return nil, err
	}
	p.provider = provider
	return p, nil
}

func (p *Pool) Store() *Store { return p.store }

// baseURL is the public address of the pool: the configured one, or the
// address a request came in on when the pool was not told (a hosted app
// without a fixed domain).
func (p *Pool) baseURL() string {
	if p.cfg.PublicURL != "" {
		return p.cfg.PublicURL
	}
	if v, ok := p.observed.Load().(string); ok && v != "" {
		return v
	}
	return ""
}

// observe remembers the public address a request came in on.
func (p *Pool) observe(r *http.Request) {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else if r.Header.Get("X-Forwarded-Ssl") == "on" {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	p.observed.Store(proto + "://" + host)
}

// claimServer tells a fresh sandbox that it is leased, so that "free" in its
// health really means "the work is finished".
func (p *Pool) claimServer(srv *Server) error {
	client := &http.Client{Timeout: 20 * time.Second}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/lease", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+srv.ArchiveToken)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

// releaseAssignment puts a failed claim back into the free pool.
func (p *Pool) releaseAssignment(leaseID, serverID string) {
	p.store.Update(func(d *data) {
		for _, l := range d.Leases {
			if l.ID == leaseID && l.State == "active" {
				l.State = "done"
				now := time.Now().UTC()
				l.Ended = &now
			}
		}
		for _, s := range d.Servers {
			if s.ID == serverID {
				s.State = StateFree
				s.LeaseID = ""
			}
		}
	})
}

// Start launches the background workers.
func (p *Pool) Start(ctx context.Context) {
	go p.recoverProvisioning()
	go p.queueLoop(ctx)
	go p.healthLoop(ctx)
}

// recoverProvisioning finishes or drops servers that were left half created by
// a restart of the pool.
func (p *Pool) recoverProvisioning() {
	var stale []*Server
	p.store.View(func(d *data) {
		for _, s := range d.Servers {
			if s.State == StateProvisioning {
				stale = append(stale, s)
			}
		}
	})
	if p.provider.Name() == "local" {
		// Local sandboxes are child processes: they die with the pool.
		p.store.View(func(d *data) {
			for _, s := range d.Servers {
				if s.State == StateBusy || s.State == StateFree {
					stale = append(stale, s)
				}
			}
		})
	}
	for _, srv := range stale {
		logf("recovering server %s (%s)", srv.ID, srv.URL)
		if err := p.deploy(srv); err != nil {
			logf("redeploy %s failed: %v", srv.ID, err)
			p.store.Update(func(d *data) {
				for _, s := range d.Servers {
					if s.ID == srv.ID {
						s.State = StateError
						s.Error = err.Error()
					}
				}
			})
			continue
		}
		p.store.Update(func(d *data) {
			for _, s := range d.Servers {
				if s.ID == srv.ID {
					s.State = StateFree
					s.Error = ""
					s.LastSeen = time.Now().UTC()
				}
			}
		})
	}
}

// ---------------------------------------------------------------- accounts --

// AddAccount registers a Vibe account that can host sandboxes.
func (p *Pool) AddAccount(label, apiKey string) (*Account, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("api_key is required")
	}
	acc, err := p.vibe.GetAccount(apiKey)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out *Account
	p.store.Update(func(d *data) {
		for _, a := range d.Accounts {
			if a.APIKey == apiKey {
				a.CreditUSD = acc.Credits.TotalUSD
				a.Verified = acc.PhoneVerified
				out = a
				return
			}
		}
		a := &Account{
			ID:        p.store.nextID("acc"),
			Label:     label,
			APIKey:    apiKey,
			Verified:  acc.PhoneVerified,
			CreditUSD: acc.Credits.TotalUSD,
			Created:   now,
		}
		d.Accounts = append(d.Accounts, a)
		out = a
	})
	return out, nil
}

// StartPhoneSignup asks Vibe to text a code to the phone number.
func (p *Pool) StartPhoneSignup(phone string) error {
	_, err := p.vibe.CreateWithPhone(phone, "", nil, false)
	return err
}

// FinishPhoneSignup verifies the code and returns the fresh server.
func (p *Pool) FinishPhoneSignup(phone, code string, neverSleep bool) (*VibeServer, error) {
	return p.vibe.CreateWithPhone(phone, code, nil, neverSleep)
}

func (p *Pool) pickAccount() *Account {
	var picked *Account
	p.store.View(func(d *data) {
		var best *Account
		for _, a := range d.Accounts {
			if a.Disabled {
				continue
			}
			if best == nil || a.LastUsed.Before(best.LastUsed) {
				best = a
			}
		}
		picked = best
	})
	return picked
}

// ----------------------------------------------------------------- leases --

type AcquireResult struct {
	Queued    bool   `json:"queued"`
	Position  int    `json:"position,omitempty"`
	LeaseID   string `json:"lease_id,omitempty"`
	ServerID  string `json:"server_id,omitempty"`
	MCPURL    string `json:"mcp_url,omitempty"`
	Token     string `json:"token,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Acquire leases a free sandbox or queues the request.
func (p *Pool) Acquire(ownerID string, chatID int64, task string) AcquireResult {
	ownerID = sanitizeID(ownerID)
	var res AcquireResult
	var queueID string
	var assigned *Server
	p.store.Update(func(d *data) {
		for _, s := range d.Servers {
			if s.State == StateFree && s.Role != "control" {
				lease := &Lease{
					ID:       p.store.nextID("lease"),
					ServerID: s.ID,
					OwnerID:  ownerID,
					ChatID:   chatID,
					State:    "active",
					Created:  time.Now().UTC(),
				}
				s.State = StateBusy
				s.OwnerID = ownerID
				s.LeaseID = lease.ID
				s.Phase = "idle"
				d.Leases = append(d.Leases, lease)
				assigned = s
				res = AcquireResult{
					LeaseID:  lease.ID,
					ServerID: s.ID,
					MCPURL:   s.URL,
					Token:    s.SandboxToken,
				}
				return
			}
		}
		item := &QueueItem{
			ID:      p.store.nextID("q"),
			OwnerID: ownerID,
			ChatID:  chatID,
			Task:    truncate(task, 200),
			State:   "waiting",
			Created: time.Now().UTC(),
		}
		d.Queue = append(d.Queue, item)
		queueID = item.ID
		res = AcquireResult{Queued: true, Position: len(d.Queue), Message: "no free sandbox, request queued"}
	})
	if !res.Queued {
		if err := p.claimServer(assigned); err != nil {
			logf("claim %s failed: %v", assigned.ID, err)
			p.releaseAssignment(res.LeaseID, res.ServerID)
			p.store.Update(func(d *data) {
				item := &QueueItem{
					ID:      p.store.nextID("q"),
					OwnerID: ownerID,
					ChatID:  chatID,
					Task:    truncate(task, 200),
					State:   "waiting",
					Created: time.Now().UTC(),
				}
				d.Queue = append(d.Queue, item)
			})
			return AcquireResult{Queued: true, Position: 1, Message: "sandbox claim failed, request queued"}
		}
		return res
	}
	// Long poll: the queue worker provisions a machine and assigns the lease.
	deadline := time.Now().Add(p.cfg.AcquireWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var out AcquireResult
		p.store.View(func(d *data) {
			for _, q := range d.Queue {
				if q.ID != queueID || q.LeaseID == "" {
					continue
				}
				for _, l := range d.Leases {
					if l.ID != q.LeaseID {
						continue
					}
					for _, s := range d.Servers {
						if s.ID != l.ServerID {
							continue
						}
						out = AcquireResult{
							LeaseID:  l.ID,
							ServerID: s.ID,
							MCPURL:   s.URL,
							Token:    s.SandboxToken,
						}
					}
				}
			}
		})
		if out.LeaseID != "" {
			return out
		}
	}
	return res
}

// Release ends a lease. The sandbox archives itself; the pool deletes the VPS.
func (p *Pool) Release(leaseID string) error {
	var server *Server
	p.store.Update(func(d *data) {
		for _, l := range d.Leases {
			if l.ID != leaseID {
				continue
			}
			if l.State == "done" {
				return
			}
			l.State = "done"
			now := time.Now().UTC()
			l.Ended = &now
			for _, s := range d.Servers {
				if s.ID == l.ServerID {
					s.LeaseID = ""
					s.State = StateDeleting
					server = s
				}
			}
		}
	})
	if server != nil {
		go p.destroyServer(server)
	}
	return nil
}

// AddMachine registers a machine that was provisioned by hand (any VPS with
// root SSH). The pool deploys the sandbox on it and leases it like any other.
func (p *Pool) AddMachine(host string, port int, privateKey, url, slug string) (*Server, error) {
	if host == "" || port == 0 || privateKey == "" || url == "" {
		return nil, fmt.Errorf("host, port, key and url are required")
	}
	if slug == "" {
		slug = fmt.Sprintf("%s-%s", p.cfg.SubdomainPrefix, randomHex(4))
	}
	keyPath := filepath.Join(p.cfg.KeysDir, "manual_"+randomHex(4)+".key")
	if err := os.WriteFile(keyPath, []byte(privateKey), 0600); err != nil {
		return nil, err
	}
	os.Chmod(keyPath, 0600)
	srv := &Server{
		ID:           p.store.nextID("sbx"),
		VibeID:       "manual_" + randomHex(4),
		Subdomain:    slug,
		URL:          url,
		SSHHost:      host,
		SSHPort:      port,
		SSHKeyFile:   keyPath,
		SandboxToken: randomToken(32),
		ArchiveToken: randomToken(32),
		State:        StateProvisioning,
		Role:         "sandbox",
		Provider:     p.provider.Name(),
		Created:      time.Now().UTC(),
	}
	p.store.Update(func(d *data) {
		d.Servers = append(d.Servers, srv)
	})
	go func() {
		if err := p.deploy(srv); err != nil {
			logf("deploy %s failed: %v", srv.ID, err)
			p.store.Update(func(d *data) {
				for _, s := range d.Servers {
					if s.ID == srv.ID {
						s.State = StateError
						s.Error = err.Error()
					}
				}
			})
			return
		}
		p.store.Update(func(d *data) {
			for _, s := range d.Servers {
				if s.ID == srv.ID {
					s.State = StateFree
					s.Error = ""
					s.LastSeen = time.Now().UTC()
				}
			}
		})
	}()
	return srv, nil
}

// addRegistration remembers a sandbox job that is starting.
func (p *Pool) addRegistration(code, slug, ownerID string) {
	p.store.Update(func(d *data) {
		if d.Registrations == nil {
			d.Registrations = map[string]*Registration{}
		}
		d.Registrations[code] = &Registration{
			Code:    code,
			Slug:    slug,
			OwnerID: ownerID,
			Created: time.Now().UTC(),
		}
	})
}

// takeRegistration returns the machine of a job that registered itself.
func (p *Pool) takeRegistration(code string) *Machine {
	var out *Machine
	p.store.View(func(d *data) {
		reg, ok := d.Registrations[code]
		if !ok || !reg.Registered {
			return
		}
		out = &Machine{
			ID:       reg.RunID,
			Host:     reg.Host,
			Port:     443,
			URL:      reg.URL,
			Provider: "github",
			Slug:     reg.Slug,
			Extra: map[string]string{
				"sandbox_token": reg.SandboxToken,
				"archive_token": reg.ArchiveToken,
			},
		}
	})
	return out
}

// registerMachine handles a sandbox job announcing its public address.
func (p *Pool) registerMachine(code, url, runID, host string) (map[string]any, error) {
	var out map[string]any
	var failure error
	p.store.Update(func(d *data) {
		reg, ok := d.Registrations[code]
		if !ok {
			failure = fmt.Errorf("unknown registration code")
			return
		}
		if time.Since(reg.Created) > 30*time.Minute {
			failure = fmt.Errorf("the registration code expired")
			return
		}
		sandboxToken := reg.SandboxToken
		archiveToken := reg.ArchiveToken
		if !reg.Registered {
			// The first registration hands out the tokens; later ones only
			// refresh the public address (the tunnel may have moved).
			sandboxToken = randomToken(32)
			archiveToken = randomToken(32)
		}
		reg.Registered = true
		reg.URL = url
		reg.Host = host
		reg.RunID = runID
		reg.SandboxToken = sandboxToken
		reg.ArchiveToken = archiveToken
		// Keep the server record in sync with a moved tunnel.
		for _, srv := range d.Servers {
			if srv.VibeID == runID && runID != "" {
				srv.URL = url
			}
		}
		out = map[string]any{
			"sandbox_id":    reg.Slug,
			"sandbox_token": sandboxToken,
			"archive_token": archiveToken,
			"owner_id":      reg.OwnerID,
			"archive_url":   p.baseURL(),
			"idle_timeout":  "15m",
			"run_id":        runID,
		}
	})
	return out, failure
}

// ---------------------------------------------------------------- workers --

func (p *Pool) queueLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.QueueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.serveQueue()
		}
	}
}

// serveQueue assigns queued requests to free sandboxes and provisions new ones.
func (p *Pool) serveQueue() {
	p.noteQueue()
	var needProvision bool
	var ownerID string
	var chatID int64
	var claimed *Server
	var claimedLease string
	// Everything happens in one Update: the store mutex is not reentrant, so
	// calling back into the store from inside a callback deadlocks the pool.
	p.store.Update(func(d *data) {
		waiting := 0
		free := 0
		for _, q := range d.Queue {
			if q.State == "waiting" {
				waiting++
			}
		}
		for _, s := range d.Servers {
			if s.State == StateFree && s.Role != "control" {
				free++
			}
		}
		if waiting == 0 {
			return
		}
		if free > 0 {
			for _, q := range d.Queue {
				if q.State != "waiting" {
					continue
				}
				for _, s := range d.Servers {
					if s.State != StateFree || s.Role == "control" {
						continue
					}
					lease := &Lease{
						ID:       p.store.nextID("lease"),
						ServerID: s.ID,
						OwnerID:  q.OwnerID,
						ChatID:   q.ChatID,
						State:    "active",
						Created:  time.Now().UTC(),
					}
					s.State = StateBusy
					s.OwnerID = q.OwnerID
					s.LeaseID = lease.ID
					q.State = "assigned"
					q.ServerID = s.ID
					q.LeaseID = lease.ID
					d.Leases = append(d.Leases, lease)
					claimed = s
					claimedLease = lease.ID
					break
				}
				if q.State == "assigned" {
					break
				}
			}
			return
		}
		// Never call the store from inside a View callback: the mutex is not
		// reentrant and that deadlocks the whole pool.
		if len(d.Servers) < p.cfg.MaxServers {
			needProvision = true
			for _, q := range d.Queue {
				if q.State == "waiting" {
					ownerID = q.OwnerID
					chatID = q.ChatID
					break
				}
			}
		}
	})
	if needProvision && ownerID != "" {
		p.startProvision(ownerID, chatID)
	}
	if claimed != nil {
		go func(srv *Server, leaseID string) {
			if err := p.claimServer(srv); err != nil {
				logf("claim %s failed: %v", srv.ID, err)
				p.releaseAssignment(leaseID, srv.ID)
				return
			}
		}(claimed, claimedLease)
	}
}

func (p *Pool) countServers() int {
	n := 0
	p.store.View(func(d *data) { n = len(d.Servers) })
	return n
}

func (p *Pool) startProvision(ownerID string, chatID int64) {
	p.mu.Lock()
	if p.busy["provision"] {
		p.mu.Unlock()
		return
	}
	p.busy["provision"] = true
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.busy, "provision")
			p.mu.Unlock()
		}()
		srv, err := p.provision(ownerID)
		if err != nil {
			log.Printf("provision failed: %v", err)
			p.notify("error", ownerID, chatID, "Не удалось поднять песочницу: "+err.Error())
			return
		}
		p.notify("ready", ownerID, chatID, "Песочница готова: "+srv.URL)
	}()
}

// provision creates a machine through the provider and deploys the sandbox on it.
func (p *Pool) provision(ownerID string) (*Server, error) {
	slug := fmt.Sprintf("%s-%s", p.cfg.SubdomainPrefix, randomHex(4))
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ProvisionTimeout)
	defer cancel()

	machine, err := p.provider.Create(ctx, slug, ownerID)
	if err != nil {
		return nil, fmt.Errorf("create machine: %w", err)
	}
	sandboxToken := machine.Extra["sandbox_token"]
	if sandboxToken == "" {
		sandboxToken = randomToken(32)
	}
	archiveToken := machine.Extra["archive_token"]
	if archiveToken == "" {
		archiveToken = randomToken(32)
	}
	srv := &Server{
		ID:           p.store.nextID("sbx"),
		VibeID:       machine.ID,
		Subdomain:    machine.Slug,
		URL:          machine.URL,
		SSHHost:      machine.Host,
		SSHPort:      machine.Port,
		SSHKeyFile:   machine.SSHKeyFile,
		OwnerID:      ownerID,
		SandboxToken: sandboxToken,
		ArchiveToken: archiveToken,
		State:        StateProvisioning,
		Role:         "sandbox",
		Provider:     p.provider.Name(),
		Root:         machine.Extra["root"],
		Created:      time.Now().UTC(),
	}
	p.store.Update(func(d *data) {
		d.Servers = append(d.Servers, srv)
	})
	if err := p.deploy(srv); err != nil {
		p.store.Update(func(d *data) {
			for _, s := range d.Servers {
				if s.ID == srv.ID {
					s.State = StateError
					s.Error = err.Error()
				}
			}
		})
		_ = p.provider.Delete(ctx, machine)
		return nil, err
	}
	p.store.Update(func(d *data) {
		for _, s := range d.Servers {
			if s.ID == srv.ID {
				s.State = StateFree
				s.Error = ""
				s.LastSeen = time.Now().UTC()
			}
		}
	})
	return srv, nil
}

// deploy installs the sandbox on a machine and waits until it answers.
func (p *Pool) deploy(srv *Server) error {
	machine := machineFromServer(srv, p.provider.Name())
	archiveURL := p.cfg.ArchiveURL
	if archiveURL == "" {
		archiveURL = p.baseURL()
	}
	env := Env{
		ID:           srv.ID,
		OwnerID:      srv.OwnerID,
		PublicURL:    srv.URL,
		Token:        srv.SandboxToken,
		ArchiveToken: srv.ArchiveToken,
		ArchiveURL:   archiveURL,
		IdleTimeout:  "15m",
		Root:         "/workspace",
		StateFile:    "/root/.sandbox-state/state.json",
	}
	if p.provider.Name() == "local" {
		if srv.Root != "" {
			env.Root = srv.Root
		}
		env.StateFile = filepath.Join(p.cfg.StateDir, "state", srv.ID+".json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ProvisionTimeout)
	defer cancel()
	if err := p.provider.Deploy(ctx, machine, env); err != nil {
		return err
	}
	return p.waitHealthy(ctx, srv)
}

// waitHealthy polls the sandbox until it reports itself.
func (p *Pool) waitHealthy(ctx context.Context, srv *Server) error {
	machine := machineFromServer(srv, p.provider.Name())
	var last error
	err := waitFor(ctx, p.cfg.ProvisionTimeout, func() (bool, error) {
		ok, err := p.provider.Healthy(ctx, machine)
		if err != nil {
			last = err
			return false, nil
		}
		return ok, nil
	})
	if err != nil {
		return fmt.Errorf("sandbox did not become healthy: %v", last)
	}
	return nil
}

type healthView struct {
	OK       bool   `json:"ok"`
	ServerID string `json:"server_id"`
	State    string `json:"state"`
	Phase    string `json:"phase"`
	Idle     int64  `json:"idle_seconds"`
	Error    string `json:"error"`
}

// sandboxHealth reads the health document of a sandbox.
func (p *Pool) sandboxHealth(srv *Server) (*healthView, error) {
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health %d", resp.StatusCode)
	}
	var out healthView
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// healthLoop keeps leased sandboxes awake and reclaims finished ones.
func (p *Pool) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.checkHealth()
		}
	}
}

func (p *Pool) checkHealth() {
	var targets []*Server
	p.store.View(func(d *data) {
		for _, s := range d.Servers {
			if s.State == StateBusy || s.State == StateProvisioning {
				targets = append(targets, s)
			}
		}
	})
	for _, srv := range targets {
		h, err := p.sandboxHealth(srv)
		if err != nil {
			logf("health %s: %v", srv.ID, err)
			p.noteHealth(err)
			dead := false
			p.store.Update(func(d *data) {
				for _, s := range d.Servers {
					if s.ID != srv.ID {
						continue
					}
					s.HealthFails++
					if s.HealthFails >= 3 {
						s.State = StateDeleting
						s.LeaseID = ""
						dead = true
					}
				}
			})
			if dead {
				p.notify("error", srv.OwnerID, 0, "Песочница "+srv.URL+" перестала отвечать, она удаляется.")
				go p.destroyServer(srv)
			}
			continue
		}
		p.store.Update(func(d *data) {
			for _, s := range d.Servers {
				if s.ID != srv.ID {
					continue
				}
				s.LastSeen = time.Now().UTC()
				s.Phase = h.Phase
				s.HealthFails = 0
				if h.State == "free" && s.State == StateBusy {
					s.State = StateDeleting
					s.LeaseID = ""
					for _, l := range d.Leases {
						if l.ServerID == s.ID && l.State == "active" {
							l.State = "done"
							now := time.Now().UTC()
							l.Ended = &now
						}
					}
				}
			}
		})
		if h.State == "free" {
			p.noteHealth(nil)
			go p.destroyServer(srv)
		} else {
			p.noteHealth(nil)
		}
	}
}

// destroyServer removes the VPS once its work is archived.
func (p *Pool) destroyServer(srv *Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	machine := machineFromServer(srv, p.provider.Name())
	if err := p.provider.Delete(ctx, machine); err != nil {
		log.Printf("delete server %s: %v", srv.VibeID, err)
		p.store.Update(func(d *data) {
			for _, s := range d.Servers {
				if s.ID == srv.ID {
					s.State = StateError
					s.Error = err.Error()
				}
			}
		})
		return
	}
	p.store.Update(func(d *data) {
		out := d.Servers[:0]
		for _, s := range d.Servers {
			if s.ID == srv.ID {
				if s.SSHKeyFile != "" {
					os.Remove(s.SSHKeyFile)
				}
				continue
			}
			out = append(out, s)
		}
		d.Servers = out
		for _, a := range d.Accounts {
			if a.ID == srv.AccountID && a.Servers > 0 {
				a.Servers--
			}
		}
	})
}

// accountForMachine finds the account that owns a provider machine.
func (p *Pool) accountForMachine(machineID string) *Account {
	var out *Account
	p.store.View(func(d *data) {
		for _, s := range d.Servers {
			if s.VibeID == machineID {
				for _, a := range d.Accounts {
					if a.ID == s.AccountID {
						out = a
						return
					}
				}
			}
		}
	})
	return out
}

func (p *Pool) accountByID(id string) *Account {
	var out *Account
	p.store.View(func(d *data) {
		for _, a := range d.Accounts {
			if a.ID == id {
				out = a
				return
			}
		}
	})
	return out
}

// ------------------------------------------------------------------ misc --

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}
