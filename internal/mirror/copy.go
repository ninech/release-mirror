package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"time"

	"github.com/minio/minio-go/v7"
)

var (
	errNotAllowed       = errors.New("path not allowed")
	errDenied           = errors.New("path denied")
	errAlreadyMirrored  = errors.New("already mirrored")
	errUpstreamMissing  = errors.New("asset missing upstream")
	errTooLarge         = errors.New("asset too large")
	errUnexpectedStatus = errors.New("unexpected status")
)

// copy streams the asset from its upstream host into the bucket while holding the asset lock.
func (m *Mirror) copy(ctx context.Context, logger *slog.Logger, asset Asset, key string) error {
	src := asset.URL().String()
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", src, err)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", src, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w: %s: %s", errUpstreamMissing, src, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("get %s: %w: %s", src, errUnexpectedStatus, resp.Status)
	}

	body := io.Reader(resp.Body)
	if m.maxSize > 0 {
		if resp.ContentLength > m.maxSize {
			return fmt.Errorf("%w: %s is %d bytes, limit is %d", errTooLarge, src, resp.ContentLength, m.maxSize)
		}
		// Guard against missing or understated Content-Length headers.
		body = &limitReader{r: body, limit: m.maxSize, remaining: m.maxSize}
	}

	logger.DebugContext(ctx, "copying asset", "asset", key, "source", src, "size", resp.ContentLength)

	info, err := m.store.PutObject(ctx, m.bucket, key, body, resp.ContentLength, minio.PutObjectOptions{
		ContentType:  contentType(resp.Header.Get("Content-Type"), asset.Name()),
		CacheControl: m.cacheControl,
		UserMetadata: map[string]string{"Source": src},
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}

	logger.InfoContext(ctx, "mirrored asset",
		"asset", key,
		"source", src,
		"size", info.Size,
		"etag", info.ETag,
		"duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// contentType determines the MIME type, preferring upstream headers with file extension fallback.
func contentType(upstream, name string) string {
	const fallback = "application/octet-stream"

	if upstream != "" && upstream != fallback {
		if mediaType, _, err := mime.ParseMediaType(upstream); err == nil {
			return mediaType
		}
	}
	if byExt := mime.TypeByExtension(path.Ext(name)); byExt != "" {
		if mediaType, _, err := mime.ParseMediaType(byExt); err == nil {
			return mediaType
		}
	}
	return fallback
}

// limitReader fails with errTooLarge if bytes read exceed limit (unlike io.LimitReader which truncates).
type limitReader struct {
	r         io.Reader
	limit     int64
	remaining int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, l.err()
	}

	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return 0, l.err()
	}

	// Must pass through io.EOF unwrapped so io.Copy doesn't treat normal completion as an error.
	return n, err //nolint:wrapcheck // io.Copy expects unwrapped io.EOF.
}

func (l *limitReader) err() error {
	return fmt.Errorf("%w: exceeds %d bytes", errTooLarge, l.limit)
}
