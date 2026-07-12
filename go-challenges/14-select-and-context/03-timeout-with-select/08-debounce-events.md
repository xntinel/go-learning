# Exercise 8: Debounce a burst of change events

"Reload the config once after the file stops changing" and "invalidate the cache
after the writes settle" are the same operation: coalesce a rapid burst of triggers
and act only once, after a quiet period. That is debouncing. This exercise builds
`Debounce`, which resets a `time.Timer` on every trigger and emits a single action
when the timer finally survives a full quiet window.

## What you'll build

```text
debouncer/                    module example.com/debounce
  go.mod
  debounce.go                 Debounce(triggers, quiet, emit)
  cmd/demo/main.go            a burst then a flush-on-close
  debounce_test.go            burst-one-emit, spaced-each-emit, timing, close-flush
```

Files: `debounce.go`, `cmd/demo/main.go`, `debounce_test.go`.
Implement: `Debounce(triggers <-chan struct{}, quiet time.Duration, emit func())`.
Test: a tight burst yields one emit; triggers spaced beyond the window yield one each; the emit lands ~quiet after the last trigger; a pending debounce flushes on close.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/03-timeout-with-select/08-debounce-events/cmd/demo
cd go-solutions/14-select-and-context/03-timeout-with-select/08-debounce-events
```

### Reset on every trigger, act on the survivor

`Debounce` keeps one timer and an `armed` flag. Each trigger resets the timer to a
fresh `quiet` window and marks the debounce armed. Only when the timer actually
fires — meaning a full `quiet` interval passed with no new trigger to reset it —
does `emit` run and `armed` clear. A tight burst of triggers keeps resetting the
timer before it can fire, so the many triggers collapse into a single emit that
lands one quiet window after the *last* trigger, not the first.

The timer is created and immediately stopped, so it does not fire before the first
trigger arms it. Each trigger re-arms with the portable Stop-drain-Reset form; on
this `go 1.23+` module the drain is a no-op, but it keeps the code correct on older
toolchains where a stale tick could otherwise fire the emit prematurely. After the
timer fires and emits, `armed` is false and the timer is left expired until the next
trigger resets it.

The close contract is a real design decision, and this implementation chooses to
*flush*: when `triggers` closes with a debounce still armed (a trigger arrived but
the quiet window had not yet elapsed), `Debounce` emits once before returning, so a
pending config reload is not silently dropped on shutdown. If instead the timer had
already fired and cleared `armed` before the close, there is nothing pending and it
returns without emitting. The alternative contract — drop the pending action on
close — is equally defensible; what matters is that the choice is explicit and the
timer is stopped cleanly either way so nothing leaks.

Create `debounce.go`:

```go
package debounce

import "time"

// Debounce coalesces a burst of triggers into a single emit that runs once no
// trigger has arrived for quiet. Each trigger resets the window. If triggers is
// closed while a debounce is pending (armed but not yet fired), emit runs once
// before returning; otherwise it returns without emitting.
func Debounce(triggers <-chan struct{}, quiet time.Duration, emit func()) {
	timer := time.NewTimer(quiet)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	armed := false
	for {
		select {
		case _, ok := <-triggers:
			if !ok {
				if armed {
					emit() // flush a pending debounce on close
				}
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
			armed = true
		case <-timer.C:
			emit()
			armed = false
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

	"example.com/debounce"
)

func main() {
	triggers := make(chan struct{})
	done := make(chan struct{})
	go func() {
		debounce.Debounce(triggers, 40*time.Millisecond, func() {
			fmt.Println("reload config")
		})
		close(done)
	}()

	// First burst: four writes within the window, then wait for the quiet emit.
	for range 4 {
		triggers <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond) // quiet elapses -> one reload

	// Second burst, then close immediately: the pending debounce flushes.
	for range 2 {
		triggers <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}
	close(triggers)
	<-done
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reload config
reload config
```

The first burst produces one reload after the quiet window. The second burst is
still pending (armed, timer not yet fired) when `triggers` closes, so the flush-on-
close contract produces the second reload.

### Tests

`TestBurstOneEmit` fires five triggers spaced under the window and asserts exactly
one emit after the quiet period, and still one after close. `TestSpacedEmitsEach`
spaces triggers beyond the window so each debounces independently into its own emit.
`TestEmitsAfterLast` measures that the emit lands roughly `quiet` after the *last*
trigger, not the first. `TestCloseFlushes` arms a debounce with a very long quiet
window (so the timer cannot fire) and closes, asserting the pending action flushes.

Create `debounce_test.go`:

```go
package debounce

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBurstOneEmit(t *testing.T) {
	t.Parallel()
	triggers := make(chan struct{})
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		Debounce(triggers, 50*time.Millisecond, func() { count.Add(1) })
		close(done)
	}()

	for range 5 {
		triggers <- struct{}{}
		time.Sleep(5 * time.Millisecond) // total 25ms < 50ms window
	}
	time.Sleep(120 * time.Millisecond) // quiet elapses; one emit
	if got := count.Load(); got != 1 {
		t.Fatalf("emits = %d, want 1", got)
	}
	close(triggers)
	<-done
	if got := count.Load(); got != 1 {
		t.Fatalf("emits = %d after close, want 1", got)
	}
}

func TestSpacedEmitsEach(t *testing.T) {
	t.Parallel()
	triggers := make(chan struct{})
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		Debounce(triggers, 30*time.Millisecond, func() { count.Add(1) })
		close(done)
	}()

	for range 3 {
		triggers <- struct{}{}
		time.Sleep(80 * time.Millisecond) // > quiet, so each fires
	}
	if got := count.Load(); got != 3 {
		t.Fatalf("emits = %d, want 3", got)
	}
	close(triggers)
	<-done
}

func TestEmitsAfterLast(t *testing.T) {
	t.Parallel()
	triggers := make(chan struct{})
	emitted := make(chan time.Time, 1)
	go Debounce(triggers, 60*time.Millisecond, func() { emitted <- time.Now() })

	triggers <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	triggers <- struct{}{} // resets the window
	last := time.Now()

	when := <-emitted
	gap := when.Sub(last)
	if gap < 50*time.Millisecond {
		t.Fatalf("emit too soon after last trigger: %v", gap)
	}
	if gap > 400*time.Millisecond {
		t.Fatalf("emit too late after last trigger: %v", gap)
	}
	close(triggers)
}

func TestCloseFlushes(t *testing.T) {
	t.Parallel()
	triggers := make(chan struct{})
	var count atomic.Int32
	done := make(chan struct{})
	go func() {
		Debounce(triggers, time.Hour, func() { count.Add(1) })
		close(done)
	}()

	triggers <- struct{}{} // arm; the hour-long timer will not fire
	close(triggers)        // close flushes the pending debounce
	<-done
	if got := count.Load(); got != 1 {
		t.Fatalf("emits = %d, want 1 (flush on close)", got)
	}
}
```

## Review

The debouncer is correct when a burst collapses to one emit landing a quiet window
after the last trigger, and when the close contract is honored exactly as
documented. The subtle bug is emitting after the *first* trigger instead of the
last — that happens if you start the timer once and never reset it; the reset on
every trigger is the whole mechanism. The second is the stale-tick trap: without the
Stop-drain-Reset guard on a pre-1.23 build, a buffered tick from a just-expired
timer could fire the emit immediately on the next trigger. Run `go test -race`; the
`emit` closure runs on the debounce goroutine, and the tests communicate results
through an atomic counter or a channel, so there is no race.

## Resources

- [`time.Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — resetting the quiet window on each trigger.
- [Go 1.23 release notes: timer changes](https://go.dev/doc/go1.23#timer-changes) — why the reset drain is now optional.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — the counter the tests use to observe emits race-free.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-heartbeat-liveness-monitor.md](07-heartbeat-liveness-monitor.md) | Next: [09-deadline-budget-split.md](09-deadline-budget-split.md)
