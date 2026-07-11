# Exercise 3: A Reusable assertNoLeak Test Helper with Stack Diagnostics

Exercise 1 counted goroutines inline in one test. Real codebases that have not
adopted goleak extract that logic into a shared helper — and the good ones make the
failure message name the leaking goroutine instead of just printing a number. This
exercise builds that helper: poll `runtime.NumGoroutine` back to a baseline with
backoff, and on failure dump the offending stacks via `runtime.Stack(buf, true)`.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It uses only the standard library.

## What you'll build

```text
leakassert/                  independent module: example.com/leakassert
  go.mod
  leakassert.go              type TB; func AssertNoLeak(t, within, fn)
  cmd/
    demo/
      main.go                runnable demo: helper on a clean fn and a leaky fn
  leakassert_test.go         table-driven pass/fail, Helper() check, stack-frame assert
```

- Files: `leakassert.go`, `cmd/demo/main.go`, `leakassert_test.go`.
- Implement: `AssertNoLeak(t TB, within time.Duration, fn func())` that runs `fn`, polls the goroutine count back to baseline with backoff, and on timeout reports the full stack dump.
- Test: table-driven — passes for a clean function, fails for a leaking one, the failure message contains the leaking frame, and `Helper()` is called.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakassert/cmd/demo
cd ~/go-exercises/leakassert
go mod init example.com/leakassert
```

### Why an interface instead of *testing.T

The helper takes a small `TB` interface — `Helper()` plus `Errorf` — rather than
`*testing.T`. Two reasons. First, it lets the helper be *tested*: a fake reporter
that records whether `Errorf` fired lets you assert the helper fails on a real leak
without failing the enclosing test. Second, `Helper()` marks the function as a test
helper so the failure is reported at the *caller's* line, not inside the helper —
the same reason `t.Helper()` exists. `*testing.T` and `*testing.B` both satisfy this
interface, so real call sites pass `t` directly.

### The poll-with-backoff, and the stack dump

The mechanism mirrors Exercise 1's `TestNoLeak` but generalizes it. Record a
baseline count *after* `runtime.GC()`, run `fn`, then loop: GC, compare
`NumGoroutine()` to the baseline, and return the moment it is back at or below it.
The backoff — a sleep that grows each iteration — keeps a fast function fast (it
returns on the first check) while giving a slow-to-exit goroutine time to settle
without busy-spinning the CPU.

The diagnostic payoff is on failure. Once the deadline passes and the count is still
elevated, `runtime.Stack(buf, true)` writes *every* goroutine's stack into a buffer;
the helper puts that text in the `Errorf` message. Now the failure names the leaking
function — `example.com/leakassert.leakyWorker` — instead of leaving you to guess
from a bare "3 != 2". That is the whole reason to prefer this over a raw count
assertion.

Two honest trade-offs versus goleak, worth stating in the code. The count is a
global signal, so this helper is **not** safe under `t.Parallel()`: a sibling test's
goroutines move the baseline. And it depends on GC timing, so `within` must be
generous enough to absorb a returning goroutine's descheduling latency. goleak
sidesteps both by snapshotting stacks and diffing identities; the homegrown helper
is what you reach for before adopting it.

Create `leakassert.go`:

```go
package leakassert

import (
	"runtime"
	"time"
)

// TB is the subset of testing.TB that AssertNoLeak needs. Both *testing.T and
// *testing.B satisfy it, and a fake implementation makes the helper testable.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertNoLeak runs fn and then polls the goroutine count back to the baseline
// captured before fn. If the count does not return within `within`, it calls
// Errorf with a full stack dump so the failure names the leaking goroutine.
//
// It relies on runtime.NumGoroutine, a global signal, so it must NOT be used
// under t.Parallel: a sibling test's goroutines would move the baseline.
func AssertNoLeak(t TB, within time.Duration, fn func()) {
	t.Helper()

	runtime.GC()
	baseline := runtime.NumGoroutine()

	fn()

	deadline := time.Now().Add(within)
	backoff := time.Millisecond
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Errorf("goroutine leak: baseline=%d current=%d after %s\nall stacks:\n%s",
				baseline, runtime.NumGoroutine(), within, buf[:n])
			return
		}
		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}
```

### The runnable demo

The demo supplies a tiny reporter that records failure without printing the (huge,
nondeterministic) stack, so the output is stable. It runs the helper on a clean
function and on a leaky one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/leakassert"
)

// reporter is a minimal leakassert.TB that records whether a leak was reported.
type reporter struct{ leaked bool }

func (r *reporter) Helper() {}

func (r *reporter) Errorf(string, ...any) { r.leaked = true }

func main() {
	// A clean function: the spawned goroutine finishes before fn returns.
	clean := &reporter{}
	leakassert.AssertNoLeak(clean, time.Second, func() {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	})
	fmt.Println("clean fn leaked:", clean.leaked)

	// A leaky function: the spawned goroutine blocks forever.
	block := make(chan struct{})
	leaky := &reporter{}
	leakassert.AssertNoLeak(leaky, 50*time.Millisecond, func() {
		go func() { <-block }()
	})
	fmt.Println("leaky fn leaked:", leaky.leaked)
	close(block) // release the parked goroutine before exit
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean fn leaked: false
leaky fn leaked: true
```

### The tests

The table drives both outcomes through the same helper with a fake `recorder`. The
clean case's `fn` spawns a goroutine that exits before `fn` returns, so the helper
passes. The leaky case spawns a goroutine parked on `block`, which the test's
`Cleanup` closes *after* the assertion — so the helper sees the leak during its poll
window, reports it, and the goroutine is then released so the package leaves nothing
behind. The leak case also asserts the message names `leakyWorker`, proving the stack
dump is useful, and every case asserts `Helper()` was called.

Create `leakassert_test.go`:

```go
package leakassert

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// recorder is a fake TB that captures the helper's calls.
type recorder struct {
	helperCalls int
	failed      bool
	msg         string
}

func (r *recorder) Helper() { r.helperCalls++ }
func (r *recorder) Errorf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

// leakyWorker parks until block is closed. Being a named function, its frame
// appears in the stack dump so the leak message can name it.
func leakyWorker(block <-chan struct{}) { <-block }

func TestAssertNoLeak(t *testing.T) {
	// Not parallel: the helper reads a global goroutine count.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	tests := []struct {
		name      string
		within    time.Duration
		fn        func()
		wantLeak  bool
		wantFrame string
	}{
		{
			name:   "clean function",
			within: 2 * time.Second,
			fn: func() {
				done := make(chan struct{})
				go func() { close(done) }()
				<-done
			},
			wantLeak: false,
		},
		{
			name:      "leaking function",
			within:    100 * time.Millisecond,
			fn:        func() { go leakyWorker(block) },
			wantLeak:  true,
			wantFrame: "leakyWorker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			AssertNoLeak(rec, tt.within, tt.fn)

			if rec.failed != tt.wantLeak {
				t.Fatalf("failed = %v, want %v (msg=%q)", rec.failed, tt.wantLeak, rec.msg)
			}
			if rec.helperCalls == 0 {
				t.Error("Helper() was not called")
			}
			if tt.wantLeak && !strings.Contains(rec.msg, tt.wantFrame) {
				t.Errorf("leak message did not name %q:\n%s", tt.wantFrame, rec.msg)
			}
		})
	}
}

func ExampleAssertNoLeak() {
	r := &recorder{}
	AssertNoLeak(r, time.Second, func() {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	})
	fmt.Println("leaked:", r.failed)
	// Output: leaked: false
}
```

## Review

The helper is correct when it returns silently exactly when `fn` leaves no goroutine
behind, and reports a stack-bearing failure exactly when it does. The table proves
both directions through one code path, and the `leakyWorker` frame assertion proves
the diagnostic is more than a number. The `Cleanup` that closes `block` after the
assertion is the key to keeping the test honest *and* clean: the leak is real during
the poll, then released.

The mistakes to avoid are the ones the doc comment calls out. Do not run this helper
under `t.Parallel()` — the global count makes it flaky when siblings overlap. Do not
set `within` too tight; a returning goroutine can linger in the count for a scheduler
tick, which is why the poll and the GC exist. And prefer goleak in a codebase that
can take the dependency: this helper is the honest homegrown fallback, not a
replacement for identity-level detection. Run under `-race` to confirm the fake
reporter and the helper share no unsynchronized state.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — writes every goroutine's stack when `all` is true; the diagnostic core of the helper.
- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the count the helper polls.
- [`testing.TB`](https://pkg.go.dev/testing#TB) — the interface `*testing.T`/`*testing.B` implement, which the helper's `TB` mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-goleak-testmain-adoption.md](02-goleak-testmain-adoption.md) | Next: [04-fanout-timeout-send-leak.md](04-fanout-timeout-send-leak.md)
