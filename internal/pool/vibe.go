package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type VibeClient struct {
	base string
	http *http.Client
}

type VibeServer struct {
	ID            string  `json:"id"`
	Subdomain     string  `json:"subdomain"`
	SSH           string  `json:"ssh"`
	SSHPrivateKey string  `json:"ssh_private_key"`
	APIKey        string  `json:"api_key"`
	SubdomainNote *string `json:"subdomain_note"`
	AccountNote   *string `json:"account_note"`
	NeverSleep    bool    `json:"never_sleep"`
	State         string  `json:"state"`
}

type VibeCredits struct {
	Tier             string  `json:"tier"`
	FreeBalanceUSD   float64 `json:"free_balance_usd"`
	PaidBalanceUSD   float64 `json:"paid_balance_usd"`
	TotalUSD         float64 `json:"total_usd"`
	FreeCreditsMonth float64 `json:"free_credits_monthly"`
}

type VibeAccount struct {
	AccountID     string       `json:"account_id"`
	Tier          string       `json:"tier"`
	PhoneVerified bool         `json:"phone_verified"`
	Projects      int          `json:"projects"`
	Credits       VibeCredits  `json:"credits"`
}

func NewVibeClient(base string) *VibeClient {
	return &VibeClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 90 * time.Second}}
}

func (c *VibeClient) do(method, path, token string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, nil
}

func apiError(body []byte, status int) error {
	var envelope struct {
		Detail struct {
			Error   string `json:"error"`
			Reason  string `json:"reason"`
			Account string `json:"account_note"`
		} `json:"detail"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	msg := envelope.Detail.Error
	if msg == "" {
		msg = envelope.Error
	}
	if msg == "" {
		msg = envelope.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	return fmt.Errorf("vibe api %d: %s", status, msg)
}

// CreateServer provisions a new sandbox server under an account.
func (c *VibeClient) CreateServer(apiKey string, subdomains []string, neverSleep bool) (*VibeServer, error) {
	body, status, err := c.do(http.MethodPost, "/create", apiKey, map[string]any{
		"subdomains":  subdomains,
		"never_sleep": neverSleep,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(body, status)
	}
	var out VibeServer
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}
	return &out, nil
}

// CreateWithPhone finishes a phone signup; with an empty code it sends the SMS.
func (c *VibeClient) CreateWithPhone(phone, code string, subdomains []string, neverSleep bool) (*VibeServer, error) {
	payload := map[string]any{"subdomains": subdomains, "never_sleep": neverSleep, "phone": phone}
	if code != "" {
		payload["code"] = code
	}
	body, status, err := c.do(http.MethodPost, "/create", "", payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(body, status)
	}
	var out VibeServer
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}
	return &out, nil
}

func (c *VibeClient) ListServers(apiKey string) ([]VibeServer, error) {
	body, status, err := c.do(http.MethodGet, "/servers", apiKey, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(body, status)
	}
	var out []VibeServer
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *VibeClient) DeleteServer(apiKey, id string) error {
	body, status, err := c.do(http.MethodDelete, "/servers/"+id, apiKey, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(body, status)
	}
	return nil
}

func (c *VibeClient) Sleep(apiKey, id string) error {
	body, status, err := c.do(http.MethodPost, "/servers/"+id+"/sleep", apiKey, map[string]any{})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(body, status)
	}
	return nil
}

func (c *VibeClient) Wake(apiKey, id string) error {
	body, status, err := c.do(http.MethodPost, "/servers/"+id+"/wake", apiKey, map[string]any{})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(body, status)
	}
	return nil
}

func (c *VibeClient) SetNeverSleep(apiKey, id string, on bool) error {
	body, status, err := c.do(http.MethodPost, "/servers/"+id+"/never-sleep", apiKey, map[string]any{"never_sleep": on})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError(body, status)
	}
	return nil
}

func (c *VibeClient) GetAccount(apiKey string) (*VibeAccount, error) {
	body, status, err := c.do(http.MethodGet, "/account", apiKey, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(body, status)
	}
	var out VibeAccount
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *VibeClient) GetUsage(apiKey, id string) (float64, error) {
	body, status, err := c.do(http.MethodGet, "/servers/"+id+"/usage", apiKey, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, apiError(body, status)
	}
	var out struct {
		DiskMBUsed float64 `json:"disk_mb_used"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	return out.DiskMBUsed, nil
}

var sshRe = regexp.MustCompile(`root@([^\s]+)\s+-p\s+(\d+)`)

// ParseSSH extracts host and port from the ssh command Vibe returns.
func ParseSSH(cmd string) (string, int) {
	m := sshRe.FindStringSubmatch(cmd)
	if len(m) != 3 {
		return "", 0
	}
	port, _ := strconv.Atoi(m[2])
	return m[1], port
}
