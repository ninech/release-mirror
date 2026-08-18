package mirror

import (
	"slices"
	"testing"
)

func TestNewPolicy(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		allow, deny         []string
		wantAllow, wantDeny []string
		wantErr             bool
	}{
		"empty":             {},
		"blank are dropped": {allow: []string{"", "   "}},
		"normalized":        {allow: []string{" /GitHub.com/Ninech/Foo/ "}, wantAllow: []string{"github.com/ninech/foo"}},
		"deduplicated":      {allow: []string{"a.com/b", "a.com/b", "A.com/B"}, wantAllow: []string{"a.com/b"}},
		"sorted":            {allow: []string{"b.com/*", "a.com/*"}, wantAllow: []string{"a.com/*", "b.com/*"}},
		"deny compiled":     {allow: []string{"a.com/*"}, deny: DefaultDeniedPaths, wantAllow: []string{"a.com/*"}, wantDeny: []string{"*/releases/latest/*", "permalink/latest"}},
		"malformed allow":   {allow: []string{"a.com/[b"}, wantErr: true},
		"malformed deny":    {deny: []string{"a.com/[b"}, wantErr: true},
		"reported later":    {allow: []string{"ok.com/*", "bad.com/[a-"}, wantErr: true},
		"empty segment":     {allow: []string{"a.com//b"}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := NewPolicy(tc.allow, tc.deny)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NewPolicy(%q, %q) = %v, want error", tc.allow, tc.deny, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPolicy(%q, %q): %v", tc.allow, tc.deny, err)
			}
			if !slices.Equal(got.Allowed(), tc.wantAllow) {
				t.Errorf("Allowed() = %q, want %q", got.Allowed(), tc.wantAllow)
			}
			if !slices.Equal(got.Denied(), tc.wantDeny) {
				t.Errorf("Denied() = %q, want %q", got.Denied(), tc.wantDeny)
			}
		})
	}
}

// TestPolicyAllowsIsAnchored covers the prefix semantics of allow patterns: they
// start at the host and cover everything below the last segment they name.
func TestPolicyAllowsIsAnchored(t *testing.T) {
	t.Parallel()

	const asset = "github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz"

	for name, tc := range map[string]struct {
		patterns []string
		key      string
		want     bool
	}{
		"repository prefix":         {[]string{"github.com/ninech/*"}, asset, true},
		"exact repository":          {[]string{"github.com/ninech/foo"}, asset, true},
		"whole host":                {[]string{"github.com"}, asset, true},
		"other owner":               {[]string{"github.com/other/*"}, asset, false},
		"other host":                {[]string{"codeberg.org/ninech/*"}, asset, false},
		"host wildcard":             {[]string{"*/ninech/*"}, asset, true},
		"partial wildcard":          {[]string{"github.com/prometheus/alert*"}, "github.com/prometheus/alertmanager/releases/download/v1/a.gz", true},
		"partial wildcard mismatch": {[]string{"github.com/prometheus/alert*"}, "github.com/prometheus/prometheus/releases/download/v1/a.gz", false},
		"match everything":          {[]string{"*/*/*"}, asset, true},
		"case insensitive":          {[]string{"github.com/ninech/foo"}, "GitHub.com/Ninech/Foo/releases/download/v1/a.gz", true},
		// A bare wildcard is a whole-host pattern, and by the prefix rule that
		// covers everything below it. Crossing a slash still needs a segment of
		// its own: "*/foo" cannot reach the "foo" two levels down.
		"bare wildcard allows every host": {[]string{"*"}, asset, true},
		"wildcard does not cross slash":   {[]string{"*/foo"}, asset, false},
		"pattern longer than key":         {[]string{"github.com/ninech/foo/releases/download/v1/a/b/c"}, asset, false},
		"not anchored at the host":        {[]string{"ninech/foo"}, asset, false},
		"second pattern matches":          {[]string{"a.com/b", "github.com/ninech/*"}, asset, true},
		"empty list denies":               {nil, asset, false},
		"gitlab nested namespace":         {[]string{"gitlab.com/group/*"}, "gitlab.com/group/sub/proj/-/releases/v1/downloads/a.gz", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPolicy(tc.patterns, nil)
			if err != nil {
				t.Fatalf("NewPolicy(%q): %v", tc.patterns, err)
			}
			if got := p.Allows(tc.key); got != tc.want {
				t.Errorf("Allows(%q) with %q = %t, want %t", tc.key, tc.patterns, got, tc.want)
			}
		})
	}
}

// TestPolicyDeniesAnywhere covers the unanchored semantics of deny patterns:
// they match at any depth, which is what lets one pattern cover forges that
// place the same path element at different depths.
func TestPolicyDeniesAnywhere(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		deny []string
		key  string
		want bool
	}{
		"github latest": {
			DefaultDeniedPaths,
			"github.com/ninech/foo/releases/latest/download/foo.tar.gz",
			true,
		},
		"gitlab permalink": {
			DefaultDeniedPaths,
			"gitlab.com/group/sub/proj/-/releases/permalink/latest/downloads/foo.tar.gz",
			true,
		},
		"codeberg latest": {
			DefaultDeniedPaths,
			"codeberg.org/ninech/foo/releases/latest/download/foo.tar.gz",
			true,
		},
		"pinned github release is untouched": {
			DefaultDeniedPaths,
			"github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz",
			false,
		},
		"pinned gitlab release is untouched": {
			DefaultDeniedPaths,
			"gitlab.com/group/proj/-/releases/v1.0.0/downloads/foo.tar.gz",
			false,
		},
		"a tag merely named latest is untouched": {
			DefaultDeniedPaths,
			"github.com/ninech/foo/releases/download/latest/foo.tar.gz",
			false,
		},
		"case insensitive": {
			DefaultDeniedPaths,
			"github.com/ninech/foo/releases/LATEST/download/foo.tar.gz",
			true,
		},
		"pattern longer than key": {
			[]string{"a/b/c/d/e"},
			"github.com/foo",
			false,
		},
		"matches at the host": {
			[]string{"blocked.example.com"},
			"blocked.example.com/foo/bar.gz",
			true,
		},
		"empty deny list": {
			nil,
			"github.com/ninech/foo/releases/latest/download/foo.tar.gz",
			false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPolicy([]string{"*"}, tc.deny)
			if err != nil {
				t.Fatalf("NewPolicy(deny=%q): %v", tc.deny, err)
			}
			if got := p.Denies(tc.key); got != tc.want {
				t.Errorf("Denies(%q) with %q = %t, want %t", tc.key, tc.deny, got, tc.want)
			}
		})
	}
}

// TestPolicyDenyOverridesAllow is the invariant a caller relies on: Allows alone
// is enough, a denied key can never be mirrored however wide the allow list is.
func TestPolicyDenyOverridesAllow(t *testing.T) {
	t.Parallel()

	p, err := NewPolicy([]string{"*/*/*"}, DefaultDeniedPaths)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	const denied = "github.com/ninech/foo/releases/latest/download/foo.tar.gz"
	if p.Allows(denied) {
		t.Errorf("Allows(%q) = true, want false: the key is denied", denied)
	}

	const allowed = "github.com/ninech/foo/releases/download/v1.0.0/foo.tar.gz"
	if !p.Allows(allowed) {
		t.Errorf("Allows(%q) = false, want true", allowed)
	}
}

func TestPolicyNilDenies(t *testing.T) {
	t.Parallel()

	var p *Policy
	if !p.Empty() {
		t.Error("Empty() = false, want true for the nil policy")
	}
	if p.Allows("github.com/ninech/foo") {
		t.Error("Allows() = true, want false for the nil policy")
	}
	if p.Denies("github.com/ninech/foo") {
		t.Error("Denies() = true, want false for the nil policy")
	}
	if p.Allowed() != nil || p.Denied() != nil {
		t.Errorf("Allowed() = %q, Denied() = %q, want nil", p.Allowed(), p.Denied())
	}
}

// TestPolicyTraversalNotAllowed guards the seam between [ParseAsset] and the
// policy. Matching is anchored at the host and never cleans the key, so a
// request cannot dress a foreign host up as an allowed one; anything containing
// a relative segment is rejected before it reaches the policy at all.
func TestPolicyTraversalNotAllowed(t *testing.T) {
	t.Parallel()

	p, err := NewPolicy([]string{"github.com/ninech/*"}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	// An allowed host cannot be reached by naming it below a different one.
	for _, key := range []string{
		"other.com/../github.com/ninech/foo",
		"other.com/github.com/ninech/foo",
	} {
		if p.Allows(key) {
			t.Errorf("Allows(%q) = true, want false", key)
		}
	}

	// And a key that would climb out of its repository never gets built.
	for _, path := range []string{
		"ninech/foo/../../../other/foo.tar.gz",
		"../other/foo.tar.gz",
	} {
		if asset, err := ParseAsset("github.com", path); err == nil {
			t.Errorf("ParseAsset(%q) = %q, want it rejected", path, asset.Key())
		}
	}
}

func TestPolicyString(t *testing.T) {
	t.Parallel()

	p, err := NewPolicy([]string{"github.com/ninech/*"}, []string{"*/releases/latest/*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if want := "github.com/ninech/*,!*/releases/latest/*"; p.String() != want {
		t.Errorf("String() = %q, want %q", p.String(), want)
	}
}
