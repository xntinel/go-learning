# Exercise 6: Semantic Version Tag Validator for a Release Gate

A CI release gate should refuse to deploy from a malformed git tag. This module
builds the validator: it parses a tag against the official SemVer 2.0.0 regex —
the one published on semver.org — with named groups for major, minor, patch,
prerelease, and build, returns a structured `Version`, and rejects anything that is
not a whole, anchored semantic version before a deploy proceeds.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
semver/                     independent module: example.com/semver
  go.mod                    go 1.26
  semver.go                 type Version; official anchored SemVer regex; Parse via SubexpIndex; Atoi
  cmd/
    demo/
      main.go               runnable demo: parse valid tags, reject a bad one
  semver_test.go            table-driven: spec examples parse; invalid tags rejected; anchors pinned
```

- Files: `semver.go`, `cmd/demo/main.go`, `semver_test.go`.
- Implement: `Parse(tag string) (Version, error)` using the official anchored SemVer regex with named groups, an explicit optional leading `v`, `SubexpIndex` for field lookup, and `strconv.Atoi` for the numeric fields.
- Test: `1.2.3`, `1.0.0-alpha.1`, `1.0.0+build.5`, `1.0.0-rc.1+meta` parse with correct named fields; `1.2`, `1.2.3.4`, `01.2.3`, and an embedded match are rejected; the leading `v` is handled explicitly.
- Verify: `go test -count=1 -race ./...`

### The official regex, anchors, and the leading v

The SemVer 2.0.0 specification publishes a canonical regex (semver.org, the
"suggested regular expression"). Using it verbatim — rather than hand-rolling one —
is the same lesson as "use `net/url` for URLs": the format has a real grammar with
corners (a prerelease is dot-separated identifiers, a numeric identifier may not
have a leading zero, build metadata is separate from prerelease) that a casual
pattern gets wrong. The published pattern encodes all of it, with named groups
`major`, `minor`, `patch`, `prerelease`, and `buildmetadata`.

Two details make it a correct *validator* rather than a matcher. First, it is
**anchored** with `^...$`, so `Parse` accepts only a string that is entirely a
version — `1.2.3-garbage` is rejected because the anchors demand the whole string
conform, and `x1.2.3y` never matches. An unanchored version regex is a classic
validation-bypass bug. Second, the leading `v` common on git tags (`v1.2.3`) is
**not** part of SemVer, so `Parse` strips exactly one optional leading `v`
explicitly before matching, rather than smuggling it into the pattern. That keeps
the regex the canonical one and makes the `v`-handling a visible, testable decision.

Numeric fields are converted with `strconv.Atoi` so the returned `Version` carries
integers you can compare, not strings. Because the regex already guarantees each
numeric group is digits with no leading zero, the `Atoi` cannot fail on a matched
tag — but the code checks the error anyway, because swallowing it would be exactly
the kind of latent bug this lesson warns against.

Create `semver.go`:

```go
package semver

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrInvalid is returned for a tag that is not a valid semantic version.
var ErrInvalid = errors.New("invalid semantic version")

// semverRe is the official SemVer 2.0.0 regular expression (semver.org), anchored
// so the whole string must be a version.
var semverRe = regexp.MustCompile(
	`^(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)` +
		`(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+(?P<buildmetadata>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

var (
	idxMajor = semverRe.SubexpIndex("major")
	idxMinor = semverRe.SubexpIndex("minor")
	idxPatch = semverRe.SubexpIndex("patch")
	idxPre   = semverRe.SubexpIndex("prerelease")
	idxBuild = semverRe.SubexpIndex("buildmetadata")
)

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch int
	Prerelease          string
	Build               string
}

// Parse validates a tag against the SemVer spec and returns a structured Version.
// A single optional leading "v" (common on git tags) is stripped explicitly.
func Parse(tag string) (Version, error) {
	s := strings.TrimPrefix(tag, "v")
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalid, tag)
	}
	major, err := strconv.Atoi(m[idxMajor])
	if err != nil {
		return Version{}, fmt.Errorf("%w: major: %v", ErrInvalid, err)
	}
	minor, err := strconv.Atoi(m[idxMinor])
	if err != nil {
		return Version{}, fmt.Errorf("%w: minor: %v", ErrInvalid, err)
	}
	patch, err := strconv.Atoi(m[idxPatch])
	if err != nil {
		return Version{}, fmt.Errorf("%w: patch: %v", ErrInvalid, err)
	}
	return Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Prerelease: m[idxPre],
		Build:      m[idxBuild],
	}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/semver"
)

func main() {
	for _, tag := range []string{"v1.2.3", "1.0.0-rc.1+meta", "1.2"} {
		v, err := semver.Parse(tag)
		if errors.Is(err, semver.ErrInvalid) {
			fmt.Printf("%-16s REJECTED\n", tag)
			continue
		}
		fmt.Printf("%-16s major=%d minor=%d patch=%d pre=%q build=%q\n",
			tag, v.Major, v.Minor, v.Patch, v.Prerelease, v.Build)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1.2.3           major=1 minor=2 patch=3 pre="" build=""
1.0.0-rc.1+meta  major=1 minor=0 patch=0 pre="rc.1" build="meta"
1.2              REJECTED
```

### Tests

Create `semver_test.go`:

```go
package semver

import (
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tag  string
		want Version
	}{
		{"1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"1.0.0-alpha.1", Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "alpha.1"}},
		{"1.0.0+build.5", Version{Major: 1, Minor: 0, Patch: 0, Build: "build.5"}},
		{"1.0.0-rc.1+meta", Version{Major: 1, Minor: 0, Patch: 0, Prerelease: "rc.1", Build: "meta"}},
		{"10.20.30", Version{Major: 10, Minor: 20, Patch: 30}},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.tag)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.tag, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.tag, got, tc.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()
	// Includes leading-zero, too-few and too-many components, and embedded matches
	// that anchors must reject.
	for _, tag := range []string{"1.2", "1.2.3.4", "01.2.3", "1.2.3-", "x1.2.3", "1.2.3y", ""} {
		if _, err := Parse(tag); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Parse(%q) err = %v, want ErrInvalid", tag, err)
		}
	}
}

func TestAnchorsRejectEmbedded(t *testing.T) {
	t.Parallel()
	// A valid version embedded in surrounding text must not validate.
	if _, err := Parse("release-1.2.3-build"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("embedded version accepted: %v", err)
	}
}
```

## Review

The validator is correct because it uses the specification's own regex and anchors
it: `TestParseValid` confirms every SemVer example decomposes into the right named
fields, and `TestParseInvalid` plus `TestAnchorsRejectEmbedded` confirm the
`^...$` anchors reject `1.2` (too few), `1.2.3.4` (too many), `01.2.3` (leading
zero), and any version embedded in other text. Field lookup is by `SubexpIndex`,
so the pattern's group order is not baked into the code. The leading-`v` strip is
explicit and tested, keeping the regex canonical rather than a local variant. The
mistake this exercise exists to prevent is the unanchored "version validator" that
accepts `1.2.3-garbage` embedded in noise; the anchors are the fix. Run
`go test -race`, since the package-level regex is shared across concurrent gate
checks.

## Resources

- [Semantic Versioning 2.0.0](https://semver.org/) — the spec and its suggested regular expression, used here verbatim.
- [`regexp` package](https://pkg.go.dev/regexp) — `FindStringSubmatch`, `SubexpIndex`, and anchoring.
- [`strconv.Atoi`](https://pkg.go.dev/strconv#Atoi) — converting the numeric fields for comparison.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-regex-path-router.md](05-regex-path-router.md) | Next: [07-config-placeholder-expander.md](07-config-placeholder-expander.md)
