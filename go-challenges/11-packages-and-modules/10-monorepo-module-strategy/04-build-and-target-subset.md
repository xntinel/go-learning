# Exercise 4: Build, test, and target a subset of the monorepo

A monorepo's CI must not rebuild the world on every commit. This exercise turns
the run/verify step into real build hygiene: the whole-module commands
(`go build ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l`) plus the
package-scoped commands that let CI build only the service that changed. The
testable artifact is a `changeset` helper that maps changed file paths to the set
of service package directories CI should target — the logic behind a selective
build.

## What you'll build

```text
mono/                         single module: example.com/mono
  go.mod
  changeset/
    changeset.go              ServicesFor(changed []string) []string
    changeset_test.go         table-driven mapping + shared-change fan-out tests
  cmd/
    demo/
      main.go                 prints the targeted build set for a sample diff
```

- Files: `changeset/changeset.go`, `changeset/changeset_test.go`, `cmd/demo/main.go`.
- Implement: `ServicesFor(changed []string) []string`, mapping changed paths to the `cmd/<svc>` directories to rebuild, and returning the everything-sentinel `./...` when a shared path (`platform/`, `internal/`) changed.
- Test: table-driven cases for single-service, multi-service, shared-fan-out, and no-op diffs, asserting the exact sorted target set.
- Verify: `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, `gofmt -l .` (empty).

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/10-monorepo-module-strategy/04-build-and-target-subset/changeset go-solutions/11-packages-and-modules/10-monorepo-module-strategy/04-build-and-target-subset/cmd/demo
cd go-solutions/11-packages-and-modules/10-monorepo-module-strategy/04-build-and-target-subset
```

### The whole-module commands, and why they still matter

Even with selective builds, the whole-module commands are the baseline every
monorepo runs:

- `go build ./...` compiles every package in the module; in a single module this
  is the fastest possible "does the world still compile" check.
- `go vet ./...` runs the static analyzers (printf, lostcancel, shadow-ish
  checks) across the module.
- `go test -race -count=1 ./...` runs all package tests under the race detector,
  with `-count=1` disabling the test cache so the run is real.
- `gofmt -l .` lists files that are not gofmt-clean; CI asserts the output is
  empty, because space-indented or misformatted Go is a hard failure.
- `go list ./...` enumerates the module's import paths — the raw material for
  deciding what to build.

Package-scoped forms narrow the blast radius: `go test ./cmd/api/...` tests only
the API service and everything under it. That is what makes a monorepo's CI
scale — a one-line change to `cmd/api` should not run `cmd/worker`'s test suite.

### The selective-build helper

`ServicesFor` encodes the real CI rule. Given the list of changed files (what
`git diff --name-only` produces), it decides which service package directories to
rebuild:

- A change under `cmd/<svc>/...` targets that service's directory (`cmd/<svc>`).
- A change under a **shared** path (`platform/...` or a repo-root `internal/...`)
  can affect *every* service, so the safe answer is the everything-sentinel
  `./...` — rebuild all. Under-building a shared change is the dangerous failure:
  you ship a service compiled against stale shared code.
- Files that touch neither (docs, top-level configs) target nothing on their own.

The result is a sorted, de-duplicated set so CI output is stable. This is
deliberately conservative: when in doubt it fans out to `./...`, because a false
"nothing changed" is a broken release and a false "rebuild everything" merely
costs minutes.

Create `changeset/changeset.go`:

```go
package changeset

import (
	"slices"
	"strings"
)

// All is the sentinel meaning "rebuild and test every package": returned when a
// change touches shared code that any service may depend on.
const All = "./..."

// sharedPrefixes are the paths whose changes can affect every service.
var sharedPrefixes = []string{"platform/", "internal/"}

// ServicesFor maps a set of changed file paths to the package directories CI
// must rebuild and test. A change under a shared prefix fans out to All. Service
// changes map to their cmd/<svc> directory. The result is sorted and unique.
func ServicesFor(changed []string) []string {
	targets := make(map[string]struct{})
	for _, path := range changed {
		path = strings.TrimPrefix(path, "./")

		if isShared(path) {
			return []string{All}
		}
		if svc, ok := serviceDir(path); ok {
			targets[svc] = struct{}{}
		}
	}

	out := make([]string, 0, len(targets))
	for svc := range targets {
		out = append(out, svc)
	}
	slices.Sort(out)
	return out
}

func isShared(path string) bool {
	for _, p := range sharedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// serviceDir returns the cmd/<svc> directory for a path under cmd/, if any.
func serviceDir(path string) (string, bool) {
	const cmd = "cmd/"
	if !strings.HasPrefix(path, cmd) {
		return "", false
	}
	rest := path[len(cmd):]
	name, _, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return "", false
	}
	return cmd + name, true
}
```

### The runnable demo

The demo feeds a representative diff — a change to the API and a change to a
shared file — and prints what CI would target in each case, showing the fan-out to
`./...` when shared code moves.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mono/changeset"
)

func main() {
	diffs := map[string][]string{
		"api only":     {"cmd/api/handler.go", "cmd/api/main.go"},
		"two services": {"cmd/api/handler.go", "cmd/worker/worker.go"},
		"shared lib":   {"platform/httpx/httpx.go", "cmd/api/handler.go"},
		"docs only":    {"README.md", "docs/adr/0001.md"},
	}

	for _, name := range []string{"api only", "two services", "shared lib", "docs only"} {
		fmt.Printf("%-13s -> %v\n", name, changeset.ServicesFor(diffs[name]))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api only      -> [cmd/api]
two services  -> [cmd/api cmd/worker]
shared lib    -> [./...]
docs only     -> []
```

### Tests

The test pins the exact target set for each shape of diff, including the two
that matter most: a shared-code change must fan out to `[./...]`, and a docs-only
change must target nothing. `slices.Equal` compares the sorted result so the
assertion is order-stable.

Create `changeset/changeset_test.go`:

```go
package changeset

import (
	"slices"
	"testing"
)

func TestServicesFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"single service", []string{"cmd/api/handler.go"}, []string{"cmd/api"}},
		{"two services", []string{"cmd/worker/w.go", "cmd/api/h.go"}, []string{"cmd/api", "cmd/worker"}},
		{"dedup within a service", []string{"cmd/api/h.go", "cmd/api/main.go"}, []string{"cmd/api"}},
		{"shared platform fans out", []string{"platform/httpx/httpx.go"}, []string{All}},
		{"shared internal fans out", []string{"internal/auth/auth.go", "cmd/api/h.go"}, []string{All}},
		{"docs only targets nothing", []string{"README.md", "docs/x.md"}, []string{}},
		{"leading dot-slash normalized", []string{"./cmd/api/h.go"}, []string{"cmd/api"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ServicesFor(tc.changed)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ServicesFor(%v) = %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

func TestServicesForShortCircuitsOnShared(t *testing.T) {
	t.Parallel()

	// Even mixed with many service changes, a shared change wins with All.
	got := ServicesFor([]string{"cmd/api/h.go", "cmd/worker/w.go", "platform/log/log.go"})
	if !slices.Equal(got, []string{All}) {
		t.Errorf("mixed-with-shared = %v, want [%s]", got, All)
	}
}
```

## Review

The helper is correct when it is conservative in the right direction: any shared
change short-circuits to `./...`, so CI can never under-build a service against
stale shared code, while independent service changes map only to their own
`cmd/<svc>` directory. The docs-only case returning an empty set proves the
mapping does not invent work, and the dedup case proves multiple files in one
service collapse to a single target.

The mistake to avoid is treating a `platform/` change as local — mapping it only
to the services whose imports you *think* it touches. In a monorepo, import graphs
change under you; the safe rule is "shared moved, rebuild all". For the
whole-module verification, run `go build ./...`, `go vet ./...`, and
`go test -race -count=1 ./...`, and assert `gofmt -l .` prints nothing — the same
four commands CI runs before it ever considers a selective build.

## Resources

- [`cmd/go`: build constraints and package lists](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns) — what `./...` and `./cmd/api/...` match.
- [`go vet`](https://pkg.go.dev/cmd/vet) — the analyzers the whole-module vet runs.
- [`slices`](https://pkg.go.dev/slices) — `slices.Sort` and `slices.Equal` used by the helper and its test.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — why CI runs `go test -race`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-convert-to-workspace.md](05-convert-to-workspace.md)
