package pool

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLocalProviderEndToEnd runs the whole fleet loop on one machine: the pool
// spawns a sandbox, the agent talks MCP to it, end_work archives the workspace
// and the machine goes back to the pool.
func TestLocalProviderEndToEnd(t *testing.T) {
	bin, err := filepath.Abs("../../dist/sandboxd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("sandboxd binary is not built: %v", err)
	}

	dir := t.TempDir()
	cfg := Config{
		Listen:         "127.0.0.1:0",
		PublicURL:      "https://pool.example.test",
		Token:          "pool-token-0123456789abcdef",
		StateDir:       dir,
		KeysDir:        filepath.Join(dir, "keys"),
		BinDir:         filepath.Join(dir, "bin"),
		Provider:       "local",
		ArchiveRemote:  filepath.Join(dir, "drive"),
		RcloneBin:      filepath.Join(dir, "rclone-fake"),
		QuotaBytes:     10 << 20,
		WarnRatio:      0.8,
		HealthInterval: time.Hour,
		QueueInterval:  time.Hour,
		AcquireWait:    10 * time.Second,
		ProvisionTimeout: 60 * time.Second,
	}
	if err := os.MkdirAll(cfg.BinDir, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BinDir, "sandboxd"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	fakeRclone(t, dir)

	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The pool serves its own API: the sandbox pushes its archive here.
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()
	p.cfg.PublicURL = ts.URL

	// The sandbox is spawned as a local process: the provider has to install
	// the binary, hand it an environment and wait until it answers.
	srv, err := p.provision("owner1")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if srv.URL == "" {
		t.Fatal("no sandbox url")
	}
	health, err := p.sandboxHealth(srv)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.ServerID != srv.ID {
		t.Fatalf("wrong sandbox answered: %+v", health)
	}

	// The agent talks MCP to it.
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+srv.SandboxToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: bad json %s", name, raw)
		}
		return out
	}

	out := call("write", map[string]any{"path": "report.txt", "content": "hello fleet"})
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("write failed: %v", out)
	}
	out = call("shell", map[string]any{"command": "echo local-ok"})
	result, _ = out["result"].(map[string]any)
	text, _ := result["content"].([]any)
	first, _ := text[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "local-ok") {
		t.Fatalf("shell failed: %v", out)
	}

	// The machine is leased while it is in use.
	res := p.Acquire("owner1", 7, "e2e")
	if res.Queued || res.MCPURL != srv.URL {
		t.Fatalf("acquire failed: %+v", res)
	}

	// end_work archives the workspace and wipes it.
	out = call("end_work", map[string]any{})
	if out["result"] == nil {
		t.Fatalf("end_work failed: %v", out)
	}
	deadline := time.Now().Add(30 * time.Second)
	archived := false
	for time.Now().Before(deadline) {
		p.store.View(func(d *data) {
			for _, a := range d.Archives {
				if a.SandboxID == srv.ID {
					archived = true
				}
			}
		})
		if archived {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !archived {
		t.Fatalf("the archive never reached the pool")
	}
	// The archive really landed on the drive.
	var found string
	filepath.WalkDir(cfg.ArchiveRemote, func(path string, d os.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() && strings.HasSuffix(path, ".tar.gz") {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no archive on the drive")
	}
	// And the workspace is empty again.
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "sandboxes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		items, _ := os.ReadDir(filepath.Join(cfg.StateDir, "sandboxes", e.Name()))
		if len(items) != 0 {
			t.Fatalf("the workspace was not wiped: %v", items)
		}
	}
}
