package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	rlmiddleware "github.com/h-hmz/rate-limiter/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startBackend runs a local "app" and returns  Config.AppPort
func startBackend(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return port
}

func startProxy(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	lim, err := buildLimiter(cfg)
	require.NoError(t, err)
	reverseProxy := newReverseProxy(cfg.AppPort)

	extractor := buildExtractor(cfg)
	reverseProxyHandler := rlmiddleware.HttpMiddleware(lim, extractor)(reverseProxy)

	srv := httptest.NewServer(reverseProxyHandler)
	t.Cleanup(srv.Close)
	return srv
}

func TestEndToEndTokenBucketLimits(t *testing.T) {
	appPort := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the app")
	})

	proxy := startProxy(t, Config{
		AppPort:   appPort,
		Algorithm: algTokenBucket,
		Rate:      0.1, // one token per 10s: no refill within the test
		Burst:     2,
		Store:     storeMemory,
		Key:       keyIP,
	})

	var codes []int
	var first, limited *http.Response
	for range 10 {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", "203.0.113.9")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
		switch {
		case first == nil:
			first = resp
		case resp.StatusCode == http.StatusTooManyRequests && limited == nil:
			limited = resp
		}
	}

	assert.Equal(t, []int{200, 200, 429, 429, 429, 429, 429, 429, 429, 429}, codes)
	assert.Equal(t, "2", first.Header.Get("X-RateLimit-Limit"))
	require.NotNil(t, limited)
	assert.Equal(t, "10", limited.Header.Get("Retry-After")) // 1/rate seconds
}
