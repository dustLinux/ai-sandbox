package pool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen     string
	PublicURL  string
	Token      string
	StateDir   string
	KeysDir    string
	BinDir     string
	Provider   string
	ArchiveURL string
	VibeAPI    string
	ArchiveRemote  string
	RcloneBin  string
	RcloneConf string
	QuotaBytes int64
	WarnRatio  float64
	HealthInterval time.Duration
	QueueInterval time.Duration
	AcquireWait time.Duration
	ProvisionTimeout time.Duration
	ArchiveTimeout time.Duration
	TelegramToken string
	TelegramChat  int64
	MaxServers    int
	WarmSpares    int
	SubdomainPrefix string
	PoolName      string
}

func LoadConfig() (Config, error) {
	c := Config{
		Listen:           env("POOL_LISTEN", ":80"),
		PublicURL:        strings.TrimRight(os.Getenv("POOL_PUBLIC_URL"), "/"),
		Token:            os.Getenv("POOL_TOKEN"),
		StateDir:         env("POOL_STATE_DIR", "/root/pool"),
		VibeAPI:          strings.TrimRight(env("VIBE_API", "https://thevibehosting.com"), "/"),
		Provider:         env("POOL_PROVIDER", "vibe"),
		ArchiveRemote:    env("ARCHIVE_REMOTE", "gdrive:ai-sandbox"),
		RcloneBin:        env("RCLONE_BIN", "rclone"),
		RcloneConf:       os.Getenv("RCLONE_CONFIG"),
		QuotaBytes:       int64Env("ARCHIVE_QUOTA_BYTES", 10<<30),
		WarnRatio:        floatEnv("ARCHIVE_WARN_RATIO", 0.8),
		HealthInterval:   durationEnv("HEALTH_INTERVAL", 5*time.Minute),
		QueueInterval:    durationEnv("QUEUE_INTERVAL", 10*time.Second),
		AcquireWait:      durationEnv("ACQUIRE_WAIT", 180*time.Second),
		ProvisionTimeout: durationEnv("PROVISION_TIMEOUT", 5*time.Minute),
		ArchiveTimeout:   durationEnv("ARCHIVE_TIMEOUT", 30*time.Minute),
		TelegramToken:    os.Getenv("TELEGRAM_BOT_TOKEN"),
		MaxServers:       int(int64Env("POOL_MAX_SERVERS", 20)),
		WarmSpares:       int(int64Env("POOL_WARM_SPARES", 0)),
		SubdomainPrefix:  env("POOL_SUBDOMAIN_PREFIX", "sbx"),
		PoolName:         env("POOL_NAME", "ai-sandbox"),
	}
	c.TelegramChat, _ = strconv.ParseInt(os.Getenv("TELEGRAM_CHAT_ID"), 10, 64)
	if len(c.Token) < 16 {
		return c, errors.New("POOL_TOKEN must be at least 16 characters")
	}
	if c.PublicURL == "" {
		return c, errors.New("POOL_PUBLIC_URL is required")
	}
	c.ArchiveURL = strings.TrimRight(env("POOL_ARCHIVE_URL", c.PublicURL), "/")
	if !strings.HasPrefix(c.PublicURL, "http") {
		return c, errors.New("POOL_PUBLIC_URL must be an absolute http(s) URL")
	}
	c.KeysDir = filepath.Join(c.StateDir, "keys")
	c.BinDir = filepath.Join(c.StateDir, "bin")
	for _, d := range []string{c.StateDir, c.KeysDir, c.BinDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return c, fmt.Errorf("create %s: %w", d, err)
		}
	}
	if c.ArchiveRemote == "" {
		return c, errors.New("ARCHIVE_REMOTE is required")
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

func floatEnv(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1 {
		return def
	}
	return f
}
