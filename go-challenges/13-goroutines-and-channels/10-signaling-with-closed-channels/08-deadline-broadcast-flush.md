# Exercise 8: Deadline Broadcast: Closing A Channel To Fire A Batch Window

Micro-batching writes to a database or a queue needs two triggers: flush when the
batch reaches a size threshold, and flush when a max-latency window elapses so a
half-full batch does not sit forever. The window is implemented by closing a
channel from a `time.AfterFunc` callback — a timer that broadcasts "flush now".
The subtle correctness point: when a size flush wins, you must `Stop()` the timer
so its deadline does not fire a spurious second flush.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
batcher/                     independent module: example.com/batcher
  go.mod                     go mod init example.com/batcher
  batcher.go                 type Batch; CollectOne(in, threshold, window) Batch
  cmd/
    demo/
      main.go                runnable demo: size-triggered and deadline-triggered
  batcher_test.go            size flush stops timer; deadline flush fires window
```

Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
Implement: `CollectOne` accumulates items from `in` until it reaches `threshold` (size flush) or `window` elapses; the window is a channel closed by `time.AfterFunc`. On a size flush it calls the timer's `Stop()` and honors the return value, draining the deadline close if `Stop` reports the timer already fired.
Test: fill to threshold before the window and the flush is size-triggered with the timer stopped (no later spurious flush); stay under threshold and the deadline close triggers the flush after the window. Use short durations.
Verify: `go test -count=1 -race ./...`

### The timer-driven broadcast and its Stop() dance

`time.AfterFunc(window, f)` schedules `f` to run in its own goroutine after
`window`. Making `f` close a channel turns the timer into a broadcast deadline:
the collecting loop selects on that channel, and when the window elapses the close
wakes it. The loop:

```go
deadline := make(chan struct{})
timer := time.AfterFunc(window, func() { close(deadline) })
for {
	select {
	case v := <-in:
		items = append(items, v)
		if len(items) >= threshold {
			if !timer.Stop() {
				<-deadline // timer already fired: drain its close
			}
			return Batch{Items: items, Reason: "size"}
		}
	case <-deadline:
		return Batch{Items: items, Reason: "deadline"}
	}
}
```

The `if !timer.Stop() { <-deadline }` is the load-bearing detail.
`(*time.Timer).Stop` returns `true` if it stopped the timer before it fired —
in which case `f` never runs, `deadline` is never closed, and there is no goroutine
to clean up. It returns `false` if the timer had already fired (or been stopped),
meaning `f` has started or is about to close `deadline`; the `<-deadline` then
synchronizes with that close so a later reader of `deadline` (and this goroutine's
own cleanup reasoning) is not surprised by a pending close. Skip the `Stop()` and a
size flush leaves the timer armed; the window later fires and closes `deadline`,
which — if you reused the collector — would produce a spurious second flush. Honor
the return value and the deadline path is cleanly cancelled when size wins.

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/08-deadline-broadcast-flush/cmd/demo
cd go-solutions/13-goroutines-and-channels/10-signaling-with-closed-channels/08-deadline-broadcast-flush
```

Create `batcher.go`:

```go
package batcher

import (
	"time"
)

// Batch is a flushed group of items plus the reason the flush fired.
type Batch struct {
	Items  []int
	Reason string // "size" or "deadline"
}

// CollectOne accumulates items from in and returns one Batch, flushing when it
// reaches threshold items (Reason "size") or when window elapses (Reason
// "deadline"). The window is a channel closed by a time.AfterFunc callback.
func CollectOne(in <-chan int, threshold int, window time.Duration) Batch {
	deadline := make(chan struct{})
	timer := time.AfterFunc(window, func() { close(deadline) })

	var items []int
	for {
		select {
		case v := <-in:
			items = append(items, v)
			if len(items) >= threshold {
				// Size flush wins: stop the timer so the deadline does not fire
				// a spurious second flush. Honor Stop's return: false means the
				// timer already fired, so drain its close.
				if !timer.Stop() {
					<-deadline
				}
				return Batch{Items: items, Reason: "size"}
			}
		case <-deadline:
			return Batch{Items: items, Reason: "deadline"}
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/batcher"
)

func main() {
	// Size-triggered: five items reach the threshold before the one-second window.
	in := make(chan int, 10)
	for i := 1; i <= 5; i++ {
		in <- i
	}
	b := batcher.CollectOne(in, 5, time.Second)
	fmt.Printf("batch1: reason=%s items=%v\n", b.Reason, b.Items)

	// Deadline-triggered: two items, threshold ten never reached, window fires.
	in2 := make(chan int, 10)
	in2 <- 7
	in2 <- 8
	b2 := batcher.CollectOne(in2, 10, 30*time.Millisecond)
	fmt.Printf("batch2: reason=%s items=%v\n", b2.Reason, b2.Items)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch1: reason=size items=[1 2 3 4 5]
batch2: reason=deadline items=[7 8]
```

### Tests

Create `batcher_test.go`:

```go
package batcher

import (
	"fmt"
	"testing"
	"time"
)

func TestSizeFlushBeforeWindow(t *testing.T) {
	t.Parallel()

	in := make(chan int, 4)
	in <- 1
	in <- 2
	in <- 3
	// threshold 3 is reached; the one-second window must not fire.
	b := CollectOne(in, 3, time.Second)

	if b.Reason != "size" {
		t.Fatalf("reason = %q, want \"size\"", b.Reason)
	}
	if len(b.Items) != 3 {
		t.Fatalf("items = %v, want 3 items", b.Items)
	}
}

func TestDeadlineFlushUnderThreshold(t *testing.T) {
	t.Parallel()

	in := make(chan int, 4)
	in <- 7
	in <- 8
	// threshold 10 is never reached; the window fires and flushes the partial batch.
	start := time.Now()
	b := CollectOne(in, 10, 30*time.Millisecond)

	if b.Reason != "deadline" {
		t.Fatalf("reason = %q, want \"deadline\"", b.Reason)
	}
	if len(b.Items) != 2 {
		t.Fatalf("items = %v, want 2 items", b.Items)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("flushed after %v, want >= window (~30ms)", elapsed)
	}
}

func TestEmptyDeadlineFlush(t *testing.T) {
	t.Parallel()

	in := make(chan int) // nothing ever sent
	b := CollectOne(in, 5, 20*time.Millisecond)

	if b.Reason != "deadline" {
		t.Fatalf("reason = %q, want \"deadline\"", b.Reason)
	}
	if len(b.Items) != 0 {
		t.Fatalf("items = %v, want empty", b.Items)
	}
}

func ExampleCollectOne() {
	in := make(chan int, 3)
	in <- 1
	in <- 2
	in <- 3
	b := CollectOne(in, 3, time.Second)
	fmt.Println(b.Reason, b.Items)
	// Output: size [1 2 3]
}
```

## Review

`CollectOne` is correct when the flush reason matches which trigger won:
`TestSizeFlushBeforeWindow` proves a full batch flushes by size before the window,
and `TestDeadlineFlushUnderThreshold` proves a partial batch flushes when the
window elapses (and only after it). The bug this design prevents is the spurious
second flush: drop the `timer.Stop()` and a size flush leaves the timer armed to
fire later. Honoring `Stop()`'s boolean — draining the deadline close when it
reports the timer already fired — is the correct idiom, and it is what keeps the
timer goroutine from leaking a pending close. Run `go test -race`.

## Resources

- [pkg.go.dev: time.AfterFunc](https://pkg.go.dev/time#AfterFunc) — schedules a function after a duration; returns a `*Timer`.
- [pkg.go.dev: time.Timer.Stop](https://pkg.go.dev/time#Timer.Stop) — the return value and the drain idiom.
- [pkg.go.dev: context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the context-based alternative whose `Done` is the same deadline broadcast.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-goroutine-leak-guard.md](09-goroutine-leak-guard.md)
