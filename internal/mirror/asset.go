package mirror

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var errInvalidAsset = errors.New("invalid release asset")

// DNS name limits in presentation format.
const (
	maxHostLength  = 253
	maxLabelLength = 63
)

// Asset identifies a single file on an upstream host.
//
// The mirror does not interpret the path. Whatever layout a forge uses for its
// release assets is passed through verbatim, which is what lets one mirror serve
// github.com, gitlab.com, codeberg.org and anything else that exposes assets over
// plain HTTPS — at the price of the mirror no longer knowing whether a path
// addresses a release asset at all. Deciding that is the job of the [Policy].
//
// Path is held in decoded form. [Asset.URL] re-escapes it when addressing
// upstream, while [Asset.Key] uses it as is, so the bucket layout stays a
// readable mirror of the upstream URL layout.
type Asset struct {
	// Host is the upstream host, e.g. "github.com". It is always lower case.
	Host string
	// Path is the upstream path, without a leading slash.
	Path string
}

// ParseAsset builds an [Asset] from an upstream host and the request path below
// it. Both are expected in decoded form.
//
// Nothing is assumed about the layout of path, but every segment has to be
// usable as a segment of an object key: empty, relative and control-character
// segments are rejected, so a request can never address an object outside the
// namespace of its own host.
func ParseAsset(host, assetPath string) (Asset, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !validHost(host) {
		return Asset{}, fmt.Errorf("%w: host %q", errInvalidAsset, host)
	}

	assetPath = strings.TrimPrefix(assetPath, "/")
	if assetPath == "" {
		return Asset{}, fmt.Errorf("%w: empty path", errInvalidAsset)
	}
	for segment := range strings.SplitSeq(assetPath, "/") {
		if !validSegment(segment) {
			return Asset{}, fmt.Errorf("%w: path %q", errInvalidAsset, assetPath)
		}
	}

	return Asset{Host: host, Path: assetPath}, nil
}

// Key returns the object key the asset is mirrored under, before any configured
// key prefix is applied. It leads with the host so that two forges hosting the
// same owner and repository never collide in the bucket.
func (a Asset) Key() string {
	return a.Host + "/" + a.Path
}

// Name returns the last segment of the path. It is the file name the asset is
// downloaded as, and the only thing the mirror reads out of the path — for
// guessing a content type from the extension when upstream does not send a
// useful one.
func (a Asset) Name() string {
	if i := strings.LastIndex(a.Path, "/"); i >= 0 {
		return a.Path[i+1:]
	}
	return a.Path
}

// URL returns the upstream download URL of the asset.
func (a Asset) URL() *url.URL {
	return &url.URL{Scheme: "https", Host: a.Host, Path: "/" + a.Path}
}

func (a Asset) String() string { return a.Key() }

// validHost reports whether host is a plain DNS host name.
//
// Ports, userinfo and IP literals are rejected. The host arrives from the
// request path and is the only thing deciding where a copy connects to, so it
// has to be a name an operator can write down in a [Policy] — an IP literal
// would turn the mirror into a way to address the network it runs in.
// Internationalised names have to be given in their punycode form.
func validHost(host string) bool {
	if host == "" || len(host) > maxHostLength {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}

	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

// validLabel reports whether s is a single DNS label.
func validLabel(s string) bool {
	if s == "" || len(s) > maxLabelLength {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// validSegment reports whether s is usable as a single path segment. Empty,
// relative and control-character segments are rejected, as are segments
// containing a separator, so a segment can never widen the object key.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00") {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}
