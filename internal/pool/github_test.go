package pool

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGitHubProviderLifecycle drives the whole github provider: the pool
// dispatches a workflow, the "job" registers itself with a one time code and
// the provider hands the machine back to the pool.
func TestGitHubProviderLifecycle(t *testing.T) {
	dir := t.TempDir()
	var dispatched map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &dispatched)
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	cfg := Config{
		PublicURL:      "https://pool.example.test",
		Token:          "pool-token-0123456789abcdef",
		StateDir:       dir,
		KeysDir:        dir + "/keys",
		BinDir:         dir + "/bin",
		Provider:       "github",
		GitHubAPI:      api.URL,
		GitHubToken:    "ghs_secret",
		GitHubRepo:     "dustlinux/ai-sandbox",
		GitHubWorkflow: "sandbox.yml",
		GitHubRef:      "main",
		ProvisionTimeout: 30 * time.Second,
		AcquireWait:      2 * time.Second,
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The job registers itself while the provider waits for it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		var code string
		p.store.View(func(d *data) {
			for _, reg := range d.Registrations {
				code = reg.Code
			}
		})
		if code == "" {
			return
		}
		body := strings.NewReader(`{"code":"` + code + `","url":"https://sandbox.test","run_id":"42","host":"sandbox.test"}`)
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:0/v1/machines/register", body)
		req.Header.Set("Content-Type", "application/json")
		// call the pool handler directly
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("registration failed: %d %s", rec.Code, rec.Body.String())
		}
	}()

	machine, err := p.provider.Create(t.Context(), "sbx-test", "owner1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if machine.URL != "https://sandbox.test" {
		t.Fatalf("wrong machine url: %q", machine.URL)
	}
	if machine.ID != "42" {
		t.Fatalf("wrong run id: %q", machine.ID)
	}
	if dispatched == nil {
		t.Fatal("the workflow was not dispatched")
	}
	inputs, _ := dispatched["inputs"].(map[string]any)
	if inputs["slug"] != "sbx-test" || inputs["owner_id"] != "owner1" {
		t.Fatalf("wrong dispatch inputs: %v", inputs)
	}
	if inputs["register_code"] == "" {
		t.Fatal("no registration code in the dispatch")
	}

	// The registration carries the tokens; the server record is created by
	// the provision step.
	var reg *Registration
	p.store.View(func(d *data) {
		for _, r := range d.Registrations {
			if r.Registered {
				reg = r
			}
		}
	})
	if reg == nil {
		t.Fatal("the machine was not registered")
	}
	if reg.SandboxToken == "" || reg.ArchiveToken == "" {
		t.Fatalf("the registration has no tokens: %+v", reg)
	}
	if reg.URL != "https://sandbox.test" || reg.RunID != "42" {
		t.Fatalf("wrong registration: %+v", reg)
	}
	if machine.Extra["sandbox_token"] != reg.SandboxToken {
		t.Fatalf("the machine did not get the tokens: %+v", machine.Extra)
	}

	// A repeated registration only refreshes the address: the tunnel of a
	// long running job may move.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/machines/register",
		strings.NewReader(`{"code":"`+inputs["register_code"].(string)+`","url":"https://other.test"}`))
	req.Header.Set("Content-Type", "application/json")
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the address was not refreshed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterMachineRejectsUnknownCode(t *testing.T) {
	p, _ := testPool(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/machines/register",
		strings.NewReader(`{"code":"nope","url":"https://sandbox.test"}`))
	req.Header.Set("Content-Type", "application/json")
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}
