# Exercise 2: Adopt go.uber.org/goleak as the CI Leak Gate

The homegrown `NumGoroutine` poll from Exercise 1 works, but it gives you a number,
never a name. `go.uber.org/goleak` is the production standard: it reports the
leaking goroutine's stack so a failing test names the culprit. This exercise wires
goleak into a realistic package — a metrics flusher with a background loop — and
shows both entry points, `VerifyTestMain` and deferred `VerifyNone`, plus how to
whitelist a legitimately process-lifetime goroutine so the gate stays low-noise.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `go.uber.org/goleak`, so the gate fetches that module.

## What you'll build

```text
goleakflush/                 independent module: example.com/goleakflush
  go.mod
  flush.go                   type Flusher; NewFlusher, Record, Close; a process-lifetime sink
  cmd/
    demo/
      main.go                runnable demo: record then Close a flusher
  flush_test.go              TestMain->VerifyTestMain, VerifyNone, leak-detection via Find
```

- Files: `flush.go`, `cmd/demo/main.go`, `flush_test.go`.
- Implement: a `Flusher` whose `Close` stops and joins its background loop, plus a package-level `sink` goroutine that intentionally runs for the whole process.
- Test: `TestMain` calls `goleak.VerifyTestMain(m, IgnoreTopFunction(sink))`; one test uses deferred `VerifyNone`; one deliberately leaks and asserts `goleak.Find` reports it, then cleans up.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak@v1.3.0
```

### Two entry points, and when each applies

`goleak.VerifyTestMain(m)` runs once, in the package's `TestMain`, after every test
has finished. Because it checks a single time at the very end, it is safe even when
tests run in parallel — there is no per-test snapshot to be confused by overlapping
goroutines. This is the default choice for a package.

`goleak.VerifyNone(t)`, usually `defer`-ed at the top of one test, checks that *that
test* leaked nothing. It is precise but **incompatible with `t.Parallel()`**: a
parallel test shares its window with siblings, so a per-test snapshot cannot tell
your goroutines from theirs. Reserve `VerifyNone` for a serial test where you want
the failure attributed to exactly that test.

### Whitelisting a process-lifetime goroutine

Not every long-lived goroutine is a leak. A metrics sink, a signal handler, a
connection pool's health-checker — these are *supposed* to run for the whole process
and never return. goleak already ignores the runtime's own permanent goroutines, but
it cannot know about yours. `IgnoreTopFunction("pkg.fn")` names the single function
at the top of the goroutine's stack and subtracts exactly that one. The discipline is
to name the one function with a documented reason, never to reach for a broad ignore
that would swallow the next real leak too.

Here the `sink` goroutine is that legitimate case: a package-level metrics drain
started once and kept alive on purpose. We whitelist `sinkLoop` by its
fully-qualified name. The `Flusher`, by contrast, is a bounded component that must
release its goroutine on `Close` — and if it does not, goleak names `flushLoop` in
the failure.

Create `flush.go`:

```go
package goleakflush

import (
	"sync"
	"time"
)

// sink is a package-level metrics drain that intentionally runs for the whole
// process lifetime. Its goroutine never returns by design, so tests whitelist
// sinkLoop by name rather than treating it as a leak.
var (
	sinkOnce sync.Once
	sinkCh   chan int
)

func startSink() {
	sinkOnce.Do(func() {
		sinkCh = make(chan int, 1024)
		go sinkLoop(sinkCh)
	})
}

func sinkLoop(ch <-chan int) {
	var total int
	for v := range ch {
		total += v // a real sink would export this; here it just drains
		_ = total
	}
}

// Flusher periodically flushes recorded samples in the background. Unlike the
// sink, it is a bounded component: Close must stop and join its loop.
type Flusher struct {
	interval time.Duration
	mu       sync.Mutex
	buf      []int
	stop     chan struct{}
	done     chan struct{}
}

// NewFlusher starts a flusher that flushes every interval.
func NewFlusher(interval time.Duration) *Flusher {
	f := &Flusher{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go f.flushLoop()
	return f
}

// Record buffers a sample for the next flush.
func (f *Flusher) Record(v int) {
	f.mu.Lock()
	f.buf = append(f.buf, v)
	f.mu.Unlock()
}

func (f *Flusher) flushLoop() {
	defer close(f.done)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stop:
			f.flush()
			return
		case <-ticker.C:
			f.flush()
		}
	}
}

func (f *Flusher) flush() {
	f.mu.Lock()
	f.buf = f.buf[:0]
	f.mu.Unlock()
}

// Close stops the flush loop and waits for it to finish. It is idempotent.
func (f *Flusher) Close() error {
	select {
	case <-f.stop:
		return nil // already closed
	default:
		close(f.stop)
	}
	<-f.done
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/goleakflush"
)

func main() {
	f := goleakflush.NewFlusher(10 * time.Millisecond)
	for i := range 5 {
		f.Record(i)
	}
	time.Sleep(25 * time.Millisecond) // let a couple of flushes run

	if err := f.Close(); err != nil {
		fmt.Println("close:", err)
		return
	}
	fmt.Println("flusher closed cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flusher closed cleanly
```

### The tests

`TestMain` installs the package-wide gate: `goleak.VerifyTestMain(m, ...)` fails the
binary if any goroutine is still running at the end — except `sinkLoop`, which we
whitelist. `TestFlusherClosesCleanly` uses deferred `VerifyNone` (and the same
ignore, because the sink is alive package-wide) to attribute leak-freedom to that one
serial test. `TestLeakyFlusherIsDetected` proves goleak actually catches a real leak:
it snapshots the current goroutines with `IgnoreCurrent`, starts a flusher, does not
close it, and asserts `goleak.Find` returns an error naming `flushLoop` — then closes
it so the package-level check at the end still passes.

Create `flush_test.go`:

```go
package goleakflush

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// sinkTopFunc is the fully-qualified top function of the process-lifetime sink
// goroutine. Whitelisting exactly this one name keeps the gate precise.
const sinkTopFunc = "example.com/goleakflush.sinkLoop"

func TestMain(m *testing.M) {
	startSink() // a legitimately process-lifetime goroutine
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction(sinkTopFunc))
}

func TestFlusherClosesCleanly(t *testing.T) {
	// VerifyNone is incompatible with t.Parallel, so this test stays serial.
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(sinkTopFunc))

	f := NewFlusher(5 * time.Millisecond)
	f.Record(1)
	f.Record(2)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction(sinkTopFunc))

	f := NewFlusher(5 * time.Millisecond)
	if err := f.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestLeakyFlusherIsDetected(t *testing.T) {
	// Snapshot the goroutines that already exist (including the sink) so Find
	// reports only the leak we are about to create.
	ignoreExisting := goleak.IgnoreCurrent()

	f := NewFlusher(5 * time.Millisecond) // deliberately not closed yet

	err := goleak.Find(ignoreExisting)
	if err == nil {
		t.Fatal("goleak.Find did not detect the leaked flush loop")
	}
	if !strings.Contains(err.Error(), "flushLoop") {
		t.Fatalf("leak report did not name flushLoop:\n%v", err)
	}

	// Clean up so the package-level VerifyTestMain stays green.
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

## Review

The package is leak-clean when every `Flusher` created in a test is `Close`d and the
only surviving goroutine is the whitelisted `sinkLoop`. `TestLeakyFlusherIsDetected`
is the proof that the gate has teeth: it constructs the real leak, confirms
`goleak.Find` names `flushLoop`, then closes the flusher so the final
`VerifyTestMain` still passes — a leak demonstrated, not merely asserted away.

The mistakes to avoid are the ones that make a leak gate useless. Do not pair
`VerifyNone` with `t.Parallel()`; they are documented incompatible, and the failure
would be attributed to the wrong test. Do not silence a real leak with a broad
ignore — whitelist the one specific top function, `sinkLoop`, with the reason
written next to it. And keep `Close` idempotent and joining: a `Close` that signals
but does not wait on `done` would let `flushLoop` outlive the test and trip the gate.
Run under `-race`, since the flusher's buffer is touched by both `Record` and the
loop.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, `VerifyNone`, `Find`, `IgnoreTopFunction`, `IgnoreCurrent`.
- [goleak README](https://github.com/uber-go/goleak) — the recommended `TestMain` wiring and whitelisting guidance.
- [`testing.M`](https://pkg.go.dev/testing#M) — the `TestMain` entry point goleak hooks into.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-leak-detection-service.md](01-leak-detection-service.md) | Next: [03-homegrown-leak-assert-helper.md](03-homegrown-leak-assert-helper.md)
