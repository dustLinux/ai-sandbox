package pool

import (
	"context"
	"fmt"
	"time"
)

// githubProvider runs the sandboxes as GitHub Actions jobs.
//
// The pool dispatches a workflow; the job starts a sandbox container, opens a
// public tunnel, registers itself with the pool and waits until the pool
// cancels the run. Nothing is created outside GitHub, and every machine dies
// with its run.
type githubProvider struct {
	cfg Config
	pool *Pool
	gh   *GitHubClient
}

func (p *githubProvider) Name() string { return "github" }

// Create dispatches a sandbox job and waits until it registers itself.
func (p *githubProvider) Create(ctx context.Context, slug, ownerID string) (*Machine, error) {
	if p.cfg.GitHubToken == "" || p.cfg.GitHubRepo == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN and GITHUB_REPO are required for the github provider")
	}
	code := randomToken(16)
	p.pool.addRegistration(code, slug, ownerID)

	inputs := workflowInputs(
		slug,
		ownerID,
		p.pool.baseURL(),
		code,
		p.cfg.GitHubProfile,
	)
	if err := p.gh.Dispatch(ctx, p.cfg.GitHubRepo, p.cfg.GitHubWorkflow, p.cfg.GitHubRef, inputs); err != nil {
		return nil, fmt.Errorf("dispatch workflow: %w", err)
	}
	logf("github: dispatched %s for %s", p.cfg.GitHubWorkflow, slug)

	deadline := time.Now().Add(p.cfg.ProvisionTimeout)
	for time.Now().Before(deadline) {
		if reg := p.pool.takeRegistration(code); reg != nil {
			return reg, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("the sandbox job did not register in time")
}

// Deploy is a no-op: the job installs and starts the sandbox itself.
func (p *githubProvider) Deploy(ctx context.Context, m *Machine, env Env) error {
	return nil
}

func (p *githubProvider) Healthy(ctx context.Context, m *Machine) (bool, error) {
	return probeHealth(ctx, m.URL)
}

// Delete cancels the workflow run: the job and its sandbox die with it.
func (p *githubProvider) Delete(ctx context.Context, m *Machine) error {
	id := RunID(m.ID)
	if id == 0 {
		return nil
	}
	if err := p.gh.CancelRun(ctx, p.cfg.GitHubRepo, id); err != nil {
		return err
	}
	logf("github: cancelled run %d (%s)", id, safeURL(m.URL))
	return nil
}
