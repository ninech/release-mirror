package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	errNoEndpoint       = errors.New("no s3 endpoint configured")
	errInvalidEndpoint  = errors.New("invalid s3 endpoint")
	errNoBucket         = errors.New("no s3 bucket configured")
	errInvalidPublicURL = errors.New("public URL must be an absolute http(s) URL")
)

// S3 holds the flags needed to talk to an S3 compatible object store.
type S3 struct {
	Endpoint        string   `help:"S3 endpoint, either host[:port] or scheme://host[:port]." env:"RELEASE_MIRROR_S3_ENDPOINT" default:"s3.amazonaws.com"`
	Region          string   `help:"S3 region. Discovered from the endpoint when empty." env:"RELEASE_MIRROR_S3_REGION"`
	Bucket          string   `help:"S3 bucket holding the mirrored assets." env:"RELEASE_MIRROR_S3_BUCKET"`
	AccessKeyID     string   `help:"S3 access key ID. Falls back to the ambient AWS credential chain when empty." env:"RELEASE_MIRROR_S3_ACCESS_KEY_ID,AWS_ACCESS_KEY_ID"`
	SecretAccessKey string   `help:"S3 secret access key." env:"RELEASE_MIRROR_S3_SECRET_ACCESS_KEY,AWS_SECRET_ACCESS_KEY"`
	SessionToken    string   `help:"S3 session token for temporary credentials." env:"RELEASE_MIRROR_S3_SESSION_TOKEN,AWS_SESSION_TOKEN"`
	Insecure        bool     `help:"Talk to the S3 endpoint over plain HTTP." env:"RELEASE_MIRROR_S3_INSECURE"`
	PathStyle       bool     `help:"Use path-style bucket addressing (host/bucket/key)." env:"RELEASE_MIRROR_S3_PATH_STYLE"`
	PublicURL       *url.URL `help:"Public base URL of the bucket. Derived from endpoint and bucket when empty." env:"RELEASE_MIRROR_S3_PUBLIC_URL"`
}

// Client creates an S3 client, falling back to ambient AWS credentials when flags are unset.
func (s *S3) Client(transport http.RoundTripper) (*minio.Client, error) {
	host, secure, err := s.endpoint()
	if err != nil {
		return nil, err
	}

	lookup := minio.BucketLookupAuto
	if s.PathStyle {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(host, &minio.Options{
		Creds:        s.credentials(),
		Secure:       secure,
		Region:       s.Region,
		BucketLookup: lookup,
		Transport:    transport,
		// Required for CRC checksums minio-go prefers over MD5.
		TrailingHeaders: true,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client for %s: %w", host, err)
	}

	return client, nil
}

// PublicBaseURL returns the base redirect URL, derived from endpoint and bucket if unconfigured.
func (s *S3) PublicBaseURL() (*url.URL, error) {
	if s.PublicURL != nil {
		u := *s.PublicURL
		if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("%w: %q", errInvalidPublicURL, &u)
		}
		u.Path = strings.TrimSuffix(u.Path, "/")
		return &u, nil
	}

	if s.Bucket == "" {
		return nil, fmt.Errorf("derive public URL: %w", errNoBucket)
	}

	host, secure, err := s.endpoint()
	if err != nil {
		return nil, err
	}

	u := &url.URL{Scheme: "https", Host: host}
	if !secure {
		u.Scheme = "http"
	}
	if s.PathStyle {
		u.Path = "/" + s.Bucket
	} else {
		u.Host = s.Bucket + "." + host
	}

	return u, nil
}

// endpoint parses the configured endpoint into host and TLS settings.
func (s *S3) endpoint() (host string, secure bool, err error) {
	raw := strings.TrimSpace(s.Endpoint)
	if raw == "" {
		return "", false, errNoEndpoint
	}

	secure = !s.Insecure
	if !strings.Contains(raw, "://") {
		return raw, secure, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("parse s3 endpoint %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http":
		secure = false
	case "https":
		secure = !s.Insecure
	default:
		return "", false, fmt.Errorf("%w: unsupported scheme %q", errInvalidEndpoint, u.Scheme)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("%w: %q has no host", errInvalidEndpoint, raw)
	}

	return u.Host, secure, nil
}

func (s *S3) credentials() *credentials.Credentials {
	if s.AccessKeyID != "" && s.SecretAccessKey != "" {
		return credentials.NewStaticV4(s.AccessKeyID, s.SecretAccessKey, s.SessionToken)
	}

	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.EnvMinio{},
		&credentials.FileAWSCredentials{},
		&credentials.FileMinioClient{},
		&credentials.IAM{},
	})
}
