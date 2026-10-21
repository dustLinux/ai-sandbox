package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubClient is a small client for the workflow dispatch API.
type GitHubClient struct {
	base  string
	token string
	http  *http.Client
}

func NewGitHubClient(token string) *GitHubClient {
	return NewGitHubClientAt("https://api.github.com", token)
}

// NewGitHubClientAt points the client at another API (GitHub Enterprise, tests).
func NewGitHubClientAt(base, token string) *GitHubClient {
	return &GitHubClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *GitHubClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Dispatch starts a workflow run with the given inputs.
func (c *GitHubClient) Dispatch(ctx context.Context, repo, workflow, ref string, inputs map[string]string) error {
	path := fmt.Sprintf("/repos/%s/actions/workflows/%s/dispatches", repo, workflow)
	return c.do(ctx, http.MethodPost, path, map[string]any{
		"ref":    ref,
		"inputs": inputs,
	}, nil)
}

// CancelRun stops a workflow run.
func (c *GitHubClient) CancelRun(ctx context.Context, repo string, runID int64) error {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repo, runID)
	return c.do(ctx, http.MethodPost, path, map[string]any{}, nil)
}

// RunStatus returns the state of a workflow run.
func (c *GitHubClient) RunStatus(ctx context.Context, repo string, runID int64) (string, error) {
	var out struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	}
	path := fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	if out.Conclusion != "" {
		return out.Conclusion, nil
	}
	return out.Status, nil
}

// RunID parses a run id from a string.
func RunID(s string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id
}

// workflowInputs builds the dispatch payload for a sandbox job.
func workflowInputs(slug, ownerID, poolURL, code, profile string) map[string]string {
	return map[string]string{
		"slug":          slug,
		"owner_id":      ownerID,
		"pool_url":      poolURL,
		"register_code": code,
		"profile":       profile,
	}
}

// safeURL keeps only the scheme and host of an URL for logs.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return u.Scheme + "://" + u.Host
}
