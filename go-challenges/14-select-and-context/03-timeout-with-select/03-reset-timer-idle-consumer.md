# Exercise 3: Idle timeout on a long-lived consumer with one reusable timer

A queue consumer that must react when no message arrives within an idle window is
the textbook place to reuse a single timer. Allocating `time.After` per iteration
would churn a timer on every message; instead you create one `time.NewTimer`
outside the loop and `Reset` it on each received message. This exercise builds that
consumer and measures the allocation difference.

## What you'll build

```text
idlecon/                      module example.com/idle
  go.mod
  idle.go                     Consume(in, idle, onIdle) with a single reused timer
  cmd/demo/main.go            feed two messages, then go silent and fire idle
  idle_test.go                idle-fires-in-band, clean-close, no-early-fire
  alloc_test.go               //go:build !race — Reset allocates 0, time.After allocates
```

Files: `idle.go`, `cmd/demo/main.go`, `idle_test.go`, `alloc_test.go`.
Implement: `Consume(in <-chan string, idle time.Duration, onIdle func()) int` reusing one timer via the Stop-drain-Reset dance.
Test: messages under the window keep it running; silence fires `onIdle` once within a band; close returns cleanly without firing; `Reset` allocates zero.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/03-timeout-with-select/03-reset-timer-idle-consumer/cmd/demo
cd go-solutions/14-select-and-context/03-timeout-with-select/03-reset-timer-idle-consumer
```

### One timer, reset per message

The consumer loops on a `select` with two cases: a message arrived, or the idle
timer fired. On a message it processes it and re-arms the idle window by resetting
the timer. On an idle fire it invokes `onIdle` (log, flush, exit) and returns. The
timer is created once, before the loop, and stopped with `defer`.

The re-arming uses the portable Stop-drain-Reset form:

```go
if !timer.Stop() {
	select {
	case <-timer.C:
	default:
	}
}
timer.Reset(idle)
```

On a module that declares `go 1.23.0` or later — as this one does — the drain is
redundant: Go guarantees no stale value survives a `Stop` or `Reset`, so
`timer.Stop(); timer.Reset(idle)` would suffice, and even a bare `timer.Reset(idle)`
will not deliver a stale tick. The drain is written anyway because it is correct
under *both* the modern unbuffered-channel regime and the pre-1.23 buffered one,
so this exact code compiles and behaves correctly on an older toolchain too. The
non-blocking `default` is essential: `Stop` does not close `C`, and on a modern
build the value may already be gone, so a blocking `<-timer.C` could deadlock. The
`Stop`-returns-false path only drains when there genuinely is a buffered tick.

The allocation payoff is the reason to bother. `time.After(idle)` inside the loop
would allocate a fresh `*time.Timer` every iteration; on a busy consumer that is
one timer allocation per message, pure GC pressure. `Reset` on an existing timer
allocates nothing. The `alloc_test.go` file measures both with
`testing.AllocsPerRun` and pins the difference.

Create `idle.go`:

```go
package idle

import "time"

// Consume reads messages from in, re-arming an idle timer on each one. If no
// message arrives within idle, it calls onIdle once and returns. It also returns
// (without calling onIdle) when in is closed. The return value is the number of
// messages processed. One timer is created outside the loop and reused.
func Consume(in <-chan string, idle time.Duration, onIdle func()) int {
	timer := time.NewTimer(idle)
	defer timer.Stop()

	processed := 0
	for {
		select {
		case _, ok := <-in:
			if !ok {
				return processed
			}
			processed++
			// Re-arm the idle window. Portable Stop-drain-Reset: correct on
			// go1.23+ (where the drain is a no-op) and on older toolchains.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			onIdle()
			return processed
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

	"example.com/idle"
)

func main() {
	in := make(chan string)
	go func() {
		in <- "req-1"
		time.Sleep(30 * time.Millisecond) // under the 80ms idle window
		in <- "req-2"
		// then go silent
	}()

	n := idle.Consume(in, 80*time.Millisecond, func() {
		fmt.Println("idle: no message within window, flushing")
	})
	fmt.Printf("processed %d messages before idle\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
idle: no message within window, flushing
processed 2 messages before idle
```

### Tests

`TestConsumerIdleFires` feeds three messages spaced under the idle window (so the
timer keeps getting re-armed and never fires mid-stream), then stops; the idle
branch must fire exactly once, within a tolerance band measured from the last
message. `TestConsumerClosesCleanly` sends buffered messages and closes the
channel; the consumer processes them and returns without ever firing the idle
branch. The band lower bound proves the timer did not fire early; the upper bound
is generous for loaded CI.

Create `idle_test.go`:

```go
package idle

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumerIdleFires(t *testing.T) {
	t.Parallel()
	in := make(chan string)
	var idleCount atomic.Int32
	done := make(chan int, 1)
	go func() {
		done <- Consume(in, 50*time.Millisecond, func() { idleCount.Add(1) })
	}()

	for i := range 3 {
		in <- fmt.Sprintf("m%d", i)
		if i < 2 {
			time.Sleep(20 * time.Millisecond) // under the 50ms window
		}
	}
	start := time.Now()
	processed := <-done
	elapsed := time.Since(start)

	if processed != 3 {
		t.Fatalf("processed = %d, want 3", processed)
	}
	if got := idleCount.Load(); got != 1 {
		t.Fatalf("idle fired %d times, want 1", got)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("idle fired too early: %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("idle fired too late: %v", elapsed)
	}
}

func TestConsumerClosesCleanly(t *testing.T) {
	t.Parallel()
	in := make(chan string, 3)
	in <- "a"
	in <- "b"
	close(in)
	var idleCount atomic.Int32
	n := Consume(in, 50*time.Millisecond, func() { idleCount.Add(1) })
	if n != 2 {
		t.Fatalf("processed = %d, want 2", n)
	}
	if got := idleCount.Load(); got != 0 {
		t.Fatalf("idle fired %d times, want 0", got)
	}
}
```

The allocation assertion lives in its own file gated with `//go:build !race`,
because the race detector inflates allocation counts and would make an exact
"zero allocations" assertion unreliable. Under a normal build it proves the point;
under the gate's `-race` run it is excluded and the functional tests still run.

Create `alloc_test.go`:

```go
//go:build !race

package idle

import (
	"testing"
	"time"
)

func TestResetAllocatesZero(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	resetAllocs := testing.AllocsPerRun(100, func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Hour)
	})
	if resetAllocs != 0 {
		t.Fatalf("Reset dance allocated %v times per run, want 0", resetAllocs)
	}

	afterAllocs := testing.AllocsPerRun(100, func() {
		_ = time.After(time.Hour)
	})
	if afterAllocs < 1 {
		t.Fatalf("time.After allocated %v times per run, want >= 1", afterAllocs)
	}
}
```

## Review

The consumer is correct when the idle branch fires only after a genuine gap of the
full window and the timer is re-armed on every message with no stale tick leaking
through. The subtle failure this exercise trains you to avoid is the stale-tick
bug: on a pre-1.23 build, `Reset` without the drain would see the previous fired
tick still buffered in `C` and fire immediately, collapsing the idle window to
zero — which is why the portable drain is written even though this module's
`go.mod` makes it optional. The other trap is reaching for `time.After` inside the
loop; `alloc_test.go` shows exactly what that costs. Run `go test -race` for the
functional tests and a plain `go test` to exercise the allocation assertion.

## Resources

- [`time.Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — reset semantics and the Stop/drain caveats.
- [Go 1.23 release notes: timer changes](https://go.dev/doc/go1.23#timer-changes) — the unbuffered-channel and auto-drain guarantee.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations per call.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-timeout-demo-and-goroutine-leak.md](02-timeout-demo-and-goroutine-leak.md) | Next: [04-downstream-call-sla.md](04-downstream-call-sla.md)
