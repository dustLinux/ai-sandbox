package pool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

type archiveResult struct {
	OK     bool   `json:"ok"`
	Remote string `json:"remote"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Error  string `json:"error,omitempty"`
}

// IngestArchive streams an archive from a sandbox into the Google Drive remote.
func (p *Pool) IngestArchive(srv *Server, reason, filename string, body io.Reader) (*archiveResult, error) {
	owner := sanitizeID(srv.OwnerID)
	if owner == "" {
		owner = "unknown"
	}
	if filename == "" {
		filename = fmt.Sprintf("%s_%s.tar.gz", time.Now().UTC().Format("20060102T150405Z"), reason)
	}
	filename = sanitizeFilename(filename)
	remote := path.Join(p.cfg.ArchiveRemote, owner, srv.ID, filename)

	// Keep the sandbox awake while the upload runs: Vibe freezes idle
	// containers, which would break the stream.
	stop := p.keepAlive(srv)
	defer stop()

	hasher := sha256.New()
	var counter countingWriter
	// The local backend needs the target directory to exist; Google Drive
	// creates it implicitly, so a failure here is not fatal.
	p.rcloneMkdir(path.Dir(remote))
	cmd := exec.Command(p.cfg.RcloneBin, p.rcloneArgs("rcat", remote)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rclone: %w", err)
	}
	_, copyErr := io.Copy(io.MultiWriter(stdin, hasher, &counter), body)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if copyErr != nil {
		return nil, fmt.Errorf("upload: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("upload: %w", closeErr)
	}
	if waitErr != nil {
		p.rcloneDelete(remote)
		return nil, fmt.Errorf("rclone rcat failed: %w: %s", waitErr, tail(stderr.String(), 300))
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	size := counter.n

	meta := map[string]any{
		"sandbox_id": srv.ID,
		"owner_id":   owner,
		"reason":     reason,
		"bytes":      size,
		"sha256":     sum,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		p.rcloneUploadBytes(remote+".json", b)
	}

	entry := &ArchiveEntry{
		OwnerID:   owner,
		SandboxID: srv.ID,
		Remote:    remote,
		Bytes:     size,
		SHA256:    sum,
		Reason:    reason,
		Created:   time.Now().UTC(),
	}
	p.store.Update(func(d *data) {
		entry.ID = p.store.nextID("arc")
		d.Archives = append(d.Archives, entry)
	})
	p.enforceQuota(owner)
	return &archiveResult{OK: true, Remote: remote, Bytes: size, SHA256: sum}, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func (p *Pool) rcloneArgs(sub string, remote string) []string {
	args := []string{sub}
	if p.cfg.RcloneConf != "" {
		args = append(args, "--config", p.cfg.RcloneConf)
	}
	args = append(args, remote)
	return args
}

func (p *Pool) rcloneUploadBytes(remote string, data []byte) error {
	cmd := exec.Command(p.cfg.RcloneBin, p.rcloneArgs("rcat", remote)...)
	cmd.Stdin = strings.NewReader(string(data))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func (p *Pool) rcloneDelete(remote string) {
	cmd := exec.Command(p.cfg.RcloneBin, p.rcloneArgs("deletefile", remote)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

func (p *Pool) rcloneMkdir(dir string) {
	cmd := exec.Command(p.cfg.RcloneBin, p.rcloneArgs("mkdir", dir)...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ownerUsage sums the archived bytes of one owner.
func (p *Pool) ownerUsage(ownerID string) int64 {
	var total int64
	p.store.View(func(d *data) {
		for _, a := range d.Archives {
			if a.OwnerID == ownerID {
				total += a.Bytes
			}
		}
	})
	return total
}

// enforceQuota warns at the soft limit and drops the oldest archives when the
// hard limit is exceeded.
func (p *Pool) enforceQuota(ownerID string) {
	usage := p.ownerUsage(ownerID)
	limit := p.cfg.QuotaBytes
	if limit <= 0 {
		return
	}
	if usage > int64(float64(limit)*p.cfg.WarnRatio) {
		p.notify("quota_warning", ownerID, 0, fmt.Sprintf(
			"Архивы пользователя %s занимают %s из %s (лимит).", ownerID, humanBytes(usage), humanBytes(limit)))
	}
	if usage <= limit {
		return
	}
	var oldest []*ArchiveEntry
	p.store.View(func(d *data) {
		for _, a := range d.Archives {
			if a.OwnerID == ownerID {
				oldest = append(oldest, a)
			}
		}
	})
	sortArchives(oldest)
	var removed []string
	for _, a := range oldest {
		if usage <= limit {
			break
		}
		p.rcloneDelete(a.Remote)
		p.rcloneDelete(a.Remote + ".json")
		usage -= a.Bytes
		removed = append(removed, path.Base(a.Remote))
		p.store.Update(func(d *data) {
			out := d.Archives[:0]
			for _, x := range d.Archives {
				if x.ID == a.ID {
					continue
				}
				out = append(out, x)
			}
			d.Archives = out
		})
	}
	if len(removed) > 0 {
		p.notify("quota_pruned", ownerID, 0, fmt.Sprintf(
			"Лимит архивов %s превышен: удалены самые старые (%s).", humanBytes(limit), strings.Join(removed, ", ")))
	}
}

func sortArchives(list []*ArchiveEntry) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Created.Before(list[j-1].Created); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// keepAlive pings the sandbox health endpoint so Vibe does not freeze it
// while an archive is being uploaded.
func (p *Pool) keepAlive(srv *Server) func() {
	ctx, cancel := context.WithCancel(context.Background())
	interval := 2 * time.Minute
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := p.sandboxHealth(srv); err != nil {
					logf("keepalive %s: %v", srv.ID, err)
				}
			}
		}
	}()
	return cancel
}

func sanitizeFilename(name string) string {
	name = path.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "archive.tar.gz"
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var _ = atomic.AddInt64
