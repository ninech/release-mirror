package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/klauspost/compress/gzhttp"
	slogsentry "github.com/samber/slog-sentry/v2"
)

// initSentry initializes Sentry and returns a cleanup flush func and an error-level slog.Handler.
func initSentry(dsn *url.URL, debug bool, appName string, info *BuildInfo, logger *slog.Logger) (func(), slog.Handler) {
	if dsn == nil || dsn.String() == "" {
		logger.Debug("no DSN provided", "dsn", dsn)
		return func() {}, nil
	}

	logger.Debug("initializing", "debug", debug)
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn.String(),
		Debug:            debug,
		DebugWriter:      sentryLogWriter{logger: logger, lvl: slog.LevelDebug},
		SendDefaultPII:   true,
		ServerName:       appName,
		Release:          info.Commit,
		AttachStacktrace: true,
		HTTPClient: &http.Client{
			Timeout:   5 * time.Second,
			Transport: gzhttp.Transport(http.DefaultTransport),
		},
	}); err != nil {
		logger.Error("failed to initialize", "err", err)
		return func() {}, nil
	}

	sentryHandler := slogsentry.Option{
		Level:     slog.LevelError,
		AddSource: true,
	}.NewSentryHandler()

	cleanup := func() { sentry.Flush(2 * time.Second) }
	return cleanup, sentryHandler
}

// reportPanic captures an in-flight panic to Sentry and re-panics so the process
// exits abnormally and triggers orchestrator restarts. It must be deferred directly.
func reportPanic() {
	if p := recover(); p != nil {
		sentry.CurrentHub().Recover(p)
		panic(p)
	}
}

// fanoutHandler distributes each log record to underlying handlers.
type fanoutHandler struct {
	handlers []slog.Handler
}

func (h fanoutHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

// Handle passes a clone of the record to each handler to satisfy slog's concurrency contract.
func (h fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for i, handler := range h.handlers {
		if !handler.Enabled(ctx, r.Level) {
			continue
		}
		if err := handler.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, fmt.Errorf("handler %d: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}
	return fanoutHandler{handlers: handlers}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}
	return fanoutHandler{handlers: handlers}
}

// sentryLogWriter adapts an [slog.Logger] to the [io.Writer] interface, used to
// route the Sentry SDK's internal debug output through the structured logger.
type sentryLogWriter struct {
	logger *slog.Logger
	lvl    slog.Level
}

// Write implements the [io.Writer] interface, forwarding log messages to the structured logger.
// It strips the "[Sentry] YYYY/MM/DD HH:MM:SS" prefix that the Sentry SDK prepends via the standard log package.
func (w sentryLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if parts := strings.SplitN(msg, " ", 4); len(parts) == 4 {
		msg = strings.TrimLeft(parts[3], " ")
	}
	w.logger.Log(context.Background(), w.lvl, msg)
	return len(p), nil
}
