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
		Listen:           listenAddr(),
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
	if c.Token == "" {
		// A hosted deployment has no way to hand over a secret before the
		// first boot: keep one in the data volume instead.
		c.Token = tokenFromFile(c)
	}
	if len(c.Token) < 16 {
		return c, errors.New("POOL_TOKEN must be at least 16 characters")
	}
	if c.PublicURL == "" {
		logf("warning: POOL_PUBLIC_URL is not set, the public address is taken from the request headers")
	}
	c.ArchiveURL = strings.TrimRight(env("POOL_ARCHIVE_URL", c.PublicURL), "/")
	if c.PublicURL != "" && !strings.HasPrefix(c.PublicURL, "http") {
		return c, errors.New("POOL_PUBLIC_URL must be an absolute http(s) URL")
	}
	c.KeysDir = env("POOL_KEYS_DIR", filepath.Join(c.StateDir, "keys"))
	c.BinDir = env("POOL_BIN_DIR", filepath.Join(c.StateDir, "bin"))
	for _, d := range []string{c.StateDir, c.KeysDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return c, fmt.Errorf("create %s: %w", d, err)
		}
	}
	// The binary directory may live in the image (read-only): only check it.
	if st, err := os.Stat(c.BinDir); err != nil || !st.IsDir() {
		logf("warning: binary directory %s is not available", c.BinDir)
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

// listenAddr picks the address to bind: an explicit POOL_LISTEN, otherwise the
// platform PORT (Deplexo and friends), otherwise the classic :80.
func listenAddr() string {
	if v := strings.TrimSpace(os.Getenv("POOL_LISTEN")); v != "" {
		return v
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return "0.0.0.0:" + port
	}
	return ":80"
}

// tokenFromFile reads the pool token from the data volume and creates one when
// it is not there yet.
func tokenFromFile(c Config) string {
	path := env("POOL_TOKEN_FILE", filepath.Join(c.StateDir, "pool.token"))
	if b, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(b)); len(token) >= 16 {
			return token
		}
	}
	token := randomToken(32)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		logf("warning: cannot create %s: %v", filepath.Dir(path), err)
		return token
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		logf("warning: cannot write %s: %v", path, err)
		return token
	}
	logf("generated a new pool token in %s", path)
	return token
}
