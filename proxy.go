package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"

	ratelimiter "github.com/h-hmz/rate-limiter"
	rlmiddleware "github.com/h-hmz/rate-limiter/middleware"
)

func newProxyHandler(cfg Config, limiter ratelimiter.Limiter) http.Handler {
	reverseProxy := newReverseProxy(cfg.AppPort)
	limited := rlmiddleware.HttpMiddleware(limiter, buildExtractor(cfg))(reverseProxy)

	return bypassPaths(cfg.BypassPaths, limited, reverseProxy)
}

// bypassPaths routes paths to the app unlimited. Used mainly for healthcheck probes.
// A trailing slash is trimmed so "/healthz/" and "/healthz" are the same probe.
func bypassPaths(paths []string, limited, unlimited http.Handler) http.Handler {
	trimmed := make([]string, len(paths))
	for i, p := range paths {
		trimmed[i] = strings.TrimSuffix(p, "/")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(trimmed, strings.TrimSuffix(r.URL.Path, "/")) {
			unlimited.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
}

func newReverseProxy(appPort int) http.Handler {
	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)),
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Preserve the original XFF
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			// Preserve the client Host header so vhost routing in the backend still works.
			pr.Out.Host = pr.In.Host
		},
		Transport: http.DefaultTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "ratelimit-proxy: forwarding to 127.0.0.1:%d failed: %v\n", appPort, err)
		},
	}
}
