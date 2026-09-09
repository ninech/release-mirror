// Package cli provides a command-line interface for the application.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"

	"github.com/alecthomas/kong"
)

type root struct {
	Verbose                bool     `help:"Enable debug mode."`
	SentryDSN              *url.URL `help:"Sentry DSN for error reporting."`
	SentryAppName          string   `help:"Application name for error reporting." env:"RELEASE_MIRROR_SENTRY_APP_NAME,DEPLOIO_APP_NAME"`
	SentryTracesSampleRate float64  `help:"Fraction of requests traced and reported to Sentry (0 disables tracing)." default:"0"`

	Serve       ServeCmd       `cmd:"" default:"withargs" help:"Serve the release mirror."`
	HealthCheck HealthCheckCmd `cmd:"" help:"Check whether the server is healthy."`
	Version     VersionCmd     `cmd:"" help:"Print the version of the application."`
}

// Run is the application entry point. It wires dependencies and executes the selected subcommand.
func Run(ctx context.Context, logger *slog.Logger, lvl *slog.LevelVar, info *BuildInfo, args []string) error {
	c := cmd{
		logger: logger,
		build:  *info,
		w:      os.Stdout,
	}

	var cli root
	k, err := kong.New(&cli, kong.BindTo(ctx, (*context.Context)(nil)), kong.DefaultEnvars("RELEASE_MIRROR"))
	if err != nil {
		return fmt.Errorf("init cli: %w", err)
	}

	kctx, err := k.Parse(args[1:])
	if err != nil {
		return fmt.Errorf("parse args: %w", err)
	}

	if cli.Verbose {
		lvl.Set(slog.LevelDebug)
	}

	cleanup, sentryHandler := initSentry(sentryOptions{
		dsn:              cli.SentryDSN,
		debug:            cli.Verbose,
		appName:          cli.SentryAppName,
		tracesSampleRate: cli.SentryTracesSampleRate,
	}, info, logger.WithGroup("sentry"))
	defer cleanup()

	// Defers run LIFO: reportPanic executes before Sentry flush cleanup.
	defer reportPanic()

	if sentryHandler != nil {
		c.logger = slog.New(fanoutHandler{handlers: []slog.Handler{logger.Handler(), sentryHandler}})
		c.sentry = true
	}

	logger.InfoContext(ctx, "starting",
		"name", info.Name,
		"version", info.Version,
		"commit", info.Commit,
		"date", info.Date,
		"go", info.GoVersion)

	return kctx.Run(ctx, &c) //nolint:wrapcheck // Kong dispatch error is returned directly.
}

type cmd struct {
	logger *slog.Logger
	build  BuildInfo
	w      io.Writer
	sentry bool
}
