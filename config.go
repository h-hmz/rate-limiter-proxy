package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	algTokenBucket = "tokenbucket"
	algFixedWindow = "fixedwindow"

	storeMemory = "memory"
	storeRedis  = "redis"

	keyIP     = "ip"
	keyAPIKey = "apikey"

	defaultListenPort  = 15001
	defaultMetricsPort = 15090
	defaultKeyHeader   = "X-API-Key"
)

type Config struct {
	AppPort     int // forward target: 127.0.0.1:AppPort
	ListenPort  int
	MetricsPort int // /metrics and /healthz listener, kept off the proxy port so scrapes and probes are never rate limited

	Algorithm string
	Rate      float64       // tokenbucket: tokens/second; fixedwindow: requests per window
	Burst     int64         // tokenbucket only
	Window    time.Duration // fixedwindow only

	Store         string // memory | redis
	RedisAddr     string
	RedisPassword string

	Key       string // ip | apikey
	KeyHeader string // apikey only

	FailOpen bool

	BypassPaths []string // paths that skip the limiter but are still proxied to the app
}

// LoadConfig reads and validates every RL_* variable, collecting all errors
// rather than stopping at the first.
func LoadConfig() (Config, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	parsePort := func(name string, required bool, def int) int {
		raw := os.Getenv(name)
		if raw == "" {
			if required {
				fail("%s is required", name)
			}
			return def
		}
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			fail("%s: %q is not a valid port", name, raw)
			return def
		}
		return p
	}

	cfg := Config{
		AppPort:       parsePort("RL_APP_PORT", true, 0),
		ListenPort:    parsePort("RL_LISTEN_PORT", false, defaultListenPort),
		MetricsPort:   parsePort("RL_METRICS_PORT", false, defaultMetricsPort),
		Algorithm:     os.Getenv("RL_ALGORITHM"),
		Store:         envOr("RL_STORE", storeMemory),
		RedisAddr:     os.Getenv("RL_REDIS_ADDR"),
		RedisPassword: os.Getenv("RL_REDIS_PASSWORD"),
		Key:           envOr("RL_KEY", keyIP),
		KeyHeader:     envOr("RL_KEY_HEADER", defaultKeyHeader),
		FailOpen:      true,
	}

	if raw := os.Getenv("RL_RATE"); raw == "" {
		fail("RL_RATE is required")
	} else if v, err := strconv.ParseFloat(raw, 64); err != nil || v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		fail("RL_RATE: %q must be a number > 0", raw)
	} else {
		cfg.Rate = v
	}

	// tokenbucket takes (rate, burst), fixedwindow takes (tokensPerWindow, windowDuration).
	// RL_RATE covers the first parameter of each; RL_BURST and RL_WINDOW are per-algorithm.
	switch cfg.Algorithm {
	case algTokenBucket:
		if raw := os.Getenv("RL_BURST"); raw == "" {
			fail("RL_BURST is required for algorithm %q", algTokenBucket)
		} else if v, err := strconv.ParseInt(raw, 10, 64); err != nil || v < 1 {
			fail("RL_BURST: %q must be an integer >= 1", raw)
		} else {
			cfg.Burst = v
		}
	case algFixedWindow:
		if cfg.Rate != math.Trunc(cfg.Rate) {
			fail("RL_RATE: %v must be an integer for algorithm %q (requests per window)", cfg.Rate, algFixedWindow)
		}
		if raw := os.Getenv("RL_WINDOW"); raw == "" {
			fail("RL_WINDOW is required for algorithm %q", algFixedWindow)
		} else if d, err := time.ParseDuration(raw); err != nil || d <= 0 {
			fail("RL_WINDOW: %q must be a positive Go duration (e.g. \"30s\", \"1m\")", raw)
		} else {
			cfg.Window = d
		}
	case "":
		fail("RL_ALGORITHM is required (%q or %q)", algTokenBucket, algFixedWindow)
	default:
		fail("RL_ALGORITHM: %q must be %q or %q", cfg.Algorithm, algTokenBucket, algFixedWindow)
	}

	switch cfg.Store {
	case storeMemory:
	case storeRedis:
		if cfg.RedisAddr == "" {
			fail("RL_REDIS_ADDR is required for store %q", storeRedis)
		}
	default:
		fail("RL_STORE: %q must be %q or %q", cfg.Store, storeMemory, storeRedis)
	}

	if cfg.Key != keyIP && cfg.Key != keyAPIKey {
		fail("RL_KEY: %q must be %q or %q", cfg.Key, keyIP, keyAPIKey)
	}

	for p := range strings.SplitSeq(os.Getenv("RL_BYPASS_PATHS"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// A path that does not start with "/" can never equal r.URL.Path, reject at boot.
		if !strings.HasPrefix(p, "/") {
			fail("RL_BYPASS_PATHS: %q must start with %q", p, "/")
			continue
		}
		cfg.BypassPaths = append(cfg.BypassPaths, p)
	}

	if raw := os.Getenv("RL_FAIL_OPEN"); raw != "" {
		if v, err := strconv.ParseBool(raw); err != nil {
			fail("RL_FAIL_OPEN: %q must be a boolean", raw)
		} else {
			cfg.FailOpen = v
		}
	}

	// All three ports live in one network namespace, since the proxy runs as a
	// sidecar beside the app. Any collision is fatal: two processes cannot bind
	// the same port, and an iptables REDIRECT pointed at the app's own port
	// would never reach the proxy.
	if cfg.ListenPort == cfg.AppPort {
		fail("RL_LISTEN_PORT and RL_APP_PORT must differ, both are %d", cfg.ListenPort)
	}
	if cfg.MetricsPort == cfg.AppPort {
		fail("RL_METRICS_PORT and RL_APP_PORT must differ, both are %d", cfg.MetricsPort)
	}
	if cfg.MetricsPort == cfg.ListenPort {
		fail("RL_METRICS_PORT and RL_LISTEN_PORT must differ, both are %d", cfg.MetricsPort)
	}

	return cfg, errors.Join(errs...)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
