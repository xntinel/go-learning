# Exercise 7: Circuit Breaker Wrapping a Call in a Stateful Closure

A circuit breaker is a stateful primitive whose entire state machine lives inside
a closure. `NewBreaker(threshold, cooldown, now)` returns a
`func(op func() error) error` that captures the breaker state, the consecutive
failure count, the open-since timestamp, an injected clock, and a mutex. It opens
after `threshold` consecutive failures, fails fast while open, and probes once
after the cooldown elapses. The injected clock makes every state transition
testable without sleeping.

This module is fully self-contained.

## What you'll build

```text
breaker/                   independent module: example.com/breaker
  go.mod                   go 1.26
  breaker.go               ErrOpen, NewBreaker closure state machine
  cmd/
    demo/
      main.go              trips the breaker, waits out cooldown, recovers
  breaker_test.go          opens, fails fast, probes, closes/re-opens under -race
```

- Files: `breaker.go`, `cmd/demo/main.go`, `breaker_test.go`.
- Implement: `NewBreaker(threshold int, cooldown time.Duration, now func() time.Time) func(op func() error) error` with closed/open/half-open states captured in the closure; `ErrOpen` returned without calling `op` while open.
- Test: opens after `threshold` consecutive failures; while open, calls return `ErrOpen` and `op` is not invoked; after the cooldown a single probe runs — success closes, failure re-opens; safe under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/breaker/cmd/demo
cd ~/go-exercises/breaker
go mod init example.com/breaker
```

### The state machine as captured variables

The breaker has three states. **Closed** is normal: `op` runs, and consecutive
failures are counted. When the count reaches `threshold`, the breaker trips to
**open** and records `openedAt`. While **open**, calls fail fast — they return
`ErrOpen` immediately *without invoking `op`*, which is the whole point of a
breaker: stop hammering a downstream that is already failing. Once `cooldown` has
elapsed since `openedAt`, the next call transitions to **half-open** and lets a
single probe through. If the probe succeeds, the breaker closes and resets; if it
fails, it re-opens and the cooldown restarts.

Every piece of that state — `st`, `failures`, `openedAt` — is a captured variable
in the returned closure, guarded by a captured mutex. There is no breaker struct
exposed to callers; the closure *is* the breaker, and its only interface is "give
me an op, I decide whether to run it." The injected `now` is what makes the
time-based transitions (has the cooldown elapsed?) testable: the test freezes and
advances a fake clock to drive the breaker through open, half-open, and closed
deterministically, with no real waiting.

One subtlety worth stating: `op` is called *outside* the lock. Holding a mutex
across a downstream call would serialize every request through the breaker and
couple the lock's hold time to the downstream's latency. So the closure takes the
lock to read/transition state, releases it, calls `op`, then re-takes the lock to
record the result. Under heavy concurrency this admits more than one probe in the
half-open window; a production breaker adds a "one probe in flight" guard, which
is a natural extension but noise for the closure lesson.

Create `breaker.go`:

```go
package breaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned when the breaker is open and fails fast without calling op.
var ErrOpen = errors.New("circuit breaker open")

type state int

const (
	closed state = iota
	open
	halfOpen
)

// NewBreaker returns a closure guarding a downstream op. After threshold
// consecutive failures it opens and fails fast with ErrOpen until cooldown
// elapses, then allows one probe (half-open). A probe success closes it; a probe
// failure re-opens it. now is injected for deterministic tests.
func NewBreaker(threshold int, cooldown time.Duration, now func() time.Time) func(op func() error) error {
	var (
		mu       sync.Mutex
		st       = closed
		failures int
		openedAt time.Time
	)
	return func(op func() error) error {
		mu.Lock()
		if st == open {
			if now().Sub(openedAt) < cooldown {
				mu.Unlock()
				return ErrOpen
			}
			st = halfOpen
		}
		probing := st == halfOpen
		mu.Unlock()

		err := op()

		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures++
			if probing || failures >= threshold {
				st = open
				openedAt = now()
			}
			return err
		}
		failures = 0
		st = closed
		return nil
	}
}
```

### The runnable demo

The demo uses a fake clock. It drives four failing calls (tripping the breaker at
the third and failing fast on the fourth), then advances past the cooldown and
issues two succeeding calls: the first is the probe that closes the breaker, the
second flows through normally.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/breaker"
)

func main() {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	call := breaker.NewBreaker(3, 5*time.Second, clock)

	failing := func() error { return errors.New("downstream 503") }
	for i := range 4 {
		fmt.Printf("call %d: %v\n", i+1, call(failing))
	}

	now = now.Add(6 * time.Second) // past cooldown
	fmt.Printf("probe: %v\n", call(func() error { return nil }))
	fmt.Printf("next: %v\n", call(func() error { return nil }))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
call 1: downstream 503
call 2: downstream 503
call 3: downstream 503
call 4: circuit breaker open
probe: <nil>
next: <nil>
```

### Tests

Create `breaker_test.go`:

```go
package breaker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var errDown = errors.New("down")

func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	cur := start
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}
	return now, advance
}

func TestBreakerOpensAndFailsFast(t *testing.T) {
	t.Parallel()
	now, _ := fakeClock(time.Unix(0, 0))
	call := NewBreaker(3, time.Minute, now)

	calls := 0
	failing := func() error { calls++; return errDown }

	for range 3 { // trips on the 3rd
		if err := call(failing); !errors.Is(err, errDown) {
			t.Fatalf("err = %v, want errDown", err)
		}
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}

	if err := call(failing); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times while open, want 3 (fail fast, op not invoked)", calls)
	}
}

func TestBreakerProbesAfterCooldownAndCloses(t *testing.T) {
	t.Parallel()
	now, advance := fakeClock(time.Unix(0, 0))
	call := NewBreaker(2, 10*time.Second, now)

	failing := func() error { return errDown }
	call(failing)
	call(failing) // now open

	if err := call(func() error { return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("still-open err = %v, want ErrOpen", err)
	}

	advance(11 * time.Second)
	probed := false
	if err := call(func() error { probed = true; return nil }); err != nil {
		t.Fatalf("probe err = %v, want nil", err)
	}
	if !probed {
		t.Fatal("probe op was not invoked after cooldown")
	}
	// Breaker is closed again: a normal call runs.
	ran := false
	if err := call(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("post-close call err=%v ran=%v, want nil,true", err, ran)
	}
}

func TestBreakerReopensOnProbeFailure(t *testing.T) {
	t.Parallel()
	now, advance := fakeClock(time.Unix(0, 0))
	call := NewBreaker(1, 10*time.Second, now)

	call(func() error { return errDown }) // opens (threshold 1)
	advance(11 * time.Second)
	// Probe fails -> re-open.
	if err := call(func() error { return errDown }); !errors.Is(err, errDown) {
		t.Fatalf("probe err = %v, want errDown", err)
	}
	// Without advancing, breaker is open again: fail fast.
	if err := call(func() error { return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen after failed probe", err)
	}
}

func TestBreakerConcurrent(t *testing.T) {
	t.Parallel()
	now, _ := fakeClock(time.Unix(0, 0))
	call := NewBreaker(5, time.Minute, now)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = call(func() error { return errDown })
		}()
	}
	wg.Wait()
	// Breaker is open; a further call must fail fast.
	if err := call(func() error { return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}
```

## Review

The breaker is correct when it opens after exactly `threshold` consecutive
failures, returns `ErrOpen` without touching `op` while open (the captured call
counter stays put), admits a single probe once the injected clock passes the
cooldown, and closes on a probe success or re-opens on a probe failure. The
injected clock is what makes those time transitions deterministic — the tests
advance a fake clock instead of sleeping out real cooldowns. All state lives in
captured variables under a captured mutex, and `op` runs outside the lock so the
breaker never couples its lock hold time to downstream latency; the concurrency
test confirms the state machine is race-free. Run `go test -race`.

## Resources

- [pkg.go.dev: time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — measuring elapsed cooldown.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — matching `ErrOpen` and the downstream error.
- [Martin Fowler: CircuitBreaker](https://martinfowler.com/bliki/CircuitBreaker.html) — the closed/open/half-open state model.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-sequence-id-generator.md](06-sequence-id-generator.md) | Next: [08-validator-pipeline.md](08-validator-pipeline.md)
