// Package main is the entry point for the release-mirror CLI.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/lmittmann/tint"
	"github.com/ninech/release-mirror/internal/cli"
	"golang.org/x/term"
)

const name = "release-mirror"

var (
	version   string
	commit    string
	date      string
	goVersion string
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger, lvl := initLogger(os.Stderr)

	// Bind GOMEMLIMIT to the cgroup memory limit so the Go GC respects container ceilings.
	if _, err := memlimit.SetGoMemLimitWithOpts(); err != nil {
		logger.DebugContext(ctx, "failed to set memory limit", "err", err)
	}

	if err := cli.Run(ctx, logger, lvl, buildInfo(), os.Args); err != nil {
		logger.ErrorContext(ctx, err.Error())
		return 1
	}
	return 0
}

func initLogger(w *os.File) (*slog.Logger, *slog.LevelVar) {
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelInfo)

	_, noColor := os.LookupEnv("NO_COLOR") // adheres no-color.org
	isTTY := term.IsTerminal(int(w.Fd()))

	return slog.New(tint.NewTextHandler(w, &tint.Options{
		Level:     lvl,
		NoColor:   noColor || !isTTY || os.Getenv("TERM") == "dumb",
		AddSource: true,
	})), lvl
}

// buildInfo returns version metadata from goreleaser ldflags, falling back to embedded debug build info.
func buildInfo() *cli.BuildInfo {
	b := &cli.BuildInfo{
		Name:      name,
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: goVersion,
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}

	if b.Version == "" {
		b.Version = info.Main.Version
	}
	if b.GoVersion == "" {
		b.GoVersion = info.GoVersion
	}

	for _, kv := range info.Settings {
		switch kv.Key {
		case "vcs.revision":
			if b.Commit == "" {
				b.Commit = kv.Value
			}
		case "vcs.time":
			if b.Date == "" {
				b.Date = kv.Value
			}
		}
	}

	return b
}
