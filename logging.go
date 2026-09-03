package main

import (
	"io"
	"log/slog"
)

const (
	logFormatJSON = "json"
	logFormatText = "text"
)

// setupLogging installs the process-wide logger. It runs before LoadConfig so a
// configuration failure is reported through the same structured stream as
// everything else.
func setupLogging(w io.Writer) {
	format := envOr("RL_LOG_FORMAT", logFormatJSON)

	var h slog.Handler
	switch format {
	case logFormatText:
		h = slog.NewTextHandler(w, nil)
	default:
		h = slog.NewJSONHandler(w, nil)
	}

	slog.SetDefault(slog.New(h))

	// Reported after SetDefault, so the complaint travels through the very
	// logger it is complaining about rather than to a second stream.
	if format != logFormatJSON && format != logFormatText {
		slog.Warn("unknown log format, defaulting to json", "RL_LOG_FORMAT", format)
	}
}
