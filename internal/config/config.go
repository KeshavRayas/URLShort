// Package config loads and validates process configuration from the environment.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPepper is only acceptable outside production; Load rejects it when
// APP_ENV=production so a deployment cannot silently ship the dev value.
const DefaultPepper = "dev-pepper-change-me"

// BootstrapUser is an account seeded at startup. It exists so a fresh
// deployment has at least one credential; it is not a user-management path.
type BootstrapUser struct {
	Name   string
	APIKey string
}

// Config is the fully resolved runtime configuration.
type Config struct {
	Env           string
	HTTPAddr      string
	PublicBaseURL *url.URL

	DatabaseURL   string
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	APIKeyPepper     string
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration

	RateLimitPerMinute int
	ClickBuffer        int
	ShutdownTimeout    time.Duration

	AllowPrivateTargets bool
	LogLevel            string
	BootstrapUsers      []BootstrapUser
	TrustedProxyCIDRs   []*net.IPNet
}

// Load reads the environment and returns a Config, or an error describing the
// first invalid value. Every setting has a development-friendly default so
// `go run ./cmd/api` works against docker-compose without extra setup.
func Load() (*Config, error) {
	cfg := &Config{
		Env:                 getenv("APP_ENV", "development"),
		HTTPAddr:            getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:         getenv("DATABASE_URL", "postgres://shortener:shortener@localhost:5432/shortener?sslmode=disable"),
		RedisAddr:           getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:       os.Getenv("REDIS_PASSWORD"),
		RedisDB:             getint("REDIS_DB", 0),
		APIKeyPepper:        getenv("API_KEY_PEPPER", DefaultPepper),
		CacheTTL:            getdur("CACHE_TTL", 24*time.Hour),
		NegativeCacheTTL:    getdur("NEGATIVE_CACHE_TTL", 30*time.Second),
		RateLimitPerMinute:  getint("RATE_LIMIT_PER_MINUTE", 120),
		ClickBuffer:         getint("CLICK_BUFFER", 2048),
		ShutdownTimeout:     getdur("SHUTDOWN_TIMEOUT", 15*time.Second),
		AllowPrivateTargets: getbool("ALLOW_PRIVATE_TARGETS", false),
		LogLevel:            getenv("LOG_LEVEL", "info"),
	}

	base, err := url.Parse(getenv("PUBLIC_BASE_URL", "http://localhost:8080"))
	if err != nil {
		return nil, fmt.Errorf("PUBLIC_BASE_URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("PUBLIC_BASE_URL must use http or https, got %q", base.Scheme)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("PUBLIC_BASE_URL must include a host")
	}
	cfg.PublicBaseURL = base

	if cfg.Env == "production" && cfg.APIKeyPepper == DefaultPepper {
		return nil, fmt.Errorf("API_KEY_PEPPER must be set to a secret value when APP_ENV=production")
	}
	if cfg.RateLimitPerMinute < 1 {
		return nil, fmt.Errorf("RATE_LIMIT_PER_MINUTE must be positive")
	}
	if cfg.ClickBuffer < 1 {
		return nil, fmt.Errorf("CLICK_BUFFER must be positive")
	}

	users, err := parseBootstrap(os.Getenv("BOOTSTRAP_USERS"))
	if err != nil {
		return nil, err
	}
	cfg.BootstrapUsers = users

	proxies, err := parseCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return nil, err
	}
	cfg.TrustedProxyCIDRs = proxies

	return cfg, nil
}

// parseCIDRs reads a comma-separated list of networks. Only peers inside these
// ranges may set X-Forwarded-For; an empty list means every forwarded header is
// ignored, which is the safe default for a directly exposed process.
func parseCIDRs(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS entry %q: %w", part, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// parseBootstrap parses "name:key,name:key". Keys are opaque strings; they must
// not contain a comma, and a ":" would split the pair incorrectly.
func parseBootstrap(raw string) ([]BootstrapUser, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []BootstrapUser
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, key, ok := strings.Cut(part, ":")
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if !ok || name == "" || key == "" {
			return nil, fmt.Errorf("BOOTSTRAP_USERS entry %q must be name:key", part)
		}
		if len(key) < 8 {
			return nil, fmt.Errorf("BOOTSTRAP_USERS key for %q must be at least 8 characters", name)
		}
		out = append(out, BootstrapUser{Name: name, APIKey: key})
	}
	return out, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getint(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getdur(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func getbool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
