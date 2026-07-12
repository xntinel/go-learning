# Exercise 7: Rate-Limited Source (Token Bucket)

A bursty origin can overwhelm a downstream that has a fixed capacity. This `RateLimitedSource` shapes its own output with a token bucket: it never emits faster than a configured rate, but lets a short burst pass through immediately. It is the producer-side backpressure complement to the buffer-side strategies of the earlier sources.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, bundles the shared `Record`, `Metrics`, and `Source` definitions it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
rate-limited-source/
  go.mod
  source.go                    Record, Metrics, Source, ErrSourceClosed
  rate_limited_source.go       token bucket: ticker refill, burst, per-record token
  rate_limited_source_test.go  rate is capped, burst passes immediately
  cmd/demo/main.go             emit a fixed set at 100/sec
```

- Files: `source.go`, `rate_limited_source.go`, `rate_limited_source_test.go`, `cmd/demo/main.go`.
- Implement: `RateLimitedSource` with a `perSecond` refill rate and a `burst` capacity, built on a token-bucket channel fed by a `time.Ticker`.
- Test: emitting N records with burst 1 takes at least (N-1) intervals; a burst of B records drains before the first refill tick.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/01-source-connectors/07-rate-limited-source/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/01-source-connectors/07-rate-limited-source
```

### The shared vocabulary

`source.go` bundles the usual `Record`, `Metrics`, and `Source`. Rate limiting is purely about *when* records leave, not what they contain, so nothing here changes the record shape.

Create `source.go`:

```go
package ratelimitedsource

import (
	"context"
	"errors"
	"time"
)

// Record is the atomic unit flowing through the pipeline.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Source    string
	Metadata  map[string]string
}

// Metrics is a point-in-time snapshot of a source's counters.
type Metrics struct {
	RecordsEmitted int64
	BytesRead      int64
	ErrorsTotal    int64
	BacklogSize    int64
}

// ErrSourceClosed is returned by Close when the source was never opened.
var ErrSourceClosed = errors.New("ratelimitedsource: source not open")

// Source is the common interface for all data origins.
type Source interface {
	Open(ctx context.Context) (<-chan Record, <-chan error)
	Close() error
	Metrics() Metrics
}
```

### The token bucket as a channel plus a ticker

The token bucket is the standard rate-shaping algorithm, and in Go it maps almost directly onto language primitives. The bucket is a buffered channel of capacity `burst`, pre-filled with `burst` tokens. A refiller goroutine adds one token every `1/perSecond` via a `time.Ticker`, using a non-blocking send so the bucket never exceeds its capacity — a token that arrives when the bucket is full is simply dropped, which is what caps the long-run rate. The emitter goroutine, before sending each record, must first take a token: `select { case <-tokens: case <-ctx.Done(): return }`. When tokens are available (a burst) records flow with no delay; once the initial burst is spent, each subsequent record waits for the ticker to deposit the next token, so the sustained rate is exactly `perSecond`.

Two details make it robust. First, the emitter calls `cancel()` after the last value, which stops the refiller and lets the closer goroutine close the channels — so a bounded set of values terminates cleanly. Second, like the replayable source, `Close` waits on a per-open `done` channel rather than the `WaitGroup` directly, so the source can be reopened safely. The production-grade form of this exact design is `golang.org/x/time/rate.Limiter`, whose `Wait`/`Allow`/`Reserve` methods implement the same token bucket with a richer API; building it by hand here makes the mechanism concrete and keeps the module dependency-free.

The guarantee a rate limiter offers is a *lower* bound on elapsed time, and that is what makes it testable without flakiness: a `time.Ticker` never fires early, so "emitting N records with burst 1 takes at least (N-1) intervals" holds deterministically, where an upper-bound assertion would be at the mercy of the scheduler.

Create `rate_limited_source.go`:

```go
package ratelimitedsource

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimitedSource emits a fixed set of values but never faster than perSecond
// records per second, smoothing bursty input into a steady stream. It implements
// a token bucket: burst tokens are available immediately, and the bucket refills
// one token every 1/perSecond. A record may be emitted only when a token is
// available, so the long-run rate is capped at perSecond while short bursts up to
// burst pass through without waiting.
type RateLimitedSource struct {
	values     [][]byte
	perSecond  int
	burst      int
	bufferSize int

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	done    chan struct{}
	records chan Record
	errs    chan error

	emitted atomic.Int64
	bytes   atomic.Int64
}

// NewRateLimitedSource caps emission at perSecond records per second with an
// initial burst allowance of burst tokens.
func NewRateLimitedSource(values [][]byte, perSecond, burst, bufferSize int) *RateLimitedSource {
	if burst < 1 {
		burst = 1
	}
	return &RateLimitedSource{
		values:     values,
		perSecond:  perSecond,
		burst:      burst,
		bufferSize: bufferSize,
	}
}

func (rs *RateLimitedSource) Open(ctx context.Context) (<-chan Record, <-chan error) {
	inner, cancel := context.WithCancel(ctx)
	rs.cancel = cancel
	records := make(chan Record, rs.bufferSize)
	errs := make(chan error, 4)
	rs.records = records
	rs.errs = errs

	// The token bucket: a buffered channel of capacity burst, pre-filled.
	tokens := make(chan struct{}, rs.burst)
	for i := 0; i < rs.burst; i++ {
		tokens <- struct{}{}
	}
	interval := time.Second / time.Duration(rs.perSecond)

	// Refiller: add one token every interval, dropping it when the bucket is
	// full so the bucket never exceeds burst.
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-inner.Done():
				return
			case <-ticker.C:
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Emitter: take a token per record, then send.
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		for _, v := range rs.values {
			select {
			case <-tokens:
			case <-inner.Done():
				return
			}
			rec := Record{
				Value:     append([]byte(nil), v...),
				Timestamp: time.Now().UTC(),
				Source:    "ratelimited",
			}
			rs.bytes.Add(int64(len(v)))
			select {
			case records <- rec:
				rs.emitted.Add(1)
			case <-inner.Done():
				return
			}
		}
		// All values emitted: stop the refiller and let the channels close.
		cancel()
	}()

	rs.done = make(chan struct{})
	done := rs.done
	go func() {
		rs.wg.Wait()
		close(records)
		close(errs)
		close(done)
	}()

	return records, errs
}

func (rs *RateLimitedSource) Close() error {
	if rs.cancel == nil {
		return ErrSourceClosed
	}
	rs.cancel()
	<-rs.done
	rs.cancel = nil
	return nil
}

func (rs *RateLimitedSource) Metrics() Metrics {
	return Metrics{
		RecordsEmitted: rs.emitted.Load(),
		BytesRead:      rs.bytes.Load(),
	}
}

var _ Source = (*RateLimitedSource)(nil)
```

### The runnable demo

The demo emits five values at 100 records/second with a burst of 1, so they leave roughly 10 ms apart. The output is the sequence of values and the final count; the rate shows up in the timing, not the text.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	rl "example.com/rate-limited-source"
)

func main() {
	values := [][]byte{
		[]byte("event-0"), []byte("event-1"), []byte("event-2"),
		[]byte("event-3"), []byte("event-4"),
	}

	// 100 records/sec, burst of 1: a steady stream, one record per ~10ms.
	rs := rl.NewRateLimitedSource(values, 100, 1, 16)
	recs, _ := rs.Open(context.Background())

	for r := range recs {
		fmt.Printf("emitted: %s\n", r.Value)
	}
	fmt.Printf("total emitted=%d\n", rs.Metrics().RecordsEmitted)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
emitted: event-0
emitted: event-1
emitted: event-2
emitted: event-3
emitted: event-4
total emitted=5
```

### Tests

`TestRateIsCapped` emits six records at 200/sec with burst 1 and asserts the total elapsed time is at least about five intervals — the lower-bound guarantee a ticker makes reliable. `TestBurstPassesImmediately` configures a slow 2/sec refill but a burst of 4 and asserts all four records drain in well under a single 500 ms tick, proving the burst allowance bypasses the rate until it is spent. `TestCloseBeforeOpen` asserts the sentinel.

Create `rate_limited_source_test.go`:

```go
package ratelimitedsource

import (
	"context"
	"testing"
	"time"
)

func collect(ch <-chan Record, max int, timeout time.Duration) []Record {
	var out []Record
	deadline := time.After(timeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
			if len(out) >= max {
				return out
			}
		case <-deadline:
			return out
		}
	}
}

// TestRateIsCapped verifies the source never emits faster than the configured
// rate. With burst 1 and N values, emitting all of them must take at least
// (N-1) intervals. This is a lower-bound assertion: a ticker never fires early,
// so it is not timing-flaky.
func TestRateIsCapped(t *testing.T) {
	t.Parallel()

	const perSecond = 200 // 5ms per token
	const n = 6
	values := make([][]byte, n)
	for i := range values {
		values[i] = []byte{byte('a' + i)}
	}
	rs := NewRateLimitedSource(values, perSecond, 1, 16)

	start := time.Now()
	recs, _ := rs.Open(context.Background())
	got := collect(recs, n, 5*time.Second)
	elapsed := time.Since(start)

	if len(got) != n {
		t.Fatalf("got %d records, want %d", len(got), n)
	}
	interval := time.Second / perSecond
	min := time.Duration(n-1) * interval
	// Allow a small slack below the theoretical bound for scheduling jitter.
	if elapsed < min-interval {
		t.Errorf("emitted %d records in %v, want at least ~%v", n, elapsed, min)
	}
	rs.Close()
}

// TestBurstPassesImmediately verifies that up to burst records are available
// without waiting for the bucket to refill.
func TestBurstPassesImmediately(t *testing.T) {
	t.Parallel()

	const perSecond = 2 // 500ms per token: slow refill
	const burst = 4
	values := make([][]byte, burst)
	for i := range values {
		values[i] = []byte("x")
	}
	rs := NewRateLimitedSource(values, perSecond, burst, 16)

	start := time.Now()
	recs, _ := rs.Open(context.Background())
	got := collect(recs, burst, 2*time.Second)
	elapsed := time.Since(start)

	if len(got) != burst {
		t.Fatalf("got %d records, want %d", len(got), burst)
	}
	// The whole burst should drain well before a single 500ms refill tick.
	if elapsed > 300*time.Millisecond {
		t.Errorf("burst of %d took %v, want < 300ms", burst, elapsed)
	}
	rs.Close()
}

// TestCloseBeforeOpen verifies the sentinel.
func TestCloseBeforeOpen(t *testing.T) {
	t.Parallel()
	rs := NewRateLimitedSource(nil, 10, 1, 1)
	if err := rs.Close(); err != ErrSourceClosed {
		t.Errorf("Close = %v, want %v", err, ErrSourceClosed)
	}
}
```

## Review

The source is correct when the sustained rate is capped at `perSecond` while an initial burst of up to `burst` records passes without waiting. Confirm the bucket is pre-filled to `burst`, the refiller uses a non-blocking send so the bucket never overfills, and every token acquire and record send has a `ctx.Done()` arm so `Close` is prompt. Test the *lower* bound on time, never the upper, since only the lower bound is deterministic. The common mistakes are blocking the refiller's send (which lets the bucket grow past `burst` and defeats the cap), busy-waiting instead of selecting on the token channel, and writing a flaky upper-bound timing assertion. The capped-rate and burst tests under `-race` confirm the shaping and the goroutine safety.

## Resources

- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — the production token-bucket limiter (`Limiter`, `Wait`, `Allow`, `Reserve`) this module mirrors by hand.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the periodic refill source, and why a ticker never fires early (the basis of the lower-bound test).
- [Token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket) — the rate, burst, and refill semantics this implementation follows.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-replayable-source.md](06-replayable-source.md) | Next: [../02-operators-map-filter-flatmap/00-concepts.md](../02-operators-map-filter-flatmap/00-concepts.md)
