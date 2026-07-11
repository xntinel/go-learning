# Exercise 9: Operating the Detector in CI -- the race Build Tag and GORACE Tuning

The final layer is operational: how a team runs the detector in CI without it
fighting them. This exercise builds the two pieces that make that work -- a
`raceEnabled` constant selected by `//go:build race` versus `//go:build !race`
files so code can know whether it was built with the detector, and an
intentionally-racy micro-benchmark gated behind `//go:build !race` so
`go test -race` never trips on it -- and documents the CI race target with its
`GORACE` tuning and its cost budget.

This module is self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
racemode/                   independent module: example.com/racemode
  go.mod                    go 1.26
  mode.go                   RaceEnabled() bool
  race_on.go                //go:build race     -> const raceEnabled = true
  race_off.go               //go:build !race    -> const raceEnabled = false
  cmd/
    demo/
      main.go               prints whether the detector is compiled in
  mode_test.go              TestReportsRaceMode cross-checks raceEnabled vs a CI sentinel
  bench_test.go             //go:build !race  -> intentionally-racy BenchmarkHotPath
```

Files: `mode.go`, `race_on.go`, `race_off.go`, `cmd/demo/main.go`,
`mode_test.go`, `bench_test.go`.
Implement: `RaceEnabled()` backed by build-tag-selected constants; an
intentionally-racy benchmark excluded from race builds.
Test: `TestReportsRaceMode` documents/cross-checks the mode; the benchmark
compiles only under `!race`.
Verify: `go test -count=1 -race ./...` (green); `go test -bench . -run '^$'`
(runs the benchmark, no `-race`).

Set up the module:

```bash
mkdir -p ~/go-exercises/racemode/cmd/demo
cd ~/go-exercises/racemode
go mod init example.com/racemode
```

### The race build tag

When you build with `-race`, the toolchain automatically defines the `race` build
constraint, so a file guarded by `//go:build race` is included only in race
builds and a file guarded by `//go:build !race` only in non-race builds. That
lets code branch on whether the detector is present -- to log it, to adjust a
timeout that the 2-20x slowdown would otherwise blow, or, as here, to exclude
code that must not run under the detector. The idiomatic form is two tiny files
that each define the same constant with opposite values:

Create `race_on.go`:

```go
//go:build race

package racemode

// raceEnabled is true because this file is compiled only with `-race` (the race
// build constraint is set automatically by `go test -race` / `go build -race`).
const raceEnabled = true
```

Create `race_off.go`:

```go
//go:build !race

package racemode

// raceEnabled is false in ordinary (non-race) builds.
const raceEnabled = false
```

Create `mode.go`, which exposes the constant through an exported accessor so
`cmd/demo` (a separate package) can read it:

```go
package racemode

// RaceEnabled reports whether this binary was built with the race detector,
// as selected by the race build tag in race_on.go / race_off.go. Exactly one of
// those files is compiled into any given build, so raceEnabled is unambiguous.
func RaceEnabled() bool { return raceEnabled }
```

Only one of `race_on.go` / `race_off.go` is ever compiled into a build, so
`raceEnabled` is always defined exactly once -- there is no duplicate-symbol
problem, and `go vet` (a non-race build) sees the `false` file while
`go test -race` sees the `true` file.

### Keeping an intentionally-racy benchmark out of the race build

A micro-benchmark that deliberately measures an unsynchronized hot path would, if
compiled under `-race`, make the CI race gate red forever. The fix is to gate the
whole benchmark file behind `//go:build !race`. Then `go test -race` never
compiles it (so the gate stays green), while a plain `go test -bench` -- built
without `-race` -- does compile and run it. This is the standard way to keep a
deliberately-racy artifact from breaking the race gate; the alternative is to keep
it under `cmd/racy` and run it manually, as the earlier exercises did.

Create `bench_test.go`:

```go
//go:build !race

package racemode

import (
	"sync"
	"testing"
)

// sharedCounter is incremented with no synchronization on purpose. This file is
// gated behind //go:build !race, so `go test -race` never compiles it and the
// race gate stays green. Run the benchmark WITHOUT -race:
//
//	go test -bench . -run '^$'
var sharedCounter int

// BenchmarkHotPath measures an intentionally-unsynchronized increment across
// goroutines. It is a demonstration of a benchmark that must be excluded from
// race builds, not a pattern to imitate in real code.
func BenchmarkHotPath(b *testing.B) {
	for b.Loop() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sharedCounter++ // deliberate data race; only ever built under !race
			}()
		}
		wg.Wait()
	}
}
```

`b.Loop()` (Go 1.24+) is the modern benchmark loop: it runs the body the right
number of times and keeps the benchmarked work from being optimized away, so you
do not write the old `for i := 0; i < b.N; i++` form.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/racemode"
)

func main() {
	fmt.Printf("race detector compiled in: %v\n", racemode.RaceEnabled())
}
```

Run it without the detector:

```bash
go run ./cmd/demo
```

Expected output:

```text
race detector compiled in: false
```

Run it with the detector and the line flips to `true`:

```bash
go run -race ./cmd/demo
```

### The CI race target and its GORACE tuning

A CI race job runs the tests with `-race`, varies interleavings with `-count`,
and configures the detector through `GORACE` so a detected race fails the build
with a known exit code and readable stacks. A representative target:

```make
# Run in CI only; never a production build flag. -race adds ~2-20x CPU and
# ~5-10x memory and needs cgo + a C compiler, so it is a test/CI flag exclusively.
.PHONY: race
race:
	GORACE="halt_on_error=1 exitcode=1 log_path=stdout history_size=2" \
		go test -race -count=1 ./...
```

The `GORACE` options: `halt_on_error=1` stops at the first race so the job fails
fast; `exitcode=1` makes a detected race exit non-zero with a code the harness
recognizes (the default is 66); `log_path=stdout` sends reports to stdout so CI
captures them with the rest of the log; `history_size=2` enlarges the
per-goroutine access history so deep call chains do not print "failed to restore
the stack." Add `-count=N` on the concurrent suites to widen interleaving
coverage. The one rule that never bends: this is a CI and load-test flag. You do
not ship a production binary built with `-race`.

### Tests

`TestReportsRaceMode` documents the compiled mode and, when the CI target sets a
`WANT_RACE` sentinel, cross-checks that `RaceEnabled()` matches it -- so the job
can assert "this run really was built with `-race`." With no sentinel set (the
default local run and the offline gate), it simply logs the value and passes,
which keeps it honest rather than asserting something it cannot know.

Create `mode_test.go`:

```go
package racemode

import (
	"os"
	"testing"
)

func TestReportsRaceMode(t *testing.T) {
	t.Parallel()

	got := RaceEnabled()

	// The CI race target may export WANT_RACE=1 (and the non-race job WANT_RACE=0)
	// so this test can verify the build mode is what CI intended.
	if want, ok := os.LookupEnv("WANT_RACE"); ok {
		if (want == "1") != got {
			t.Fatalf("RaceEnabled() = %v, but WANT_RACE=%q", got, want)
		}
	}

	t.Logf("RaceEnabled() = %v", got)
}

func TestRaceEnabledIsStable(t *testing.T) {
	t.Parallel()

	// RaceEnabled must be a pure constant read: two calls always agree.
	if RaceEnabled() != RaceEnabled() {
		t.Fatal("RaceEnabled() is not stable")
	}
}
```

## Review

The operational layer is correct when the race gate stays green on real code and
the intentionally-racy artifact is provably excluded from it. The proof is
twofold: `go test -race -count=1 ./...` passes because `bench_test.go` is gated
out of the race build and `race_on.go` supplies `raceEnabled = true`; and
`go test -bench . -run '^$'` (no `-race`) compiles and runs the benchmark from the
`!race` build. `TestReportsRaceMode` lets CI assert the build mode via a sentinel
without failing a local run.

The mistakes to avoid: leaving an intentionally-racy benchmark or demo in the
normal suite so `go test -race` trips on it (gate it behind `//go:build !race` or
keep it under `cmd/racy`), shipping a production binary built with `-race` (it is
CI-only), and ignoring `GORACE` in CI so a report passes unnoticed or a deep
stack prints "failed to restore the stack" (set `exitcode`, `halt_on_error`, and
`history_size`). Run `go test -count=1 -race ./...`.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) -- the `race` build tag, `GORACE` options, supported platforms, and the runtime-only nature of the detector.
- [Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) -- `//go:build race` / `//go:build !race` and how the `race` tag is set.
- [`testing.B`](https://pkg.go.dev/testing#B.Loop) -- `b.Loop`, the modern benchmark loop.
- [cmd/go: Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) -- `-race`, `-count`, and `-bench`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-httptest-concurrent-handler-race.md](08-httptest-concurrent-handler-race.md) | Next: [../22-testmain-setup-teardown/00-concepts.md](../22-testmain-setup-teardown/00-concepts.md)
