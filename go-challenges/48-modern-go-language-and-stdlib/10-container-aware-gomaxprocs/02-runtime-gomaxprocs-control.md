# Exercise 2: Pin, Restore, and Force-Refresh GOMAXPROCS at Runtime

`GOMAXPROCS` is process-global state with a sharp edge: reading it is safe, but any
positive-argument write both changes it *and* silently disables the Go 1.25
container-aware auto-updating default. This exercise builds a small operational
control library around that asymmetry and a diagnostic that exposes the
`NumCPU`-vs-`GOMAXPROCS` gap.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
maxprocs/                  independent module: example.com/maxprocs
  go.mod                   go 1.25 (SetDefaultGOMAXPROCS needs it)
  maxprocs.go              Current, Pin (returns restore closure), RestoreDefault
  cmd/
    demo/
      main.go              prints the NumCPU vs GOMAXPROCS(0) gap and pin/restore
  maxprocs_test.go         non-parallel; snapshots a baseline, restores via t.Cleanup
```

- Files: `maxprocs.go`, `cmd/demo/main.go`, `maxprocs_test.go`.
- Implement: `Current() int` (query via `runtime.GOMAXPROCS(0)`, no mutation), `Pin(n int) (restore func())` (set `n`, return a closure that puts the previous value back), `RestoreDefault()` (re-enable the auto-updating container-aware default via `runtime.SetDefaultGOMAXPROCS`).
- Test: capture a baseline with `runtime.GOMAXPROCS(0)`; assert `Current` does not mutate; assert `Pin(2)` then `restore()` round-trips; assert that after `runtime.GOMAXPROCS(1)`, `RestoreDefault()` re-derives a default `>= 1` that is not stuck at the pinned 1 on a multi-core host. No `t.Parallel` anywhere.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/maxprocs/cmd/demo
cd ~/go-exercises/maxprocs
go mod init example.com/maxprocs
go mod edit -go=1.25
```

### The query/mutate asymmetry

`runtime.GOMAXPROCS(n)` is one function doing two jobs. With `n < 1` it is a pure
query: it returns the current setting and changes nothing. With `n >= 1` it is a
mutation: it sets `GOMAXPROCS` to `n`, returns the *previous* value, and — this is
the part that bites — disables both cgroup-awareness and the once-per-second
auto-updates for the rest of the process's life. So `Current()` must pass `0`, and
any code that passes a positive value to "check" the setting is silently pinning
it. `Pin` embraces the mutation deliberately and captures the previous value so it
can be restored:

```go
func Pin(n int) (restore func()) {
	prev := runtime.GOMAXPROCS(n)
	return func() { runtime.GOMAXPROCS(prev) }
}
```

There is a subtlety in that restore closure worth naming: `runtime.GOMAXPROCS(prev)`
puts the numeric value back, but because it is itself a positive-argument call, it
does *not* re-enable auto-updating. If the process was relying on the container-
aware default before `Pin`, restoring the number is not the same as restoring the
behavior. That is exactly why `RestoreDefault` exists and why it calls a different
function.

### Why SetDefaultGOMAXPROCS is the only way back

Once auto-updating is off — whether from the `GOMAXPROCS` environment variable, a
`runtime.GOMAXPROCS(n)` call, or our own `Pin` — there is exactly one in-process
lever to turn it back on: `runtime.SetDefaultGOMAXPROCS()` (added in Go 1.25). It
recomputes `GOMAXPROCS` from the current logical CPU count, affinity mask, and
cgroup quota *as if the environment variable were unset*, and re-enables the
periodic re-reading. It is also the right tool to force an immediate refresh the
moment you know the quota changed, rather than waiting up to a second for the
runtime's own poll. `RestoreDefault` is a one-line wrapper that documents this
intent:

```go
func RestoreDefault() { runtime.SetDefaultGOMAXPROCS() }
```

Create `maxprocs.go`:

```go
// Package maxprocs provides small operational controls over the process-global
// GOMAXPROCS setting: a non-mutating query, a temporary pin with a restore
// closure, and a way to re-enable the Go 1.25 auto-updating container-aware
// default after it has been disabled.
package maxprocs

import "runtime"

// Current returns the current GOMAXPROCS without changing it. It passes 0 to
// runtime.GOMAXPROCS, which is the query form; passing a positive value would
// mutate the setting and disable auto-updating.
func Current() int {
	return runtime.GOMAXPROCS(0)
}

// Pin sets GOMAXPROCS to n and returns a closure that restores the previous
// numeric value. Pin mutates process-global state and, like any positive-argument
// runtime.GOMAXPROCS call, disables cgroup-awareness and periodic auto-updates;
// the returned closure restores the number but not the auto-updating behavior.
// Use RestoreDefault to re-enable that.
func Pin(n int) (restore func()) {
	prev := runtime.GOMAXPROCS(n)
	return func() { runtime.GOMAXPROCS(prev) }
}

// RestoreDefault re-enables the Go 1.25 auto-updating, container-aware GOMAXPROCS
// default, recomputing it from the current CPU count, affinity mask, and cgroup
// quota as if the GOMAXPROCS environment variable were unset. It also forces an
// immediate refresh, which is useful when you know the cgroup quota just changed.
func RestoreDefault() {
	runtime.SetDefaultGOMAXPROCS()
}
```

### The runnable demo

The diagnostic prints the two numbers senior engineers most need to compare. On an
unconstrained host with no `GOMAXPROCS` override they are equal; inside a
CPU-limited container on Go 1.25+ `GOMAXPROCS(0)` is the smaller, cgroup-derived
value and the first line prints `false` — which is the whole point of the feature.
It then pins to 2 and restores to prove the closure round-trips.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"

	"example.com/maxprocs"
)

func main() {
	// Equal on an unconstrained host; false inside a CPU-limited container,
	// where GOMAXPROCS(0) reflects the cgroup quota and NumCPU does not.
	fmt.Printf("NumCPU == GOMAXPROCS(0): %v\n", runtime.NumCPU() == maxprocs.Current())

	restore := maxprocs.Pin(2)
	fmt.Printf("pinned GOMAXPROCS(0) = %d\n", maxprocs.Current())
	restore()

	maxprocs.RestoreDefault()
	fmt.Printf("restored to default (>= 1): %v\n", maxprocs.Current() >= 1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on an unconstrained host with no `GOMAXPROCS` set):

```
NumCPU == GOMAXPROCS(0): true
pinned GOMAXPROCS(0) = 2
restored to default (>= 1): true
```

### Tests

Because `GOMAXPROCS` is process-global, these tests must never run in parallel with
each other or with anything else that touches it — there is no `t.Parallel` call
anywhere in this file, by design. Each test snapshots a baseline with the query
form and restores it with `t.Cleanup` so a test cannot bleed its mutation into the
next. `TestCurrentDoesNotMutate` calls `Current` twice and asserts equality, proving
the query form leaves the value alone. `TestPinRestore` checks `Pin(2)` makes
`Current() == 2` and the closure returns it to the baseline. `TestRestoreDefault`
pins to 1 (the value that disables parallelism), then asserts `RestoreDefault`
re-derives a default `>= 1` that, on a multi-core host, is not stuck at 1 — proving
the auto-updating default was actually re-enabled rather than left pinned.

Create `maxprocs_test.go`:

```go
package maxprocs

import (
	"fmt"
	"runtime"
	"testing"
)

// snapshot captures the current setting and schedules its restoration, so a
// mutating test cannot leak into the next. GOMAXPROCS is process-global, so no
// test in this file may call t.Parallel.
func snapshot(t *testing.T) int {
	t.Helper()
	base := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(base) })
	return base
}

func TestCurrentDoesNotMutate(t *testing.T) {
	base := snapshot(t)
	if got := Current(); got != base {
		t.Fatalf("Current() = %d, want baseline %d", got, base)
	}
	// A second query must still equal the first: the query form never mutates.
	if got := Current(); got != base {
		t.Fatalf("second Current() = %d, want %d; query form mutated", got, base)
	}
}

func TestPinRestore(t *testing.T) {
	base := snapshot(t)
	restore := Pin(2)
	if got := Current(); got != 2 {
		t.Fatalf("after Pin(2), Current() = %d, want 2", got)
	}
	restore()
	if got := Current(); got != base {
		t.Fatalf("after restore(), Current() = %d, want baseline %d", got, base)
	}
}

func TestRestoreDefault(t *testing.T) {
	snapshot(t)
	// Pin to 1, the value that disables scheduler parallelism, then re-enable
	// the auto-updating container-aware default.
	runtime.GOMAXPROCS(1)
	RestoreDefault()

	got := Current()
	if got < 1 {
		t.Fatalf("after RestoreDefault(), Current() = %d, want >= 1", got)
	}
	if runtime.NumCPU() > 1 && got == 1 {
		t.Fatalf("after RestoreDefault(), Current() = 1 on a %d-CPU host; default not re-enabled", runtime.NumCPU())
	}
}

func ExamplePin() {
	restore := Pin(3)
	fmt.Println(Current())
	restore()
	// Output: 3
}
```

## Review

The core correctness fact is the query/mutate split, and the tests encode it
directly: `Current` must pass `0` (verified by calling it twice and getting the
same value), while `Pin` deliberately passes a positive value and captures the
previous one for its restore closure. If `Current` were implemented with a positive
argument, `TestCurrentDoesNotMutate` would still pass its equality check on the
first two reads but would have silently disabled auto-updating — which is why the
prose, not just the test, is load-bearing here.

The subtle mistake is treating `Pin`'s `restore()` as a full undo. It restores the
number, but auto-updating stays off because `runtime.GOMAXPROCS(prev)` is itself a
positive-argument write; only `RestoreDefault` (via `runtime.SetDefaultGOMAXPROCS`)
brings the container-aware behavior back. `TestRestoreDefault` proves that on a
multi-core machine the value is no longer pinned to 1 after the call. Never mark
these tests `t.Parallel`: they mutate global state, and the `t.Cleanup` baseline
restore is what keeps them from interfering. Run `go test -race` to confirm.

## Resources

- [`runtime.SetDefaultGOMAXPROCS`](https://pkg.go.dev/runtime#SetDefaultGOMAXPROCS) — added go1.25.0; re-enables the auto-updating default and forces an immediate refresh.
- [`runtime.GOMAXPROCS`](https://pkg.go.dev/runtime#GOMAXPROCS) — the query (`n < 1`) vs mutate (`n >= 1`) semantics and the auto-update disabling.
- [Go 1.25 release notes](https://go.dev/doc/go1.25) — how the environment variable and a `GOMAXPROCS` call disable container-awareness and periodic updates.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-cgroup-cpu-limit-parser.md](01-cgroup-cpu-limit-parser.md) | Next: [03-gomaxprocs-tracking-worker-pool.md](03-gomaxprocs-tracking-worker-pool.md)
