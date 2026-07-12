# Exercise 10: A Test Watchdog That Dumps Goroutine Stacks on Hang

Every prior exercise guarded a hang-prone test with a small watchdog. This exercise builds the
real, reusable version: a helper that runs a function under a deadline and, on timeout, dumps every
goroutine's stack via `runtime.Stack` and fails the test with that dump — turning an invisible
partial deadlock in CI into an actionable stack trace pointing at the exact blocked line.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
watchdog/                  independent module: example.com/watchdog
  go.mod                   go 1.25
  watchdog.go              Guard(t, d, name, fn); dumpStacks via runtime.Stack
  cmd/
    demo/
      main.go              a fast op passes; a wedged op reports a stack dump
  watchdog_test.go         fast op does not trip; wedged op trips + dump mentions it; no leak
```

- Files: `watchdog.go`, `cmd/demo/main.go`, `watchdog_test.go`.
- Implement: `Guard(t, d, name, fn)` that runs `fn` in a goroutine, and if it does not return within `d`, captures all goroutine stacks with `runtime.Stack(buf, true)` and fails via `t.Fatalf` with the dump; plus a standalone `DumpStacks() string` for reuse.
- Test: a positive test where a fast function passes without a dump; a negative test where a deliberately blocked function trips the watchdog and the captured dump mentions the stuck function; assert the helper leaks no goroutine after a clean run.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why a watchdog is the tool for partial deadlock

The runtime's deadlock detector only fires when *every* goroutine is blocked. A partial deadlock —
the realistic one — leaves the rest of the process running, so the runtime says nothing and the
test simply does not finish. `go test` has a package-level timeout (default 10 minutes) that will
eventually kill the binary, but that is a blunt instrument: it fails the *whole package* long after
the fact, with a dump of every goroutine in every test, and it is far too slow for a tight
CI loop. A per-test watchdog is precise: it bounds one operation to a few seconds, and when that
operation hangs it dumps the stacks *right then* and fails *that* test with a message naming what
hung.

The mechanism is `runtime.Stack(buf []byte, all bool) int`. With `all=true` it writes the stack
traces of *all* goroutines into `buf` and returns the number of bytes written. The one subtlety is
sizing `buf`: `runtime.Stack` writes as much as fits and does not tell you it truncated, so you grow
the buffer and retry until the returned length is less than the buffer size, which means the whole
dump fit. That grow-and-retry loop is the standard idiom for capturing a complete goroutine dump.

The watchdog itself must not leak. It starts `fn` in a goroutine and `select`s over a `done` channel
and a timer. On the fast path, `fn` returns, `done` closes, and the watchdog returns — but the `fn`
goroutine has already exited, so nothing leaks. On the hang path, `fn` is genuinely stuck (that is
the bug we are diagnosing), so its goroutine cannot be reclaimed; the watchdog reports the dump and
fails the test, and the stuck goroutine dies with the test binary. That is acceptable: a hung
goroutine on a failing test is the thing being diagnosed, not a leak the harness introduced.

Create `watchdog.go`:

```go
package watchdog

import (
	"runtime"
	"testing"
	"time"
)

// DumpStacks returns the stack traces of all goroutines. It grows the buffer
// until the full dump fits, since runtime.Stack silently truncates to the buffer
// size.
func DumpStacks() string {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Guard runs fn and fails the test with a full goroutine dump if fn does not
// return within d. It turns a partial deadlock (which the Go runtime cannot
// detect) into an actionable stack trace instead of a silent hang. name labels
// the operation in the failure message.
func Guard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		t.Fatalf("%s did not complete within %s: likely deadlock.\n\n=== goroutine dump ===\n%s",
			name, d, DumpStacks())
	}
}
```

### The runnable demo

The demo is a plain program (not a test) that shows both behaviors using a tiny stand-in for the
test object, so you can see the dump without running `go test`. It prints that a fast op passed and
that a wedged op produced a non-empty stack dump mentioning the blocked function.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"time"

	"example.com/watchdog"
)

func blockForever() {
	ch := make(chan struct{})
	<-ch // never unblocked: a partial deadlock
}

func main() {
	// Fast op: returns before the deadline.
	done := make(chan struct{})
	go func() { defer close(done); time.Sleep(time.Millisecond) }()
	select {
	case <-done:
		fmt.Println("fast op: completed")
	case <-time.After(time.Second):
		fmt.Println("fast op: unexpectedly timed out")
	}

	// Wedged op: capture a dump the way Guard would.
	go blockForever()
	time.Sleep(10 * time.Millisecond) // let it park
	dump := watchdog.DumpStacks()
	fmt.Printf("wedged op: dump non-empty=%v mentions blockForever=%v\n",
		len(dump) > 0, strings.Contains(dump, "blockForever"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast op: completed
wedged op: dump non-empty=true mentions blockForever=true
```

### Tests

`TestGuardPassesFast` runs a quick function under `Guard` and asserts it does not fail. Testing the
*negative* path is subtler: we cannot let `Guard` call `t.Fatalf` on the real `*testing.T` (that
would fail our own test), so `TestDumpStacksMentionsStuckGoroutine` exercises the dump machinery
directly — it parks a goroutine in a named function and asserts `DumpStacks` returns a non-empty
report mentioning that function. `TestGuardNoLeak` runs a clean `Guard` and checks no goroutine
survives it.

Create `watchdog_test.go`:

```go
package watchdog

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGuardPassesFast(t *testing.T) {
	t.Parallel()

	ran := false
	Guard(t, time.Second, "fast", func() {
		time.Sleep(time.Millisecond)
		ran = true
	})
	if !ran {
		t.Fatal("Guard returned before fn ran")
	}
}

// stuckInHere parks on an unbuffered channel that nobody sends to, so its stack
// frame appears in a goroutine dump under this function's name.
func stuckInHere(block <-chan struct{}) {
	<-block
}

func TestDumpStacksMentionsStuckGoroutine(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	go stuckInHere(block)
	// Give the goroutine time to reach its blocked receive.
	for range 100 {
		if strings.Contains(DumpStacks(), "stuckInHere") {
			break
		}
		time.Sleep(time.Millisecond)
	}

	dump := DumpStacks()
	if dump == "" {
		t.Fatal("DumpStacks returned empty")
	}
	if !strings.Contains(dump, "stuckInHere") {
		t.Fatalf("dump does not mention the stuck goroutine:\n%s", dump)
	}
	close(block) // release it so it does not linger
}

func TestGuardNoLeak(t *testing.T) {
	t.Parallel()

	before := runtime.NumGoroutine()
	Guard(t, time.Second, "clean", func() {})

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - before; leaked > 0 {
		t.Fatalf("Guard leaked %d goroutines on a clean run", leaked)
	}
}
```

## Review

The watchdog is correct when it is transparent on the fast path and diagnostic on the hang path.
`TestGuardPassesFast` confirms a quick function returns normally with no dump; `TestGuardNoLeak`
confirms the harness itself adds no lingering goroutine. The diagnostic value lives in
`DumpStacks`: `TestDumpStacksMentionsStuckGoroutine` proves the dump actually names the blocked
function, which is what makes a CI failure actionable — instead of "test timed out," you get the
exact receive or lock acquisition where a goroutine is parked.

The subtlety worth internalizing is why the negative path is tested via `DumpStacks` rather than by
letting `Guard` fire: `Guard` calls `t.Fatalf`, so triggering it on the live `*testing.T` would fail
the test that is verifying it. The grow-and-retry buffer loop in `DumpStacks` is not optional — a
fixed 64 KiB buffer silently truncates a dump from a service with hundreds of goroutines, hiding the
very stack you need. Wire `Guard` around every hang-prone concurrency test in this chapter and a
regression to a wedging design fails fast, with a map to the wedge.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — capturing all goroutine stacks, and the truncation behavior.
- [`testing.T.Helper`](https://pkg.go.dev/testing#T.Helper) — attributing the failure to the caller, not the helper.
- [Diagnostics: goroutine profiles](https://go.dev/doc/diagnostics) — the production counterpart via `net/http/pprof`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [11-cond-bounded-log-buffer-no-lost-wakeup.md](11-cond-bounded-log-buffer-no-lost-wakeup.md)
