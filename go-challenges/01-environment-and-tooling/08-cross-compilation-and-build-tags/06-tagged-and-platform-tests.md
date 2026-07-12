# Exercise 6: Testing Build-Constraint Selection and Tag-Gated Tests

Build constraints apply to `_test.go` files exactly as they do to source files, so
you can test that the host selected a real platform implementation and run an extra
suite only under a tag. This module does both: a plain test rejecting the fallback,
and a `//go:build debug` test that observes a debug-only `init`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
buildsel/                      module example.com/buildsel
  go.mod                       package buildsel
  buildsel.go                  debugInits; Fact(); DebugInits()
  fact_linux.go / _darwin.go / _windows.go / _other.go   one platformFact() each
  debug.go                     //go:build debug -> init() increments debugInits
  platform_test.go             always: host must not select the fallback
  default_test.go              //go:build !debug -> DebugInits()==0
  debug_test.go                //go:build debug  -> DebugInits()==1
  cmd/demo/main.go             prints Fact() and DebugInits()
```

- Files: `buildsel.go`, `fact_*.go`, `debug.go`, `cmd/demo/main.go`, `platform_test.go`, `default_test.go`, `debug_test.go`.
- Implement: a platform-selected `Fact()`, a debug-only `init` that bumps a counter, and the accessors `Fact()`/`DebugInits()`.
- Test: assert the host chose a real platform file; assert the debug counter is 0 without the tag and 1 with it, using `!debug`/`debug`-guarded test files.
- Verify: `go test -count=1 -race ./...` and `go test -count=1 -tags debug ./...` both pass; `go list -tags debug -f '{{.TestGoFiles}}'` shows `debug_test.go` only under the tag.

### Two axes of test selection, and why default_test.go is separate

There are two independent things to test here. First, that the host build selected
a dedicated `fact_<goos>.go` rather than the negated fallback — a property of the
constraint system that holds on every real platform, so `platform_test.go` runs
unconditionally and rejects any `Fact()` beginning with `fallback:`. Second, that
the `//go:build debug` code actually runs when the tag is set. The clean way to
observe a compile-time-only side effect is a package-level counter (`debugInits`)
that the debug `init` increments; a test then asserts the count.

The counter has *different* correct values in the two builds — 0 by default, 1
under `-tags debug` — so a single test cannot cover both. That is why the "expect
0" assertion lives in `default_test.go` guarded by `//go:build !debug` and the
"expect 1" assertion lives in `debug_test.go` guarded by `//go:build debug`. If you
naively put the "expect 0" test in an always-compiled file, it would run under
`-tags debug` too, see the count is 1, and fail. Splitting them along the same tag
that gates the code keeps each assertion in the build where it is true. This is the
general shape: a test whose expectation depends on a tag must itself be gated by
that tag.

Create `buildsel.go`:

```go
// Package buildsel is exercised by two kinds of test: an ordinary test that
// asserts the host selected a real platform file, and a //go:build debug test
// that only runs under -tags debug and observes a debug init side effect.
package buildsel

// debugInits counts how many times the debug init ran; it is 0 in a default
// build (debug.go is excluded) and 1 under -tags debug.
var debugInits int

// Fact reports the platform-selected fact string.
func Fact() string { return platformFact() }

// DebugInits reports how many times the debug-only init ran.
func DebugInits() int { return debugInits }
```

Create `fact_linux.go`:

```go
//go:build linux

package buildsel

func platformFact() string { return "linux: dedicated platform file selected" }
```

Create `fact_darwin.go`:

```go
//go:build darwin

package buildsel

func platformFact() string { return "darwin: dedicated platform file selected" }
```

Create `fact_windows.go`:

```go
//go:build windows

package buildsel

func platformFact() string { return "windows: dedicated platform file selected" }
```

Create `fact_other.go`:

```go
//go:build !linux && !darwin && !windows

package buildsel

func platformFact() string { return "fallback: no dedicated platform file" }
```

Create `debug.go` — the debug-only side effect the tagged test observes:

```go
//go:build debug

package buildsel

func init() { debugInits++ }
```

### The runnable demo

The demo prints the selected fact and the debug counter, so running it with and
without `-tags debug` shows the counter move from 0 to 1.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/buildsel"
)

func main() {
	fmt.Println(buildsel.Fact())
	fmt.Printf("debug inits: %d\n", buildsel.DebugInits())
}
```

Run it both ways:

```bash
go run ./cmd/demo
go run -tags debug ./cmd/demo
```

Expected output on darwin/arm64, default build:

```text
darwin: dedicated platform file selected
debug inits: 0
```

Expected output under `-tags debug`:

```text
darwin: dedicated platform file selected
debug inits: 1
```

### The tests

`platform_test.go` runs in every build and rejects the fallback. `default_test.go`
(`//go:build !debug`) asserts the counter is 0. `debug_test.go` (`//go:build
debug`) asserts it is 1 and runs only under the tag.

Create `platform_test.go`:

```go
package buildsel

import (
	"strings"
	"testing"
)

func TestHostSelectedRealPlatformFile(t *testing.T) {
	t.Parallel()
	got := Fact()
	if strings.HasPrefix(got, "fallback:") {
		t.Fatalf("Fact() = %q; host fell through to the fallback file", got)
	}
}
```

Create `default_test.go`:

```go
//go:build !debug

package buildsel

import "testing"

func TestDefaultBuildRanNoDebugInit(t *testing.T) {
	t.Parallel()
	if got := DebugInits(); got != 0 {
		t.Fatalf("DebugInits() = %d in a default build; want 0", got)
	}
}
```

Create `debug_test.go`:

```go
//go:build debug

package buildsel

import "testing"

func TestDebugInitRan(t *testing.T) {
	if got := DebugInits(); got != 1 {
		t.Fatalf("DebugInits() = %d under -tags debug; want 1", got)
	}
}
```

Confirm the test file set differs by tag:

```bash
go list -f '{{.TestGoFiles}}' .
go list -tags debug -f '{{.TestGoFiles}}' .
```

```text
[default_test.go platform_test.go]
[debug_test.go platform_test.go]
```

## Review

The module is correct when `go test` passes by default and `go test -tags debug`
also passes — the second only works because the "expect 0" and "expect 1"
assertions are split along the same tag that gates the code. The default run is the
one CI executes on every commit; the tagged run belongs in a separate stage. The
mistake to avoid is asserting a tag-dependent value in an always-compiled test
file: it passes in one build and fails in the other, which reads as a flaky test
until you notice it is really a mis-scoped constraint. The `go list -tags` diff is
the proof that `debug_test.go` is compiled into the test binary only under the tag.

## Resources

- [Build constraints on test files (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — constraints apply to `_test.go` files identically.
- [`testing`](https://pkg.go.dev/testing) — the test runner and `t.Parallel`.
- [`go test` flags: -tags (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — running a tagged test suite.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-version-stamping-ldflags.md](07-version-stamping-ldflags.md)
