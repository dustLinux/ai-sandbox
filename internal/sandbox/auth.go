package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type oauthClient struct {
	ID           string
	RedirectURIs []string
	Name         string
	Expiry       time.Time
}

type oauthCode struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    string
	Scope       string
	Expiry      time.Time
}

type loginTxn struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    string
	Scope       string
	State       string
	Expiry      time.Time
}

type oauthStore struct {
	mu       sync.Mutex
	clients  map[string]*oauthClient
	codes    map[string]*oauthCode
	txns     map[string]*loginTxn
	attempts map[string][]time.Time
}

func newOAuthStore() *oauthStore {
	return &oauthStore{
		clients:  map[string]*oauthClient{},
		codes:    map[string]*oauthCode{},
		txns:     map[string]*loginTxn{},
		attempts: map[string][]time.Time{},
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// validRedirect checks the redirect against the allowlist.
func (s *Server) validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return u.Scheme == "http" || u.Scheme == "https"
	}
	for _, suffix := range s.allowedRedirectHosts() {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return u.Scheme == "https"
		}
	}
	return false
}

func (s *Server) allowedRedirectHosts() []string {
	raw := strings.TrimSpace(env("OAUTH_REDIRECT_HOSTS", "chatgpt.com,openai.com,claude.ai,claude.com"))
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) resourceURL() string { return s.cfg.PublicURL + "/mcp" }

func (s *Server) issuerURL() string { return s.cfg.PublicURL }

// ---------------------------------------------------------------- metadata --

func (s *Server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resourceURL(),
		"authorization_servers":    []string{s.issuerURL()},
		"scopes_supported":         []string{"mcp"},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "AI Sandbox " + s.cfg.SandboxID,
	})
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuerURL(),
		"authorization_endpoint":                s.issuerURL() + "/oauth/authorize",
		"token_endpoint":                        s.issuerURL() + "/oauth/token",
		"registration_endpoint":                 s.issuerURL() + "/oauth/register",
		"scopes_supported":                      []string{"mcp"},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"authorization_response_iss_parameter_supported": true,
		"service_documentation":                          s.issuerURL() + "/health",
	})
}

// ------------------------------------------------------------ registration --

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.rateLimit(w, r, "register", 20, time.Hour) {
		return
	}
	var in struct {
		RedirectURIs  []string `json:"redirect_uris"`
		ClientName    string   `json:"client_name"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
		return
	}
	for _, u := range in.RedirectURIs {
		if !s.validRedirect(u) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
			return
		}
	}
	if in.AuthMethod != "" && in.AuthMethod != "none" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_token_endpoint_auth_method"})
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
			return
		}
	}
	for _, rt := range in.ResponseTypes {
		if rt != "code" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_response_type"})
			return
		}
	}
	id := randomToken(24)
	s.oauth.mu.Lock()
	s.oauth.clients[id] = &oauthClient{
		ID:           id,
		RedirectURIs: in.RedirectURIs,
		Name:         truncate(in.ClientName, 100),
		Expiry:       time.Now().UTC().Add(24 * time.Hour),
	}
	s.oauth.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_name":                truncate(in.ClientName, 100),
		"redirect_uris":              in.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// ------------------------------------------------------------- authorize --

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize AI Sandbox</title></head>
<body style="font:16px system-ui;max-width:460px;margin:10vh auto;padding:24px;background:#101114;color:#eee">
<h1>Authorize AI Sandbox</h1>
<p><b>{{.Name}}</b> wants to use the sandbox <code>{{.Scope}}</code>.</p>
<p style="color:#f88">This sandbox runs shell commands as <b>root</b> on a dedicated machine.</p>
{{if .Error}}<p style="color:#f88">{{.Error}}</p>{{end}}
<form method="post" action="/oauth/authorize">
<input type="hidden" name="tx" value="{{.Tx}}">
<label>Sandbox key<br><input name="key" type="password" autocomplete="off" required style="width:100%;padding:10px;margin:6px 0 14px"></label>
<button name="approve" value="yes" style="padding:12px 20px">Authorize</button>
<button name="approve" value="no" formnovalidate style="padding:12px 20px">Cancel</button>
</form>
<p style="color:#999;font-size:13px">The key is the per-server token issued with this sandbox.</p>
</body></html>`))

type loginData struct {
	Tx, Name, Scope, Error string
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.authorizeGET(w, r)
	case http.MethodPost:
		s.authorizePOST(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) authorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirect := q.Get("redirect_uri")
	challenge := q.Get("code_challenge")
	state := q.Get("state")
	resource := q.Get("resource")
	scope := q.Get("scope")
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		clientID == "" || redirect == "" || state == "" || len(state) > 512 || !validChallenge(challenge) {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	if resource != "" && resource != s.resourceURL() {
		http.Error(w, "invalid resource", http.StatusBadRequest)
		return
	}
	if scope == "" {
		scope = "mcp"
	}
	if scope != "mcp" && scope != "mcp:full" {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	s.oauth.mu.Lock()
	client, ok := s.oauth.clients[clientID]
	if ok && time.Now().After(client.Expiry) {
		delete(s.oauth.clients, clientID)
		ok = false
	}
	registered := ok && clientHasRedirect(client, redirect)
	s.oauth.mu.Unlock()
	if !registered {
		http.Error(w, "unknown client or redirect_uri", http.StatusBadRequest)
		return
	}
	tx := randomToken(24)
	s.oauth.mu.Lock()
	s.oauth.txns[tx] = &loginTxn{
		ClientID:    clientID,
		RedirectURI: redirect,
		Challenge:   challenge,
		Resource:    s.resourceURL(),
		Scope:       scope,
		State:       state,
		Expiry:      time.Now().UTC().Add(5 * time.Minute),
	}
	name := client.Name
	s.oauth.mu.Unlock()
	if name == "" {
		name = "an MCP client"
	}
	securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginPage.Execute(w, loginData{Tx: tx, Name: name, Scope: scope})
}

func (s *Server) authorizePOST(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimit(w, r, "authorize", 12, time.Minute) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tx := r.PostForm.Get("tx")
	s.oauth.mu.Lock()
	t, ok := s.oauth.txns[tx]
	if ok && time.Now().After(t.Expiry) {
		delete(s.oauth.txns, tx)
		ok = false
	}
	s.oauth.mu.Unlock()
	if !ok {
		http.Error(w, "authorization expired, start again", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("approve") != "yes" {
		u, err := url.Parse(t.RedirectURI)
		if err != nil {
			http.Error(w, "invalid redirect", http.StatusBadRequest)
			return
		}
		q := u.Query()
		q.Set("error", "access_denied")
		q.Set("state", t.State)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	key := r.PostForm.Get("key")
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.Token)) != 1 {
		securityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = loginPage.Execute(w, loginData{Tx: tx, Name: "an MCP client", Scope: t.Scope, Error: "Wrong sandbox key."})
		return
	}
	code := randomToken(32)
	s.oauth.mu.Lock()
	s.oauth.codes[tokenHash(code)] = &oauthCode{
		ClientID:    t.ClientID,
		RedirectURI: t.RedirectURI,
		Challenge:   t.Challenge,
		Resource:    t.Resource,
		Scope:       t.Scope,
		Expiry:      time.Now().UTC().Add(2 * time.Minute),
	}
	s.oauth.mu.Unlock()
	u, err := url.Parse(t.RedirectURI)
	if err != nil {
		http.Error(w, "invalid redirect", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	q.Set("state", t.State)
	q.Set("iss", s.issuerURL())
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// ------------------------------------------------------------------ token --

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request", http.StatusBadRequest)
		return
	}
	grant := r.PostForm.Get("grant_type")
	clientID := r.PostForm.Get("client_id")
	switch grant {
	case "authorization_code":
		s.tokenCodeGrant(w, r, clientID)
	case "refresh_token":
		// The sandbox token is long lived: refreshing returns the same token.
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("refresh_token")), []byte(s.cfg.Token)) != 1 {
			oauthError(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": s.cfg.Token,
			"token_type":   "Bearer",
			"scope":        "mcp",
			"expires_in":   31536000,
		})
	default:
		oauthError(w, "unsupported_grant_type", http.StatusBadRequest)
	}
}

func (s *Server) tokenCodeGrant(w http.ResponseWriter, r *http.Request, clientID string) {
	code := r.PostForm.Get("code")
	redirect := r.PostForm.Get("redirect_uri")
	verifier := r.PostForm.Get("code_verifier")
	resource := r.PostForm.Get("resource")
	if resource != "" && resource != s.resourceURL() {
		oauthError(w, "invalid_target", http.StatusBadRequest)
		return
	}
	s.oauth.mu.Lock()
	rec, ok := s.oauth.codes[tokenHash(code)]
	if ok {
		delete(s.oauth.codes, tokenHash(code))
	}
	client, clientOK := s.oauth.clients[clientID]
	s.oauth.mu.Unlock()
	if !ok || !clientOK || time.Now().After(rec.Expiry) || rec.ClientID != clientID {
		oauthError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	if !clientHasRedirect(client, redirect) || redirect != rec.RedirectURI {
		oauthError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	if !verifyPKCE(verifier, rec.Challenge) {
		oauthError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": s.cfg.Token,
		"token_type":   "Bearer",
		"scope":        "mcp",
		"expires_in":   31536000,
	})
}

// ------------------------------------------------------------------ misc --

func validChallenge(v string) bool {
	if len(v) != 43 {
		return false
	}
	for _, c := range v {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func clientHasRedirect(c *oauthClient, uri string) bool {
	for _, u := range c.RedirectURIs {
		if subtle.ConstantTimeCompare([]byte(u), []byte(uri)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request, kind string, limit int, window time.Duration) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	key := kind + "|" + ip
	now := time.Now()
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	values := s.oauth.attempts[key][:0]
	for _, v := range s.oauth.attempts[key] {
		if now.Sub(v) < window {
			values = append(values, v)
		}
	}
	if len(values) >= limit {
		s.oauth.attempts[key] = values
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return false
	}
	s.oauth.attempts[key] = append(values, now)
	return true
}

func oauthError(w http.ResponseWriter, code string, status int) {
	writeJSON(w, status, map[string]string{"error": code})
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
}

var errUnauthorized = errors.New("unauthorized")

func (s *Server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return false
	}
	token := strings.TrimSpace(header[7:])
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) == 1
}

func fmtErr(format string, args ...any) error { return fmt.Errorf(format, args...) }
