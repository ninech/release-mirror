package cli

import (
	"net/http"
	"net/url"
	"testing"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestS3Endpoint(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		s3         S3
		wantHost   string
		wantSecure bool
		wantErr    bool
	}{
		"bare host":              {S3{Endpoint: "s3.amazonaws.com"}, "s3.amazonaws.com", true, false},
		"bare host and port":     {S3{Endpoint: "minio.example.com:9000"}, "minio.example.com:9000", true, false},
		"insecure flag":          {S3{Endpoint: "minio.example.com:9000", Insecure: true}, "minio.example.com:9000", false, false},
		"https URL":              {S3{Endpoint: "https://minio.example.com:9000"}, "minio.example.com:9000", true, false},
		"http URL":               {S3{Endpoint: "http://localhost:9000"}, "localhost:9000", false, false},
		"insecure overrides URL": {S3{Endpoint: "https://minio.example.com", Insecure: true}, "minio.example.com", false, false},
		"whitespace trimmed":     {S3{Endpoint: "  s3.amazonaws.com  "}, "s3.amazonaws.com", true, false},
		"empty":                  {S3{}, "", false, true},
		"unsupported scheme":     {S3{Endpoint: "s3://bucket"}, "", false, true},
		"no host":                {S3{Endpoint: "https:///path"}, "", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			host, secure, err := tc.s3.endpoint()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("endpoint() error = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if host != tc.wantHost || secure != tc.wantSecure {
				t.Errorf("endpoint() = (%q, %t), want (%q, %t)", host, secure, tc.wantHost, tc.wantSecure)
			}
		})
	}
}

func TestS3PublicBaseURL(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		s3      S3
		want    string
		wantErr bool
	}{
		"virtual host style": {
			s3:   S3{Endpoint: "s3.amazonaws.com", Bucket: "assets"},
			want: "https://assets.s3.amazonaws.com",
		},
		"path style": {
			s3:   S3{Endpoint: "minio.example.com:9000", Bucket: "assets", PathStyle: true},
			want: "https://minio.example.com:9000/assets",
		},
		"insecure path style": {
			s3:   S3{Endpoint: "localhost:9000", Bucket: "assets", PathStyle: true, Insecure: true},
			want: "http://localhost:9000/assets",
		},
		"explicit public URL wins": {
			s3:   S3{Endpoint: "s3.amazonaws.com", Bucket: "assets", PublicURL: mustParseURL(t, "https://cdn.example.com/mirror")},
			want: "https://cdn.example.com/mirror",
		},
		"trailing slash trimmed": {
			s3:   S3{Bucket: "assets", PublicURL: mustParseURL(t, "https://cdn.example.com/mirror/")},
			want: "https://cdn.example.com/mirror",
		},
		"relative public URL":  {s3: S3{Bucket: "assets", PublicURL: mustParseURL(t, "/mirror")}, wantErr: true},
		"no bucket":            {s3: S3{Endpoint: "s3.amazonaws.com"}, wantErr: true},
		"unusable endpoint":    {s3: S3{Endpoint: "s3://x", Bucket: "assets"}, wantErr: true},
		"public URL no scheme": {s3: S3{Bucket: "assets", PublicURL: mustParseURL(t, "//cdn.example.com")}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.s3.PublicBaseURL()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("PublicBaseURL() error = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.String() != tc.want {
				t.Errorf("PublicBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestS3PublicBaseURLDoesNotAliasFlag makes sure the returned URL can be mutated
// without changing the parsed flag value.
func TestS3PublicBaseURLDoesNotAliasFlag(t *testing.T) {
	t.Parallel()

	s3 := S3{Bucket: "assets", PublicURL: mustParseURL(t, "https://cdn.example.com/mirror")}
	got, err := s3.PublicBaseURL()
	if err != nil {
		t.Fatalf("PublicBaseURL: %v", err)
	}
	got.Path = "/changed"

	if s3.PublicURL.Path != "/mirror" {
		t.Errorf("flag value changed to %q, want %q", s3.PublicURL.Path, "/mirror")
	}
}

func TestS3Client(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		s3      S3
		wantErr bool
	}{
		"static credentials":  {S3{Endpoint: "s3.amazonaws.com", AccessKeyID: "key", SecretAccessKey: "secret"}, false},
		"credential chain":    {S3{Endpoint: "s3.amazonaws.com"}, false},
		"path style endpoint": {S3{Endpoint: "http://localhost:9000", PathStyle: true}, false},
		"bad endpoint":        {S3{Endpoint: "s3://bucket"}, true},
		"no endpoint":         {S3{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, err := tc.s3.Client(http.DefaultTransport)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Client() error = %v, want error %t", err, tc.wantErr)
			}
			if !tc.wantErr && client == nil {
				t.Error("Client() = nil, want a client")
			}
		})
	}
}
