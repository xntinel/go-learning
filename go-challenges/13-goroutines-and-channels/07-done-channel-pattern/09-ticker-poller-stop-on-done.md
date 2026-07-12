# Exercise 9: A Periodic Health Poller You Can Stop Cleanly

An in-process health loop — pinging a database, probing an upstream — runs a check on a fixed interval
and must stop the instant the service shuts down, without leaking the ticker. This exercise drives a
`time.Ticker` inside a `for`/`select` that watches both the tick and a `done` channel, records the
outcome of each check, and stops the ticker on the way out.

## What you'll build

```text
healthpoller/                      independent module: example.com/healthpoller
  go.mod
  poller.go                        type Poller; Poll(done, interval, check); Checks/ConsecutiveFailures/LastErr
  cmd/
    demo/
      main.go                      runnable demo: poll a flaky check, stop after a few ticks
  poller_test.go                   runs-checks, stops-on-done, records-failures; -race
```

Files: `poller.go`, `cmd/demo/main.go`, `poller_test.go`.
Implement: `Poller.Poll(done <-chan struct{}, interval time.Duration, check func() error)` driving a `time.NewTicker`, running `check` each tick, recording total checks, consecutive failures, and the last error; `defer ticker.Stop()` on exit.
Test: the poller runs checks over time; closing `done` stops it (its goroutine returns); a failing check increments the consecutive-failure counter.
Verify: `go test -count=1 -race ./...`

### The ticker loop and why Stop matters

`time.NewTicker(interval)` returns a ticker whose channel `C` delivers a value every interval. The loop
selects on `C` and on `done`:

```go
t := time.NewTicker(interval)
defer t.Stop()
for {
	select {
	case <-t.C:
		p.record(check())
	case <-done:
		return
	}
}
```

The `defer t.Stop()` is not optional. `Stop` does not close `t.C` — it halts delivery and lets the
runtime reclaim the underlying timer. Return from this loop without calling `Stop` and the timer keeps
firing into a channel nobody reads, kept alive until the garbage collector eventually notices; in a
long-lived service that starts and stops many pollers, forgetting `Stop` is a slow resource leak.
`defer t.Stop()` right after `NewTicker` ties the ticker's lifetime to the loop's.

The two select cases are the same two-signal shape as every other worker in this lesson: a tick is the
work, and `done` is cancellation. Because the poller returns promptly on `done`, a caller can shut it
down immediately at SIGTERM.

### Recording status without a data race

`check` returns an error or nil each tick. The poller records three things: a total check count, the
number of *consecutive* failures (reset to zero on any success), and the last error observed. A health
scraper on another goroutine reads these while the loop writes them, so the writes and reads must be
synchronized. The total count is an `atomic.Int64` (read often, cheaply); the consecutive-failure count
and last error are guarded by a mutex, since they update together. `record` writes all three; the
accessors read them under the same discipline.

Create `poller.go`:

```go
package healthpoller

import (
	"sync"
	"sync/atomic"
	"time"
)

// Poller runs a health check on an interval and records its recent status.
// It is safe for a scraper on another goroutine to read the status while Poll
// runs.
type Poller struct {
	checks     atomic.Int64
	mu         sync.Mutex
	consecFail int
	lastErr    error
}

// New returns a ready Poller.
func New() *Poller {
	return &Poller{}
}

func (p *Poller) record(err error) {
	p.checks.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.consecFail++
		p.lastErr = err
	} else {
		p.consecFail = 0
		p.lastErr = nil
	}
}

// Poll runs check every interval until done is closed. It stops the ticker on
// exit. done is receive-only: Poll observes cancellation but never triggers it.
func (p *Poller) Poll(done <-chan struct{}, interval time.Duration, check func() error) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.record(check())
		case <-done:
			return
		}
	}
}

// Checks reports how many checks have run.
func (p *Poller) Checks() int64 {
	return p.checks.Load()
}

// ConsecutiveFailures reports the current run of consecutive failing checks.
func (p *Poller) ConsecutiveFailures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.consecFail
}

// LastErr reports the most recent check error, or nil after a success.
func (p *Poller) LastErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}
```

### The runnable demo

The demo stops the poller deterministically from inside `check` — the check closes `done` on its third
call. Because the check and its recording take microseconds while the interval is 20 ms, the ticker's
channel is empty when the loop re-enters the select right after the third tick, so `done` is the only
ready case and the loop exits after exactly three checks.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/healthpoller"
)

func main() {
	p := healthpoller.New()
	done := make(chan struct{})

	n := 0
	check := func() error {
		n++
		switch n {
		case 2:
			return errors.New("db ping failed")
		case 3:
			close(done)
			return errors.New("db ping failed")
		}
		return nil
	}

	p.Poll(done, 20*time.Millisecond, check)

	fmt.Printf("checks run: %d\n", p.Checks())
	fmt.Printf("consecutive failures: %d\n", p.ConsecutiveFailures())
	fmt.Printf("last error: %v\n", p.LastErr())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
checks run: 3
consecutive failures: 2
last error: db ping failed
```

### Tests

`TestPollerRunsChecks` starts the poller on a 1 ms interval, waits until it has run a few checks, then
closes `done` and confirms the count is frozen once the goroutine has returned. `TestPollerStopsOnDone`
closes `done` and asserts the poller goroutine returns within a budget — a leak (a forgotten `done`
case) would make it hang. `TestPollerRecordsFailures` uses a check that always fails and asserts the
consecutive-failure counter climbs and `LastErr` matches the sentinel via `errors.Is`.

Create `poller_test.go`:

```go
package healthpoller

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPollerRunsChecks(t *testing.T) {
	t.Parallel()

	p := New()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		p.Poll(done, time.Millisecond, func() error { return nil })
		close(stopped)
	}()

	waitFor(t, func() bool { return p.Checks() >= 3 })
	close(done)
	<-stopped

	c := p.Checks()
	time.Sleep(20 * time.Millisecond) // the poller has returned; count must not grow
	if p.Checks() != c {
		t.Fatalf("checks grew after stop: %d -> %d", c, p.Checks())
	}
}

func TestPollerStopsOnDone(t *testing.T) {
	t.Parallel()

	p := New()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		p.Poll(done, time.Millisecond, func() error { return nil })
		close(stopped)
	}()

	close(done)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after done closed")
	}
}

func TestPollerRecordsFailures(t *testing.T) {
	t.Parallel()

	p := New()
	done := make(chan struct{})
	stopped := make(chan struct{})
	wantErr := errors.New("probe failed")
	go func() {
		p.Poll(done, time.Millisecond, func() error { return wantErr })
		close(stopped)
	}()

	waitFor(t, func() bool { return p.ConsecutiveFailures() >= 2 })
	close(done)
	<-stopped

	if p.ConsecutiveFailures() < 2 {
		t.Fatalf("consecutive failures = %d, want >= 2", p.ConsecutiveFailures())
	}
	if !errors.Is(p.LastErr(), wantErr) {
		t.Fatalf("last err = %v, want %v", p.LastErr(), wantErr)
	}
}

func ExamplePoller_Poll() {
	p := New()
	done := make(chan struct{})
	n := 0
	check := func() error {
		n++
		if n == 2 {
			close(done)
		}
		return nil
	}
	p.Poll(done, 20*time.Millisecond, check)
	fmt.Println(p.Checks() >= 2)
	// Output: true
}
```

## Review

The poller is correct when it runs checks on the interval, freezes the moment `done` closes, and records
status without a data race. The stops-on-done test is the leak guard: if the loop dropped its `done`
case it would never return and the test would time out. The records-failures test pins the consecutive-
failure semantics — a real health loop uses that counter to decide when to flip a readiness probe. The
one detail that separates a correct poller from a leaky one is `defer t.Stop()`: it releases the timer
when the loop exits. Run `go test -race` to confirm the `atomic.Int64` and the mutex-guarded status hold
up under a concurrent reader. Note the demo's determinism relies on the check being far faster than the
interval, so the ticker channel is empty when the loop re-checks `done` — a real poller does not depend
on that, but it makes the demo output exact.

## Resources

- [pkg.go.dev: time.NewTicker and Ticker.Stop](https://pkg.go.dev/time#NewTicker)
- [Go Language Spec: Select statements](https://go.dev/ref/spec#Select_statements)
- [pkg.go.dev: sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-graceful-shutdown-coordinator.md](08-graceful-shutdown-coordinator.md) | Next: [10-bridge-done-to-context.md](10-bridge-done-to-context.md)
