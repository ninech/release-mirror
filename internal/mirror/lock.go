package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/go-chi/traceid"
	"github.com/minio/minio-go/v7"
)

// Lock configuration defaults.
const (
	DefaultLockPrefix  = ".locks/"
	DefaultLockTTL     = 15 * time.Minute
	lockReleaseTimeout = 15 * time.Second
)

var errLockHeld = errors.New("lock held by another owner")

// locker provides S3-backed advisory locking via conditional writes ("If-None-Match: *").
// Stale locks older than TTL are stolen to recover from crashed processes.
type locker struct {
	store  Store
	logger *slog.Logger
	bucket string
	prefix string
	ttl    time.Duration
}

func newLocker(store Store, logger *slog.Logger, bucket, prefix string, ttl time.Duration) *locker {
	return &locker{
		store:  store,
		logger: logger,
		bucket: bucket,
		prefix: prefix,
		ttl:    ttl,
	}
}

// acquire attempts to obtain the lock for object. Returns a release callback on success,
// or errLockHeld if another process currently holds an unexpired lock.
func (l *locker) acquire(ctx context.Context, object string) (release func(), err error) {
	name := l.name(object)

	held, err := l.put(ctx, name)
	if err != nil {
		return nil, err
	}
	if !held {
		return l.releaser(ctx, name), nil
	}

	// The lock exists. Steal it when it is old enough to be the leftover of a
	// crashed process, otherwise leave the work to its owner.
	info, err := l.store.StatObject(ctx, l.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			// Released between the write and the stat. A later request retries.
			return nil, errLockHeld
		}
		return nil, fmt.Errorf("stat lock %s: %w", name, err)
	}
	age := time.Since(info.LastModified).Round(time.Second)
	if age < l.ttl {
		return nil, fmt.Errorf("%w for %s", errLockHeld, age)
	}
	l.logger.WarnContext(ctx, "stealing expired lock", "lock", name, "age", age, "ttl", l.ttl)

	if err := l.store.RemoveObject(ctx, l.bucket, name, minio.RemoveObjectOptions{}); err != nil {
		return nil, fmt.Errorf("steal lock %s: %w", name, err)
	}
	switch held, err := l.put(ctx, name); {
	case err != nil:
		return nil, err
	case held:
		// Another process won the race for the stale lock.
		return nil, errLockHeld
	}

	return l.releaser(ctx, name), nil
}

func (l *locker) name(object string) string {
	return path.Join(l.prefix, path.Clean(object)) + ".lock"
}

// put creates the lock object using a conditional write ("If-None-Match: *").
// Returns held=true if the lock already exists (precondition failed).
//
// The body records the trace ID of the copy holding the lock, which is also
// attached to its log lines and to its request to upstream, so a leftover lock
// can be traced back. It deliberately carries no host or process identity: the
// bucket is served publicly and lock keys are derived from asset keys, so
// anybody can read them.
func (l *locker) put(ctx context.Context, name string) (held bool, err error) {
	body, err := json.Marshal(struct {
		Owner      string    `json:"owner,omitempty"`
		AcquiredAt time.Time `json:"acquiredAt"`
	}{Owner: traceid.FromContext(ctx), AcquiredAt: time.Now().UTC()})
	if err != nil {
		return false, fmt.Errorf("encode lock: %w", err)
	}

	opts := minio.PutObjectOptions{
		ContentType:  "application/json",
		CacheControl: "no-store",
	}
	opts.SetMatchETagExcept("*")

	if _, err := l.store.PutObject(ctx, l.bucket, name, bytes.NewReader(body), int64(len(body)), opts); err != nil {
		if isPreconditionFailed(err) {
			return true, nil
		}
		return false, fmt.Errorf("write lock %s: %w", name, err)
	}

	return false, nil
}

// releaser deletes the lock object using an uncancelled context to ensure cleanup on shutdown.
func (l *locker) releaser(ctx context.Context, name string) func() {
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
		defer cancel()
		if err := l.store.RemoveObject(releaseCtx, l.bucket, name, minio.RemoveObjectOptions{}); err != nil && !isNotFound(err) {
			l.logger.ErrorContext(releaseCtx, "release lock", "lock", name, "err", err)
		}
	}
}
