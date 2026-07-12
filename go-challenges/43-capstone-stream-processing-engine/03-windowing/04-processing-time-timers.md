# Exercise 4: Processing-Time Windows with Timers

Event-time windows fire on the timestamps inside records; processing-time windows fire on the wall clock, driven by a timer that has nothing to do with record content. That makes them simple and low-latency but historically painful to test, because asserting on a timer means really waiting and risking a flaky boundary. This exercise builds a processing-time tumbling window driven by a `time.Ticker` and tests it with `testing/synctest`, whose fake clock makes a one-second window fire in zero real time and at an exact virtual instant.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ptwindow.go            Window, Windower, New, Add, Windows, Close
cmd/
  demo/
    main.go            feed readings, let real ticks close two windows
ptwindow_test.go       synctest: two windows fire exactly, empty interval emits nothing
```

- Files: `ptwindow.go`, `cmd/demo/main.go`, `ptwindow_test.go`.
- Implement: `Windower` with `Add(value)`, a `Windows()` result channel, and `Close()`, driven internally by a `time.Ticker` of the window size.
- Test: under `testing/synctest`, that two successive windows carry exactly the records added in their intervals and that an interval with no records emits nothing.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/03-windowing/04-processing-time-timers/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/03-windowing/04-processing-time-timers
go mod edit -go=1.26
```

### Why processing-time windows are timer-driven, and why that resists testing

A processing-time window is defined by the wall clock: "everything that arrives between 10:00:00 and 10:00:01 of real time belongs to this window, regardless of the timestamps the records carry." Nothing in a record can close such a window — only the passage of real time can — so the operator must own a timer. Here a `time.Ticker` fires every window size; on each tick the goroutine snapshots whatever has accumulated since the last tick, resets the accumulator, and publishes the window. Records arriving after the tick belong to the next window. This is the lowest-latency windowing there is, because emission never waits for late data; it is also the least reproducible, because the same logical event lands in different windows depending on when the engine happened to see it.

That wall-clock dependence is exactly what makes a naive test bad. To check that a one-second window fires, the obvious test calls `time.Sleep(time.Second)` and then asserts — so the test really takes a second, and a scheduling hiccup that delays a record across the one-second boundary moves it into the adjacent window and fails the assertion at random. Multiply by a suite of such tests and CI becomes slow and intermittently red, with failures that do not reproduce on a developer's faster machine.

### How testing/synctest makes timer tests exact

`testing/synctest`, stable since Go 1.25, runs a function inside a *bubble* with a fake clock. Inside the bubble, time starts at a fixed instant and does not advance until every goroutine in the bubble is blocked; at that point the fake clock jumps forward to the next timer, which then fires. A `time.Sleep(time.Second)` or a `time.Ticker(time.Second)` therefore completes instantly in real time but at the exact virtual instant, so there is no boundary jitter and no real waiting. The companion `synctest.Wait` blocks the caller until every other goroutine in the bubble is durably blocked, which is how a test synchronises with the operator's background goroutine: after `Wait` returns, the tick has been processed and the window has been published, so the subsequent channel receive sees a fully-settled result. The test below advances virtual time by exactly one window size at a time and reads exactly one window per interval, deterministically.

The operator itself contains no test scaffolding — it uses real `time.Ticker` and real channels. `synctest` works by faking the clock those APIs consult, so the same code is exact under test and ordinary in production. The one design choice that matters for both is that the background goroutine only publishes a window when it is non-empty, so an idle interval produces no output and the consumer never has to filter empties.

Create `ptwindow.go`:

```go
// Package ptwindow implements a processing-time tumbling window: a background
// goroutine driven by a time.Ticker emits the records accumulated in each
// wall-clock interval. It is exact under testing/synctest.
package ptwindow

import (
	"sync"
	"time"
)

// Window is the aggregate of the records that arrived during one processing-time
// interval.
type Window struct {
	Sum   int64
	Count int
}

// Windower accumulates Add calls and, every size of wall-clock time, publishes the
// accumulated Window on the channel returned by Windows. It is safe for concurrent
// use.
type Windower struct {
	size time.Duration

	mu  sync.Mutex
	cur Window

	out  chan Window
	stop chan struct{}
	done chan struct{}
}

// New starts a Windower whose ticker fires every size. Call Close to stop it.
func New(size time.Duration) *Windower {
	w := &Windower{
		size: size,
		out:  make(chan Window),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go w.run()
	return w
}

// Add folds value into the current (still-open) window.
func (w *Windower) Add(value int64) {
	w.mu.Lock()
	w.cur.Sum += value
	w.cur.Count++
	w.mu.Unlock()
}

// Windows returns the channel on which closed windows are published. A window is
// published only when it contains at least one record.
func (w *Windower) Windows() <-chan Window {
	return w.out
}

// Close stops the background goroutine and waits for it to exit.
func (w *Windower) Close() {
	close(w.stop)
	<-w.done
}

func (w *Windower) run() {
	defer close(w.done)
	t := time.NewTicker(w.size)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			w.mu.Lock()
			win := w.cur
			w.cur = Window{}
			w.mu.Unlock()
			if win.Count == 0 {
				continue // idle interval: publish nothing
			}
			select {
			case w.out <- win:
			case <-w.stop:
				return
			}
		case <-w.stop:
			return
		}
	}
}
```

### The runnable demo

The demo runs against the real clock. It adds two readings, sleeps past one 20-millisecond tick to let the first window close, reads it, then adds three readings into the next interval and reads that window. Because every `Add` completes long before the following tick, the output is stable: the first window sums the two readings, the second sums the three.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ptwindow"
)

func main() {
	w := ptwindow.New(20 * time.Millisecond)
	defer w.Close()

	// First interval: two readings.
	w.Add(10)
	w.Add(20)
	time.Sleep(30 * time.Millisecond) // cross the first tick
	win := <-w.Windows()
	fmt.Printf("window 1: sum=%d count=%d\n", win.Sum, win.Count)

	// Second interval: three readings.
	w.Add(1)
	w.Add(2)
	w.Add(3)
	time.Sleep(30 * time.Millisecond) // cross the second tick
	win = <-w.Windows()
	fmt.Printf("window 2: sum=%d count=%d\n", win.Sum, win.Count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
window 1: sum=30 count=2
window 2: sum=6 count=3
```

### Tests

The tests run inside `synctest.Test`, so the 100-millisecond window fires in zero real time and at an exact virtual boundary. `TestTwoWindowsFireExactly` adds two readings, advances virtual time by one window with `time.Sleep`, calls `synctest.Wait` to let the background goroutine process the tick, and reads exactly the expected window — then repeats for a second window with different records, proving the accumulator resets. `TestEmptyIntervalEmitsNothing` advances past two ticks with no records and asserts the result channel has nothing, confirming idle intervals stay silent.

Create `ptwindow_test.go`:

```go
package ptwindow

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestTwoWindowsFireExactly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := New(100 * time.Millisecond)
		defer w.Close()

		// First interval: two readings, then advance exactly one window.
		w.Add(10)
		w.Add(20)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		got := <-w.Windows()
		if got.Sum != 30 || got.Count != 2 {
			t.Fatalf("window 1 = %+v, want {Sum:30 Count:2}", got)
		}

		// Second interval: three readings; the accumulator must have reset.
		w.Add(1)
		w.Add(2)
		w.Add(3)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		got = <-w.Windows()
		if got.Sum != 6 || got.Count != 3 {
			t.Fatalf("window 2 = %+v, want {Sum:6 Count:3}", got)
		}
	})
}

func TestEmptyIntervalEmitsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := New(100 * time.Millisecond)
		defer w.Close()

		// Advance past two ticks without adding anything.
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()

		select {
		case got := <-w.Windows():
			t.Fatalf("idle intervals must emit nothing, got %+v", got)
		default:
		}
	})
}

func TestSingleReadingWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := New(50 * time.Millisecond)
		defer w.Close()

		w.Add(42)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		got := <-w.Windows()
		if got.Sum != 42 || got.Count != 1 {
			t.Fatalf("window = %+v, want {Sum:42 Count:1}", got)
		}
	})
}
```

## Review

The operator is correct when each published window holds exactly the records added during its interval and the accumulator resets cleanly on every tick. The most common error is publishing empty windows: if the tick path sends unconditionally, every idle interval emits a zero window and the consumer must filter it, so the `Count == 0` guard belongs in the operator. The second error is forgetting that `Close` must unblock a goroutine that is parked trying to send on `out` — the inner `select` on `w.stop` is what lets a shutdown proceed when no one is reading. The third is a testing error rather than a code one: testing this with real `time.Sleep` instead of `testing/synctest` reintroduces the slowness and boundary flakiness the whole design is meant to avoid. With `synctest.Test` the clock is fake, `synctest.Wait` fences the background goroutine, and the suite is exact and instant; running it under `go test -race` additionally proves the `Add`/tick access to the shared accumulator is synchronised.

## Resources

- [testing/synctest](https://pkg.go.dev/testing/synctest) — the package, stable since Go 1.25, that supplies the fake clock and `Wait` used to test the ticker deterministically.
- [Testing concurrent code with testing/synctest (Go blog)](https://go.dev/blog/synctest) — the rationale and worked examples for bubbled time, including timer- and ticker-driven code like this operator.
- [pkg.go.dev/time: Ticker](https://pkg.go.dev/time#Ticker) — the real ticker the operator drives, faked transparently inside a synctest bubble.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-pane-sharing.md](03-pane-sharing.md) | Next: [Watermarks and Late Data](../04-watermarks-late-data/00-concepts.md)
