package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// namedHandler is a test HTTP handler that responds with its configured name.
type namedHandler struct{ name string }

func (m namedHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(m.name))
}

func TestBypassPathsRoutesExactMatchesAroundTheLimiter(t *testing.T) {
	// "/readyz/" is configured with a trailing slash, "/healthz" without, so trim is covered.
	h := bypassPaths([]string{"/healthz", "/readyz/"}, namedHandler{"limited"}, namedHandler{"unlimited"})

	cases := []struct {
		path string
		want string
	}{
		{"/healthz", "unlimited"},
		{"/healthz/", "unlimited"},
		{"/readyz/", "unlimited"},
		{"/readyz", "unlimited"},
		{"/", "limited"},
		{"/healthz/sub", "limited"}, // bypassing a subtree would be a limiter hole
		{"/healthzz", "limited"},    // trimming must not turn into a prefix match
		{"/other", "limited"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			assert.Equal(t, tc.want, rec.Body.String())
		})
	}
}

func TestBypassedProbeReachesTheAppWithoutXFF(t *testing.T) {
	appPort := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("app says ok"))
	})
	cfg := Config{
		AppPort: appPort, Algorithm: algTokenBucket,
		Rate: 1, Burst: 1,
		Store: storeMemory, Key: keyIP,
		BypassPaths: []string{"/healthz"},
	}
	proxy := startProxy(t, cfg)

	get := func(path string) (int, string) {
		resp, err := http.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}

	code, body := get("/healthz")
	assert.Equal(t, http.StatusOK, code, "a probe without XFF must not be rejected")
	assert.Equal(t, "app says ok", body, "and it must reach the app, not the proxy")

	code, _ = get("/")
	assert.Equal(t, http.StatusBadRequest, code, "ordinary traffic without XFF is still rejected")
}
