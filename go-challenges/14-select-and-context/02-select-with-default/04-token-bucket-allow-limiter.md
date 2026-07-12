# Exercise 4: Token-Bucket Rate Limiter With Non-Blocking Allow()

An in-process rate limiter guards a handler: `Allow()` returns true if the caller
may proceed, false if it has exceeded its rate. The classic implementation is a
token bucket, and it maps cleanly onto a buffered channel — tokens are the buffered
elements, `Allow` is a non-blocking receive, and a ticker refills with a
non-blocking send that drops when full to cap the bucket. No mutex touches the hot
path.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
limiter/                    independent module: example.com/limiter
  go.mod                    go 1.26
  limiter.go                type Limiter; Allow (try-recv), Refill (ticker try-send)
  cmd/
    demo/
      main.go               burst of Allow calls, then a refilled one
  limiter_test.go           burst exactness, cap, non-blocking, concurrent -race
```

- Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`.
- Implement: `New(burst int)` (starts full), `Allow() bool` (non-blocking token receive), `Refill(ctx, interval)` (ticker loop adding one token per tick, dropping when full), and an unexported `refillOnce` the ticker calls.
- Test: with `burst` tokens and no refill, exactly `burst` `Allow()`s succeed and the next fails; `refillOnce` never overflows the cap; `Allow` never blocks; a running `Refill` eventually replenishes; concurrent `Allow` issues no token twice.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/02-select-with-default/04-token-bucket-allow-limiter/cmd/demo
cd go-solutions/14-select-and-context/02-select-with-default/04-token-bucket-allow-limiter
go mod edit -go=1.26
```

### Tokens are buffered channel elements

A token bucket has two numbers: `burst` (the bucket's capacity, the most requests
that can pass in an instantaneous spike) and a refill rate (one token every
`interval`). Model the bucket as `make(chan struct{}, burst)` pre-filled with
`burst` tokens. `Allow()` is a non-blocking receive: a token present means "permit,
consume it"; an empty channel means "rejected". Because the channel holds exactly
`burst` tokens and each successful receive removes one, exactly `burst` `Allow()`s
succeed before the bucket is empty — and this is true even under concurrent callers,
because a buffered-channel receive is atomic. That is the property that makes the
limiter correct without a mutex: the channel *is* the synchronized counter.

Refill is the mirror. A ticker fires every `interval`; on each tick the limiter does
a non-blocking send of one token. The non-blocking send is essential: if the bucket
is already full (`burst` tokens waiting), the send takes `default` and the refill is
dropped, which is exactly what caps the bucket. Without the `default`, a full bucket
would block the refill goroutine, and worse, tokens would accumulate past `burst`
the moment a caller drained one — unbounded credit for an idle-then-bursty client.
The drop-when-full refill is the cap.

`refillOnce` is factored out so the cap logic can be tested deterministically
without waiting on a real ticker: it performs exactly one non-blocking send. `Refill`
is the loop that calls `refillOnce` on each `ticker.C` tick and exits on
`ctx.Done()`, with `defer t.Stop()` to release the runtime timer.

Create `limiter.go`:

```go
package limiter

import (
	"context"
	"time"
)

// Limiter is a token-bucket rate limiter backed by a buffered channel of tokens.
// Allow is a non-blocking receive; Refill tops the bucket up on a ticker. It is
// safe for concurrent use with no mutex on the Allow path.
type Limiter struct {
	tokens chan struct{}
}

// New returns a Limiter with capacity burst, initially full (a fresh client may
// spend its whole burst immediately).
func New(burst int) *Limiter {
	l := &Limiter{tokens: make(chan struct{}, burst)}
	for range burst {
		l.tokens <- struct{}{}
	}
	return l
}

// Allow consumes one token and returns true if the bucket is non-empty, or
// returns false without blocking if it is empty (rate exceeded).
func (l *Limiter) Allow() bool {
	select {
	case <-l.tokens:
		return true
	default:
		return false
	}
}

// refillOnce adds a single token, dropping it if the bucket is already full. This
// non-blocking send is what caps the bucket at burst.
func (l *Limiter) refillOnce() {
	select {
	case l.tokens <- struct{}{}:
	default:
	}
}

// Refill adds one token every interval until ctx is done. Excess refills onto a
// full bucket are dropped, capping accumulated credit at burst.
func (l *Limiter) Refill(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.refillOnce()
		}
	}
}
```

### The runnable demo

The demo spends the whole burst, shows the next call rejected, then starts a refill
goroutine and polls until a token comes back — a real "throttled, then allowed
again" sequence.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/limiter"
)

func main() {
	l := limiter.New(3)

	allowed := 0
	for range 3 {
		if l.Allow() {
			allowed++
		}
	}
	fmt.Println("allowed in burst:", allowed)
	fmt.Println("next allowed:", l.Allow())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Refill(ctx, 5*time.Millisecond)

	// Wait for the bucket to refill a token.
	for !l.Allow() {
		time.Sleep(time.Millisecond)
	}
	fmt.Println("allowed after refill: true")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
allowed in burst: 3
next allowed: false
allowed after refill: true
```

### Tests

`TestBurstExact` proves exactly `burst` `Allow()`s succeed and the next fails, with
no refill running. `TestRefillCapsBucket` calls `refillOnce` more than `burst` times
on a full bucket and drains to confirm the count never exceeds `burst` — the cap,
tested with no timing. `TestAllowNeverBlocks` guards `Allow` behind a timeout.
`TestRefillReplenishes` drains the bucket, runs `Refill` with a short interval, and
asserts a token eventually returns (generous deadline, so it is robust, not flaky),
then cancels the refill goroutine. `TestConcurrentAllowIssuesEachTokenOnce` runs
many concurrent `Allow` callers against a fixed set of tokens and asserts exactly
`burst` succeed — the buffered channel guarantees this even under `-race`.

Create `limiter_test.go`:

```go
package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBurstExact(t *testing.T) {
	t.Parallel()

	const burst = 5
	l := New(burst)

	granted := 0
	for range burst {
		if l.Allow() {
			granted++
		}
	}
	if granted != burst {
		t.Fatalf("granted %d of the initial burst, want %d", granted, burst)
	}
	if l.Allow() {
		t.Fatal("Allow succeeded on an empty bucket")
	}
}

func TestRefillCapsBucket(t *testing.T) {
	t.Parallel()

	const burst = 3
	l := New(burst) // starts full

	// Ten refills onto a full bucket must not exceed the cap.
	for range 10 {
		l.refillOnce()
	}

	count := 0
	for l.Allow() {
		count++
	}
	if count != burst {
		t.Fatalf("bucket held %d tokens after over-refill, want cap %d", count, burst)
	}
}

func TestAllowNeverBlocks(t *testing.T) {
	t.Parallel()

	l := New(0) // empty bucket: Allow must reject, not block
	done := make(chan bool, 1)
	go func() { done <- l.Allow() }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("Allow on an empty bucket returned true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Allow blocked; it must be non-blocking")
	}
}

func TestRefillReplenishes(t *testing.T) {
	t.Parallel()

	l := New(1)
	if !l.Allow() {
		t.Fatal("initial token missing")
	}
	if l.Allow() {
		t.Fatal("bucket not empty after draining")
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		l.Refill(ctx, 2*time.Millisecond)
		close(stopped)
	}()

	// Generous deadline: a token must eventually reappear.
	deadline := time.After(2 * time.Second)
	for {
		if l.Allow() {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("Refill never replenished a token")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	<-stopped // Refill goroutine exits on cancel: no leak
}

func TestConcurrentAllowIssuesEachTokenOnce(t *testing.T) {
	t.Parallel()

	const burst, callers = 50, 200
	l := New(burst) // no refill running

	var granted atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow() {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if granted.Load() != burst {
		t.Fatalf("granted %d tokens across %d concurrent callers, want exactly %d",
			granted.Load(), callers, burst)
	}
}
```

## Review

The limiter is correct when the number of successful `Allow()`s between refills
equals the tokens in the bucket, no more: exactly `burst` at a cold start, capped at
`burst` no matter how many refills fire. The two mistakes that break it are a
blocking refill send (which lets a full bucket stall the refill goroutine and, worse,
lets credit accumulate past `burst`) and a mutex-guarded counter on the `Allow` path
(which reintroduces the contention the channel was chosen to avoid). The concurrency
proof is `TestConcurrentAllowIssuesEachTokenOnce`: 200 goroutines race for 50
tokens and exactly 50 win, because a buffered-channel receive is atomic — the
channel is the synchronized token count. For production rate limiting with a smooth
rate rather than a fixed bucket, reach for `golang.org/x/time/rate`; this
channel-based bucket is the right shape when you want a simple, dependency-free,
lock-free in-process gate.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking receive (Allow) and send (refill).
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — `NewTicker`, `Ticker.C`, `Ticker.Stop` for the refill loop.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the standard smoothed-rate limiter, for comparison with this bucket.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-load-shedding-worker-submit.md](05-load-shedding-worker-submit.md)
