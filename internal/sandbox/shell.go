package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type shellResult struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   *int   `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
}

type cappedBuffer struct {
	buf []byte
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - len(c.buf); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf = append(c.buf, p...)
	}
	return n, nil
}

func (c *cappedBuffer) truncated() bool { return len(c.buf) >= c.max }

func (s *Server) runShell(command, cwd string, timeout time.Duration) (shellResult, error) {
	res := shellResult{Command: command}
	command = strings.TrimSpace(command)
	if command == "" {
		return res, errors.New("command must not be empty")
	}
	if len(command) > 16<<10 {
		return res, errors.New("command is too long")
	}
	if timeout <= 0 {
		timeout = s.cfg.ShellTimeout
	}
	if timeout > s.cfg.ShellTimeoutMax {
		timeout = s.cfg.ShellTimeoutMax
	}
	workdir, err := s.shellCwd(cwd)
	if err != nil {
		return res, err
	}
	res.Cwd = workdir

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = workdir
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := &cappedBuffer{max: s.cfg.MaxShellOutput}
	stderr := &cappedBuffer{max: s.cfg.MaxShellOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return res, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		res.TimedOut = ctx.Err() == context.DeadlineExceeded
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		runErr = <-done
	}
	res.DurationMS = time.Since(start).Milliseconds()
	res.Stdout = string(stdout.buf)
	res.Stderr = string(stderr.buf)
	res.Truncated = stdout.truncated() || stderr.truncated()
	if res.TimedOut {
		code := -1
		res.ExitCode = &code
		return res, nil
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code := ee.ExitCode()
			res.ExitCode = &code
			return res, nil
		}
		return res, runErr
	}
	code := 0
	res.ExitCode = &code
	return res, nil
}

// shellCwd resolves the working directory for a command.
func (s *Server) shellCwd(cwd string) (string, error) {
	if cwd == "" {
		cwd = s.cfg.Root
	}
	var abs string
	if filepath.IsAbs(cwd) {
		abs = filepath.Clean(cwd)
	} else {
		abs = filepath.Clean(filepath.Join(s.cfg.Root, cwd))
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	if !st.IsDir() {
		return "", errNotDir
	}
	return abs, nil
}

// cleanEnv is an allowlist: the sandbox never sees the server's secrets.
func cleanEnv() []string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home,
		"TMPDIR=/tmp",
		"TERM=dumb",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"SHELL=/bin/bash",
		"USER=root",
		"LOGNAME=root",
		"DEBIAN_FRONTEND=noninteractive",
		"LS_COLORS=",
	}
}
