package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// execCmd is a running local sandbox process.
type execCmd struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	done bool
}

func (e *execCmd) kill() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done || e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Kill()
}

// freePort asks the kernel for an unused TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// startLocal launches a sandbox process on the control box itself.
func startLocal(binary string, env Env, port int) *execCmd {
	values := env.Lines()
	cmd := exec.Command(binary)
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"LISTEN=127.0.0.1:" + fmt.Sprintf("%d", port),
		"SANDBOX_ID=" + env.ID,
		"OWNER_ID=" + sanitizeID(env.OwnerID),
		"PUBLIC_URL=" + env.PublicURL,
		"SANDBOX_TOKEN=" + env.Token,
		"SANDBOX_ROOT=" + env.Root,
		"STATE_FILE=" + env.StateFile,
		"IDLE_TIMEOUT=" + env.IdleTimeout,
		"ARCHIVE_MODE=" + strings.TrimSpace(valueOf(values, "ARCHIVE_MODE")),
		"ARCHIVE_URL=" + env.ArchiveURL,
		"ARCHIVE_TOKEN=" + env.ArchiveToken,
		"MIN_FREE_DISK_MB=128",
		"LOG_COMMANDS=0",
	}
	logFile := filepath.Join(filepath.Dir(env.StateFile), "sandbox.log")
	os.MkdirAll(filepath.Dir(logFile), 0700)
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.SysProcAttr = newProcAttr()
	_ = cmd.Start()
	return &execCmd{cmd: cmd}
}

func valueOf(envLines, key string) string {
	for _, line := range strings.Split(envLines, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

// probeHealth checks the public health endpoint of a sandbox.
func probeHealth(ctx context.Context, url string) (bool, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("health %d", resp.StatusCode)
	}
	var out struct {
		OK       bool   `json:"ok"`
		ServerID string `json:"server_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return false, err
	}
	return out.ServerID != "", nil
}

// newProcAttr detaches the sandbox process into its own process group.
func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
