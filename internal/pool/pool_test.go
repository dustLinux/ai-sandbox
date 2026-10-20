package pool

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPool(t *testing.T) (*Pool, *Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Listen:         "127.0.0.1:0",
		PublicURL:      "https://pool.example.test",
		Token:          "pool-token-0123456789abcdef",
		StateDir:       dir,
		KeysDir:        filepath.Join(dir, "keys"),
		BinDir:         filepath.Join(dir, "bin"),
		VibeAPI:        "https://thevibehosting.com",
		ArchiveRemote:  filepath.Join(dir, "drive"),
		RcloneBin:      filepath.Join(dir, "rclone-fake"),
		QuotaBytes:     10 << 20,
		WarnRatio:      0.8,
		HealthInterval: time.Hour,
		QueueInterval:  time.Hour,
		AcquireWait:    2 * time.Second,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	return p, p.store
}

// fakeRclone writes a stand-in for rclone: it stores rcat payloads on disk.
func fakeRclone(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
sub="$1"
shift
remote=""
for a in "$@"; do remote="$a"; done
case "$sub" in
  rcat)
    mkdir -p "$(dirname "$remote")"
    cat > "$remote"
    ;;
  mkdir)
    mkdir -p "$remote"
    ;;
  deletefile)
    rm -f "$remote"
    ;;
  *)
    echo "unsupported $sub" >&2
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "rclone-fake")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Update(func(d *data) {
		d.Accounts = append(d.Accounts, &Account{ID: "acc_1", APIKey: "vibe_x", CreditUSD: 1.5})
	})
	again, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.View(func(d *data) {
		if len(d.Accounts) != 1 || d.Accounts[0].APIKey != "vibe_x" {
			t.Fatalf("state was not persisted: %+v", d.Accounts)
		}
	})
}

func TestSanitizeID(t *testing.T) {
	if got := sanitizeID("../../etc/passwd"); got != "....etcpasswd" {
		t.Fatalf("unexpected id %q", got)
	}
	if got := sanitizeID("tg-123_user.name"); got != "tg-123_user.name" {
		t.Fatalf("unexpected id %q", got)
	}
}

func TestParseSSH(t *testing.T) {
	host, port := ParseSSH("ssh -i .thevibehosting_key -o IdentitiesOnly=yes root@46.225.188.115 -p 34656")
	if host != "46.225.188.115" || port != 34656 {
		t.Fatalf("bad parse: %q %d", host, port)
	}
}

func TestAcquireUsesFreeServer(t *testing.T) {
	p, store := testPool(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"state":"busy"}`)
	}))
	defer fake.Close()
	store.Update(func(d *data) {
		d.Servers = append(d.Servers, &Server{
			ID: "sbx_1", State: StateFree, URL: fake.URL,
			SandboxToken: "sandbox-token", ArchiveToken: "archive-token",
		})
	})
	res := p.Acquire("owner", 42, "task")
	if res.Queued || res.LeaseID == "" || res.MCPURL != fake.URL {
		t.Fatalf("unexpected acquire result: %+v", res)
	}
	store.View(func(d *data) {
		if d.Servers[0].State != StateBusy || d.Servers[0].OwnerID != "owner" {
			t.Fatalf("server was not leased: %+v", d.Servers[0])
		}
		if len(d.Leases) != 1 || d.Leases[0].State != "active" {
			t.Fatalf("lease was not created: %+v", d.Leases)
		}
	})
}

func TestAcquireQueuesWhenBusy(t *testing.T) {
	p, store := testPool(t)
	store.Update(func(d *data) {
		d.Servers = append(d.Servers, &Server{ID: "sbx_1", State: StateBusy, URL: "https://sandbox.test"})
	})
	res := p.Acquire("owner", 42, "task")
	if !res.Queued || res.Position != 1 {
		t.Fatalf("expected a queue position: %+v", res)
	}
	store.View(func(d *data) {
		if len(d.Queue) != 1 || d.Queue[0].State != "waiting" {
			t.Fatalf("queue item missing: %+v", d.Queue)
		}
	})
}

func TestServeQueueAssigns(t *testing.T) {
	p, store := testPool(t)
	store.Update(func(d *data) {
		d.Queue = append(d.Queue, &QueueItem{ID: "q_1", OwnerID: "owner", State: "waiting"})
		d.Servers = append(d.Servers, &Server{ID: "sbx_1", State: StateFree, URL: "https://sandbox.test"})
	})
	p.serveQueue()
	store.View(func(d *data) {
		if d.Queue[0].State != "assigned" || d.Queue[0].LeaseID == "" {
			t.Fatalf("queue item was not assigned: %+v", d.Queue[0])
		}
		if d.Servers[0].State != StateBusy {
			t.Fatalf("server was not leased: %+v", d.Servers[0])
		}
	})
}

func TestArchiveIngestAndQuota(t *testing.T) {
	p, store := testPool(t)
	fakeRclone(t, p.cfg.StateDir)
	store.Update(func(d *data) {
		d.Servers = append(d.Servers, &Server{
			ID: "sbx_1", OwnerID: "owner1", ArchiveToken: "archive-token", State: StateBusy,
		})
	})
	srv := &Server{ID: "sbx_1", OwnerID: "owner1", ArchiveToken: "archive-token"}
	payload := strings.Repeat("a", 1024)
	res, err := p.IngestArchive(srv, "end_work", "arch.tar.gz", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Bytes != int64(len(payload)) {
		t.Fatalf("wrong size: %d", res.Bytes)
	}
	want := filepath.Join(p.cfg.ArchiveRemote, "owner1", "sbx_1", "arch.tar.gz")
	if res.Remote != want {
		t.Fatalf("wrong remote: %q", res.Remote)
	}
	body, err := os.ReadFile(want)
	if err != nil || len(body) != len(payload) {
		t.Fatalf("archive not written: %v", err)
	}
	store.View(func(d *data) {
		if len(d.Archives) != 1 || d.Archives[0].Bytes != int64(len(payload)) {
			t.Fatalf("archive not recorded: %+v", d.Archives)
		}
	})

	// The quota is tiny: a second archive must evict the oldest one.
	p.cfg.QuotaBytes = 1500
	p.cfg.WarnRatio = 0.5
	store.Update(func(d *data) {
		d.Archives = append(d.Archives, &ArchiveEntry{
			ID: "arc_old", OwnerID: "owner1", Remote: want, Bytes: 1024,
			Created: time.Now().UTC().Add(-time.Hour),
		})
	})
	if _, err := p.IngestArchive(srv, "end_work", "arch2.tar.gz", strings.NewReader(payload)); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	store.View(func(d *data) {
		if len(d.Archives) != 1 {
			t.Fatalf("the oldest archive was not evicted: %+v", d.Archives)
		}
		if d.Archives[0].Remote == want {
			t.Fatalf("the wrong archive was evicted: %+v", d.Archives[0])
		}
	})
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("the evicted archive is still on disk")
	}
}

func TestArchiveEndpointRejectsUnknownToken(t *testing.T) {
	p, _ := testPool(t)
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/archive", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestPoolAPIRequiresToken(t *testing.T) {
	p, _ := testPool(t)
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(b), "accounts") {
		t.Fatalf("status failed: %d %s", resp2.StatusCode, b)
	}
}
