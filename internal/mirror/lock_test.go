package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/go-chi/traceid"
	"github.com/minio/minio-go/v7"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestLocker(store Store) *locker {
	return newLocker(store, testLogger(), "bucket", DefaultLockPrefix, DefaultLockTTL)
}

func TestLockerAcquireAndRelease(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	l := newTestLocker(store)
	ctx := t.Context()

	release, err := l.acquire(ctx, "a/b/asset.tar.gz")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	name := DefaultLockPrefix + "a/b/asset.tar.gz.lock"
	if _, ok := store.get(name); !ok {
		t.Fatalf("lock object %q not created, have %q", name, store.keys())
	}

	if _, err := l.acquire(ctx, "a/b/asset.tar.gz"); !errors.Is(err, errLockHeld) {
		t.Errorf("second acquire = %v, want errLockHeld", err)
	}

	release()
	if _, ok := store.get(name); ok {
		t.Errorf("lock object %q still present after release", name)
	}

	if _, err := l.acquire(ctx, "a/b/asset.tar.gz"); err != nil {
		t.Errorf("acquire after release: %v", err)
	}
}

// TestLockerRecordsTraceIDAsOwner pins the content of the lock object: the trace
// ID of the holder and nothing that identifies the host or process, because the
// bucket the lock lives in is served publicly.
func TestLockerRecordsTraceIDAsOwner(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	ctx := traceid.NewContext(t.Context())

	release, err := newTestLocker(store).acquire(ctx, "asset")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	obj, ok := store.get(DefaultLockPrefix + "asset.lock")
	if !ok {
		t.Fatalf("lock object missing, have %q", store.keys())
	}

	var body struct {
		Owner      string    `json:"owner"`
		AcquiredAt time.Time `json:"acquiredAt"`
	}
	if err := json.Unmarshal(obj.data, &body); err != nil {
		t.Fatalf("decode lock body %q: %v", obj.data, err)
	}
	if want := traceid.FromContext(ctx); body.Owner != want {
		t.Errorf("owner = %q, want the trace ID %q", body.Owner, want)
	}
	if body.AcquiredAt.IsZero() {
		t.Errorf("acquiredAt is zero in %q", obj.data)
	}
}

func TestLockerReleasesAfterCancel(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	l := newTestLocker(store)

	ctx, cancel := context.WithCancel(t.Context())
	release, err := l.acquire(ctx, "asset")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	cancel()
	release()

	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("objects after release = %q, want none", keys)
	}
}

func TestLockerStealsExpiredLock(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	l := newTestLocker(store)
	name := DefaultLockPrefix + "asset.lock"

	store.put(name, []byte("{}"), time.Now().Add(-2*DefaultLockTTL))

	release, err := l.acquire(t.Context(), "asset")
	if err != nil {
		t.Fatalf("acquire expired lock: %v", err)
	}
	defer release()

	obj, ok := store.get(name)
	if !ok {
		t.Fatal("lock object missing after steal")
	}
	if string(obj.data) == "{}" {
		t.Error("lock object was not replaced by the stealing owner")
	}
}

func TestLockerKeepsFreshLock(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	l := newTestLocker(store)
	store.put(DefaultLockPrefix+"asset.lock", []byte("{}"), time.Now().Add(-DefaultLockTTL/2))

	if _, err := l.acquire(t.Context(), "asset"); !errors.Is(err, errLockHeld) {
		t.Errorf("acquire = %v, want errLockHeld", err)
	}
}

func TestLockerAcquireIsExclusive(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	ctx := t.Context()

	const acquirers = 16
	results := make(chan error, acquirers)
	start := make(chan struct{})
	for range acquirers {
		go func() {
			<-start
			_, err := newTestLocker(store).acquire(ctx, "asset")
			results <- err
		}()
	}
	close(start)

	var won int
	for range acquirers {
		switch err := <-results; {
		case err == nil:
			won++
		case errors.Is(err, errLockHeld):
		default:
			t.Errorf("acquire: unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d acquirers won the lock, want exactly 1", won)
	}
}

func TestLockerPropagatesUnexpectedErrors(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.statErr = minio.ErrorResponse{Code: minio.AccessDenied, StatusCode: 403}
	store.put(DefaultLockPrefix+"asset.lock", []byte("{}"), time.Now())

	_, err := newTestLocker(store).acquire(t.Context(), "asset")
	if err == nil || errors.Is(err, errLockHeld) {
		t.Errorf("acquire = %v, want an access error", err)
	}
}

func TestLockerName(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prefix string
		want   string
	}{
		"trailing slash":    {"locks/", "locks/a/b/c.tar.gz.lock"},
		"no trailing slash": {"locks", "locks/a/b/c.tar.gz.lock"},
		"nested prefix":     {"internal/locks/", "internal/locks/a/b/c.tar.gz.lock"},
		"no prefix":         {"", "a/b/c.tar.gz.lock"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newLocker(newFakeStore(), testLogger(), "bucket", tc.prefix, DefaultLockTTL)
			if got := l.name("a/b/c.tar.gz"); got != tc.want {
				t.Errorf("name() = %q, want %q", got, tc.want)
			}
		})
	}
}
