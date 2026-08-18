package mirror

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// DefaultDeniedPaths are the deny patterns a mirror applies unless it is
// configured otherwise.
//
// They reject the aliases forges expose for "whatever the newest release happens
// to be": github.com/<owner>/<repo>/releases/latest/download/<file> and GitLab's
// permalink/latest. Those resolve to different bytes over time, while a mirrored
// object is copied once and served with an immutable cache lifetime — mirroring
// one would pin a moving target for a year with no way to invalidate it.
var DefaultDeniedPaths = []string{"*/releases/latest/*", "permalink/latest"}

// errEmptySegment reports a pattern with a segment that could never match.
var errEmptySegment = errors.New("empty segment")

// Policy decides which upstream paths may be mirrored. It matches an asset key,
// "<host>/<upstream path>", as a sequence of segments against shell-style glob
// patterns as understood by [path.Match]. A wildcard never crosses a slash, so
// every segment is addressed on its own.
//
// Allow patterns are anchored at the host and match a prefix of the key, so a
// pattern covers everything below what it names:
//
//	github.com/ninech/*        every repository of the ninech organisation
//	codeberg.org/ninech/foo    a single repository on Codeberg
//	*/prometheus/alert*        prometheus repositories starting with "alert", on any host
//	github.com                 everything on github.com
//
// Deny patterns are not anchored and match anywhere in the key. Because the
// mirror does not parse upstream layouts, a deny pattern is the only way to
// reject a path element wherever a given forge happens to place it:
//
//	*/releases/latest/*        GitHub's alias for the newest release
//	permalink/latest           GitLab's permalink to the newest release
//
// A denied key is never mirrored, whether or not an allow pattern covers it.
// Matching is case-insensitive, because forges treat owner and repository names
// that way; object keys keep the case they were requested with.
//
// A Policy without allow patterns denies every key, which is the safe default.
type Policy struct {
	allow [][]string
	deny  [][]string
}

// NewPolicy compiles allow and deny patterns into a [Policy]. Blank patterns are
// ignored, surrounding slashes are accepted, and duplicates are collapsed. It
// returns an error for malformed patterns.
func NewPolicy(allow, deny []string) (*Policy, error) {
	allowed, err := compilePatterns(allow)
	if err != nil {
		return nil, fmt.Errorf("allow pattern %w", err)
	}
	denied, err := compilePatterns(deny)
	if err != nil {
		return nil, fmt.Errorf("deny pattern %w", err)
	}
	return &Policy{allow: allowed, deny: denied}, nil
}

// Allows reports whether key, given as "<host>/<path>", may be mirrored.
//
// It applies the deny patterns itself, so a caller that only ever asks Allows
// can never mirror a denied key. [Policy.Denies] exists to tell the two outcomes
// apart when reporting why a key was skipped.
func (p *Policy) Allows(key string) bool {
	if p.Empty() {
		return false
	}

	segments := splitKey(key)
	if p.matchesDeny(segments) {
		return false
	}
	for _, pattern := range p.allow {
		// An allow pattern is anchored, so it has to match from the host on.
		if len(pattern) <= len(segments) && matchSegments(pattern, segments) {
			return true
		}
	}
	return false
}

// Denies reports whether a deny pattern matches anywhere in key.
func (p *Policy) Denies(key string) bool {
	if p == nil {
		return false
	}
	return p.matchesDeny(splitKey(key))
}

// matchesDeny slides every deny pattern along segments, so it matches at any
// depth rather than only at the host.
func (p *Policy) matchesDeny(segments []string) bool {
	for _, pattern := range p.deny {
		for offset := range len(segments) - len(pattern) + 1 {
			if matchSegments(pattern, segments[offset:]) {
				return true
			}
		}
	}
	return false
}

// Empty reports whether the policy holds no allow pattern and therefore denies
// every key.
func (p *Policy) Empty() bool {
	return p == nil || len(p.allow) == 0
}

// Allowed returns the compiled allow patterns in a stable order.
func (p *Policy) Allowed() []string {
	if p == nil {
		return nil
	}
	return joinPatterns(p.allow)
}

// Denied returns the compiled deny patterns in a stable order.
func (p *Policy) Denied() []string {
	if p == nil {
		return nil
	}
	return joinPatterns(p.deny)
}

// String returns the comma-separated patterns, with deny patterns prefixed by "!".
func (p *Policy) String() string {
	parts := p.Allowed()
	for _, denied := range p.Denied() {
		parts = append(parts, "!"+denied)
	}
	return strings.Join(parts, ",")
}

// compilePatterns normalizes patterns and splits them into their segments.
func compilePatterns(patterns []string) ([][]string, error) {
	normalized := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.ToLower(strings.Trim(strings.TrimSpace(p), "/"))
		if p == "" {
			continue
		}
		for segment := range strings.SplitSeq(p, "/") {
			if segment == "" {
				return nil, fmt.Errorf("%q: %w", p, errEmptySegment)
			}
			// path.Match only ever reports ErrBadPattern, so matching against an
			// arbitrary name is enough to validate the pattern itself.
			if _, err := path.Match(segment, ""); err != nil {
				return nil, fmt.Errorf("%q: %w", p, err)
			}
		}
		normalized = append(normalized, p)
	}

	slices.Sort(normalized)
	normalized = slices.Compact(normalized)

	compiled := make([][]string, 0, len(normalized))
	for _, p := range normalized {
		compiled = append(compiled, strings.Split(p, "/"))
	}
	return compiled, nil
}

// splitKey lowercases key and splits it into the segments patterns are matched against.
func splitKey(key string) []string {
	return strings.Split(strings.ToLower(strings.Trim(key, "/")), "/")
}

// matchSegments reports whether every segment of pattern matches the segment of
// segments at the same index. Extra trailing segments are ignored, so a caller
// decides by the length it passes whether the match is anchored.
func matchSegments(pattern, segments []string) bool {
	if len(pattern) > len(segments) {
		return false
	}
	for i, glob := range pattern {
		// The error case is unreachable: compilePatterns rejected bad patterns.
		if ok, err := path.Match(glob, segments[i]); !ok || err != nil {
			return false
		}
	}
	return true
}

// joinPatterns renders compiled patterns back into their normalized text form.
func joinPatterns(patterns [][]string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, strings.Join(p, "/"))
	}
	return out
}
