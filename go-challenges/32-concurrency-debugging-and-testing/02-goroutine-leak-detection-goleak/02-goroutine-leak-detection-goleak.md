# 2. Goroutine Leak Detection

A goroutine leak occurs when a goroutine is started and never terminates. Goroutines are cheap (a few KB each), so leaks go unnoticed in short tests but accumulate in long-running processes until the program exhausts memory or file descriptors. The hard part is not spotting a single leak in isolation — it is reliably detecting leaks across a test suite where goroutines from one test can outlive the test boundary and be attributed to the next. This lesson builds a stdlib-only goroutine leak checker so the gate runs offline, teaches the four common leak patterns, and shows how to write tests that self-destruct when a leak is introduced.

```text
leakcheck/
  go.mod
  leakcheck.go
  leakcheck_test.go
  patterns/
    patterns.go
    patterns_test.go
  cmd/demo/
    main.go
```

## Concepts

### Why Goroutines Leak

A goroutine runs until its function returns. Four patterns prevent that from happening:

**Blocked channel receive.** A goroutine waits on `<-ch` but no sender will ever write. The goroutine parks until the process exits.

**Blocked channel send.** A goroutine writes to an unbuffered channel but the receiver has already returned, or a buffered channel is full and nobody drains it.

**Unreachable select.** A goroutine is blocked in `select` waiting for a channel that is neither closed nor written to, and no default branch fires.

**Ticker not stopped.** `time.NewTicker` starts an internal goroutine. If `ticker.Stop()` is never called and the ticker's channel is no longer drained, the goroutine leaks.

### Detecting Leaks With runtime.NumGoroutine

`runtime.NumGoroutine()` returns the current count of live goroutines. A goroutine checker records the count before a test, runs the test, gives leaked goroutines a short grace period to exit, then compares. A significant increase (above the expected set of background goroutines) signals a leak.

This approach is coarser than a dedicated library: it gives you a count difference, not a stack trace of the leaked goroutine. To identify which goroutine leaked, pair it with `runtime.Stack(buf, true)`, which captures all goroutine stacks to a byte slice.

### Grace Period and Background Goroutines

The Go runtime itself runs several goroutines (finalizer, GC, signal handler). `runtime.NumGoroutine()` includes them. A checker must allow for a small constant count of background goroutines and a short grace period (typically 100–200 ms) for goroutines that exit slightly after the test function returns.

### Fixing Leaks

The canonical fix is to give every long-lived goroutine a shutdown signal via a `context.Context` or a `done` channel, and to ensure the caller waits for the goroutine to exit (via `sync.WaitGroup` or a channel) before the test ends. Use `t.Cleanup` to register the shutdown call so it fires even if the test fails.

```
Start goroutine -> register cleanup with t.Cleanup(cancel + wg.Wait) -> test body -> cleanup fires
```

## Exercises

### Exercise 1: Goroutine Leak Checker

Create `leakcheck.go`:

```go
package leakcheck

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Snapshot records the goroutine count at a point in time.
type Snapshot struct {
	count int
}

// Take records the current goroutine count.
func Take() Snapshot {
	return Snapshot{count: runtime.NumGoroutine()}
}

// CheckResult holds the result of a leak check.
type CheckResult struct {
	Before int
	After  int
	// Stacks contains the full goroutine dump when a leak is detected.
	Stacks string
}

// Leaked returns true when the goroutine count has grown by more than delta
// above the snapshot. delta accounts for expected background goroutines.
func (s Snapshot) Leaked(delta int) (CheckResult, bool) {
	return s.leakedAfter(delta, 200*time.Millisecond)
}

func (s Snapshot) leakedAfter(delta int, grace time.Duration) (CheckResult, bool) {
	deadline := time.Now().Add(grace)
	var after int
	for {
		after = runtime.NumGoroutine()
		if after <= s.count+delta {
			return CheckResult{Before: s.count, After: after}, false
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	stacks := string(buf[:n])

	return CheckResult{
		Before: s.count,
		After:  after,
		Stacks: filterStacks(stacks),
	}, true
}

// filterStacks removes the checker's own goroutine from the dump to reduce noise.
func filterStacks(dump string) string {
	var out bytes.Buffer
	for _, block := range strings.Split(dump, "\n\n") {
		if strings.Contains(block, "leakcheck.") {
			continue
		}
		out.WriteString(block)
		out.WriteString("\n\n")
	}
	return strings.TrimSpace(out.String())
}

// Format returns a human-readable summary of the result.
func (r CheckResult) Format() string {
	return fmt.Sprintf("goroutines before=%d after=%d\n%s", r.Before, r.After, r.Stacks)
}

// Count returns the goroutine count at snapshot time.
func (s Snapshot) Count() int { return s.count }
```

The checker is a value type; `Take()` snapshots the count before and `Leaked` polls with a 200 ms grace period, then returns a `CheckResult` with the filtered goroutine dump.

### Exercise 2: Test the Checker Itself

Create `leakcheck_test.go`:

```go
package leakcheck

import (
	"runtime"
	"testing"
	"time"
)

// TestLeakedReturnsFalseWhenNoLeak verifies that Leaked reports clean when
// no extra goroutines are started.
func TestLeakedReturnsFalseWhenNoLeak(t *testing.T) {
	t.Parallel()

	snap := Take()
	r, leaked := snap.Leaked(0)
	if leaked {
		t.Fatalf("expected no leak; got goroutines before=%d after=%d\n%s",
			r.Before, r.After, r.Stacks)
	}
}

// TestLeakedReturnsTrueWhenGoroutineLeaks starts a permanently-blocked
// goroutine and asserts that Leaked detects it and returns a non-empty Stacks.
func TestLeakedReturnsTrueWhenGoroutineLeaks(t *testing.T) {
	// Not parallel: manipulates goroutine count; easier to reason about serial.

	snap := Take()

	ch := make(chan struct{}) // never closed, never sent to
	go func() {
		<-ch
	}()

	r, leaked := snap.Leaked(0)
	if !leaked {
		t.Fatal("expected Leaked to detect the blocked goroutine, but it did not")
	}
	if r.Before >= r.After {
		t.Fatalf("expected After > Before; got before=%d after=%d", r.Before, r.After)
	}
	if r.Stacks == "" {
		t.Fatal("expected non-empty Stacks in the leak result")
	}
	// Clean up: unblock the goroutine so it exits before the next test.
	close(ch)
	// Give the runtime a moment to schedule the goroutine exit.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
}

// TestLeakedAfterZeroGrace verifies leakedAfter with zero grace still detects
// a permanently-blocked goroutine.
func TestLeakedAfterZeroGrace(t *testing.T) {
	// Not parallel: same reason as above.

	snap := Take()

	ch := make(chan struct{})
	go func() {
		<-ch
	}()

	r, leaked := snap.leakedAfter(0, 0)
	if !leaked {
		t.Fatal("expected leakedAfter with zero grace to detect the leak")
	}
	if r.Stacks == "" {
		t.Fatal("expected non-empty Stacks")
	}
	close(ch)
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
}
```

The test suite covers three cases: no-leak clean path, a permanently-blocked goroutine detected via `Leaked`, and `leakedAfter` with a zero grace period. The internal `leakedAfter` is accessible because the test is in `package leakcheck` (white-box).

### Exercise 3: Four Leak Patterns and Their Fixes

Create `patterns/patterns.go`:

```go
package patterns

import (
	"context"
	"sync"
	"time"
)

// LeakyChannelReceive starts a goroutine that blocks on a channel that
// nobody will ever close or send to. Call this and then cancel the returned
// context to see the leak.
func LeakyChannelReceive() {
	ch := make(chan int)
	go func() {
		<-ch // blocks forever: nobody sends
	}()
}

// SafeChannelReceive starts a goroutine that exits when ctx is cancelled.
func SafeChannelReceive(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
	}()
}

// LeakyTicker starts a ticker that is never stopped. The ticker's internal
// goroutine leaks.
func LeakyTicker() {
	ticker := time.NewTicker(time.Hour)
	go func() {
		for range ticker.C {
			// process tick
		}
	}()
	// BUG: ticker.Stop() is never called; the goroutine draining ticker.C
	// never exits because Stop() does not close the channel.
}

// SafeTicker starts a ticker that is stopped via ctx cancellation.
func SafeTicker(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(time.Hour)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// process tick
			case <-ctx.Done():
				return
			}
		}
	}()
}

// LeakyWorker starts a goroutine that processes from a channel but the
// channel is never closed, so the goroutine blocks forever when the
// channel is empty.
func LeakyWorker() chan<- int {
	ch := make(chan int)
	go func() {
		for v := range ch { // blocks when ch is empty and not closed
			_ = v
		}
	}()
	return ch
}

// SafeWorker starts a goroutine that exits when ctx is cancelled or ch
// is closed. The caller must close ch or cancel ctx.
func SafeWorker(ctx context.Context, wg *sync.WaitGroup) chan<- int {
	ch := make(chan int, 8)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
				// process value
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
```

Create `patterns/patterns_test.go`:

```go
package patterns

import (
	"context"
	"sync"
	"testing"

	"example.com/leakcheck"
)

// TestSafeChannelReceiveNoLeak verifies the safe variant leaves no goroutines behind.
func TestSafeChannelReceiveNoLeak(t *testing.T) {
	t.Parallel()

	snap := leakcheck.Take()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	SafeChannelReceive(ctx, &wg)

	cancel()
	wg.Wait()

	if r, leaked := snap.Leaked(0); leaked {
		t.Fatalf("goroutine leak detected:\n%s", r.Format())
	}
}

// TestLeakyChannelReceiveLeaks confirms the leaky variant leaks at least one goroutine.
// This test is intentionally expected to detect a leak; it passes when the
// checker detects one (demonstrating the detection works).
// Not parallel: goroutine count snapshots are easier to reason about in serial.
func TestLeakyChannelReceiveLeaks(t *testing.T) {
	snap := leakcheck.Take()
	LeakyChannelReceive() // starts a goroutine that blocks forever on <-ch

	// Leaked checks with a 200 ms grace period. During that period the count
	// stays above snap.Count() because the goroutine is permanently blocked.
	if _, leaked := snap.Leaked(0); !leaked {
		t.Fatal("expected a goroutine leak from LeakyChannelReceive, but checker saw none")
	}
	// The leaked goroutine will be cleaned up when the test binary exits.
	// In real code this would be a bug; here it demonstrates detection.
}

// TestSafeTickerNoLeak verifies the safe ticker variant leaves no goroutines behind.
func TestSafeTickerNoLeak(t *testing.T) {
	t.Parallel()

	snap := leakcheck.Take()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	SafeTicker(ctx, &wg)

	cancel()
	wg.Wait()

	if r, leaked := snap.Leaked(0); leaked {
		t.Fatalf("goroutine leak detected:\n%s", r.Format())
	}
}

// TestSafeWorkerNoLeak verifies the safe worker exits when the context is cancelled.
func TestSafeWorkerNoLeak(t *testing.T) {
	t.Parallel()

	snap := leakcheck.Take()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ch := SafeWorker(ctx, &wg)
	ch <- 1
	ch <- 2

	cancel()
	wg.Wait()

	if r, leaked := snap.Leaked(0); leaked {
		t.Fatalf("goroutine leak detected:\n%s", r.Format())
	}
}

// TestCleanupPattern shows the idiomatic t.Cleanup approach.
func TestCleanupPattern(t *testing.T) {
	t.Parallel()

	snap := leakcheck.Take()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	SafeChannelReceive(ctx, &wg)

	t.Cleanup(func() {
		cancel()
		wg.Wait()
		if r, leaked := snap.Leaked(0); leaked {
			t.Errorf("goroutine leak in TestCleanupPattern:\n%s", r.Format())
		}
	})
}
```

Your turn: add `TestSafeWorkerCloseChannel` that sends three values to the safe worker, closes the channel instead of cancelling the context, waits for the WaitGroup, and asserts no leak. This tests the `!ok` branch in `SafeWorker`.

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/leakcheck"
)

func startWorker(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
	}()
}

func main() {
	snap := leakcheck.Take()
	fmt.Printf("goroutines at start: %d\n", snap.Count())

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		startWorker(ctx, &wg)
	}

	snapAfterStart := leakcheck.Take()
	fmt.Printf("goroutines after starting 5 workers: %d\n", snapAfterStart.Count())

	cancel()
	wg.Wait()

	if r, leaked := snap.Leaked(0); leaked {
		fmt.Printf("LEAK detected: %s\n", r.Format())
	} else {
		fmt.Printf("no leak: goroutines before=%d after=%d\n", r.Before, r.After)
	}
}
```

## Common Mistakes

### Missing `defer cancel()` on Context

Wrong:

```go
ctx, cancel := context.WithCancel(context.Background())
go doWork(ctx)
// cancel is never called; doWork blocks on ctx.Done() forever
```

What happens: the goroutine started by `doWork` never exits, leaking as long as the process runs.

Fix:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
go doWork(ctx)
```

### `ticker.Stop()` Does Not Close the Channel

Wrong: assuming that calling `ticker.Stop()` causes `<-ticker.C` to return.

What happens: `Stop` prevents future ticks but does not drain or close the channel. A goroutine blocked on `for range ticker.C` stays blocked after `Stop`.

Fix: use a `select` with `ctx.Done()` alongside `ticker.C` and call `ticker.Stop()` in a `defer`:

```go
defer ticker.Stop()
for {
	select {
	case <-ticker.C:
		// handle tick
	case <-ctx.Done():
		return
	}
}
```

### Using `runtime.NumGoroutine` Without a Grace Period

Wrong: checking the count immediately after the test body without a grace period.

What happens: goroutines that exit slightly after the function returns (scheduling lag) cause false positives.

Fix: poll with a short grace period (100–200 ms) and report a leak only after the deadline passes, as shown in `leakcheck.go`.

## Verification

From `~/go-exercises/leakcheck`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

`TestLeakyChannelReceiveLeaks` is expected to pass (it asserts that the leak is detected). All safe variants must produce no `goroutine leak detected` output. The demo must print `no leak`.

## Summary

- Goroutines leak when they block forever on a channel, context, or ticker without a shutdown signal.
- `runtime.NumGoroutine()` gives a count; `runtime.Stack(buf, true)` gives the full dump to identify which goroutine leaked.
- A leak checker must allow a grace period before concluding that a goroutine has leaked.
- `t.Cleanup` is the right place to cancel contexts and wait for goroutines: it fires even when the test fails.
- Fix: every goroutine must have a shutdown path — a context cancellation, a channel close, or a done signal — and the caller must wait for it to exit.

## What's Next

Next: [Testing Concurrent Code](../03-testing-concurrent-code/03-testing-concurrent-code.md).

## Resources

- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine)
- [runtime.Stack](https://pkg.go.dev/runtime#Stack)
- [context package](https://pkg.go.dev/context)
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [time.Ticker](https://pkg.go.dev/time#Ticker)
