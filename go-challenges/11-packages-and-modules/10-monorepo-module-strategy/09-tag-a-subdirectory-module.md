# Exercise 9: Release the platform module with subdirectory tags and /v2

A module that lives in a repo subdirectory does not tag bare `v1.4.0`. Its release
tags carry the subdirectory as a prefix — `platform/v1.4.0` — and a breaking change
requires the `/v2` suffix in the module path *and* the tag. This exercise builds
the helper that produces the correct tag string and enforces the invariant that
ties the tag's major version to the module path's suffix, using
`golang.org/x/mod/semver`.

## What you'll build

```text
release/                      module: example.com/mono/release
  go.mod                      requires golang.org/x/mod
  release.go                  TagFor(subdir, modulePath, version)
  release_test.go             prefix + /v2 invariant cases
  cmd/demo/main.go            prints tags for v1 and v2 of a subdir module
```

- Files: `release.go`, `release_test.go`, `cmd/demo/main.go`.
- Implement: `TagFor(subdir, modulePath, version string) (string, error)` producing `subdir + "/" + version`, rejecting an invalid version, a v2+ version whose module path lacks the matching `/vN` suffix, and a v0/v1 version whose path carries a stray suffix.
- Test: `platform` + `v1.4.0` → `platform/v1.4.0`; `platform` + `/v2` path + `v2.0.0` → `platform/v2.0.0`; a v2 version with a suffix-less path is rejected.
- Verify: `go test -count=1 -race ./...` (needs `golang.org/x/mod`).

Set up the module:

```bash
mkdir -p ~/go-exercises/release/cmd/demo
cd ~/go-exercises/release
go mod init example.com/mono/release
go get golang.org/x/mod
```

### Two invariants: the prefix and the suffix

When a module lives at `example.com/mono/platform` (the `platform/` subdirectory of
the repo), the go command resolves its versions by looking for git tags prefixed
with the subdirectory: `platform/v1.4.0`, not `v1.4.0`. A bare `v1.4.0` tag would be
read as a version of the *repo-root* module, not `platform`. This is the same
convention the standard-library ecosystem uses — `golang.org/x/tools/gopls` tags
`gopls/vX.Y.Z`. So the first invariant is: the tag prefix must equal the module's
subdirectory.

The second invariant is **semantic import versioning**. Major versions 2 and above
must appear as a suffix in the module path itself: the breaking release of
`example.com/mono/platform` is a *new module path*, `example.com/mono/platform/v2`,
declared in the `module` directive of `platform/go.mod`. Importers change their
import to `.../platform/v2` to adopt it, which is what lets v1 and v2 coexist in
one build. The tag is `platform/v2.0.0`. Major versions v0 and v1 use **no**
suffix — `example.com/mono/platform` with a `/v1` suffix is wrong.

`TagFor` enforces both. It validates the version with `semver.IsValid`, derives the
major with `semver.Major` (which returns `v1`, `v2`, …), and checks that the module
path's `/vN` suffix matches: present and equal for v2+, absent for v0/v1. Only then
does it join the subdirectory prefix to the version. A wrong tag or a mismatched
suffix means `go get` resolves the wrong thing — or nothing — so catching it in a
release script is cheap insurance.

Create `release.go`:

```go
package release

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

var (
	// ErrInvalidVersion is returned for a non-canonical semantic version.
	ErrInvalidVersion = errors.New("invalid semantic version")
	// ErrPathMajorMismatch is returned when the module path's /vN suffix does
	// not match the version's major (the semantic-import-versioning rule).
	ErrPathMajorMismatch = errors.New("module path major suffix does not match version")
)

// majorSuffixRe matches a /v2, /v3, ... suffix (v0 and v1 never carry one).
var majorSuffixRe = regexp.MustCompile(`/v([2-9][0-9]*)$`)

// TagFor returns the git tag for a module in repo subdirectory subdir at the
// given version, e.g. TagFor("platform", "example.com/mono/platform", "v1.4.0")
// is "platform/v1.4.0". It enforces the semantic-import-versioning invariant
// between modulePath's /vN suffix and version's major.
func TagFor(subdir, modulePath, version string) (string, error) {
	if !semver.IsValid(version) {
		return "", fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	if err := checkMajorSuffix(modulePath, semver.Major(version)); err != nil {
		return "", err
	}

	subdir = strings.Trim(subdir, "/")
	if subdir == "" {
		return version, nil // repo-root module: bare tag.
	}
	return subdir + "/" + version, nil
}

// checkMajorSuffix verifies modulePath's /vN suffix agrees with major ("v1",
// "v2", ...). v0 and v1 must have no suffix; v2+ must have the matching one.
func checkMajorSuffix(modulePath, major string) error {
	m := majorSuffixRe.FindStringSubmatch(modulePath)

	if major == "v0" || major == "v1" {
		if m != nil {
			return fmt.Errorf("%w: %s must not carry a %q suffix for %s",
				ErrPathMajorMismatch, modulePath, m[0], major)
		}
		return nil
	}

	want := major[1:] // "v2" -> "2"
	if m == nil || m[1] != want {
		return fmt.Errorf("%w: %s lacks the required /%s suffix for %s",
			ErrPathMajorMismatch, modulePath, major, major)
	}
	return nil
}
```

### The runnable demo

The demo produces the v1 tag for `platform`, then the v2 tag once the module path
gains its `/v2` suffix, and finally shows the guard rejecting a v2 version against a
suffix-less path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/release"
)

func main() {
	v1, _ := release.TagFor("platform", "example.com/mono/platform", "v1.4.0")
	fmt.Printf("v1 tag: %s\n", v1)

	v2, _ := release.TagFor("platform", "example.com/mono/platform/v2", "v2.0.0")
	fmt.Printf("v2 tag: %s\n", v2)

	_, err := release.TagFor("platform", "example.com/mono/platform", "v2.0.0")
	fmt.Printf("bad v2: %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1 tag: platform/v1.4.0
v2 tag: platform/v2.0.0
bad v2: module path major suffix does not match version: example.com/mono/platform lacks the required /v2 suffix for v2
```

### Tests

The tests encode the tag ↔ module-path invariant as assertions. Valid v1 and v2
combinations produce the prefixed tag; a v2 version against a suffix-less path and
a v1 version against a `/v2` path both fail with `ErrPathMajorMismatch`; a
non-canonical version fails with `ErrInvalidVersion`; and a repo-root module (empty
subdir) tags bare.

Create `release_test.go`:

```go
package release

import (
	"errors"
	"testing"
)

func TestTagFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		subdir     string
		modulePath string
		version    string
		wantTag    string
		wantErr    error
	}{
		{"v1 subdir", "platform", "example.com/mono/platform", "v1.4.0", "platform/v1.4.0", nil},
		{"v0 subdir", "platform", "example.com/mono/platform", "v0.3.0", "platform/v0.3.0", nil},
		{"v2 with suffix", "platform", "example.com/mono/platform/v2", "v2.0.0", "platform/v2.0.0", nil},
		{"nested subdir", "libs/platform", "example.com/mono/libs/platform", "v1.0.0", "libs/platform/v1.0.0", nil},
		{"repo-root bare tag", "", "example.com/mono", "v1.2.0", "v1.2.0", nil},
		{"v2 without suffix", "platform", "example.com/mono/platform", "v2.0.0", "", ErrPathMajorMismatch},
		{"v1 with stray suffix", "platform", "example.com/mono/platform/v2", "v1.4.0", "", ErrPathMajorMismatch},
		{"non-canonical version", "platform", "example.com/mono/platform", "1.4.0", "", ErrInvalidVersion},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := TagFor(tc.subdir, tc.modulePath, tc.version)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantTag {
				t.Errorf("TagFor = %q, want %q", got, tc.wantTag)
			}
		})
	}
}
```

## Review

The helper is correct when both invariants hold together: the tag always carries
the subdirectory prefix, and the module path's `/vN` suffix agrees with the
version's major — present for v2+, absent for v0/v1. The `platform/v1.4.0` and
`platform/v2.0.0` cases show the happy path; the two mismatch cases are the ones a
release script must catch, because a bare `v2.0.0` tag or a suffix-less path sends
`go get` to the wrong module or to nothing.

The mistake to avoid is thinking a major bump is "just a bigger number". Under
semantic import versioning it is a new module path and a new import statement for
every consumer; the tag prefix and the `/vN` suffix are not decoration, they are how
the proxy and `go get` locate the code. Wire `TagFor` into the release automation so
a wrong tag fails before it is pushed, not after a consumer's build breaks.

## Resources

- [Go Modules Reference: Major version suffixes](https://go.dev/ref/mod#major-version-suffixes) — the `/vN` module-path rule.
- [Go Modules Reference: Mapping versions to commits](https://go.dev/ref/mod#vcs-version) — subdirectory tag prefixes for modules in subdirectories.
- [Developing a major version update](https://go.dev/doc/modules/major-version) — the v2+ workflow end to end.
- [`golang.org/x/mod/semver`](https://pkg.go.dev/golang.org/x/mod/semver) — `IsValid` and `Major` used by the guard.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-vendor-the-workspace-for-ci.md](10-vendor-the-workspace-for-ci.md)
