package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestProfileBasicAuthOnlyGuardsProfiling(t *testing.T) {
	t.Parallel()
	s, err := New(
		WithProfile(),
		WithProfileBasicAuth("profiler", "secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	s.Mount("/app", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("application endpoint is public", func(t *testing.T) {
		t.Parallel()
		res := serve(t, s.router, "/app", "", "")
		if res.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
		}
	})

	t.Run("profiling rejects missing credentials", func(t *testing.T) {
		t.Parallel()
		res := serve(t, s.router, "/debug/pprof/goroutine", "", "")
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
		}
		if got := res.Header().Get("WWW-Authenticate"); got != `Basic realm="profiling"` {
			t.Fatalf("WWW-Authenticate = %q", got)
		}
	})

	t.Run("profiling rejects incorrect credentials", func(t *testing.T) {
		t.Parallel()
		res := serve(t, s.router, "/debug/pprof/goroutine", "profiler", "wrong")
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
		}
	})

	t.Run("profiling accepts correct credentials", func(t *testing.T) {
		t.Parallel()
		res := serve(t, s.router, "/debug/pprof/goroutine", "profiler", "secret")
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
		}
	})

	t.Run("fgprof is guarded", func(t *testing.T) {
		t.Parallel()
		res := serve(t, s.router, "/debug/fgprof", "", "")
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
		}
	})
}

func TestProfileWithoutBasicAuthIsPublic(t *testing.T) {
	t.Parallel()
	s, err := New(WithProfile())
	if err != nil {
		t.Fatal(err)
	}

	res := serve(t, s.router, "/debug/pprof/goroutine", "", "")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
}

func TestProfileDisabled(t *testing.T) {
	t.Parallel()
	s, err := New(WithProfileBasicAuth("profiler", "secret"))
	if err != nil {
		t.Fatal(err)
	}

	res := serve(t, s.router, "/debug/pprof/goroutine", "profiler", "secret")
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

// TestMountedHandlerSeesFullPath guards the middleware stack against rewriting
// the path it routes. Mounted handlers route file names, so an extension must
// never be stripped for content negotiation: "foo.tar.gz" has to stay
// "foo.tar.gz" and not become "foo.tar".
func TestMountedHandlerSeesFullPath(t *testing.T) {
	t.Parallel()
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}

	routed := make(chan string, 1)
	mounted := chi.NewRouter()
	mounted.Get("/{owner}/*", func(w http.ResponseWriter, r *http.Request) {
		routed <- chi.URLParam(r, "*")
		w.WriteHeader(http.StatusNoContent)
	})
	s.Mount("/app", mounted)

	for _, want := range []string{
		"v1.0.0/foo.tar.gz",
		"v1.0.0/checksums.txt",
		"v1.0.0/foo",
	} {
		if res := serve(t, s.router, "/app/ninech/"+want, "", ""); res.Code != http.StatusNoContent {
			t.Fatalf("GET /app/ninech/%s = %d, want %d", want, res.Code, http.StatusNoContent)
		}
		if got := <-routed; got != want {
			t.Errorf("mounted handler routed %q, want %q", got, want)
		}
	}
}

// TestListenMetrics guards the isolation of the metrics listener: it serves the
// metrics endpoint and nothing else, and enabling metrics does not expose them
// on the application listener.
func TestListenMetrics(t *testing.T) {
	t.Parallel()
	s, err := New(WithMetrics(), WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- s.ListenMetrics(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve metrics: %v", err)
		}
	})

	for path, want := range map[string]int{
		"/metrics": http.StatusOK,
		"/healthz": http.StatusNotFound,
		"/":        http.StatusNotFound,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+l.Addr().String()+path, http.NoBody)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// The application router only serves metrics when they are mounted explicitly.
	if res := serve(t, s.router, "/metrics", "", ""); res.Code != http.StatusNotFound {
		t.Errorf("GET /metrics on application router = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func serve(t *testing.T, handler http.Handler, path, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}
