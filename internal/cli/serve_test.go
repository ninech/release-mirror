package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/ninech/release-mirror/internal/mirror"
	"github.com/ninech/release-mirror/internal/server"
)

var errBoom = errors.New("boom")

// listen serves srv on a random loopback port for the duration of the test and
// returns the address it listens on.
func listen(t *testing.T, srv *server.Server) string {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- srv.Listen(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})

	return l.Addr().String()
}

// TestServeMirrorRouting exercises the wiring the way a request arrives in
// production: through the server middleware stack, into the mirror mounted under
// the upstream host. It is the only place where the interaction between the two is
// visible, and it guards against middleware that rewrites the routed path.
func TestServeMirrorRouting(t *testing.T) {
	t.Parallel()

	cmd := &ServeCmd{
		S3:           S3{Endpoint: "s3.example.com", Bucket: "assets"},
		AllowedPaths: []string{"github.com/ninech/*", "codeberg.org/ninech/*"},
		DeniedPaths:  mirror.DefaultDeniedPaths,
	}

	srv, err := server.New(server.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	m, err := cmd.mirror(t.Context(), srv, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("create mirror: %v", err)
	}

	srv.Mount("/", m.Handler())
	addr := listen(t, srv)

	// Redirects must be observed, not followed: the bucket does not exist here.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	for name, tc := range map[string]struct {
		path         string
		wantStatus   int
		wantLocation string
	}{
		"github asset": {
			path:         "/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://assets.s3.example.com/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"codeberg asset": {
			path:         "/codeberg.org/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://assets.s3.example.com/codeberg.org/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		// GitLab nests namespaces arbitrarily and puts its own separator in the
		// path. Passing the path through unparsed is what makes this work.
		"gitlab asset": {
			path:         "/gitlab.com/group/sub/proj/-/releases/v1.0.0/downloads/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://assets.s3.example.com/gitlab.com/group/sub/proj/-/releases/v1.0.0/downloads/foo.tar.gz",
		},
		"double extension is preserved": {
			path:         "/github.com/ninech/foo/releases/download/v1.0.0/checksums.txt.sig",
			wantStatus:   http.StatusFound,
			wantLocation: "https://assets.s3.example.com/github.com/ninech/foo/releases/download/v1.0.0/checksums.txt.sig",
		},
		"no extension": {
			path:         "/github.com/ninech/foo/releases/download/v1.0.0/foo",
			wantStatus:   http.StatusFound,
			wantLocation: "https://assets.s3.example.com/github.com/ninech/foo/releases/download/v1.0.0/foo",
		},
		"host without a path": {
			path:       "/github.com",
			wantStatus: http.StatusNotFound,
		},
		"not a host": {
			path:       "/healthz-ish/foo.tar.gz",
			wantStatus: http.StatusNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+tc.path, http.NoBody)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Location"); got != tc.wantLocation {
				t.Errorf("Location = %q, want %q", got, tc.wantLocation)
			}
		})
	}
}

// TestServeServerEndpointsSurviveRootMount guards the one risk of mounting the
// mirror at the root: the server's own static endpoints must still be reachable
// rather than being swallowed by the /{host}/* route.
func TestServeServerEndpointsSurviveRootMount(t *testing.T) {
	t.Parallel()

	cmd := &ServeCmd{
		S3:           S3{Endpoint: "s3.example.com", Bucket: "assets"},
		AllowedPaths: []string{"github.com/ninech/*"},
	}

	srv, err := server.New(server.WithLogger(slog.New(slog.DiscardHandler)), server.WithMetrics())
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	m, err := cmd.mirror(t.Context(), srv, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("create mirror: %v", err)
	}
	srv.Mount("/", m.Handler())
	srv.Mount("/metrics", srv.MetricsHandler())
	addr := listen(t, srv)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, path := range []string{"/healthz", "/metrics", "/favicon"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, http.NoBody)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= http.StatusMultipleChoices {
			t.Errorf("GET %s = %d, want the server endpoint to answer", path, resp.StatusCode)
		}
	}
}

// TestServeDefaultDeniedPaths keeps the flag default and the package default in
// step: the help text and the documented behaviour are the same list.
func TestServeDefaultDeniedPaths(t *testing.T) {
	t.Parallel()

	var cli root
	k, err := kong.New(&cli)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := k.Parse([]string{"serve", "--s3-bucket=assets"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(cli.Serve.DeniedPaths, mirror.DefaultDeniedPaths) {
		t.Errorf("--denied-paths default = %q, want %q", cli.Serve.DeniedPaths, mirror.DefaultDeniedPaths)
	}
}

func TestServeMirrorRejectsBadConfiguration(t *testing.T) {
	t.Parallel()

	srv, err := server.New(server.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	for name, cmd := range map[string]*ServeCmd{
		"no bucket":                {S3: S3{Endpoint: "s3.example.com"}},
		"bad endpoint":             {S3: S3{Endpoint: "s3://assets", Bucket: "assets"}},
		"malformed allow patterns": {S3: S3{Endpoint: "s3.example.com", Bucket: "assets"}, AllowedPaths: []string{"github.com/ninech/[a-"}},
		"malformed deny patterns":  {S3: S3{Endpoint: "s3.example.com", Bucket: "assets"}, DeniedPaths: []string{"[a-"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := cmd.mirror(t.Context(), srv, slog.New(slog.DiscardHandler)); err == nil {
				t.Error("mirror() = nil error, want a configuration error")
			}
		})
	}
}

// TestServeAll guards the invariant that no listener keeps the process alive on
// its own: whichever returns first must bring the others down.
func TestServeAll(t *testing.T) {
	t.Parallel()

	t.Run("first error stops the remaining listeners", func(t *testing.T) {
		t.Parallel()

		stopped := make(chan struct{})
		err := serveAll(
			t.Context(),
			func(context.Context) error { return errBoom },
			func(ctx context.Context) error {
				<-ctx.Done()
				close(stopped)
				return nil
			},
		)
		if !errors.Is(err, errBoom) {
			t.Errorf("serveAll() = %v, want %v", err, errBoom)
		}
		select {
		case <-stopped:
		default:
			t.Error("serveAll() returned while a listener was still running")
		}
	})

	t.Run("cancellation shuts down cleanly", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var served atomic.Int64
		listeners := make([]func(context.Context) error, 2)
		for i := range listeners {
			listeners[i] = func(ctx context.Context) error {
				<-ctx.Done()
				served.Add(1)
				return nil
			}
		}
		if err := serveAll(ctx, listeners...); err != nil {
			t.Errorf("serveAll() = %v, want nil", err)
		}
		if got := served.Load(); got != int64(len(listeners)) {
			t.Errorf("%d listeners returned, want %d", got, len(listeners))
		}
	})
}

// TestServeAddress cannot run in parallel: the subtests set environment variables.
func TestServeAddress(t *testing.T) {
	t.Run("bind flag wins", func(t *testing.T) {
		t.Setenv("ADDR", ":9999")
		if got := (&ServeCmd{Bind: ":1234"}).address(); got != ":1234" {
			t.Errorf("address() = %q, want :1234", got)
		}
	})

	t.Run("ADDR", func(t *testing.T) {
		t.Setenv("ADDR", ":9999")
		if got := (&ServeCmd{}).address(); got != ":9999" {
			t.Errorf("address() = %q, want :9999", got)
		}
	})

	t.Run("PORT", func(t *testing.T) {
		t.Setenv("ADDR", "")
		t.Setenv("PORT", "3000")
		if got := (&ServeCmd{}).address(); got != ":3000" {
			t.Errorf("address() = %q, want :3000", got)
		}
	})

	t.Run("default", func(t *testing.T) {
		t.Setenv("ADDR", "")
		t.Setenv("PORT", "")
		if got := (&ServeCmd{}).address(); got != ":8080" {
			t.Errorf("address() = %q, want :8080", got)
		}
	})
}
