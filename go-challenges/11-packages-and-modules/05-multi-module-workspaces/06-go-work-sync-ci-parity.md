# Exercise 6: Sync Workspace-Selected Versions Back Into go.mod

A workspace's Minimal Version Selection sees every module at once, so it can pick
a higher version of a shared dependency than a lagging service's `go.mod` records.
Your local build uses that higher version; CI — which never reads `go.work` —
uses the lower one the `go.mod` still names. `go work sync` closes the gap by
writing the workspace-selected versions back into each `go.mod`. This exercise
builds the exact selection `go work sync` performs (the maximum of the required
minimums) so the drift is concrete and testable.

## What you'll build

```text
platform/                      gated module: example.com/platform
  go.mod                       go 1.26
  mvs/
    mvs.go                     package mvs; Select returns the MVS-chosen version
    mvs_test.go                the drift scenario: workspace picks the higher minimum
  cmd/
    demo/
      main.go                  prints the version go work sync would write back
```

- Files: `mvs/mvs.go`, `mvs/mvs_test.go`, `cmd/demo/main.go`.
- Implement: `Select(required []string) (string, error)` returning the maximum of a set of `vMAJOR.MINOR.PATCH` minimums, with a wrapped `ErrBadVersion` sentinel.
- Test: a drift case where one module lags (`v1.2.0`) and the workspace selects the higher `v1.5.0`.
- Verify: before sync a `GOWORK=off` build in the lagging module differs from the workspace build; after `go work sync` both select the identical version.

Set up the gated module:

```bash
mkdir -p ~/platform/mvs ~/platform/cmd/demo
cd ~/platform
go mod init example.com/platform
go mod edit -go=1.26
```

### The drift, and how sync removes it

Two services share a dependency. `greeter/go.mod` requires it at `v1.5.0`;
`billing/go.mod` still records `v1.2.0`. Under the workspace, MVS unions the
requirements across both main modules and selects the maximum minimum — `v1.5.0` —
so your local build of `billing` silently uses `v1.5.0`. CI builds `billing`
alone, without `go.work`, and selects the `v1.2.0` its `go.mod` names. If `billing`
started depending on an API added in `v1.5.0`, it compiles for you and fails in CI.

The reconciler is `go work sync`. It walks the workspace, computes the selected
version of every dependency, and writes those versions back into each module's
`go.mod`:

```bash
cd ~/mono
go work sync
# billing/go.mod now requires the shared dep at v1.5.0, matching the workspace
```

Verify parity by dropping the overlay and building the way CI does:

```bash
cd ~/mono/billing
GOWORK=off go build ./...        # before sync: may resolve v1.2.0 and fail
# ... run go work sync at the root ...
GOWORK=off go build ./...        # after sync: resolves v1.5.0, matches local
```

Contrast the manual fix, `go mod edit -require=example.com/dep@v1.5.0` in each
module — correct only if you remember every module and never mistype a version.
`go work sync` derives the versions from the actual workspace selection.

The gated artifact is that selection. `Select` takes the required minimums the
modules declare and returns the maximum — the version `go work sync` would write
into each `go.mod`. It parses `vMAJOR.MINOR.PATCH` and compares numerically, so
`v1.10.0` correctly outranks `v1.9.0` (a lexical string compare would get that
wrong).

Create `mvs/mvs.go`:

```go
// mvs/mvs.go
package mvs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrBadVersion is returned for a version not of the form vMAJOR.MINOR.PATCH.
var ErrBadVersion = errors.New("mvs: malformed version")

type parsed struct {
	major, minor, patch int
	raw                 string
}

func parse(v string) (parsed, error) {
	s, ok := strings.CutPrefix(v, "v")
	if !ok {
		return parsed{}, fmt.Errorf("%q: missing v prefix: %w", v, ErrBadVersion)
	}
	fields := strings.Split(s, ".")
	if len(fields) != 3 {
		return parsed{}, fmt.Errorf("%q: want MAJOR.MINOR.PATCH: %w", v, ErrBadVersion)
	}
	nums := make([]int, 3)
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parsed{}, fmt.Errorf("%q: bad number %q: %w", v, f, ErrBadVersion)
		}
		nums[i] = n
	}
	return parsed{nums[0], nums[1], nums[2], v}, nil
}

// less reports whether a precedes b in version order.
func less(a, b parsed) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

// Select returns the version MVS chooses from a set of required minimums: the
// maximum of them. This is the version go work sync writes back into each go.mod.
func Select(required []string) (string, error) {
	if len(required) == 0 {
		return "", fmt.Errorf("no requirements: %w", ErrBadVersion)
	}
	best, err := parse(required[0])
	if err != nil {
		return "", err
	}
	for _, v := range required[1:] {
		p, err := parse(v)
		if err != nil {
			return "", err
		}
		if less(best, p) {
			best = p
		}
	}
	return best.raw, nil
}
```

### The demo

The demo runs the drift scenario: two modules declare different minimums and the
demo prints the version `go work sync` would settle on.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/platform/mvs"
)

func main() {
	// greeter requires v1.5.0; billing lags at v1.2.0.
	selected, err := mvs.Select([]string{"v1.5.0", "v1.2.0"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("go work sync would write:", selected)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
go work sync would write: v1.5.0
```

### Tests

The drift row is the core case. The double-digit row pins numeric (not lexical)
comparison, and the malformed row asserts the sentinel via `errors.Is`.

Create `mvs/mvs_test.go`:

```go
// mvs/mvs_test.go
package mvs

import (
	"errors"
	"testing"
)

func TestSelect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		required []string
		want     string
		wantErr  error
	}{
		{"drift picks higher", []string{"v1.5.0", "v1.2.0"}, "v1.5.0", nil},
		{"order independent", []string{"v1.2.0", "v1.5.0", "v1.3.7"}, "v1.5.0", nil},
		{"numeric not lexical", []string{"v1.9.0", "v1.10.0"}, "v1.10.0", nil},
		{"malformed", []string{"v1.2"}, "", ErrBadVersion},
		{"empty", nil, "", ErrBadVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Select(tc.required)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Select error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Select(%v) = %q, want %q", tc.required, got, tc.want)
			}
		})
	}
}
```

## Review

The invariant to hold is that the local build and the CI build select the same
version graph. Drift appears because the workspace's MVS unions requirements
across all main modules and picks the maximum, while CI resolves each module from
its own `go.mod`. `Select` computes exactly that maximum — the value `go work sync`
writes back — and the numeric-comparison row guards the classic bug of comparing
`v1.10.0` against `v1.9.0` as strings. The two-step verification is the discipline:
`GOWORK=off go build` reproduces CI's selection before and after `go work sync`,
and the passing state is that both builds resolve the synced version. Prefer
`go work sync` over hand-editing each `go.mod`; the tool derives the versions from
the real selection instead of relying on you to update every module by hand.

## Resources

- [go command — Workspace maintenance](https://pkg.go.dev/cmd/go#hdr-Workspace_maintenance) — `go work sync` and what it writes back.
- [Minimal version selection](https://go.dev/ref/mod#minimal-version-selection) — how MVS unions requirements and selects the maximum minimum.
- [Go Modules Reference — Versions](https://go.dev/ref/mod#versions) — the `vMAJOR.MINOR.PATCH` form and semantic version precedence.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-gowork-off-reproduce-ci.md](07-gowork-off-reproduce-ci.md)
