package sandbox

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

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(t.TempDir(), "state.json")
	return Config{
		Listen:          "127.0.0.1:0",
		PublicURL:       "https://sandbox.example.test",
		SandboxID:       "sbx_test",
		OwnerID:         "owner_test",
		Root:            root,
		Token:           "test-token-0123456789abcdef",
		IdleTimeout:     200 * time.Millisecond,
		StateFile:       state,
		MinFreeDisk:     0,
		MaxRead:         1 << 20,
		MaxWrite:        1 << 20,
		MaxInline:       1 << 20,
		MaxRequest:      1 << 20,
		MaxShellOutput:  64 << 10,
		ShellTimeout:    5 * time.Second,
		ShellTimeoutMax: 30 * time.Second,
		DownloadTTL:     time.Minute,
		ArchiveMode:     "off",
		ArchiveTimeout:  time.Minute,
		ArchiveRetry:    50 * time.Millisecond,
	}
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t)
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func rpc(t *testing.T, ts *httptest.Server, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token-0123456789abcdef")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	return out
}

func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", resp)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func TestUnauthorized(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "oauth-protected-resource") {
		t.Fatalf("missing resource metadata hint: %q", got)
	}
}

func TestInitializeAndTools(t *testing.T) {
	_, ts := newTestServer(t)
	out := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	result, _ := out["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("bad protocol version: %v", result["protocolVersion"])
	}
	out = rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ = out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	want := map[string]bool{"read": false, "write": false, "upload": false, "delete": false, "shell": false, "execute": false, "end_work": false}
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		name, _ := m["name"].(string)
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("tool %s is missing", name)
		}
	}
}

func TestWriteReadDelete(t *testing.T) {
	srv, ts := newTestServer(t)
	out := rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write","arguments":{"path":"dir/file.txt","content":"hello"}}}`)
	if !strings.Contains(toolText(t, out), "wrote 5 bytes") {
		t.Fatalf("unexpected write result: %v", out)
	}
	out = rpc(t, ts, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read","arguments":{"path":"dir/file.txt"}}}`)
	if !strings.Contains(toolText(t, out), "hello") {
		t.Fatalf("unexpected read result: %v", out)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.Root, "dir", "file.txt")); err != nil {
		t.Fatalf("file missing: %v", err)
	}
	out = rpc(t, ts, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"delete","arguments":{"path":"dir","recursive":true}}}`)
	if !strings.Contains(toolText(t, out), "deleted") {
		t.Fatalf("unexpected delete result: %v", out)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.Root, "dir")); !os.IsNotExist(err) {
		t.Fatalf("directory was not removed")
	}
}

func TestPathEscape(t *testing.T) {
	srv, ts := newTestServer(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []string{"../secret.txt", outside, "~/secret.txt", "dir/../../escape"}
	for _, path := range cases {
		out := rpc(t, ts, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read","arguments":{"path":"`+path+`"}}}`)
		result, _ := out["result"].(map[string]any)
		if result["isError"] != true {
			t.Fatalf("path %q was not refused: %v", path, out)
		}
	}
	// A symlink out of the workspace must be refused as well.
	link := filepath.Join(srv.cfg.Root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out := rpc(t, ts, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"path":"link"}}}`)
	result, _ := out["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("symlink was not refused: %v", out)
	}
}

func TestShellAndExecute(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tool := range []string{"shell", "execute"} {
		out := rpc(t, ts, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"`+tool+`","arguments":{"command":"echo sandbox-$((2+3))"}}}`)
		if !strings.Contains(toolText(t, out), "sandbox-5") {
			t.Fatalf("%s failed: %v", tool, out)
		}
	}
}

func TestShellTimeout(t *testing.T) {
	srv, _ := newTestServer(t)
	res, err := srv.runShell("sleep 5", "", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("command was not killed: %+v", res)
	}
}

func TestShellEnvIsClean(t *testing.T) {
	t.Setenv("SANDBOX_TOKEN", "super-secret")
	t.Setenv("ARCHIVE_TOKEN", "archive-secret")
	srv, _ := newTestServer(t)
	res, err := srv.runShell("echo \"[$SANDBOX_TOKEN][$ARCHIVE_TOKEN]\"", "", 5*time.Second)
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if strings.Contains(res.Stdout, "secret") {
		t.Fatalf("secrets leaked into the shell: %q", res.Stdout)
	}
}

func TestUploadRawAndURL(t *testing.T) {
	srv, ts := newTestServer(t)
	if err := os.WriteFile(filepath.Join(srv.cfg.Root, "a.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	out := rpc(t, ts, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"upload","arguments":{"path":"a.txt"}}}`)
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	first, _ := content[0].(map[string]any)
	if first["type"] != "resource" {
		t.Fatalf("upload did not return a resource: %v", out)
	}
	res, _ := first["resource"].(map[string]any)
	blob, _ := res["blob"].(string)
	if blob != "ZGF0YQ==" {
		t.Fatalf("unexpected blob %q", blob)
	}
	out = rpc(t, ts, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"upload","arguments":{"path":"a.txt","mode":"url"}}}`)
	result, _ = out["result"].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	url, _ := structured["url"].(string)
	if !strings.HasPrefix(url, "https://sandbox.example.test/f/") {
		t.Fatalf("unexpected url %q", url)
	}
	// The signed link must work and must be tamper proof.
	token := strings.TrimPrefix(url, "https://sandbox.example.test/f/")
	resp, err := http.Get(url)
	if err == nil {
		defer resp.Body.Close()
	}
	if _, _, err := srv.verifyDownload(token); err != nil {
		t.Fatalf("valid link rejected: %v", err)
	}
	if _, _, err := srv.verifyDownload(token + "x"); err == nil {
		t.Fatalf("tampered link accepted")
	}
}

func TestLeaseAndRelease(t *testing.T) {
	srv, ts := newTestServer(t)
	if got := srv.Snapshot().Status; got != StatusFree {
		t.Fatalf("new sandbox should be free, got %s", got)
	}
	out := rpc(t, ts, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"write","arguments":{"path":"x.txt","content":"x"}}}`)
	if out["result"] == nil {
		t.Fatalf("write failed: %v", out)
	}
	if got := srv.Snapshot().Status; got != StatusBusy {
		t.Fatalf("sandbox should be busy, got %s", got)
	}
	out = rpc(t, ts, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"end_work","arguments":{}}}`)
	if !strings.Contains(toolText(t, out), "archiving") {
		t.Fatalf("unexpected end_work result: %v", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Snapshot().Status == StatusFree {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := srv.Snapshot()
	if st.Status != StatusFree {
		t.Fatalf("sandbox was not released: %+v", st)
	}
	entries, _ := os.ReadDir(srv.cfg.Root)
	if len(entries) != 0 {
		t.Fatalf("workspace was not wiped: %v", entries)
	}
}

func TestIdleTimeoutReleases(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.touch("write")
	if err := os.WriteFile(filepath.Join(srv.cfg.Root, "idle.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Snapshot().Status == StatusFree {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("idle sandbox was not released: %+v", srv.Snapshot())
}

func TestHealthPayload(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["state"] != StatusFree {
		t.Fatalf("unexpected state: %v", out["state"])
	}
	for _, key := range []string{"idle_timeout_seconds", "workspace", "disk", "archive", "version"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("health is missing %q: %v", key, out)
		}
	}
}
