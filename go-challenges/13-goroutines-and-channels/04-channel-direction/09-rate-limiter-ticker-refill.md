# Exercise 9: A Token Refill Loop Consuming a Receive-Only Tick Channel

The core of a ticker-driven rate limiter is a refill loop: consume a tick channel,
emit one token per tick. `time.Ticker.C` is a `<-chan time.Time` — the runtime
owns and feeds it — and the tokens go out on a `chan<- int` the loop only feeds.
Injecting the tick channel instead of hard-wiring a real ticker makes the loop
deterministically testable.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ratelimit/                   independent module: example.com/ratelimit
  go.mod                     go 1.26
  ratelimit.go               Refill(ticks <-chan time.Time, tokens chan<- int,
                             stop <-chan struct{})
  cmd/
    demo/
      main.go                runnable demo: real ticker drives token refill
  ratelimit_test.go          one-token-per-tick, stop, no-block-when-full
```

Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
Implement: `Refill(ticks <-chan time.Time, tokens chan<- int, stop <-chan struct{})` — emit one token per tick, exit on stop, never block when the token buffer is full.
Test: N fake ticks yield N tokens, the stop signal ends the loop, a full token buffer does not block the loop.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/04-channel-direction/09-rate-limiter-ticker-refill/cmd/demo
cd go-solutions/13-goroutines-and-channels/04-channel-direction/09-rate-limiter-ticker-refill
```

### Why every channel here is directional, and why the token send is non-blocking

`Refill` takes three channels, each with the narrowest direction it needs.
`ticks <-chan time.Time` is receive-only — the loop consumes ticks; in production
this is `time.Ticker.C`, which is itself `<-chan time.Time`, so the injected
parameter matches the real thing exactly. `tokens chan<- int` is send-only — the
loop only produces tokens. `stop <-chan struct{}` is receive-only — the loop only
waits for the stop signal (a closed channel is the idiomatic broadcast stop).

The loop `select`s on `stop` and `ticks`. On a tick it increments a counter and
tries to send a token. That send is itself a non-blocking `select { case tokens
<- n: default: }`: if the token buffer is full — the consumer is not taking
tokens fast enough — the loop drops the token rather than blocking. That is the
correct backpressure policy for a rate limiter: a full bucket means the caller is
already saturated with permits, so a dropped refill is harmless, and a refill loop
that blocked on a full bucket would stop reacting to `stop` and to time. Direction
does not decide this; the `default` case does. But the send-only type is what
guarantees the loop can only ever feed `tokens`, never accidentally drain it.

Injecting `ticks` is the testability win. A test drives a plain `chan time.Time`
by hand — one send per simulated tick — so N ticks deterministically produce N
token attempts with no real time elapsed and no flakiness. The demo wires a real
`time.NewTicker` to show the production shape.

Create `ratelimit.go`:

```go
package ratelimit

import "time"

// Refill emits one token per tick until stop is closed. ticks is receive-only
// (in production, time.Ticker.C); tokens is send-only; stop is receive-only.
// The token send is non-blocking: if the token buffer is full, the refill is
// dropped rather than blocking the loop.
func Refill(ticks <-chan time.Time, tokens chan<- int, stop <-chan struct{}) {
	n := 0
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			n++
			select {
			case tokens <- n:
			default:
			}
		}
	}
}
```

### The runnable demo

The demo drives `Refill` with a real 5 ms ticker, collects four tokens, then
closes the stop channel. The token *values* are the running refill count 1..4.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	tokens := make(chan int, 4)
	stop := make(chan struct{})
	go ratelimit.Refill(ticker.C, tokens, stop)

	for i := 0; i < 4; i++ {
		fmt.Printf("token %d\n", <-tokens)
	}
	close(stop)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token 1
token 2
token 3
token 4
```

### Tests

The tests inject a manually-driven `chan time.Time` for determinism.
`TestRefillEmitsOneTokenPerTick` interleaves one tick and one token read, N times,
asserting the running count. `TestRefillStopsOnStopSignal` closes stop and asserts
the goroutine returns. `TestRefillDoesNotBlockWhenTokenBufferFull` feeds more ticks
than the token buffer can hold and asserts the loop keeps consuming ticks (never
blocks), dropping the surplus.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestRefillEmitsOneTokenPerTick(t *testing.T) {
	t.Parallel()

	ticks := make(chan time.Time)
	tokens := make(chan int, 1)
	stop := make(chan struct{})
	defer close(stop)
	go Refill(ticks, tokens, stop)

	for i := 1; i <= 5; i++ {
		ticks <- time.Now()
		got := <-tokens
		if got != i {
			t.Fatalf("tick %d produced token %d, want %d", i, got, i)
		}
	}
}

func TestRefillStopsOnStopSignal(t *testing.T) {
	t.Parallel()

	ticks := make(chan time.Time)
	tokens := make(chan int, 1)
	stop := make(chan struct{})

	done := make(chan struct{})
	go func() {
		Refill(ticks, tokens, stop)
		close(done)
	}()

	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Refill did not return after stop closed")
	}
}

func TestRefillDoesNotBlockWhenTokenBufferFull(t *testing.T) {
	t.Parallel()

	ticks := make(chan time.Time)
	tokens := make(chan int, 1) // room for exactly one token
	stop := make(chan struct{})
	defer close(stop)
	go Refill(ticks, tokens, stop)

	// Send more ticks than the buffer can hold. Each send blocks only until
	// Refill receives the tick, so if the loop never blocks on a full token
	// buffer, all sends complete. A blocking loop would deadlock this test.
	for range 10 {
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("Refill stopped consuming ticks; token send blocked")
		}
	}
}
```

## Review

The refill loop is correct when it emits exactly one token per tick, exits
promptly on stop, and never blocks on a full token buffer. The no-block test is
the load-bearing one: a rate limiter whose refill loop blocks on a saturated
bucket stops honoring `stop` and stops reading time — it becomes unresponsive
exactly when the system is busiest. The three directional parameters document the
loop's role precisely — it drains ticks and stop, and only feeds tokens — while
the non-blocking send is what implements the drop-on-full backpressure. Run
`go test -race` to confirm the tick/token handoff is clean.

## Resources

- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — `Ticker.C` is `<-chan Time`; `NewTicker`/`Stop`, the production shape this loop consumes.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the `default` case that makes the token send non-blocking.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the send-only and receive-only parameter types documenting the loop's role.

---

Prev: [08-signal-shutdown-notify.md](08-signal-shutdown-notify.md) | Back to [00-concepts.md](00-concepts.md) | Next: [10-load-balancing-stream-distributor.md](10-load-balancing-stream-distributor.md)
