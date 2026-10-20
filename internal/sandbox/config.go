package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen          string
	PublicURL       string
	SandboxID       string
	OwnerID         string
	Root            string
	Token           string
	IdleTimeout     time.Duration
	StateFile       string
	MinFreeDisk     int64
	MaxRead         int64
	MaxWrite        int64
	MaxInline       int64
	MaxRequest      int64
	MaxShellOutput  int
	ShellTimeout    time.Duration
	ShellTimeoutMax time.Duration
	DownloadTTL     time.Duration
	ArchiveMode     string
	ArchiveURL      string
	ArchiveToken    string
	ArchiveTimeout  time.Duration
	ArchiveRetry    time.Duration
	LocalArchiveDir string
	LogCommands     bool
}

func LoadConfig() (Config, error) {
	c := Config{
		Listen:          env("LISTEN", ":80"),
		PublicURL:       strings.TrimRight(os.Getenv("PUBLIC_URL"), "/"),
		SandboxID:       os.Getenv("SANDBOX_ID"),
		OwnerID:         os.Getenv("OWNER_ID"),
		Root:            env("SANDBOX_ROOT", "/workspace"),
		Token:           os.Getenv("SANDBOX_TOKEN"),
		IdleTimeout:     durationEnv("IDLE_TIMEOUT", 15*time.Minute),
		StateFile:       env("STATE_FILE", "/var/lib/ai-sandbox/state.json"),
		MinFreeDisk:     int64Env("MIN_FREE_DISK_MB", 128) << 20,
		MaxRead:         int64Env("MAX_READ_BYTES", 1<<20),
		MaxWrite:        int64Env("MAX_WRITE_BYTES", 10<<20),
		MaxInline:       int64Env("MAX_INLINE_BYTES", 8<<20),
		MaxRequest:      int64Env("MAX_REQUEST_BYTES", 32<<20),
		MaxShellOutput:  int(int64Env("MAX_SHELL_OUTPUT_BYTES", 64<<10)),
		ShellTimeout:    durationEnv("SHELL_TIMEOUT", 120*time.Second),
		ShellTimeoutMax: durationEnv("SHELL_TIMEOUT_MAX", 900*time.Second),
		DownloadTTL:     durationEnv("DOWNLOAD_TTL", 15*time.Minute),
		ArchiveMode:     strings.ToLower(env("ARCHIVE_MODE", "push")),
		ArchiveURL:      strings.TrimRight(os.Getenv("ARCHIVE_URL"), "/"),
		ArchiveToken:    os.Getenv("ARCHIVE_TOKEN"),
		ArchiveTimeout:  durationEnv("ARCHIVE_TIMEOUT", 30*time.Minute),
		ArchiveRetry:    durationEnv("ARCHIVE_RETRY", 5*time.Minute),
		LocalArchiveDir: os.Getenv("LOCAL_ARCHIVE_DIR"),
		LogCommands:     boolEnv("LOG_COMMANDS", false),
	}
	if c.SandboxID == "" {
		return c, errors.New("SANDBOX_ID is required")
	}
	if len(c.Token) < 16 {
		return c, errors.New("SANDBOX_TOKEN must be at least 16 characters")
	}
	if c.PublicURL == "" {
		return c, errors.New("PUBLIC_URL is required")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("PUBLIC_URL must be an absolute http(s) URL without query/fragment")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return c, fmt.Errorf("resolve SANDBOX_ROOT: %w", err)
	}
	c.Root = filepath.Clean(root)
	if err := os.MkdirAll(c.Root, 0700); err != nil {
		return c, fmt.Errorf("create root: %w", err)
	}
	if st, err := os.Stat(c.Root); err != nil || !st.IsDir() {
		return c, errors.New("SANDBOX_ROOT is not a directory")
	}
	if c.MaxRead < 1024 || c.MaxWrite < 1024 || c.MaxInline < 1024 {
		return c, errors.New("byte limits are too small")
	}
	if c.MaxShellOutput < 1024 {
		return c, errors.New("MAX_SHELL_OUTPUT_BYTES is too small")
	}
	if c.ShellTimeout <= 0 || c.ShellTimeoutMax < c.ShellTimeout {
		return c, errors.New("invalid shell timeouts")
	}
	switch c.ArchiveMode {
	case "push":
		if c.ArchiveURL == "" || len(c.ArchiveToken) < 16 {
			return c, errors.New("ARCHIVE_URL and ARCHIVE_TOKEN are required for ARCHIVE_MODE=push")
		}
	case "local":
		if c.LocalArchiveDir == "" {
			return c, errors.New("LOCAL_ARCHIVE_DIR is required for ARCHIVE_MODE=local")
		}
		if err := os.MkdirAll(c.LocalArchiveDir, 0700); err != nil {
			return c, fmt.Errorf("create LOCAL_ARCHIVE_DIR: %w", err)
		}
	case "off":
	default:
		return c, fmt.Errorf("unknown ARCHIVE_MODE %q (push|local|off)", c.ArchiveMode)
	}
	return c, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func durationEnv(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func int64Env(k string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func boolEnv(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
