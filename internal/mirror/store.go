package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
)

// Store is the subset of the S3 API the mirror depends on (implemented by *minio.Client).
type Store interface {
	StatObject(ctx context.Context, bucket, object string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	PutObject(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	RemoveObject(ctx context.Context, bucket, object string, opts minio.RemoveObjectOptions) error
}

// codeNotFound is returned by S3 HEAD requests for missing objects where no XML body is returned.
const codeNotFound = "NotFound"

// exists reports whether object is present in the mirror's bucket.
func (m *Mirror) exists(ctx context.Context, object string) (bool, error) {
	if _, err := m.store.StatObject(ctx, m.bucket, object, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", object, err)
	}
	return true, nil
}

// isNotFound reports if err indicates a missing object (excluding NoSuchBucket, which is a config error).
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case minio.NoSuchKey, codeNotFound:
		return true
	case minio.NoSuchBucket:
		return false
	}
	return resp.StatusCode == http.StatusNotFound
}

// isPreconditionFailed reports if err indicates a conditional write conflict (412 Precondition Failed or 409 Conflict).
func isPreconditionFailed(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case minio.PreconditionFailed, minio.Conflict:
		return true
	}
	return resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusConflict
}
