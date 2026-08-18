// Package mirror implements the mirror handlers.
//
// The mirror never serves release assets itself. Every request is answered with
// a redirect to the public URL of the S3 bucket, which keeps the mirror out of
// the download path entirely — no bandwidth, no request-scoped timeouts, no
// dependency on the mirror while a large asset is transferred.
//
// Filling the bucket happens out of band. A request additionally enqueues the
// requested asset for a background worker, which
//
//  1. checks the asset against the [Policy],
//  2. skips the asset when it is already present in the bucket,
//  3. takes a lock in the bucket so that only one worker — across all instances
//     of the mirror — copies a given asset, and
//  4. streams the asset from the upstream host into the bucket.
//
// The mirror is not tied to a single forge. The upstream host is the first
// segment of the request path and the rest is passed through unparsed, so
// github.com, gitlab.com, codeberg.org and any other host serving assets over
// HTTPS are reached through the same handler.
//
// The redirect is therefore optimistic: the very first request for an asset that
// has not been mirrored yet is redirected to an object that does not exist. The
// asset appears once the background copy has finished, and every later request
// resolves. The [Policy] decides only what is copied, never what is answered, so
// a request for a path that is never going to be mirrored is redirected all the
// same.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/traceid"
)

// Mirror configuration defaults.
const (
	DefaultWorkers      = 4
	DefaultQueueSize    = 256
	DefaultCopyTimeout  = 10 * time.Minute
	DefaultMaxAssetSize = 2 << 30 // 2 GiB
	// DefaultCacheControl marks mirrored release assets as immutable.
	DefaultCacheControl = "public, max-age=31536000, immutable"
)

var (
	errNoStore          = errors.New("mirror: store is required")
	errNoBucket         = errors.New("mirror: bucket is required")
	errNoPublicURL      = errors.New("mirror: public URL is required")
	errInvalidPublicURL = errors.New("mirror: public URL must be an absolute http(s) URL")
)

// Mirror redirects release asset requests to public S3 and mirrors assets in the background.
type Mirror struct {
	store     Store
	bucket    string
	publicURL *url.URL
	prefix    string
	policy    *Policy
	client    *http.Client
	logger    *slog.Logger
	lock      *locker

	workers      int
	queueSize    int
	copyTimeout  time.Duration
	maxSize      int64
	cacheControl string
	lockPrefix   string
	lockTTL      time.Duration

	handler  http.Handler
	jobs     chan Asset
	started  atomic.Bool
	inflight sync.Map // object key -> struct{}, deduplicates queued assets
}

// Option configures a [Mirror].
type Option func(*Mirror)

// WithLogger sets the logger (defaults to slog.Default).
func WithLogger(logger *slog.Logger) Option {
	return func(m *Mirror) { m.logger = logger }
}

// WithHTTPClient sets the HTTP client used to download assets from upstream.
func WithHTTPClient(c *http.Client) Option {
	return func(m *Mirror) { m.client = c }
}

// WithPolicy sets the patterns deciding what may be mirrored. Defaults to empty
// (mirrors nothing).
func WithPolicy(p *Policy) Option {
	return func(m *Mirror) { m.policy = p }
}

// WithKeyPrefix stores mirrored assets under a key prefix inside the bucket.
func WithKeyPrefix(prefix string) Option {
	return func(m *Mirror) { m.prefix = strings.TrimPrefix(prefix, "/") }
}

// WithWorkers sets concurrent copy workers (defaults to DefaultWorkers).
func WithWorkers(n int) Option {
	return func(m *Mirror) {
		if n > 0 {
			m.workers = n
		}
	}
}

// WithQueueSize sets maximum queued copy jobs (defaults to DefaultQueueSize).
func WithQueueSize(n int) Option {
	return func(m *Mirror) {
		if n > 0 {
			m.queueSize = n
		}
	}
}

// WithCopyTimeout sets timeout for copying an individual asset (defaults to DefaultCopyTimeout).
func WithCopyTimeout(d time.Duration) Option {
	return func(m *Mirror) {
		if d > 0 {
			m.copyTimeout = d
		}
	}
}

// WithMaxAssetSize sets maximum asset size limit in bytes (0 or negative disables limit).
func WithMaxAssetSize(n int64) Option {
	return func(m *Mirror) { m.maxSize = n }
}

// WithLock configures the S3 lock prefix and TTL.
func WithLock(prefix string, ttl time.Duration) Option {
	return func(m *Mirror) {
		if prefix != "" {
			m.lockPrefix = strings.TrimPrefix(prefix, "/")
		}
		if ttl > 0 {
			m.lockTTL = ttl
		}
	}
}

// New creates a Mirror storing assets in bucket and redirecting requests below
// publicURL, the publicly reachable base URL of that bucket.
//
// The returned Mirror does not copy anything until [Mirror.Start] has been
// called.
func New(store Store, bucket string, publicURL *url.URL, opts ...Option) (*Mirror, error) {
	if store == nil {
		return nil, errNoStore
	}
	if bucket == "" {
		return nil, errNoBucket
	}
	if publicURL == nil {
		return nil, errNoPublicURL
	}
	if publicURL.Host == "" || (publicURL.Scheme != "http" && publicURL.Scheme != "https") {
		return nil, fmt.Errorf("%w: %q", errInvalidPublicURL, publicURL)
	}

	base := *publicURL
	base.RawPath, base.RawQuery, base.Fragment = "", "", ""

	m := &Mirror{
		store:        store,
		bucket:       bucket,
		publicURL:    &base,
		workers:      DefaultWorkers,
		queueSize:    DefaultQueueSize,
		copyTimeout:  DefaultCopyTimeout,
		maxSize:      DefaultMaxAssetSize,
		cacheControl: DefaultCacheControl,
		lockPrefix:   DefaultLockPrefix,
		lockTTL:      DefaultLockTTL,
	}
	for _, opt := range opts {
		opt(m)
	}

	if m.logger == nil {
		m.logger = slog.Default()
	}
	if m.client == nil {
		m.client = http.DefaultClient
	}
	if m.policy == nil {
		// The zero Policy denies every path, which is the safe default.
		m.policy = &Policy{}
	}
	m.jobs = make(chan Asset, m.queueSize)
	m.lock = newLocker(store, m.logger, bucket, m.lockPrefix, m.lockTTL)
	m.handler = m.buildHandler()

	if m.lockTTL <= m.copyTimeout {
		// Slow copies running longer than lock TTL risk being duplicated by another worker.
		m.logger.Warn("lock TTL is not longer than the copy timeout: a slow copy may be duplicated",
			"lock_ttl", m.lockTTL, "copy_timeout", m.copyTimeout)
	}

	return m, nil
}

// Handler returns the HTTP handler serving the mirror. It routes on the upstream
// host itself and is mounted at the root, so requests look like
// /github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz.
func (m *Mirror) Handler() http.Handler {
	return m.handler
}

func (m *Mirror) buildHandler() http.Handler {
	r := chi.NewRouter()
	r.Get("/{host}/*", m.serveAsset)
	return r
}

// Key returns the object key a is mirrored under, including the configured prefix.
func (m *Mirror) Key(a Asset) string {
	if m.prefix == "" {
		return a.Key()
	}
	return path.Join(m.prefix, a.Key())
}

// PublicURL returns the public URL of a in the bucket.
func (m *Mirror) PublicURL(a Asset) string {
	u := *m.publicURL
	u.Path = "/" + strings.TrimPrefix(path.Join(u.Path, m.Key(a)), "/")
	return u.String()
}

// serveAsset redirects to the public URL of the requested asset and enqueues it for mirroring.
func (m *Mirror) serveAsset(w http.ResponseWriter, r *http.Request) {
	asset, err := ParseAsset(routeParam(r, "host"), routeParam(r, "*"))
	if err != nil {
		m.logger.DebugContext(r.Context(), "rejecting request", "path", r.URL.Path, "err", err)
		http.NotFound(w, r)
		return
	}

	m.enqueue(r.Context(), asset)

	// Optimistic redirect: asset may not be in bucket yet, so prevent caching 302.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, m.PublicURL(asset), http.StatusFound)
}

// enqueue submits an asset to the background worker pool without blocking.
func (m *Mirror) enqueue(ctx context.Context, asset Asset) {
	key := m.Key(asset)
	if _, loaded := m.inflight.LoadOrStore(key, struct{}{}); loaded {
		return
	}

	select {
	case m.jobs <- asset:
	default:
		m.inflight.Delete(key)
		m.logger.WarnContext(ctx, "mirror queue full, dropping asset", "asset", key)
	}
}

// Start launches the background workers and returns a wait function for graceful shutdown.
func (m *Mirror) Start(ctx context.Context) (wait func()) {
	if m.started.Swap(true) {
		m.logger.ErrorContext(ctx, "mirror workers are already running, ignoring Start")
		return func() {}
	}

	var wg sync.WaitGroup
	for i := range m.workers {
		wg.Go(func() { m.work(ctx, m.logger.With("worker", i)) })
	}
	m.logger.DebugContext(ctx, "mirror workers started", "workers", m.workers, "bucket", m.bucket)

	return wg.Wait
}

func (m *Mirror) work(ctx context.Context, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case asset := <-m.jobs:
			m.process(ctx, logger, asset)
		}
	}
}

// process mirrors a single asset and logs expected vs unexpected outcomes.
func (m *Mirror) process(ctx context.Context, logger *slog.Logger, asset Asset) {
	key := m.Key(asset)
	defer m.inflight.Delete(key)

	// A copy outlives the request that triggered it, so it gets a trace ID of its
	// own. It ties together the log lines of the copy, the lock object it writes
	// and the request it sends to upstream.
	ctx = traceid.NewContext(ctx)

	ctx, cancel := context.WithTimeout(ctx, m.copyTimeout)
	defer cancel()

	switch err := m.mirror(ctx, logger, asset, key); {
	case err == nil:
	case errors.Is(err, errNotAllowed),
		errors.Is(err, errDenied),
		errors.Is(err, errAlreadyMirrored),
		errors.Is(err, errLockHeld),
		errors.Is(err, errUpstreamMissing),
		errors.Is(err, errTooLarge):
		logger.DebugContext(ctx, "asset not mirrored", "asset", key, "reason", err)
	default:
		logger.ErrorContext(ctx, "mirror asset", "asset", key, "err", err)
	}
}

func (m *Mirror) mirror(ctx context.Context, logger *slog.Logger, asset Asset, key string) error {
	// The policy matches the upstream identity of the asset, never the storage
	// key: a configured key prefix must not be able to satisfy a pattern.
	switch {
	case m.policy.Denies(asset.Key()):
		return fmt.Errorf("%w: %s", errDenied, asset.Key())
	case !m.policy.Allows(asset.Key()):
		return fmt.Errorf("%w: %s", errNotAllowed, asset.Key())
	}

	switch ok, err := m.exists(ctx, key); {
	case err != nil:
		return err
	case ok:
		return errAlreadyMirrored
	}

	release, err := m.lock.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()

	// Double check under lock in case another worker completed the copy concurrently.
	switch ok, err := m.exists(ctx, key); {
	case err != nil:
		return err
	case ok:
		return errAlreadyMirrored
	}

	return m.copy(ctx, logger, asset, key)
}

// routeParam returns the decoded URL parameter value, avoiding double decoding when RawPath is unset.
func routeParam(r *http.Request, name string) string {
	value := chi.URLParam(r, name)
	if r.URL.RawPath == "" {
		return value
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}
