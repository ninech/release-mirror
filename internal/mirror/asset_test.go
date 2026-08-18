package mirror

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAsset(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		host, path string
		want       Asset
		wantErr    bool
	}{
		"github release asset": {
			host: "github.com", path: "ninech/foo/releases/download/v1.0.0/foo_linux_amd64.tar.gz",
			want: Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1.0.0/foo_linux_amd64.tar.gz"},
		},
		"gitlab release asset": {
			host: "gitlab.com", path: "group/sub/proj/-/releases/v1.0.0/downloads/foo.tar.gz",
			want: Asset{Host: "gitlab.com", Path: "group/sub/proj/-/releases/v1.0.0/downloads/foo.tar.gz"},
		},
		"codeberg release asset": {
			host: "codeberg.org", path: "ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			want: Asset{Host: "codeberg.org", Path: "ninech/foo/releases/download/v1.0.0/foo.tar.gz"},
		},
		"leading slash is accepted": {
			host: "github.com", path: "/ninech/foo/releases/download/v1/foo.tar.gz",
			want: Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1/foo.tar.gz"},
		},
		"host is normalized": {
			host: "GitHub.Com.", path: "ninech/foo/releases/download/v1/foo.tar.gz",
			want: Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1/foo.tar.gz"},
		},
		"name with spaces": {
			host: "github.com", path: "ninech/foo/releases/download/v1/my asset.zip",
			want: Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1/my asset.zip"},
		},
		// Nothing is assumed about the layout, so a short path is a valid asset.
		// Whether it may be mirrored is a question for the Policy.
		"arbitrary layout": {
			host: "example.com", path: "downloads/foo.tar.gz",
			want: Asset{Host: "example.com", Path: "downloads/foo.tar.gz"},
		},
		"single segment": {
			host: "example.com", path: "foo.tar.gz",
			want: Asset{Host: "example.com", Path: "foo.tar.gz"},
		},

		"empty path":          {host: "github.com", path: "", wantErr: true},
		"root path":           {host: "github.com", path: "/", wantErr: true},
		"trailing slash":      {host: "github.com", path: "ninech/foo/", wantErr: true},
		"empty inner segment": {host: "github.com", path: "ninech//foo.tar.gz", wantErr: true},
		"dotdot segment":      {host: "github.com", path: "ninech/../../x/foo.tar.gz", wantErr: true},
		"dot segment":         {host: "github.com", path: "ninech/./foo.tar.gz", wantErr: true},
		"backslash in name":   {host: "github.com", path: `ninech/foo\bar.zip`, wantErr: true},
		"newline in name":     {host: "github.com", path: "ninech/foo\nbar.zip", wantErr: true},
		"nul in name":         {host: "github.com", path: "ninech/foo\x00bar.zip", wantErr: true},

		"empty host":         {host: "", path: "foo.tar.gz", wantErr: true},
		"host without dot":   {host: "localhost", path: "foo.tar.gz", wantErr: true},
		"host with port":     {host: "github.com:8080", path: "foo.tar.gz", wantErr: true},
		"host with userinfo": {host: "user@github.com", path: "foo.tar.gz", wantErr: true},
		"host with path":     {host: "github.com/x", path: "foo.tar.gz", wantErr: true},
		"ipv4 host":          {host: "169.254.169.254", path: "foo.tar.gz", wantErr: true},
		"ipv6 host":          {host: "::1", path: "foo.tar.gz", wantErr: true},
		"empty label":        {host: "github..com", path: "foo.tar.gz", wantErr: true},
		"leading hyphen":     {host: "-github.com", path: "foo.tar.gz", wantErr: true},
		"trailing hyphen":    {host: "github-.com", path: "foo.tar.gz", wantErr: true},
		"unicode host":       {host: "gïthub.com", path: "foo.tar.gz", wantErr: true},
		"overlong host":      {host: strings.Repeat("a", 250) + ".com", path: "foo.tar.gz", wantErr: true},
		"overlong label":     {host: strings.Repeat("a", 64) + ".com", path: "foo.tar.gz", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseAsset(tc.host, tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAsset(%q, %q) = %#v, want error", tc.host, tc.path, got)
				}
				if !errors.Is(err, errInvalidAsset) {
					t.Errorf("error = %v, want it to wrap errInvalidAsset", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAsset(%q, %q): %v", tc.host, tc.path, err)
			}
			if got != tc.want {
				t.Errorf("ParseAsset(%q, %q) = %#v, want %#v", tc.host, tc.path, got, tc.want)
			}
		})
	}
}

func TestAssetKeyNameAndURL(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		asset             Asset
		wantKey, wantName string
		wantURL           string
	}{
		"github": {
			asset:    Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1.0.0/foo.tar.gz"},
			wantKey:  "github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			wantName: "foo.tar.gz",
			wantURL:  "https://github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
		},
		"gitlab": {
			asset:    Asset{Host: "gitlab.com", Path: "group/sub/proj/-/releases/v1/downloads/foo.tar.gz"},
			wantKey:  "gitlab.com/group/sub/proj/-/releases/v1/downloads/foo.tar.gz",
			wantName: "foo.tar.gz",
			wantURL:  "https://gitlab.com/group/sub/proj/-/releases/v1/downloads/foo.tar.gz",
		},
		"name needing escaping": {
			asset:    Asset{Host: "github.com", Path: "ninech/foo/releases/download/v1/my asset.zip"},
			wantKey:  "github.com/ninech/foo/releases/download/v1/my asset.zip",
			wantName: "my asset.zip",
			wantURL:  "https://github.com/ninech/foo/releases/download/v1/my%20asset.zip",
		},
		"single segment": {
			asset:    Asset{Host: "example.com", Path: "foo.tar.gz"},
			wantKey:  "example.com/foo.tar.gz",
			wantName: "foo.tar.gz",
			wantURL:  "https://example.com/foo.tar.gz",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tc.asset.Key(); got != tc.wantKey {
				t.Errorf("Key() = %q, want %q", got, tc.wantKey)
			}
			if got := tc.asset.Name(); got != tc.wantName {
				t.Errorf("Name() = %q, want %q", got, tc.wantName)
			}
			if got := tc.asset.URL().String(); got != tc.wantURL {
				t.Errorf("URL() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

// TestAssetKeyStaysBelowHost is a property-style guard: whatever [ParseAsset]
// accepts must produce a key below "<host>/", and the host of the upstream URL
// must be the one that was asked for. Both are what keeps a request from
// reaching outside its own namespace.
func TestAssetKeyStaysBelowHost(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"ninech/foo/releases/download/v1/foo.tar.gz",
		"a/b/c/foo.tar.gz",
		"..v1/foo.tar.gz",
		"v1/...tar.gz",
		"ninech/../../other/foo.tar.gz",
		"../../../etc/passwd",
		"foo.tar.gz",
	} {
		asset, err := ParseAsset("github.com", path)
		if err != nil {
			continue
		}
		if prefix := "github.com/"; !strings.HasPrefix(asset.Key(), prefix) {
			t.Errorf("ParseAsset(%q).Key() = %q, want it below %q", path, asset.Key(), prefix)
		}
		if got := asset.URL().Host; got != "github.com" {
			t.Errorf("ParseAsset(%q).URL().Host = %q, want github.com", path, got)
		}
	}
}
