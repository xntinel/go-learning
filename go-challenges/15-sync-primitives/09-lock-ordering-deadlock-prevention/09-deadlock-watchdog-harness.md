# Exercise 9: A Deadlock Regression Test Harness with Goroutine Dumps

A deadlocked test does not fail — it hangs, because Go's runtime deadlock
detector never fires while the test runner's goroutines are alive. This
exercise builds `deadtest`, a small reusable harness for CI: run a function
with a time budget, and if it does not finish, fail the test with a full
all-goroutine stack dump so the blocked `Lock` call sites are readable
straight from the CI log.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
deadtest/                  independent module: example.com/deadtest
  go.mod
  deadtest.go              TB interface (Helper, Fatalf); RunWithTimeout;
                           AllStacks (runtime.Stack with a growing buffer)
  cmd/
    demo/
      main.go              runnable demo: a fast task passes; a deliberate
                           cross-order wedge is detected and its dump probed
  deadtest_test.go         harness self-tests via a recording TB; an ordered
                           and a deliberately unordered bank transfer as the
                           workloads under the watchdog
```

- Files: `deadtest.go`, `cmd/demo/main.go`, `deadtest_test.go`.
- Implement: `RunWithTimeout(tb, d, fn)` — run `fn` in a goroutine, `select` its completion against a `time.NewTimer`, and on expiry call `tb.Fatalf` with the output of `runtime.Stack(buf, true)`; `AllStacks` that doubles its buffer until the dump fits.
- Test: a fast function passes; a deterministically wedged cross-order lock pair is detected via a mock `TB` that records `Fatalf`, and the captured dump contains `sync.(*Mutex)` frames; the ordered transfer completes within budget under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/09-deadlock-watchdog-harness/cmd/demo
cd go-solutions/15-sync-primitives/09-lock-ordering-deadlock-prevention/09-deadlock-watchdog-harness
```

### Why tests need their own watchdog

The runtime's `fatal error: all goroutines are asleep - deadlock!` requires
*every* goroutine in the process to be blocked. Inside `go test`, that never
happens: the test framework's main goroutine is alive waiting on your test,
and background goroutines from the runtime and any parallel tests keep
running. So a two-goroutine lock cycle in the code under test produces
silence. `go test` does have a last-resort backstop — after `-timeout`
(default 10 minutes) the whole binary panics and prints all stacks — but as a
deadlock detector it is poor: it burns ten minutes of CI per occurrence, it
kills the entire binary so every other test in the package is lost, and its
budget is per-binary, not per-operation, so you cannot say "this transfer
storm must finish in two seconds".

`RunWithTimeout` scopes the budget to one operation and turns the hang into a
first-class test failure. The mechanics are three small decisions:

1. `fn` runs in a *separate* goroutine signalling a `done` channel, while the
   caller's goroutine selects between `done` and a `time.NewTimer`. This
   orientation matters because of `Fatalf`: `(*testing.T).Fatalf` calls
   `runtime.Goexit` and is only allowed from the goroutine running the test
   function, so the harness must keep the *test's* goroutine as the waiter and
   push the workload out to the disposable goroutine, not the other way round.
2. On timeout, the failure message embeds `runtime.Stack(buf, true)` — the
   `true` means all goroutines, the same content as the production pprof
   endpoint `/debug/pprof/goroutine?debug=2`. This is the difference between
   "test timed out" and a diagnosis: the dump shows each wedged goroutine
   parked in `sync.(*Mutex).lockSlow` with the full call stack identifying
   *which* lock at *which* line, both halves of the cycle side by side.
   `runtime.Stack` truncates to the buffer you hand it, so `AllStacks` retries
   with a doubling buffer until the dump fits — a truncated dump that cuts off
   before the second goroutine of the cycle is worthless.
3. On timeout the workload goroutine is *abandoned* — there is no way to kill
   a goroutine, which is precisely why the wedge existed. That leak is
   acceptable in a failing test binary and must be documented, not hidden.

The `TB` interface (just `Helper` and `Fatalf`) exists so the harness itself
is testable: a real `*testing.T` satisfies it in normal use, and the self-test
substitutes a recorder to observe the `Fatalf` a wedge produces — you cannot
assert "this test failed correctly" from inside the same failing test any
other way. This is the standard trick for testing test helpers.

The workload in the self-test earns a note: forcing a cross-order deadlock
*reliably* needs more than two goroutines and luck, because the racy window is
narrow. The unordered transfer takes a `gate func()` called between its two
lock acquisitions; the test injects a rendezvous barrier as the gate, which
forces both goroutines to hold their first lock before either attempts its
second. Deterministic-interleaving injection like this is how you pin a
timing bug in a test instead of reproducing it one run in fifty.

Create `deadtest.go`:

```go
// Package deadtest detects wedged (deadlocked or unboundedly slow)
// operations in tests. Go's runtime deadlock detector cannot fire while
// the test framework's goroutines are alive, so a deadlocked test hangs
// until the go test -timeout kill instead of failing. RunWithTimeout
// bounds one operation and, on expiry, fails the test with a full
// all-goroutine stack dump — the same content as the production
// /debug/pprof/goroutine?debug=2 endpoint — so the blocked Lock call
// sites are visible directly in the CI log.
package deadtest

import (
	"runtime"
	"time"
)

// TB is the subset of testing.TB the harness needs. *testing.T
// satisfies it; harness self-tests substitute a recorder.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// RunWithTimeout runs fn and waits at most d for it to finish. On
// timeout it calls tb.Fatalf with an all-goroutine dump. The workload
// goroutine is abandoned on timeout (goroutines cannot be killed); a
// failing binary exiting shortly after makes that acceptable.
//
// RunWithTimeout must be called from the test's own goroutine, because
// (*testing.T).Fatalf may only be called there.
func RunWithTimeout(tb TB, d time.Duration, fn func()) {
	tb.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		tb.Fatalf("deadtest: operation still running after %v; all-goroutine dump:\n\n%s", d, AllStacks())
	}
}

// AllStacks returns the stack traces of every goroutine in the process,
// doubling the buffer until runtime.Stack no longer truncates.
func AllStacks() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}
```

### The runnable demo

The demo plays both roles: a fast task sails through, then a deliberately
sequenced cross-order lock pair wedges and the watchdog fires. The recorder
prints whether the dump names blocked mutex frames rather than dumping
hundreds of lines — run it yourself and change `verbose` to `true` to read a
real two-sided deadlock dump.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"example.com/deadtest"
)

// recorder satisfies deadtest.TB outside a test binary.
type recorder struct {
	failed bool
	msg    string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

func main() {
	const verbose = false

	fast := &recorder{}
	deadtest.RunWithTimeout(fast, time.Second, func() {
		time.Sleep(10 * time.Millisecond) // a healthy workload
	})
	fmt.Println("fast task detected as wedged:", fast.failed)

	wedged := &recorder{}
	deadtest.RunWithTimeout(wedged, 200*time.Millisecond, func() {
		var a, b sync.Mutex
		aHeld := make(chan struct{})
		bHeld := make(chan struct{})
		go func() {
			a.Lock()
			close(aHeld)
			<-bHeld
			b.Lock() // wedge: partner holds b
		}()
		go func() {
			b.Lock()
			close(bHeld)
			<-aHeld
			a.Lock() // wedge: partner holds a
		}()
		select {} // wait forever for goroutines that cannot finish
	})
	fmt.Println("wedged task detected as wedged:", wedged.failed)
	fmt.Println("dump names blocked mutex frames:", strings.Contains(wedged.msg, "sync.(*Mutex)"))
	if verbose {
		fmt.Println(wedged.msg)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast task detected as wedged: false
wedged task detected as wedged: true
dump names blocked mutex frames: true
```

### Tests

The self-tests exercise the harness from both sides. `TestFastFunctionPasses`
uses a recorder and asserts no failure was recorded. `TestWedgeDetectedWithDump`
builds the deterministic cross-order deadlock out of `transferUnordered` — the
gate injection forces goroutine 1 to hold account A and goroutine 2 to hold
account B before either tries its second lock — and asserts that the recorder
captured a failure whose dump contains `sync.(*Mutex)` frames (the parked
`lockSlow` stacks of both halves of the cycle). Then the harness switches from
subject to tool: `TestOrderedTransferBounded` gates the *ordered* transfer
from this chapter under a real `*testing.T` with a generous budget, which is
how every deadlock-prone module in this lesson should wire its storm tests.

Create `deadtest_test.go`:

```go
package deadtest

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder satisfies TB and captures Fatalf instead of ending the test,
// so the harness's own failure path can be asserted.
type recorder struct {
	failed bool
	msg    string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

// account is a minimal per-mutex resource for the workloads.
type account struct {
	id      int
	mu      sync.Mutex
	balance int
}

// transferOrdered locks by ascending id: deadlock-free.
func transferOrdered(from, to *account, amount int) {
	first, second := from, to
	if from.id > to.id {
		first, second = to, from
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()
	from.balance -= amount
	to.balance += amount
}

// transferUnordered locks in caller-argument order — the chapter's bug.
// gate runs between the two acquisitions so a test can force the fatal
// interleaving deterministically instead of hoping for it.
func transferUnordered(from, to *account, amount int, gate func()) {
	from.mu.Lock()
	defer from.mu.Unlock()
	gate()
	to.mu.Lock()
	defer to.mu.Unlock()
	from.balance -= amount
	to.balance += amount
}

func TestFastFunctionPasses(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	ran := false
	RunWithTimeout(rec, 5*time.Second, func() { ran = true })
	if !ran {
		t.Fatal("workload did not run")
	}
	if rec.failed {
		t.Fatalf("healthy workload reported wedged: %s", rec.msg)
	}
}

func TestWedgeDetectedWithDump(t *testing.T) {
	t.Parallel()

	a := &account{id: 1, balance: 100}
	b := &account{id: 2, balance: 100}

	// Rendezvous gate: neither goroutine may try its second lock until
	// both hold their first. This makes the deadlock certain, not racy.
	var barrier sync.WaitGroup
	barrier.Add(2)
	gate := func() {
		barrier.Done()
		barrier.Wait()
	}

	rec := &recorder{}
	RunWithTimeout(rec, 300*time.Millisecond, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			transferUnordered(a, b, 1, gate) // locks a, then wants b
		}()
		go func() {
			defer wg.Done()
			transferUnordered(b, a, 1, gate) // locks b, then wants a
		}()
		wg.Wait() // unreachable: both workers are wedged
	})

	if !rec.failed {
		t.Fatal("cross-order deadlock was not detected")
	}
	if !strings.Contains(rec.msg, "deadtest: operation still running after") {
		t.Errorf("failure message missing harness preamble: %q", rec.msg)
	}
	if !strings.Contains(rec.msg, "sync.(*Mutex)") {
		t.Errorf("dump does not show blocked mutex frames; diagnosis impossible from CI log")
	}
	if !strings.Contains(rec.msg, "transferUnordered") {
		t.Errorf("dump does not name the wedged function")
	}
}

func TestOrderedTransferBounded(t *testing.T) {
	t.Parallel()

	// The harness as a gate: the ordered transfer must finish a
	// both-directions storm well inside the budget.
	a := &account{id: 1, balance: 1000}
	b := &account{id: 2, balance: 1000}

	RunWithTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 1000 {
				transferOrdered(a, b, 1)
			}
		}()
		go func() {
			defer wg.Done()
			for range 1000 {
				transferOrdered(b, a, 1)
			}
		}()
		wg.Wait()
	})

	a.mu.Lock()
	b.mu.Lock()
	total := a.balance + b.balance
	b.mu.Unlock()
	a.mu.Unlock()
	if total != 2000 {
		t.Fatalf("total = %d, want 2000", total)
	}
}

func TestAllStacksSeesOtherGoroutines(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		<-release // parked on a channel receive, visible in the dump
	}()
	<-started

	dump := AllStacks()
	close(release)

	if !strings.Contains(dump, "goroutine ") {
		t.Fatalf("dump has no goroutine headers:\n%s", dump)
	}
	if !strings.Contains(dump, "chan receive") {
		t.Errorf("dump does not show the parked goroutine's wait reason")
	}
}

func ExampleRunWithTimeout() {
	rec := &recorder{}
	RunWithTimeout(rec, time.Second, func() {
		// a healthy, fast operation
	})
	fmt.Println("wedged:", rec.failed)
	// Output: wedged: false
}
```

## Review

The harness is thirty lines, and every line placement matters. The workload
runs in the spawned goroutine and the test goroutine waits — inverted, the
`Fatalf` would fire from a non-test goroutine, which the testing package
forbids for `Fatalf`'s `runtime.Goexit` semantics. The dump must be captured
with `all=true` and an adequately grown buffer, because the value of the
failure is entirely in showing *both* halves of the cycle; a message that says
only "timed out" reproduces the problem the harness exists to solve. And the
leak on the timeout path is honest: the wedged goroutines cannot be stopped,
so the harness documents the abandonment instead of pretending to clean up.

Choose budgets the way you choose SLOs: a storm test that legitimately takes a
second gets thirty, not two — the harness exists to catch *unbounded* waits,
and a tight budget on a loaded CI machine converts it into flake. In
production the same diagnosis runs through pprof: `kill -QUIT` or
`/debug/pprof/goroutine?debug=2` produce the dump this harness embeds, and
reading them is the same skill — find the goroutines whose state is
`sync.Mutex.Lock`, read down each stack to see which lock they hold and which
they want, and the cycle names itself. Confirm with
`go test -count=1 -race ./...`.

## Resources

- [runtime package — Stack](https://pkg.go.dev/runtime#Stack) — the all-goroutines dump and its buffer-truncation contract.
- [net/http/pprof](https://pkg.go.dev/net/http/pprof) — the production goroutine endpoint (`?debug=2`) with the same content.
- [testing package — common.Fatalf](https://pkg.go.dev/testing#T.Fatalf) — why Fatalf must run on the test goroutine.
- [Go FAQ: goroutines and deadlock](https://go.dev/doc/faq#goroutines) — the limits of the runtime's built-in detector.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-two-phase-escrow-transfer.md](10-two-phase-escrow-transfer.md)
