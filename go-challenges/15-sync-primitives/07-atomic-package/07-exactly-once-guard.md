# Exercise 7: Exactly-Once Guard without a Mutex

Sometimes a side effect must happen exactly once no matter how many goroutines race
to trigger it: fire a shutdown hook, log a "degraded mode" banner one time, close a
channel once. `sync.Once` is the standard answer, but there is a leaner lock-free
primitive when you only need the boolean and want the *caller* to know if it won:
`atomic.Bool.CompareAndSwap(false, true)`. This exercise builds that guard and
contrasts it with `sync.Once`.

This module is fully self-contained.

## What you'll build

```text
doonce/                    independent module: example.com/doonce
  go.mod
  guard.go                 type Guard; Do (CAS), Done
  cmd/
    demo/
      main.go              two calls, one wins
  guard_test.go            exactly-once concurrent test, winner/not-winner test, Example
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: a `Guard` over `atomic.Bool`; `Do(f)` runs `f` and returns `true` for the one winning caller, `false` for all others; `Done` reports whether the action has run.
- Test: many goroutines race to `Do` an action that increments a counter; assert it ran exactly once and later calls return `false`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/07-atomic-package/07-exactly-once-guard/cmd/demo
cd go-solutions/15-sync-primitives/07-atomic-package/07-exactly-once-guard
```

### CompareAndSwap as an exactly-once gate, and how it differs from sync.Once

`CompareAndSwap(false, true)` is a race that has exactly one winner. Every goroutine
attempts the transition; the hardware guarantees precisely one observes `false` and
flips it to `true`, and that one gets `true` back. Everyone else finds the flag
already `true`, their CAS fails, and they get `false`. So a guard that runs its
action only when the CAS returns `true` runs it exactly once, and — unlike
`sync.Once` — it tells the caller *whether this call was the one that ran it*. That
return value is the whole reason to reach for the atomic instead of `sync.Once`:
"close the channel and I need to know I'm the closer", "start the shutdown timer and
return true so the caller logs it".

The critical difference from `sync.Once` in semantics: `sync.Once.Do(f)` *blocks*
every caller until `f` has completed, so after any `Do` returns, `f`'s effects are
guaranteed visible. This CAS guard does **not** block the losers — they return
`false` immediately, potentially *before* the winner's `f` has finished running. So
use `sync.Once` when callers must not proceed until the one-time initialization is
complete (lazy singleton construction, "everyone needs the initialized value"). Use
the CAS guard when the losers genuinely have nothing to wait for — they just need to
not double-fire the effect — and when you want the boolean "did I win". For a
one-shot side effect like logging a banner or closing a channel, the losers have no
reason to block, and the CAS guard is the smaller, clearer tool.

`Done` is a plain `Load`, so you can check "has the action fired?" wait-free without
attempting to run it.

Create `guard.go`:

```go
package doonce

import "sync/atomic"

// Guard runs a one-shot side effect exactly once across concurrent callers, and
// tells the caller whether it was the one that ran it. Unlike sync.Once, losing
// callers are not blocked until the action completes.
type Guard struct {
	fired atomic.Bool
}

// Do runs f and returns true if this call was the one that ran it. Every other
// concurrent or later call returns false without running f. Losers do not wait
// for f to finish.
func (g *Guard) Do(f func()) bool {
	if g.fired.CompareAndSwap(false, true) {
		f()
		return true
	}
	return false
}

// Done reports whether the guarded action has been triggered.
func (g *Guard) Done() bool {
	return g.fired.Load()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/doonce"
)

func main() {
	var g doonce.Guard

	first := g.Do(func() { fmt.Println("running shutdown hook") })
	second := g.Do(func() { fmt.Println("this must not print") })

	fmt.Println("first won:", first)
	fmt.Println("second won:", second)
	fmt.Println("done:", g.Done())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
running shutdown hook
first won: true
second won: false
done: true
```

### Tests

`TestExactlyOnce` has many goroutines race to `Do` an action that increments a plain
`int` counter (guarded by the fact that only one goroutine ever runs it); the
counter must end at exactly 1, and exactly one `Do` must have returned `true`.
`TestLoserGetsFalse` pins that a call after the winner returns `false`.

Create `guard_test.go`:

```go
package doonce

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExactlyOnce(t *testing.T) {
	t.Parallel()

	var g Guard
	var runs int // only the single winner mutates this, so no lock is needed
	var winners atomic.Int64

	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if g.Do(func() { runs++ }) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if runs != 1 {
		t.Fatalf("action ran %d times, want exactly 1", runs)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("winners = %d, want exactly 1", got)
	}
	if !g.Done() {
		t.Fatal("Done() = false after the action ran")
	}
}

func TestLoserGetsFalse(t *testing.T) {
	t.Parallel()

	var g Guard
	if !g.Do(func() {}) {
		t.Fatal("first Do should win (true)")
	}
	if g.Do(func() { t.Fatal("action ran a second time") }) {
		t.Fatal("second Do should lose (false)")
	}
}

func ExampleGuard() {
	var g Guard
	fmt.Println(g.Do(func() {}))
	fmt.Println(g.Do(func() {}))
	// Output:
	// true
	// false
}
```

## Review

The guard is correct when the action runs exactly once and exactly one `Do` returns
`true` under any number of racing callers — `TestExactlyOnce` under `-race` proves
both, and the fact that only the winner touches `runs` is why the plain `int` is
safe. The judgment call is `sync.Once` versus this guard: choose `sync.Once` when
losers must block until the one-time work is visible; choose the CAS guard when
losers have nothing to wait for and you want the "did I win" boolean. Do not use
this guard for lazy-initialize-and-return-the-value, where a loser could read the
value before the winner finished writing it.

## Resources

- [`atomic.Bool.CompareAndSwap`](https://pkg.go.dev/sync/atomic#Bool.CompareAndSwap) — the one-winner primitive.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the blocking alternative, for comparison.
- [The Go Memory Model](https://go.dev/ref/mem) — the ordering guarantees behind both.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-admission-inflight-limiter.md](06-admission-inflight-limiter.md) | Next: [08-snapshot-publisher-atomic-pointer.md](08-snapshot-publisher-atomic-pointer.md)
