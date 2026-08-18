package mirror

import (
	"context"
	"crypto/md5" //nolint:gosec // an S3 ETag is an MD5 digest by specification, not a security primitive
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// errPlain is an error that carries none of the S3 metadata the helpers under test inspect.
var errPlain = errors.New("boom")

// fakeStore is an in-memory [Store] supporting conditional writes for tests.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string]fakeObject

	conditionalWrites bool
	putErr            error
	statErr           error
	onPut             func(object string)
}

type fakeObject struct {
	data         []byte
	contentType  string
	cacheControl string
	metadata     map[string]string
	modified     time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]fakeObject{}, conditionalWrites: true}
}

func (f *fakeStore) StatObject(_ context.Context, _, object string, _ minio.StatObjectOptions) (minio.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.statErr != nil {
		return minio.ObjectInfo{}, f.statErr
	}
	obj, ok := f.objects[object]
	if !ok {
		return minio.ObjectInfo{}, notFoundErr(object)
	}
	return minio.ObjectInfo{Key: object, Size: int64(len(obj.data)), LastModified: obj.modified}, nil
}

func (f *fakeStore) PutObject(_ context.Context, _, object string, r io.Reader, _ int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	f.mu.Lock()
	onPut, putErr := f.onPut, f.putErr
	conditional := opts.Header().Get("If-None-Match") == "*" && f.conditionalWrites
	f.mu.Unlock()

	if !strings.HasSuffix(object, ".lock") {
		if onPut != nil {
			onPut(object)
		}
		if putErr != nil {
			return minio.UploadInfo{}, putErr
		}
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return minio.UploadInfo{}, fmt.Errorf("read object %s: %w", object, err)
	}

	sum := md5.Sum(data) //nolint:gosec // an S3 ETag is an MD5 digest by specification, not a security primitive
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.objects[object]; exists && conditional {
		return minio.UploadInfo{}, preconditionFailedErr(object)
	}

	f.objects[object] = fakeObject{
		data:         data,
		contentType:  opts.ContentType,
		cacheControl: opts.CacheControl,
		metadata:     maps.Clone(opts.UserMetadata),
		modified:     time.Now(),
	}
	return minio.UploadInfo{Key: object, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:])}, nil
}

func (f *fakeStore) RemoveObject(_ context.Context, _, object string, _ minio.RemoveObjectOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, object)
	return nil
}

func (f *fakeStore) get(object string) (fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[object]
	return obj, ok
}

// keys returns keys of all stored objects in sorted order.
func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.objects))
}

func (f *fakeStore) put(object string, data []byte, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[object] = fakeObject{data: data, modified: modified}
}

func notFoundErr(object string) error {
	return minio.ErrorResponse{Code: minio.NoSuchKey, Key: object, StatusCode: http.StatusNotFound}
}

func preconditionFailedErr(object string) error {
	return minio.ErrorResponse{Code: minio.PreconditionFailed, Key: object, StatusCode: http.StatusPreconditionFailed}
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"no such key":  {notFoundErr("k"), true},
		"status only":  {minio.ErrorResponse{StatusCode: http.StatusNotFound}, true},
		"no bucket":    {minio.ErrorResponse{Code: minio.NoSuchBucket, StatusCode: http.StatusNotFound}, false},
		"access":       {minio.ErrorResponse{Code: minio.AccessDenied, StatusCode: http.StatusForbidden}, false},
		"plain error":  {errPlain, false},
		"precondition": {preconditionFailedErr("k"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsPreconditionFailed(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"precondition failed": {preconditionFailedErr("k"), true},
		"conflict code":       {minio.ErrorResponse{Code: minio.Conflict}, true},
		"conflict status":     {minio.ErrorResponse{StatusCode: http.StatusConflict}, true},
		"not found":           {notFoundErr("k"), false},
		"plain error":         {errPlain, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isPreconditionFailed(tc.err); got != tc.want {
				t.Errorf("isPreconditionFailed(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
