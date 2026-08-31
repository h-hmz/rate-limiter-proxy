package main

import (
	"fmt"
	"net/http"
	"strings"

	rlmiddleware "github.com/h-hmz/rate-limiter/middleware"
)

// xffIP is the `ip` KeyExtractor: written on the assumption that X-Forwarded-For is always present
func xffIP(r *http.Request) (string, error) {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return "", fmt.Errorf("missing header: X-Forwarded-For")
	}
	return xff, nil
}

var _ rlmiddleware.KeyExtractor = xffIP

func buildExtractor(cfg Config) rlmiddleware.KeyExtractor {
	switch cfg.Key {
	case keyAPIKey:
		return rlmiddleware.APIKeyHeaderExtractor(cfg.KeyHeader)
	default: // keyIP; LoadConfig rejects anything else
		return xffIP
	}
}
