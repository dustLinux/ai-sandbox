package sandbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

var noRedirect = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

func postForm(t *testing.T, ts *httptest.Server, path string, values url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := http.PostForm(ts.URL+path, values)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestOAuthDiscovery(t *testing.T) {
	_, ts := newTestServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out["resource"] != "https://sandbox.example.test/mcp" {
			t.Fatalf("%s: bad resource %v", path, out["resource"])
		}
	}
	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if meta["code_challenge_methods_supported"] == nil || meta["registration_endpoint"] == nil {
		t.Fatalf("incomplete metadata: %v", meta)
	}
}

func TestOAuthFlow(t *testing.T) {
	_, ts := newTestServer(t)

	// 1. dynamic client registration
	body := `{"redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"],"client_name":"ChatGPT","grant_types":["authorization_code"],"response_types":["code"],"token_endpoint_auth_method":"none"}`
	resp, err := http.Post(ts.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register failed: %d %v", resp.StatusCode, reg)
	}
	clientID, _ := reg["client_id"].(string)
	if clientID == "" {
		t.Fatalf("no client id: %v", reg)
	}

	// 2. authorization request
	verifier := "0123456789012345678901234567890123456789012345678901234"
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "https://chatgpt.com/connector_platform_oauth_redirect")
	q.Set("code_challenge", codeChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", "xyz")
	q.Set("resource", "https://sandbox.example.test/mcp")
	q.Set("scope", "mcp")
	resp, err = http.Get(ts.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize failed: %d", resp.StatusCode)
	}
	if !strings.Contains(string(page), `name="tx"`) {
		t.Fatalf("consent form is missing: %s", page)
	}
	tx := between(string(page), `name="tx" value="`, `"`)
	if tx == "" {
		t.Fatalf("no transaction id in the form")
	}

	// 3. consent with the wrong key
	resp, bodyText := postForm(t, ts, "/oauth/authorize", url.Values{"tx": {tx}, "key": {"wrong"}, "approve": {"yes"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(bodyText, "Wrong sandbox key") {
		t.Fatalf("wrong key was accepted: %d %s", resp.StatusCode, bodyText)
	}

	// 4. consent with the right key
	resp, err = noRedirect.PostForm(ts.URL+"/oauth/authorize", url.Values{"tx": {tx}, "key": {"test-token-0123456789abcdef"}, "approve": {"yes"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got %d", resp.StatusCode)
	}
	locURL, locErr := resp.Location()
	if locErr != nil {
		t.Fatalf("no redirect location: %v", locErr)
	}
	loc := locURL.String()
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	if parsed.Query().Get("state") != "xyz" {
		t.Fatalf("state was not preserved")
	}

	// 5. token exchange
	resp, bodyText = postForm(t, ts, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://chatgpt.com/connector_platform_oauth_redirect"},
		"code_verifier": {verifier},
		"resource":      {"https://sandbox.example.test/mcp"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange failed: %d %s", resp.StatusCode, bodyText)
	}
	var tok map[string]any
	json.Unmarshal([]byte(bodyText), &tok)
	if tok["access_token"] != "test-token-0123456789abcdef" {
		t.Fatalf("unexpected access token: %v", tok)
	}

	// 6. the code is single use
	resp, bodyText = postForm(t, ts, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://chatgpt.com/connector_platform_oauth_redirect"},
		"code_verifier": {verifier},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the code was reused: %d %s", resp.StatusCode, bodyText)
	}
}

func TestOAuthRejectsBadPKCE(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"redirect_uris":["https://chatgpt.com/cb"],"grant_types":["authorization_code"],"response_types":["code"]}`
	resp, err := http.Post(ts.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	clientID := reg["client_id"].(string)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "https://chatgpt.com/cb")
	q.Set("code_challenge", codeChallenge("0123456789012345678901234567890123456789012345678901234"))
	q.Set("code_challenge_method", "S256")
	q.Set("state", "s")
	resp, err = http.Get(ts.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tx := between(string(page), `name="tx" value="`, `"`)
	resp, err = noRedirect.PostForm(ts.URL+"/oauth/authorize", url.Values{"tx": {tx}, "key": {"test-token-0123456789abcdef"}, "approve": {"yes"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	locURL, locErr := resp.Location()
	if locErr != nil {
		t.Fatalf("no redirect location: %v", locErr)
	}
	loc := locURL.String()
	code := ""
	if i := strings.Index(loc, "code="); i >= 0 {
		rest := loc[i+5:]
		if j := strings.Index(rest, "&"); j >= 0 {
			rest = rest[:j]
		}
		code, _ = url.QueryUnescape(rest)
	}
	if code == "" {
		t.Fatalf("no code: %s", loc)
	}
	resp, bodyText := postForm(t, ts, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"https://chatgpt.com/cb"},
		"code_verifier": {"wrong-verifier-that-is-long-enough-to-pass-length-checks-xx"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong PKCE verifier was accepted: %d %s", resp.StatusCode, bodyText)
	}
}

func TestOAuthRejectsUnknownRedirect(t *testing.T) {
	_, ts := newTestServer(t)
	body := `{"redirect_uris":["https://evil.example/cb"]}`
	resp, err := http.Post(ts.URL+"/oauth/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown redirect was accepted: %d", resp.StatusCode)
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}
