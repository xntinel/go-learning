# Exercise 7: Test An Unexported Backoff Inside internal Without Widening The API

Sometimes the logic worth testing hardest is unexported — a backoff computation, a
parser, a hashing step — and you want to drive it from an external test package
without permanently exporting it into the production build. The `export_test.go`
idiom does exactly that: a file compiled only during `go test` re-exports the
private symbol to the test, and it is stripped from the production binary. It
composes cleanly with `internal`: the whole package stays hidden, its logic stays
unexported, and you still test it directly.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
retrykit/                            module example.com/retrykit
  go.mod
  internal/retry/retry.go            unexported computeBackoff, jitter; exported Schedule, Next
  internal/retry/export_test.go      re-exports computeBackoff and jitter (test-only)
  internal/retry/retry_test.go       external package retry_test drives the re-exported symbols
  cmd/demo/main.go                   runnable demo printing a backoff schedule
```

- Files: `internal/retry/retry.go`, `internal/retry/export_test.go`, `internal/retry/retry_test.go`, `cmd/demo/main.go`.
- Implement: an unexported `computeBackoff` (exponential, capped, monotonic) and `jitter`, plus exported `Schedule`/`Next` that use them.
- Test: an `export_test.go` assigns `ComputeBackoff`/`Jitter` to the unexported functions; an external `package retry_test` asserts monotonic growth, cap enforcement, and jitter bounds.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/retrykit/internal/retry ~/go-exercises/retrykit/cmd/demo
cd ~/go-exercises/retrykit
go mod init example.com/retrykit
```

### Why export_test.go, and how it composes with internal

There are two ways to test private logic. A white-box test in the same package
(`package retry`) can reach the unexported functions directly — simple, and the
right default. But when you want the test to live in the *external* test package
(`package retry_test`) — to exercise the code exactly as a real importer would, or
to keep the test's own helpers out of the production namespace — that external
package cannot see unexported symbols. The idiom that bridges the gap is a file
named `export_test.go` in the same directory and package as the code. Because its
name ends in `_test.go`, it is compiled only under `go test` and never linked into
the production binary. Inside it you write `var ComputeBackoff = computeBackoff`,
creating a test-only exported alias. The external test imports the package and drives
`retry.ComputeBackoff`, while the production API still exposes only `Schedule` and
`Next`.

This is orthogonal to `internal` and stacks with it. The whole `retry` package lives
under `internal/retry`, so no downstream module can import it at all; its core logic
stays unexported so even legal importers only see the two high-level functions; and
`export_test.go` lets the tests drive that unexported core directly. Hidden from the
wrong modules, hidden at the symbol level, and still fully tested — the three
mechanisms coexist without tension.

The backoff itself is production-shaped: exponential growth from `base`, doubling per
attempt, clamped at `max`, with overflow guarded so a large attempt count returns the
cap rather than wrapping to a negative duration. `jitter` applies full jitter —
a uniform value in `[0, d]` — from an injected `*rand.Rand` so it is deterministic
under test.

Create `internal/retry/retry.go`:

```go
// Package retry computes exponential backoff with jitter. It is internal, so
// only example.com/retrykit may import it; its core logic is unexported.
package retry

import (
	"math/rand"
	"time"
)

// computeBackoff returns base doubled per attempt (attempt 0 = base), clamped
// at max. Overflow is guarded: once the value meets or exceeds max, or wraps
// non-positive, it returns max. Monotonic non-decreasing in attempt.
func computeBackoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for range attempt {
		d *= 2
		if d >= max || d <= 0 {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// jitter returns a uniform value in [0, d] drawn from r (full jitter).
func jitter(d time.Duration, r *rand.Rand) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(r.Int63n(int64(d) + 1))
}

// Schedule returns the deterministic (un-jittered) backoff for attempts 0..n-1.
func Schedule(n int, base, max time.Duration) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := range n {
		out = append(out, computeBackoff(i, base, max))
	}
	return out
}

// Next returns the jittered backoff for attempt using r as the jitter source.
func Next(attempt int, base, max time.Duration, r *rand.Rand) time.Duration {
	return jitter(computeBackoff(attempt, base, max), r)
}
```

Create `internal/retry/export_test.go`. This file exists only for tests; it re-exports
the unexported functions so an external test can reach them:

```go
package retry

// Test-only re-exports of the unexported backoff internals. Compiled under
// `go test` and absent from the production build.
var (
	ComputeBackoff = computeBackoff
	Jitter         = jitter
)
```

### The runnable demo

The demo prints the deterministic schedule and confirms a jittered value stays within
its cap. `cmd/demo` is under the module root, so importing `internal/retry` is legal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math/rand"
	"time"

	"example.com/retrykit/internal/retry"
)

func main() {
	sched := retry.Schedule(6, 100*time.Millisecond, 2*time.Second)
	fmt.Println("schedule:", sched)

	r := rand.New(rand.NewSource(42))
	j := retry.Next(3, 100*time.Millisecond, 2*time.Second, r)
	fmt.Println("jittered attempt 3 within cap:", j >= 0 && j <= 800*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
schedule: [100ms 200ms 400ms 800ms 1.6s 2s]
jittered attempt 3 within cap: true
```

### Tests

The test file is `package retry_test` — an external test in the same directory. It
can reach the unexported logic only through the aliases the `export_test.go` file
publishes. It asserts the three properties that matter: monotonic non-decreasing
growth, the cap holds at large attempts, and jitter always lands in `[0, d]`.

Create `internal/retry/retry_test.go`:

```go
package retry_test

import (
	"math/rand"
	"testing"
	"time"

	"example.com/retrykit/internal/retry"
)

func TestComputeBackoffMonotonicAndCapped(t *testing.T) {
	t.Parallel()

	base, max := 100*time.Millisecond, 2*time.Second
	prev := time.Duration(-1)
	hitCap := false
	for attempt := range 12 {
		d := retry.ComputeBackoff(attempt, base, max)
		if d < prev {
			t.Fatalf("attempt %d: backoff %s decreased below previous %s", attempt, d, prev)
		}
		if d > max {
			t.Fatalf("attempt %d: backoff %s exceeds cap %s", attempt, d, max)
		}
		if d == max {
			hitCap = true
		}
		prev = d
	}
	if !hitCap {
		t.Fatal("backoff never reached the cap across 12 attempts")
	}
	if got := retry.ComputeBackoff(0, base, max); got != base {
		t.Fatalf("attempt 0 backoff = %s, want base %s", got, base)
	}
}

func TestJitterWithinBounds(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewSource(1))
	for _, d := range []time.Duration{0, time.Millisecond, 250 * time.Millisecond, time.Second} {
		for range 1000 {
			j := retry.Jitter(d, r)
			if j < 0 || j > d {
				t.Fatalf("Jitter(%s) = %s, want within [0, %s]", d, j, d)
			}
		}
	}
}
```

## Review

The design is correct when production exposes only `Schedule` and `Next`, the real
computation stays in unexported `computeBackoff`/`jitter`, and the external test still
drives them through the `export_test.go` aliases. Confirm the mechanism by deleting
`export_test.go` mentally: the external test would fail to compile, because
`retry.ComputeBackoff` does not exist in the production package — which is precisely
why the alias is test-only.

The traps: `export_test.go` must be in the same package as the code (`package retry`,
not `retry_test`), or it cannot see the unexported symbols to re-export. Do not
"solve" the external-test problem by exporting `ComputeBackoff` in `retry.go` — that
widens the production API permanently for a test's convenience. And keep the overflow
guard: without the `d <= 0` check, a large attempt count doubles a `time.Duration`
past its range into a negative value, and the "monotonic and capped" test catches
exactly that regression.

## Resources

- [Go Blog: Package names](https://go.dev/blog/package-names) — internal vs external test packages.
- [`testing`](https://pkg.go.dev/testing) — how `_test.go` files (including `export_test.go`) are compiled only for tests.
- [`math/rand`](https://pkg.go.dev/math/rand) — `rand.New`, `rand.NewSource`, `Rand.Int63n` for deterministic jitter.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-scoping-internal-by-depth.md](06-scoping-internal-by-depth.md) | Next: [08-cmd-wiring-internal-graceful-shutdown.md](08-cmd-wiring-internal-graceful-shutdown.md)
