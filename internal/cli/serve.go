package cli

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/traceid"
	"github.com/ninech/release-mirror/internal/mirror"
	"github.com/ninech/release-mirror/internal/server"
)

// ServeCmd starts the HTTP server.
type ServeCmd struct {
	S3 `embed:"" prefix:"s3-"`

	Bind            string        `help:"Address to listen on (e.g. :8080)."`
	Metrics         bool          `help:"Enable metrics collection and the /metrics endpoint."`
	MetricsBind     string        `help:"Address to serve metrics on (e.g. :9090). Implies --metrics. Metrics are served on --bind when empty."`
	Profile         bool          `help:"Enable profiling endpoints under /debug/."`
	TrustedProxies  int           `help:"Number of trusted reverse proxies (0 = use RemoteAddr)." default:"0"`
	AllowedPaths    []string      `help:"Host-qualified path patterns allowed to be mirrored, e.g. 'github.com/ninech/*'. Nothing is mirrored when empty." placeholder:"HOST/OWNER/REPO"`
	DeniedPaths     []string      `help:"Path patterns never mirrored, matched at any depth. Set to an empty value to disable." default:"*/releases/latest/*,permalink/latest"`
	KeyPrefix       string        `help:"Key prefix mirrored assets are stored under."`
	MirrorWorkers   int           `help:"Number of assets copied concurrently." default:"4"`
	MirrorQueueSize int           `help:"Number of assets that may wait to be copied." default:"256"`
	MirrorTimeout   time.Duration `help:"Timeout for copying a single asset." default:"10m"`
	MaxAssetSize    int64         `help:"Largest asset that is mirrored, in bytes (0 = no limit)." default:"2147483648"`
	LockTTL         time.Duration `help:"How long a mirror lock is honoured before it may be stolen." default:"15m"`
	LockPrefix      string        `help:"Key prefix lock objects are stored under." default:".locks/"`
}

// Run executes the serve command.
func (s *ServeCmd) Run(ctx context.Context, c *cmd) error {
	logger := slog.New(traceid.LogHandler(c.logger.Handler()))
	addr := s.address()

	opts := []server.Option{
		server.WithTrustedProxies(s.TrustedProxies),
		server.WithLogger(logger.WithGroup("server")),
		server.WithBuildInfo(&c.build),
	}
	if s.Profile {
		logger.WarnContext(ctx, "profiling enabled: /debug/ exposes runtime internals — do not expose to untrusted networks")
		opts = append(opts, server.WithProfile())
	}
	metricsEnabled := s.Metrics || s.MetricsBind != ""
	if metricsEnabled {
		opts = append(opts, server.WithMetrics())
	}
	if c.sentry {
		opts = append(opts, server.WithSentry(s.traceTargets()...))
	}
	srv, err := server.New(opts...)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	m, err := s.mirror(ctx, srv, logger.WithGroup("mirror"))
	if err != nil {
		return err
	}

	// Copies must outlive the triggering request. Workers run alongside the server and stop with context.
	mirrorCtx, stopMirror := context.WithCancel(ctx)
	wait := m.Start(mirrorCtx)
	defer func() {
		stopMirror()
		wait()
	}()

	logger.DebugContext(ctx, "binding", "addr", addr)
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	// The mirror routes on the upstream host itself, so it owns the root. The
	// server's own endpoints are static paths and keep winning over the wildcard.
	srv.Mount("/", m.Handler())

	listeners := []func(context.Context) error{
		func(ctx context.Context) error { return srv.Listen(ctx, l) },
	}
	if metricsEnabled {
		if s.MetricsBind == "" {
			srv.Mount("/metrics", srv.MetricsHandler())
		} else {
			ml, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.MetricsBind)
			if err != nil {
				return fmt.Errorf("listen %s: %w", s.MetricsBind, err)
			}
			logger.InfoContext(ctx, "serving metrics", "addr", ml.Addr())
			listeners = append(listeners, func(ctx context.Context) error { return srv.ListenMetrics(ctx, ml) })
		}
	}

	logger.InfoContext(ctx, "listening", "addr", l.Addr(), "version", c.build.Version)
	if err := serveAll(ctx, listeners...); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// serveAll runs every listener concurrently and returns the first error. The
// first listener to return stops the others: a single listener must never keep
// the process alive on its own.
func serveAll(ctx context.Context, listeners ...func(context.Context) error) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	errs := make(chan error, len(listeners))
	for _, listen := range listeners {
		go func() {
			defer stop()
			errs <- listen(ctx)
		}()
	}

	var first error
	for range listeners {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *ServeCmd) mirror(ctx context.Context, srv *server.Server, logger *slog.Logger) (*mirror.Mirror, error) {
	store, err := s.Client(srv.Transport())
	if err != nil {
		return nil, err
	}

	publicURL, err := s.PublicBaseURL()
	if err != nil {
		return nil, err
	}

	policy, err := mirror.NewPolicy(s.AllowedPaths, s.DeniedPaths)
	if err != nil {
		return nil, fmt.Errorf("build policy: %w", err)
	}
	if policy.Empty() {
		logger.WarnContext(ctx, "no allowed paths configured: requests are redirected but nothing is mirrored")
	}

	m, err := mirror.New(
		store, s.Bucket, publicURL,
		mirror.WithLogger(logger),
		mirror.WithPolicy(policy),
		mirror.WithKeyPrefix(s.KeyPrefix),
		mirror.WithWorkers(s.MirrorWorkers),
		mirror.WithQueueSize(s.MirrorQueueSize),
		mirror.WithCopyTimeout(s.MirrorTimeout),
		mirror.WithMaxAssetSize(s.MaxAssetSize),
		mirror.WithLock(s.LockPrefix, s.LockTTL),
		// Reuses server transport but uses copy timeout for long asset transfers.
		mirror.WithHTTPClient(&http.Client{Transport: srv.Transport(), Timeout: s.MirrorTimeout}),
	)
	if err != nil {
		return nil, fmt.Errorf("create mirror: %w", err)
	}

	logger.InfoContext(ctx, "mirror configured",
		"bucket", s.Bucket,
		"public_url", publicURL,
		"allow", policy.Allowed(),
		"deny", policy.Denied())
	return m, nil
}

// traceTargets returns the hosts Sentry may propagate a trace to. Only the
// object store qualifies: every other outgoing request fetches an asset from an
// upstream release host, which is a third party and has no use for our trace.
// An unparsable endpoint yields no target and is reported by the S3 client.
func (s *ServeCmd) traceTargets() []string {
	host, _, err := s.endpoint()
	if err != nil {
		return nil
	}
	return []string{host}
}

func (s *ServeCmd) address() string {
	return cmp.Or(s.Bind, os.Getenv("ADDR"), ":"+cmp.Or(os.Getenv("PORT"), "8080"))
}
