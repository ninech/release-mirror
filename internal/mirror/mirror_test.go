package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/traceid"
)

// errUploadFailed stands in for a store that rejects an upload.
var errUploadFailed = errors.New("upload failed")

// upstreamHandler serves body for every request, like github.com would for an
// existing asset.
func upstreamHandler(contentType, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write([]byte(body))
	})
}

// testHost stands in for a real forge. The mirror builds real upstream URLs, so
// requests to it are rerouted to a test server rather than the network.
const testHost = "github.com"

// upstreamClient returns a client that serves requests to [testHost] from h, so
// the real upstream URLs built by the mirror can be exercised against a test server.
func upstreamClient(t *testing.T, h http.Handler) *http.Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &http.Client{Transport: &hostRewriter{base: base, rt: http.DefaultTransport}}
}

// hostRewriter redirects requests for [testHost] to base.
type hostRewriter struct {
	base *url.URL
	rt   http.RoundTripper
}

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == testHost {
		req = req.Clone(req.Context())
		req.URL.Scheme = h.base.Scheme
		req.URL.Host = h.base.Host
		req.Host = ""
	}

	resp, err := h.rt.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("round trip %s: %w", req.URL, err)
	}
	return resp, nil
}

func mustPolicy(t *testing.T, allow, deny []string) *Policy {
	t.Helper()

	p, err := NewPolicy(allow, deny)
	if err != nil {
		t.Fatalf("NewPolicy(%q, %q): %v", allow, deny, err)
	}
	return p
}

// testAsset builds an asset on [testHost] below the given repository.
func testAsset(repository, rest string) Asset {
	return Asset{Host: testHost, Path: repository + "/releases/download/" + rest}
}

// newTestMirror builds a mirror storing into store, downloading from upstream and
// redirecting to https://cdn.example.com. Only "github.com/ninech/*" is allowed,
// with the default deny list applied, unless opts say otherwise.
func newTestMirror(t *testing.T, store Store, upstream http.Handler, opts ...Option) *Mirror {
	t.Helper()

	public, err := url.Parse("https://cdn.example.com")
	if err != nil {
		t.Fatalf("parse public URL: %v", err)
	}

	base := []Option{
		WithLogger(testLogger()),
		WithPolicy(mustPolicy(t, []string{testHost + "/ninech/*"}, DefaultDeniedPaths)),
	}
	if upstream != nil {
		base = append(base, WithHTTPClient(upstreamClient(t, upstream)))
	}

	m, err := New(store, "bucket", public, append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestNew(t *testing.T) {
	t.Parallel()

	public, err := url.Parse("https://cdn.example.com/base")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for name, tc := range map[string]struct {
		store     Store
		bucket    string
		publicURL *url.URL
		wantErr   bool
	}{
		"ok":            {newFakeStore(), "bucket", public, false},
		"no store":      {nil, "bucket", public, true},
		"no bucket":     {newFakeStore(), "", public, true},
		"no public URL": {newFakeStore(), "bucket", nil, true},
		"relative URL":  {newFakeStore(), "bucket", &url.URL{Path: "/base"}, true},
		"bad scheme":    {newFakeStore(), "bucket", &url.URL{Scheme: "s3", Host: "bucket"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tc.store, tc.bucket, tc.publicURL)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("New() error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestPublicURL(t *testing.T) {
	t.Parallel()

	asset := testAsset("ninech/foo", "v1.0.0/foo.tar.gz")

	for name, tc := range map[string]struct {
		public string
		opts   []Option
		want   string
	}{
		"bucket host": {
			public: "https://bucket.s3.example.com",
			want:   "https://bucket.s3.example.com/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"path style": {
			public: "https://s3.example.com/bucket",
			want:   "https://s3.example.com/bucket/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"trailing slash": {
			public: "https://s3.example.com/bucket/",
			want:   "https://s3.example.com/bucket/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"key prefix": {
			public: "https://cdn.example.com",
			opts:   []Option{WithKeyPrefix("mirror/")},
			want:   "https://cdn.example.com/mirror/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			public, err := url.Parse(tc.public)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.public, err)
			}
			m, err := New(newFakeStore(), "bucket", public, tc.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := m.PublicURL(asset); got != tc.want {
				t.Errorf("PublicURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandlerRedirects(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		method       string
		path         string
		wantStatus   int
		wantLocation string
	}{
		"asset": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"tag with slash": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/release/v1/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/release/v1/foo.tar.gz",
		},
		"escaped asset name": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1/my%20asset.zip",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1/my%20asset.zip",
		},
		// The name contains a literal percent sequence, so the request carries it
		// double-encoded. Decoding it twice would silently address the different
		// object ".../v1/foo/bar.zip".
		"asset name containing a percent encoding": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1/foo%252Fbar.zip",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1/foo%252Fbar.zip",
		},
		// Here the separator really is encoded, so chi routes on RawPath and the
		// parameter arrives escaped: decoding it once is what makes the traversal
		// visible to ParseAsset.
		"encoded separator in the asset name": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1/foo%2Fbar.zip",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1/foo/bar.zip",
		},
		"percent that is not an encoding": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1/100%25.zip",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1/100%25.zip",
		},
		// A repository outside the allow list is still redirected: the allow list
		// only decides what is copied, never what is answered.
		"disallowed repository is redirected": {
			method: http.MethodGet, path: "/github.com/other/foo/releases/download/v1/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/other/foo/releases/download/v1/foo.tar.gz",
		},
		// Nothing is assumed about the upstream layout any more, so a path that
		// is not a release download is passed through like any other. Whether it
		// is ever copied is decided by the Policy, not by the router.
		"short path": {
			method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/v1.0.0",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/releases/download/v1.0.0",
		},
		"not a download path": {
			method: http.MethodGet, path: "/github.com/ninech/foo/archive/v1.0.0.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/github.com/ninech/foo/archive/v1.0.0.tar.gz",
		},
		"another forge": {
			method: http.MethodGet, path: "/gitlab.com/group/sub/proj/-/releases/v1/downloads/foo.tar.gz",
			wantStatus:   http.StatusFound,
			wantLocation: "https://cdn.example.com/gitlab.com/group/sub/proj/-/releases/v1/downloads/foo.tar.gz",
		},
		"host only":         {method: http.MethodGet, path: "/github.com", wantStatus: http.StatusNotFound},
		"invalid host":      {method: http.MethodGet, path: "/not-a-host/ninech/foo.tar.gz", wantStatus: http.StatusNotFound},
		"ip host":           {method: http.MethodGet, path: "/169.254.169.254/latest/meta-data", wantStatus: http.StatusNotFound},
		"traversal in path": {method: http.MethodGet, path: "/github.com/ninech/foo/releases/download/..%2F..%2Fx/foo.tar.gz", wantStatus: http.StatusNotFound},
		"post rejected":     {method: http.MethodPost, path: "/github.com/ninech/foo/releases/download/v1/foo.tar.gz", wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newTestMirror(t, newFakeStore(), nil)
			rec := httptest.NewRecorder()
			m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, http.NoBody))

			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLocation {
				t.Errorf("Location = %q, want %q", got, tc.wantLocation)
			}
			if tc.wantStatus == http.StatusFound && rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestHandlerRedirectsBeforeMirroring(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	m := newTestMirror(t, store, nil)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/github.com/ninech/foo/releases/download/v1/foo.tar.gz", http.NoBody))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("objects written during the request = %q, want none", keys)
	}
	if want := 1; len(m.jobs) != want {
		t.Errorf("queued assets = %d, want %d", len(m.jobs), want)
	}
}

func TestMirrorCopiesAsset(t *testing.T) {
	t.Parallel()

	const body = "release payload"
	store := newFakeStore()
	m := newTestMirror(t, store, upstreamHandler("application/gzip", body))
	asset := testAsset("ninech/foo", "v1.0.0/foo.tar.gz")
	key := m.Key(asset)

	if err := m.mirror(t.Context(), testLogger(), asset, key); err != nil {
		t.Fatalf("mirror: %v", err)
	}

	obj, ok := store.get(key)
	if !ok {
		t.Fatalf("asset not stored under %q, have %q", key, store.keys())
	}
	if string(obj.data) != body {
		t.Errorf("stored body = %q, want %q", obj.data, body)
	}
	if obj.contentType != "application/gzip" {
		t.Errorf("content type = %q, want application/gzip", obj.contentType)
	}
	if obj.cacheControl != DefaultCacheControl {
		t.Errorf("cache control = %q, want %q", obj.cacheControl, DefaultCacheControl)
	}
	if want := "https://github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz"; obj.metadata["Source"] != want {
		t.Errorf("source metadata = %q, want %q", obj.metadata["Source"], want)
	}
	if keys := store.keys(); len(keys) != 1 {
		t.Errorf("objects after mirroring = %q, want only the asset (lock released)", keys)
	}
}

// TestProcessTracesCopy asserts a copy runs under a trace ID of its own, and that
// the same ID reaches the lock object and the request sent to upstream.
func TestProcessTracesCopy(t *testing.T) {
	t.Parallel()

	asset := testAsset("ninech/foo", "v1.0.0/foo.tar.gz")
	store := newFakeStore()

	var upstreamTrace, lockOwner string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamTrace = r.Header.Get(traceid.Header)

		// The lock is held for the duration of the copy, so it is readable here.
		if obj, ok := store.get(DefaultLockPrefix + asset.Key() + ".lock"); ok {
			var body struct {
				Owner string `json:"owner"`
			}
			if err := json.Unmarshal(obj.data, &body); err != nil {
				t.Errorf("decode lock body %q: %v", obj.data, err)
			}
			lockOwner = body.Owner
		}
		_, _ = w.Write([]byte("payload"))
	})

	// Wrap the client the way the server does, so the outgoing trace ID header is set.
	m := newTestMirror(t, store, nil, WithHTTPClient(&http.Client{
		Transport: traceid.Transport(upstreamClient(t, upstream).Transport),
	}))
	m.process(t.Context(), testLogger(), asset)

	if upstreamTrace == "" {
		t.Fatal("upstream request carried no trace ID")
	}
	if lockOwner != upstreamTrace {
		t.Errorf("lock owner = %q, want the trace ID of the copy %q", lockOwner, upstreamTrace)
	}
}

func TestMirrorSkips(t *testing.T) {
	t.Parallel()

	asset := testAsset("ninech/foo", "v1.0.0/foo.tar.gz")
	other := testAsset("other/foo", "v1.0.0/foo.tar.gz")

	for name, tc := range map[string]struct {
		asset    Asset
		upstream http.Handler
		opts     []Option
		prepare  func(t *testing.T, m *Mirror, store *fakeStore)
		wantErr  error
	}{
		"path not allowed": {
			asset:    other,
			upstream: upstreamHandler("", "payload"),
			wantErr:  errNotAllowed,
		},
		// The default deny list rejects the "newest release" aliases, which the
		// mirror cannot serve honestly: it copies once and marks the object
		// immutable, while the alias moves.
		"path denied": {
			asset:    Asset{Host: testHost, Path: "ninech/foo/releases/latest/download/foo.tar.gz"},
			upstream: upstreamHandler("", "payload"),
			wantErr:  errDenied,
		},
		"already mirrored": {
			asset:    asset,
			upstream: upstreamHandler("", "payload"),
			prepare: func(_ *testing.T, m *Mirror, store *fakeStore) {
				store.put(m.Key(asset), []byte("payload"), time.Now())
			},
			wantErr: errAlreadyMirrored,
		},
		"locked by someone else": {
			asset:    asset,
			upstream: upstreamHandler("", "payload"),
			prepare: func(t *testing.T, m *Mirror, _ *fakeStore) {
				t.Helper()
				if _, err := m.lock.acquire(t.Context(), m.Key(asset)); err != nil {
					t.Fatalf("acquire lock: %v", err)
				}
			},
			wantErr: errLockHeld,
		},
		"missing upstream": {
			asset: asset,
			upstream: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}),
			wantErr: errUpstreamMissing,
		},
		"too large by content length": {
			asset:    asset,
			upstream: upstreamHandler("", strings.Repeat("x", 64)),
			opts:     []Option{WithMaxAssetSize(8)},
			wantErr:  errTooLarge,
		},
		"too large without content length": {
			asset: asset,
			upstream: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Flushing forces a chunked response, so the mirror cannot know
				// the size up front and has to enforce the limit while reading.
				_, _ = w.Write([]byte("start"))
				_ = http.NewResponseController(w).Flush()
				_, _ = w.Write([]byte(strings.Repeat("x", 64)))
			}),
			opts:    []Option{WithMaxAssetSize(8)},
			wantErr: errTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newFakeStore()
			m := newTestMirror(t, store, tc.upstream, tc.opts...)
			if tc.prepare != nil {
				tc.prepare(t, m, store)
			}
			before := store.keys()

			err := m.mirror(t.Context(), testLogger(), tc.asset, m.Key(tc.asset))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("mirror = %v, want %v", err, tc.wantErr)
			}

			// Nothing new may have been stored except a lock that is released
			// again, so the object set must be unchanged.
			if got := store.keys(); len(got) != len(before) {
				t.Errorf("objects = %q, want %q", got, before)
			}
		})
	}
}

// TestKeysAreNamespacedByHost is the invariant that makes multi-forge mirroring
// safe: the same owner and repository on two hosts must never share an object.
func TestKeysAreNamespacedByHost(t *testing.T) {
	t.Parallel()

	m := newTestMirror(t, newFakeStore(), nil)

	const rest = "ninech/foo/releases/download/v1.0.0/foo.tar.gz"
	github := m.Key(Asset{Host: "github.com", Path: rest})
	codeberg := m.Key(Asset{Host: "codeberg.org", Path: rest})

	if github == codeberg {
		t.Fatalf("both hosts map to %q, want distinct keys", github)
	}
	if want := "github.com/" + rest; github != want {
		t.Errorf("Key() = %q, want %q", github, want)
	}
	if want := "codeberg.org/" + rest; codeberg != want {
		t.Errorf("Key() = %q, want %q", codeberg, want)
	}
}

func TestMirrorReleasesLockOnFailure(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.putErr = errUploadFailed
	m := newTestMirror(t, store, upstreamHandler("", "payload"))
	asset := testAsset("ninech/foo", "v1/foo.tar.gz")

	if err := m.mirror(t.Context(), testLogger(), asset, m.Key(asset)); err == nil {
		t.Fatal("mirror = nil, want the upload error")
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("objects after the failed copy = %q, want none", keys)
	}
}

func TestMirrorCopiesOnlyOnce(t *testing.T) {
	t.Parallel()

	const workers = 8
	store := newFakeStore()
	copies := make(chan string, workers)
	store.onPut = func(object string) { copies <- object }

	asset := testAsset("ninech/foo", "v1/foo.tar.gz")

	// Every mirror gets its own locker, standing in for a separate process.
	mirrors := make([]*Mirror, workers)
	for i := range mirrors {
		mirrors[i] = newTestMirror(t, store, upstreamHandler("", "payload"))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, m := range mirrors {
		wg.Go(func() {
			<-start
			_ = m.mirror(t.Context(), testLogger(), asset, m.Key(asset))
		})
	}
	close(start)
	wg.Wait()
	close(copies)

	if got := len(copies); got != 1 {
		t.Errorf("asset copied %d times, want exactly 1", got)
	}
}

func TestEnqueueDeduplicates(t *testing.T) {
	t.Parallel()

	m := newTestMirror(t, newFakeStore(), nil)
	asset := testAsset("ninech/foo", "v1/foo.tar.gz")

	for range 5 {
		m.enqueue(t.Context(), asset)
	}
	if got := len(m.jobs); got != 1 {
		t.Errorf("queued assets = %d, want 1", got)
	}
}

func TestEnqueueDropsWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	m := newTestMirror(t, newFakeStore(), nil, WithQueueSize(1))
	first := testAsset("ninech/foo", "v1/a.tar.gz")
	second := testAsset("ninech/foo", "v1/b.tar.gz")

	m.enqueue(t.Context(), first)
	m.enqueue(t.Context(), second)

	if got := len(m.jobs); got != 1 {
		t.Fatalf("queued assets = %d, want 1", got)
	}
	// The dropped asset must not stay marked as in flight, otherwise it could
	// never be mirrored again.
	if _, loaded := m.inflight.Load(m.Key(second)); loaded {
		t.Error("dropped asset is still marked in flight")
	}
}

func TestStartMirrorsRequestedAsset(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	copied := make(chan string, 1)
	store.onPut = func(object string) { copied <- object }
	m := newTestMirror(t, store, upstreamHandler("", "payload"), WithWorkers(2))

	ctx, cancel := context.WithCancel(t.Context())
	wait := m.Start(ctx)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/github.com/ninech/foo/releases/download/v1/foo.tar.gz", http.NoBody))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}

	select {
	case object := <-copied:
		if want := "github.com/ninech/foo/releases/download/v1/foo.tar.gz"; object != want {
			t.Errorf("copied %q, want %q", object, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("asset was not copied")
	}

	cancel()
	wait()
}

func TestStartStopsWithoutWork(t *testing.T) {
	t.Parallel()

	m := newTestMirror(t, newFakeStore(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	wait := m.Start(ctx)
	cancel()

	stopped := make(chan struct{})
	go func() {
		wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("workers did not stop")
	}
}

// TestMirrorWithoutConditionalWrites documents the degraded behaviour on an S3
// implementation that ignores conditional writes: the copy still succeeds, the
// lock just stops being exclusive.
func TestMirrorWithoutConditionalWrites(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.conditionalWrites = false
	m := newTestMirror(t, store, upstreamHandler("", "payload"))
	asset := testAsset("ninech/foo", "v1/foo.tar.gz")

	if err := m.mirror(t.Context(), testLogger(), asset, m.Key(asset)); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if _, ok := store.get(m.Key(asset)); !ok {
		t.Errorf("asset not stored, have %q", store.keys())
	}
}

func TestContentType(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		upstream, asset, want string
	}{
		"upstream wins":             {"application/gzip", "foo.tar.gz", "application/gzip"},
		"parameters are dropped":    {"text/plain; charset=utf-8", "notes.txt", "text/plain"},
		"generic falls back to ext": {"application/octet-stream", "notes.txt", "text/plain"},
		"empty falls back to ext":   {"", "notes.txt", "text/plain"},
		"unknown extension":         {"", "foo.unknownext", "application/octet-stream"},
		"no extension":              {"", "checksums", "application/octet-stream"},
		"malformed upstream":        {"not/a/type;;", "foo.unknownext", "application/octet-stream"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := contentType(tc.upstream, tc.asset); !strings.HasPrefix(got, tc.want) {
				t.Errorf("contentType(%q, %q) = %q, want %q", tc.upstream, tc.asset, got, tc.want)
			}
		})
	}
}
