package sandbox

import (
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func logf(format string, args ...any) {
	log.Printf(format, args...)
}

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func detectContentType(b []byte) string {
	if len(b) == 0 {
		return "application/octet-stream"
	}
	ct := http.DetectContentType(b)
	if ct == "text/plain; charset=utf-8" {
		return "text/plain"
	}
	return ct
}

// checkDisk refuses writes when the container is about to run out of space.
func (s *Server) checkDisk() error {
	free, total, err := diskUsage(s.cfg.Root)
	if err != nil {
		return nil // unknown: do not block
	}
	if s.cfg.MinFreeDisk > 0 && free < s.cfg.MinFreeDisk {
		return fmt.Errorf("not enough disk space: %s free, %s reserved", humanBytes(free), humanBytes(s.cfg.MinFreeDisk))
	}
	_ = total
	return nil
}

func diskUsage(path string) (free int64, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total = int64(st.Blocks) * int64(st.Bsize)
	free = int64(st.Bavail) * int64(st.Bsize)
	return free, total, nil
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

type workspaceStats struct {
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
}

func (s *Server) workspaceStats() workspaceStats {
	var st workspaceStats
	limit := 200000
	filepath.WalkDir(s.cfg.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if st.Files+st.Dirs >= limit {
			return filepath.SkipAll
		}
		if d.IsDir() {
			st.Dirs++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			st.Files++
			st.Bytes += info.Size()
		}
		return nil
	})
	return st
}

func nowUTC() time.Time { return time.Now().UTC() }

func randRead(b []byte) (int, error) { return cryptorand.Read(b) }
