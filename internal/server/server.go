// Package server implements the HTTP server middleware stack and listener
// lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/felixge/fgprof"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/metrics"
	"github.com/go-chi/traceid"
	"github.com/klauspost/compress/gzhttp"
)

// Server timeout defaults.
const (
	DefaultTimeout         = 30 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
)

// BuildInfo carries build-time version metadata.
type BuildInfo struct {
	Name      string
	Version   string
	Commit    string
	Date      string
	GoVersion string
}

// Option configures a [Server].
type Option func(*Server)

// WithLogger sets the request logger (defaults to slog.Default).
func WithLogger(logger *slog.Logger) Option {
	return func(s *Server) { s.logger = logger }
}

// WithTimeout sets read/write timeout (defaults to DefaultTimeout).
func WithTimeout(d time.Duration) Option {
	return func(s *Server) { s.timeout = d }
}

// WithBuildInfo sets version metadata for build info headers and logs.
func WithBuildInfo(info *BuildInfo) Option {
	return func(s *Server) { s.buildInfo = *info }
}

// WithProfile enables pprof and fgprof profiling endpoints under /debug/.
func WithProfile() Option {
	return func(s *Server) { s.profile = true }
}

// WithMetrics enables request metrics collection. Collection is disabled by
// default; the collected metrics are exposed by mounting [Server.MetricsHandler]
// or by serving [Server.ListenMetrics] on a separate listener.
func WithMetrics() Option {
	return func(s *Server) { s.metrics = true }
}

// WithProfileBasicAuth protects /debug/ endpoints with HTTP Basic Authentication.
func WithProfileBasicAuth(username, password string) Option {
	return func(s *Server) {
		s.profileUsername = username
		s.profilePassword = password
		s.profileAuth = true
	}
}

// WithTrustedProxies sets the number of trusted reverse proxies for X-Forwarded-For parsing (0 uses RemoteAddr).
func WithTrustedProxies(n int) Option {
	return func(s *Server) { s.trustedProxies = n }
}

// Server is the HTTP server. It manages middleware, routing, and listener lifecycle.
// Application handlers registered via [Server.Mount] are not subject to the
// optional profiling authentication.
type Server struct {
	timeout         time.Duration
	shutdownTimeout time.Duration
	logger          *slog.Logger
	buildInfo       BuildInfo
	profile         bool
	profileAuth     bool
	profileUsername string
	profilePassword string
	trustedProxies  int
	metrics         bool

	router   *chi.Mux
	compress func(http.Handler) http.HandlerFunc
}

// New creates a new Server. All options have defaults and may be omitted. It
// fails when the response compression wrapper cannot be built.
func New(opts ...Option) (*Server, error) {
	s := &Server{
		timeout:         DefaultTimeout,
		shutdownTimeout: DefaultShutdownTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}

	if s.logger == nil {
		s.logger = slog.Default()
	}

	compress, err := gzhttp.NewWrapper()
	if err != nil {
		return nil, fmt.Errorf("gzhttp wrapper: %w", err)
	}
	s.compress = compress
	s.router = s.buildRouter()

	return s, nil
}

// MetricsHandler returns the handler that serves collected request metrics.
func (s *Server) MetricsHandler() http.Handler {
	return metrics.Handler()
}

// Mount mounts an application handler at the given pattern.
func (s *Server) Mount(pattern string, handler http.Handler) {
	s.router.Mount(pattern, handler)
}

// Listen serves HTTP on l and blocks until ctx is canceled, then shuts down gracefully.
func (s *Server) Listen(ctx context.Context, l net.Listener) error {
	return s.serve(ctx, l, s.compress(s.router))
}

// ListenMetrics serves the metrics endpoint on l, without the application
// middleware stack. It blocks like [Server.Listen].
func (s *Server) ListenMetrics(ctx context.Context, l net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", s.MetricsHandler())
	return s.serve(ctx, l, mux)
}

// serve runs handler on l until ctx is canceled, then shuts down gracefully.
func (s *Server) serve(ctx context.Context, l net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  s.timeout,
		WriteTimeout: s.timeout,
		// BaseContext uses WithoutCancel so in-flight requests aren't aborted during graceful shutdown.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ErrorLog:    slog.NewLogLogger(s.logger.WithGroup("http.Server").Handler(), slog.LevelError),
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutting down server", "timeout", s.shutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		s.logger.DebugContext(ctx, "shut down successful")
		return nil
	case err := <-errCh:
		return err
	}
}

// buildRouter constructs the root chi router with shared middleware applied.
func (s *Server) buildRouter() *chi.Mux {
	r := chi.NewRouter()

	r.Use(traceid.Middleware)
	r.Use(middleware.CleanPath)
	r.Use(middleware.RedirectSlashes)
	r.Use(middleware.GetHead)
	if s.trustedProxies > 0 {
		r.Use(middleware.ClientIPFromXFFTrustedProxies(s.trustedProxies))
	} else {
		r.Use(middleware.ClientIPFromRemoteAddr)
	}
	if s.metrics {
		r.Use(metrics.Collector(metrics.CollectorOpts{Host: true, Proto: true}))
	}
	r.Use(http.NewCrossOriginProtection().Handler)

	r.Use(httplog.RequestLogger(s.logger.WithGroup("access"), &httplog.Options{
		Level:         slog.LevelInfo,
		RecoverPanics: true,
	}))

	r.Use(middleware.Heartbeat("/healthz"))

	r.Get("/favicon", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if s.profile {
		var profileRouter chi.Router = r
		if s.profileAuth {
			profileRouter = r.With(middleware.BasicAuth("profiling", map[string]string{
				s.profileUsername: s.profilePassword,
			}))
		}
		profileRouter.Mount("/debug", middleware.Profiler())
		profileRouter.Handle("/debug/fgprof", fgprof.Handler())
	}

	return r
}
