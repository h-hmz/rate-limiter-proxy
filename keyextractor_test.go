package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXFFIP(t *testing.T) {
	cases := []struct {
		name    string
		xff     string
		set     bool
		want    string
		wantErr bool
	}{
		{name: "the address the ingress wrote", xff: "203.0.113.9", set: true, want: "203.0.113.9"},
		{name: "IPv6", xff: "2001:db8::5", set: true, want: "2001:db8::5"},
		{name: "surrounding whitespace is trimmed", xff: "  203.0.113.9  ", set: true, want: "203.0.113.9"},
		{name: "absent header is an error", set: false, wantErr: true},
		{name: "empty header is an error", xff: "", set: true, wantErr: true},
		{name: "whitespace-only header is an error", xff: "   ", set: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.set {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			key, err := xffIP(r)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, key)
		})
	}
}

func TestSeparateBucketsForClientsWithDifferentXFFs(t *testing.T) {
	appPort := startBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	proxy := startProxy(t, Config{
		AppPort: appPort, Algorithm: algTokenBucket,
		Rate: 0.1, Burst: 1,
		Store: storeMemory, Key: keyIP,
	})

	get := func(client string) int {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", client)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, get("203.0.113.1"))
	assert.Equal(t, http.StatusTooManyRequests, get("203.0.113.1"), "same client, quota spent")
	assert.Equal(t, http.StatusOK, get("203.0.113.2"), "different client must have its own bucket")
}
