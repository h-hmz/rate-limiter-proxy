package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

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
