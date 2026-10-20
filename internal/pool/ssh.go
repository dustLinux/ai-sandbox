package pool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func sshArgs(keyFile, host string, port int) []string {
	return []string{
		"-i", keyFile,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=15",
		"-p", fmt.Sprintf("%d", port),
		"root@" + host,
	}
}

// WaitForSSH blocks until the machine accepts an SSH connection.
func WaitForSSH(host string, port int, keyFile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, "ssh", append(sshArgs(keyFile, host, port), "true")...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("ssh not ready: %w", last)
}

// SSH runs a command on the machine and returns its output.
func SSH(host string, port int, keyFile, script string, stdin []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(sshArgs(keyFile, host, port), script)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// SCP copies a local file to the machine.
func SCP(local, remote, host string, port int, keyFile string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{
		"-i", keyFile,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-P", fmt.Sprintf("%d", port),
		local,
		"root@" + host + ":" + remote,
	}
	cmd := exec.CommandContext(ctx, "scp", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp %s: %v: %s", local, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WriteFile streams content into a remote file with the given mode.
func WriteFile(host string, port int, keyFile, path, content string, mode string, timeout time.Duration) error {
	_, err := SSH(host, port, keyFile, "cat > "+path+" && chmod "+mode+" "+path, []byte(content), timeout)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
