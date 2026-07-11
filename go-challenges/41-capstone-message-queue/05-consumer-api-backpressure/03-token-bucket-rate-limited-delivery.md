# Exercise 3: Token-Bucket Rate-Limited Delivery

Sometimes the constraint on a consumer is neither memory nor concurrency but a hard rate: deliver at most R records per second to protect a downstream database, a third-party quota, or a billing pipeline. A token bucket is the standard primitive, and its guarantee is exact -- deliveries between any two instants can never exceed `burst + rate*elapsed`. This module builds the bucket with an injectable clock (so the bound is tested deterministically, not with flaky sleeps), wraps it in a consumer that forwards records under the limit, and adds a pause/resume override.

This module is fully self-contained: its own `go mod init`, its own bucket, consumer, demo, and `-race` tests. Nothing here imports any other exercise.

## What you'll build

```text
bucket.go            Bucket: NewBucket, Allow, Tokens (lazy refill, injectable clock)
consumer.go          Record, RateLimitedConsumer: New, Recv, Pause, Resume, Delivered, Close
cmd/
  demo/
    main.go          start paused; show zero delivery while paused, then ordered delivery
ratelimit_test.go    deterministic bound (fake clock) + live bound under -race + pause/resume
```

- Files: `bucket.go`, `consumer.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `Bucket` with lazy token regeneration and a `now()` seam, and `RateLimitedConsumer` whose forward loop gates each delivery on both a token and the pause flag.
- Test: a fake-clock test that token grants never exceed `burst + rate*elapsed`; a live test that the running consumer respects the same bound under `-race`; a pause/resume test that a paused consumer delivers nothing and a resumed one delivers every record in order.
- Verify: `go test -race ./... && go run ./cmd/demo`

Set up the module:

```bash
mkdir -p ratelimit/cmd/demo && cd ratelimit
go mod init example.com/ratelimit
```

### The token bucket: lazy refill and an injectable clock

The bucket holds up to `burst` tokens and regenerates `rate` per second. The design choice that matters is *lazy* regeneration: rather than a background goroutine adding tokens on a ticker, each call computes how many tokens accrued since the last call -- `tokens = min(burst, tokens + elapsed*rate)` -- and then tries to spend one. This has three payoffs. There is no goroutine to leak when the bucket outlives its owner; regeneration is perfectly smooth rather than arriving in tick-sized lumps; and the clock is a single `now func() time.Time` field that real code wires to `time.Now` and tests wire to a manually advanced fake clock.

That last point is what makes the rate bound *testable*. The guarantee "grants between t0 and t never exceed `burst + rate*(t-t0)`" is an invariant of the bucket, not an emergent property of wall-clock timing, so a test can advance a fake clock in fixed steps, drain the bucket at each step, and assert the cumulative grant count against the closed-form bound -- with no real time elapsing and no flakiness. `Allow` is the non-blocking spend (refill, then take one token if available); `Tokens` exposes the post-refill balance for introspection.

Create `bucket.go`:

```go
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Bucket is a token-bucket rate limiter. It starts full with burst tokens and
// regenerates rate tokens per second up to a ceiling of burst. Each successful
// Allow spends one token. The bucket therefore guarantees an upper bound: the
// number of tokens granted between two instants t0 and t never exceeds
// burst + rate*(t-t0). That inequality is the rate limit.
//
// The clock is injectable so the bound can be tested deterministically without
// real-time sleeps; NewBucket wires in time.Now.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens added per second
	burst  float64 // maximum tokens the bucket can hold
	tokens float64 // current token balance
	last   time.Time
	now    func() time.Time
}

// NewBucket returns a Bucket that regenerates rate tokens per second and holds
// at most burst tokens. It starts full.
func NewBucket(rate, burst float64) *Bucket {
	return newBucketClock(rate, burst, time.Now)
}

func newBucketClock(rate, burst float64, now func() time.Time) *Bucket {
	if burst < 1 {
		burst = 1
	}
	return &Bucket{
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   now(),
		now:    now,
	}
}

// refill credits tokens for the time elapsed since the last refill. Caller
// must hold b.mu.
func (b *Bucket) refill() {
	t := b.now()
	if elapsed := t.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
		b.last = t
	}
}

// Allow spends one token if one is available and reports whether it did. It
// never blocks.
func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Tokens returns the current token balance after refilling. Intended for tests
// and introspection.
func (b *Bucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return b.tokens
}
```

### The rate-limited consumer and pause/resume

`RateLimitedConsumer` owns a single forward goroutine that walks the upstream records and, for each one, waits in a small spin until two conditions hold: the consumer is not paused, and the bucket grants a token. Only then does it offer the record on the unbuffered delivery channel and bump the `delivered` counter. Because every delivery passes through `bucket.Allow()`, the live delivery count obeys the same `burst + rate*elapsed` bound the bucket guarantees -- the limit is enforced by construction, not by a separate accounting step.

Pause and resume are an orthogonal, operator-facing override layered on top of the automatic rate limit. They flip an `atomic.Bool` that the forward loop re-reads every iteration; while it is set the loop sleeps in `pollInterval` steps, spends no tokens, and delivers nothing, so the records simply wait. The poll interval bounds how quickly a `Pause` or `Resume` takes effect and how promptly a freshly regenerated token is noticed. Constructing the consumer with `startPaused` true makes the paused window deterministic for the demo and tests: nothing is delivered until `Resume` is called.

Create `consumer.go`:

```go
package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// pollInterval is how often the forward loop re-checks the pause flag and the
// token bucket when it is waiting (paused, or out of tokens). It bounds how
// quickly Pause/Resume take effect and how promptly a regenerated token is
// noticed.
const pollInterval = 2 * time.Millisecond

// Record is a single message delivered to the application.
type Record struct {
	Offset int64
	Value  []byte
}

// RateLimitedConsumer forwards records from a fixed upstream log to the
// application, gated by a token bucket so the delivery rate cannot exceed the
// bucket's configured rate (plus its burst). Delivery can also be paused and
// resumed at any time; while paused the forward loop spends no tokens and
// delivers nothing.
type RateLimitedConsumer struct {
	records  []Record
	bucket   *Bucket
	delivery chan Record
	closed   chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup

	paused    atomic.Bool
	delivered atomic.Int64

	closeOnce sync.Once
}

// NewRateLimitedConsumer builds a consumer over a copy of records, limited to
// rate deliveries per second with a burst of burst. If startPaused is true the
// consumer delivers nothing until Resume is called.
func NewRateLimitedConsumer(records []Record, rate, burst float64, startPaused bool) *RateLimitedConsumer {
	recs := make([]Record, len(records))
	copy(recs, records)
	c := &RateLimitedConsumer{
		records:  recs,
		bucket:   NewBucket(rate, burst),
		delivery: make(chan Record),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	c.paused.Store(startPaused)
	c.wg.Add(1)
	go c.forward()
	return c
}

// forward delivers each record once it has both (a) not-paused state and (b) a
// token from the bucket. It is the single writer of the delivery channel.
func (c *RateLimitedConsumer) forward() {
	defer c.wg.Done()
	defer close(c.done)

	for _, r := range c.records {
		// Wait until we are not paused and a token is available. Each spin
		// re-reads the pause flag, so Resume/Pause take effect within one
		// pollInterval.
		for {
			select {
			case <-c.closed:
				return
			default:
			}
			if c.paused.Load() {
				time.Sleep(pollInterval)
				continue
			}
			if c.bucket.Allow() {
				break
			}
			time.Sleep(pollInterval)
		}

		select {
		case c.delivery <- r:
			c.delivered.Add(1)
		case <-c.closed:
			return
		}
	}
}

// Recv returns the next delivered record, blocking until one is available, ctx
// is cancelled, or the consumer is closed/drained. The bool is false when no
// record will arrive.
func (c *RateLimitedConsumer) Recv(ctx context.Context) (Record, bool) {
	select {
	case r := <-c.delivery:
		return r, true
	case <-ctx.Done():
		return Record{}, false
	case <-c.closed:
		return Record{}, false
	case <-c.done:
		select {
		case r := <-c.delivery:
			return r, true
		default:
			return Record{}, false
		}
	}
}

// Pause halts delivery. The forward loop stops spending tokens within one
// pollInterval and delivers nothing until Resume is called.
func (c *RateLimitedConsumer) Pause() { c.paused.Store(true) }

// Resume restarts delivery after a Pause.
func (c *RateLimitedConsumer) Resume() { c.paused.Store(false) }

// Delivered reports how many records have been delivered so far.
func (c *RateLimitedConsumer) Delivered() int64 { return c.delivered.Load() }

// Close stops the forward loop and unblocks any pending Recv.
func (c *RateLimitedConsumer) Close() {
	c.closeOnce.Do(func() { close(c.closed) })
	c.wg.Wait()
}
```

### The runnable demo

The demo starts the consumer paused, so the pause window is fully deterministic: a background drainer is running, yet after 150 ms of being paused exactly zero records have been delivered. It then resumes and waits for all six records, which arrive in offset order (a single forward goroutine preserves order regardless of the rate limit). The printed counts do not depend on timing, only on the pause-then-resume sequence.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/ratelimit"
)

func main() {
	const records = 6
	src := make([]ratelimit.Record, records)
	for i := range src {
		src[i] = ratelimit.Record{Offset: int64(i), Value: []byte(fmt.Sprintf("msg-%d", i))}
	}

	// Start paused so the pause window is deterministic, then resume.
	c := ratelimit.NewRateLimitedConsumer(src, 50, 3, true)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []int64
	go func() {
		for {
			r, ok := c.Recv(ctx)
			if !ok {
				return
			}
			mu.Lock()
			got = append(got, r.Offset)
			mu.Unlock()
		}
	}()

	fmt.Println("rate=50/s burst=3, 6 records, started paused")
	time.Sleep(150 * time.Millisecond)
	fmt.Printf("delivered during 150ms pause: %d\n", c.Delivered())

	fmt.Println("resuming delivery")
	c.Resume()

	deadline := time.Now().Add(3 * time.Second)
	for c.Delivered() < records && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	order := make([]int64, len(got))
	copy(order, got)
	mu.Unlock()

	fmt.Printf("delivered in order: %v\n", order)
	fmt.Printf("total delivered: %d\n", c.Delivered())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
rate=50/s burst=3, 6 records, started paused
delivered during 150ms pause: 0
resuming delivery
delivered in order: [0 1 2 3 4 5]
total delivered: 6
```

### Tests

`TestBucketBoundsRate` is the deterministic bound proof: it drives the bucket with a fake clock, drains it at t0 (getting exactly the burst), then advances in 100 ms steps for a virtual second, asserting after each step that cumulative grants never exceed `burst + rate*elapsed`, and that the full-second total is the expected ceiling. No real time passes. `TestLiveRateBound` runs the actual concurrent consumer with a drainer for 150 ms and asserts the live delivery count respects `burst + rate*elapsed` -- which holds by construction because every delivery is gated by the bucket. `TestPauseResume` starts paused, asserts zero delivery during the pause window, resumes, and asserts every record arrives exactly once and in order. All three pass under `-race`.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic bucket tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// TestBucketBoundsRate drives a token bucket with a fake clock and asserts that
// the number of tokens granted never exceeds burst + rate*elapsed, which is the
// rate-limit guarantee. No real time passes, so the test is deterministic.
func TestBucketBoundsRate(t *testing.T) {
	t.Parallel()
	const (
		rate  = 10.0 // tokens per second
		burst = 5.0
	)
	start := time.Unix(0, 0)
	fc := &fakeClock{t: start}
	b := newBucketClock(rate, burst, fc.now)

	granted := 0
	drain := func() {
		for b.Allow() {
			granted++
		}
	}

	// At t0 only the initial burst is available.
	drain()
	if granted != int(burst) {
		t.Fatalf("initial drain granted %d, want %d (burst)", granted, int(burst))
	}

	// Advance in 100 ms steps for one virtual second. After each step the
	// cumulative grants must respect the bound.
	for range 10 {
		fc.advance(100 * time.Millisecond)
		drain()
		elapsed := fc.now().Sub(start).Seconds()
		bound := burst + rate*elapsed
		if float64(granted) > bound+1e-9 {
			t.Fatalf("granted %d exceeds bound %.3f at elapsed %.2fs", granted, bound, elapsed)
		}
	}

	// Over one second at rate 10 with burst 5, the ceiling is 15 grants.
	if granted != 15 {
		t.Errorf("after 1s granted %d, want 15 (burst + rate*1s)", granted)
	}
}

// TestLiveRateBound runs the real, concurrent consumer and asserts the live
// delivery count never exceeds the token-bucket bound. Because the bucket gates
// every delivery against real time, delivered <= burst + rate*elapsed holds by
// construction; the test confirms it under the race detector.
func TestLiveRateBound(t *testing.T) {
	t.Parallel()
	const (
		rate    = 100.0
		burst   = 10.0
		records = 2000
	)
	src := makeRecords(records)
	c := NewRateLimitedConsumer(src, rate, burst, false)
	defer c.Close()

	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A drainer keeps the delivery channel moving so deliveries are not
	// throttled by a slow receiver -- only by the bucket.
	go func() {
		for {
			if _, ok := c.Recv(ctx); !ok {
				return
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)

	delivered := c.Delivered()
	elapsed := time.Since(start).Seconds()
	bound := burst + rate*elapsed
	if float64(delivered) > bound+1 {
		t.Errorf("delivered %d exceeds rate bound %.2f at elapsed %.3fs", delivered, bound, elapsed)
	}
	if delivered == 0 {
		t.Error("expected some deliveries within 150ms")
	}
}

// TestPauseResume verifies that a consumer started paused delivers nothing
// while paused and delivers every record once resumed.
func TestPauseResume(t *testing.T) {
	t.Parallel()
	const records = 6
	src := makeRecords(records)
	// High rate and burst so the bucket never throttles; this test is about
	// pause/resume, not the rate bound.
	c := NewRateLimitedConsumer(src, 10000, 1000, true)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []int64
	go func() {
		for {
			r, ok := c.Recv(ctx)
			if !ok {
				return
			}
			mu.Lock()
			got = append(got, r.Offset)
			mu.Unlock()
		}
	}()

	// Paused from the start: nothing must be delivered.
	time.Sleep(80 * time.Millisecond)
	if d := c.Delivered(); d != 0 {
		t.Fatalf("delivered %d while paused, want 0", d)
	}

	c.Resume()

	deadline := time.Now().Add(2 * time.Second)
	for c.Delivered() < records && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d := c.Delivered(); d != records {
		t.Fatalf("delivered %d after resume, want %d", d, records)
	}

	// Records must arrive in offset order.
	mu.Lock()
	defer mu.Unlock()
	if len(got) != records {
		t.Fatalf("received %d records, want %d", len(got), records)
	}
	for i, off := range got {
		if off != int64(i) {
			t.Errorf("got[%d] = %d, want %d (out of order)", i, off, i)
		}
	}
}

func makeRecords(n int) []Record {
	recs := make([]Record, n)
	for i := range recs {
		recs[i] = Record{Offset: int64(i), Value: []byte{byte(i)}}
	}
	return recs
}
```

## Review

The rate limit is correct because it is an invariant of the bucket, not a timing accident: `Allow` refills from elapsed time capped at `burst` and spends one token, so the number of grants between any two instants is at most `burst + rate*elapsed`. `TestBucketBoundsRate` proves this on a fake clock with no flakiness, and `TestLiveRateBound` confirms the running consumer inherits the bound because every delivery passes through `Allow`. The lazy-refill design is what buys both the determinism and the absence of a refill goroutine; a ticker-based refill would regenerate in lumps, leak when abandoned, and force a real-time test.

Pause/resume is the coarse override on top. It flips an atomic the forward loop checks each iteration, so a paused consumer spends no tokens and delivers nothing within one `pollInterval`, while a single forward goroutine keeps delivery in offset order. The common trap is to start the consumer running and then race to pause it before the first deliveries leak out; constructing it `startPaused` removes that race and makes the pause window exact, which is why both the demo and the pause test use it.

## Resources

- [Token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket) -- the `burst + rate*elapsed` model this bucket implements.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) -- the Go standard extended library's production token-bucket limiter, with the same burst-plus-rate semantics.
- [`time.Now` and monotonic clocks](https://pkg.go.dev/time#hdr-Monotonic_Clocks) -- why elapsed-time math in the bucket is monotonic and safe across wall-clock adjustments.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../06-message-retention-compaction/00-concepts.md](../06-message-retention-compaction/00-concepts.md)
