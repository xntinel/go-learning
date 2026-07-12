# Exercise 2: Credit-Based Flow Control

A bounded prefetch channel limits how many records sit *waiting to be picked up*, but it says nothing about how many the application has already pulled out and not yet finished. Credit-based flow control bounds exactly that second window: the consumer advertises a fixed number of credits, a record may be delivered only by spending one, and a credit comes back only when the application acknowledges. The in-flight count therefore can never exceed the advertised budget, no matter how fast the upstream produces.

This module is fully self-contained: its own `go mod init`, its own minimal record and upstream, its own demo and `-race` test. Nothing here imports any other exercise.

## What you'll build

```text
credit.go            Record, CreditConsumer: NewCreditConsumer, Recv, Ack, InFlight, MaxInFlight, Close
cmd/
  demo/
    main.go          deliver under a 3-credit budget; show the 4th delivery blocks until an Ack
credit_test.go       deterministic bound (3 credits) + concurrent stress (bound holds under -race)
```

- Files: `credit.go`, `cmd/demo/main.go`, `credit_test.go`.
- Implement: `CreditConsumer` backed by a counting semaphore of tokens, with `Recv` (spend nothing; receive a delivered record), `Ack` (return one credit), and `InFlight`/`MaxInFlight` introspection.
- Test: a deterministic test that exactly three records are delivered before the fourth `Recv` blocks and one `Ack` releases exactly one more; a concurrent stress test asserting the in-flight high-water mark never exceeds the credit cap and every record is delivered once.
- Verify: `go test -race ./... && go run ./cmd/demo`

### Credits as a counting semaphore

The whole protocol reduces to one data structure: a buffered channel of empty structs used as a counting semaphore. Create it with capacity `MaxCredits` and pre-fill it with `MaxCredits` tokens, so the bucket starts with a full credit balance. Spending a credit is a receive (`<-c.tokens`); returning one is a send (`c.tokens <- struct{}{}`). Because the channel can hold at most `MaxCredits` tokens, the number of tokens taken-but-not-returned -- which is precisely the in-flight window -- is bounded by the channel capacity for free, with no separate counter to keep consistent.

The fetcher loop is then almost trivial: for each record, first acquire a credit (this is the blocking point that enforces backpressure -- if every credit is in flight the fetcher simply waits on `<-c.tokens`), then offer the record downstream on an *unbuffered* delivery channel so that "delivered" means "actually received by the application," not "sitting in a buffer." The `inFlight` atomic is incremented immediately after a credit is acquired and decremented in `Ack` just before the credit is returned, so it tracks the semaphore exactly; `maxInFlight` records its high-water mark with a compare-and-swap loop so a test can assert the bound was never exceeded.

The application contract is one rule: call `Ack` exactly once for every record `Recv` returns. Each `Ack` returns one token, so an extra `Ack` would inflate the balance above `MaxCredits` and silently break the bound -- which is the subject of one of this lesson's common mistakes. `Close` is made safe to call once via `sync.Once` and unblocks any pending `Recv` or `Ack` through a `closed` channel.

Create `credit.go`:

```go
package credit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Record is a single message delivered to the application.
type Record struct {
	Offset int64
	Value  []byte
}

// ErrClosed is returned by Recv after the consumer has been closed and its
// upstream is fully drained.
var ErrClosed = errors.New("credit: consumer closed")

// CreditConsumer delivers records from a fixed upstream log under a
// credit-based flow-control protocol. The consumer advertises exactly
// MaxCredits credits to the fetcher: a record may be delivered only by
// spending one credit, and a credit is returned only when the application
// calls Ack. The number of records delivered-but-not-yet-acked (the in-flight
// window) therefore never exceeds MaxCredits, regardless of how fast the
// upstream produces. This is the same model AMQP/RabbitMQ call "prefetch" and
// the Reactive Streams specification calls "demand".
//
// Contract: the application must call Ack exactly once for every record
// returned by Recv. Acking more than that breaks the credit accounting.
type CreditConsumer struct {
	records []Record

	tokens   chan struct{} // available credits; capacity == MaxCredits
	delivery chan Record   // unbuffered: a record here has been delivered
	done     chan struct{} // closed when the fetcher has offered every record

	inFlight    atomic.Int64 // credits spent but not yet returned by Ack
	maxInFlight atomic.Int64 // high-water mark of inFlight, for assertions

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// NewCreditConsumer builds a consumer over a copy of records that will never
// allow more than maxCredits records to be in flight at once. maxCredits must
// be at least 1; a smaller value is clamped to 1.
func NewCreditConsumer(records []Record, maxCredits int) *CreditConsumer {
	if maxCredits < 1 {
		maxCredits = 1
	}
	recs := make([]Record, len(records))
	copy(recs, records)

	c := &CreditConsumer{
		records:  recs,
		tokens:   make(chan struct{}, maxCredits),
		delivery: make(chan Record),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
	}
	// Seed the full credit balance: maxCredits records may be delivered before
	// the first Ack is required.
	for range maxCredits {
		c.tokens <- struct{}{}
	}

	c.wg.Add(1)
	go c.fetch()
	return c
}

// fetch offers each record downstream, spending one credit per record. When no
// credit is available it blocks on the token channel -- that block is the
// backpressure: the fetcher cannot run ahead of the application's Acks.
func (c *CreditConsumer) fetch() {
	defer c.wg.Done()
	defer close(c.done)

	for _, r := range c.records {
		// Acquire one credit (block until the application returns one via Ack).
		select {
		case <-c.tokens:
		case <-c.closed:
			return
		}

		// A credit was spent: this record is now in flight.
		n := c.inFlight.Add(1)
		for {
			hi := c.maxInFlight.Load()
			if n <= hi || c.maxInFlight.CompareAndSwap(hi, n) {
				break
			}
		}

		select {
		case c.delivery <- r:
		case <-c.closed:
			return
		}
	}
}

// Recv returns the next delivered record. It blocks until a record is
// available, ctx is cancelled, or the consumer is closed and drained. The
// boolean is false when no record will ever arrive (drained or closed).
func (c *CreditConsumer) Recv(ctx context.Context) (Record, bool) {
	select {
	case r := <-c.delivery:
		return r, true
	case <-ctx.Done():
		return Record{}, false
	case <-c.closed:
		return Record{}, false
	case <-c.done:
		// Upstream exhausted: drain any record the fetcher offered before it
		// finished, then report end-of-stream.
		select {
		case r := <-c.delivery:
			return r, true
		default:
			return Record{}, false
		}
	}
}

// Ack returns one credit to the consumer, allowing the fetcher to deliver one
// more record. Call it exactly once per record returned by Recv.
func (c *CreditConsumer) Ack() {
	c.inFlight.Add(-1)
	select {
	case c.tokens <- struct{}{}:
	case <-c.closed:
	}
}

// InFlight reports the number of records currently delivered but not acked.
func (c *CreditConsumer) InFlight() int64 { return c.inFlight.Load() }

// MaxInFlight reports the high-water mark of InFlight observed so far. Under
// the credit protocol this value never exceeds MaxCredits.
func (c *CreditConsumer) MaxInFlight() int64 { return c.maxInFlight.Load() }

// Close stops the fetcher and unblocks any pending Recv/Ack calls.
func (c *CreditConsumer) Close() {
	c.closeOnce.Do(func() { close(c.closed) })
	c.wg.Wait()
}
```

### The runnable demo

The demo makes the bound visible. It advertises three credits over eight records, receives three without acking (exhausting the budget), then shows a fourth `Recv` under a 100 ms context times out because no credit is available -- that timeout *is* the backpressure. It then returns one credit and watches exactly one more record appear, drains the rest one credit at a time, and prints the high-water mark, which is the credit cap.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/credit"
)

func main() {
	records := make([]credit.Record, 8)
	for i := range records {
		records[i] = credit.Record{Offset: int64(i), Value: []byte(fmt.Sprintf("msg-%d", i))}
	}

	c := credit.NewCreditConsumer(records, 3)
	defer c.Close()

	ctx := context.Background()
	fmt.Println("records=8 credits=3")

	// Receive up to the full credit balance without acking.
	for range 3 {
		r, _ := c.Recv(ctx)
		fmt.Printf("recv offset=%d\n", r.Offset)
	}
	fmt.Printf("in-flight=%d (credits exhausted)\n", c.InFlight())

	// With every credit spent, a further Recv must block until a credit is
	// returned. Prove it with a bounded context.
	ctxTimeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	if _, ok := c.Recv(ctxTimeout); !ok {
		fmt.Println("4th recv timed out: backpressure holds")
	}
	cancel()

	// Return one credit; exactly one more record is delivered.
	c.Ack()
	r, _ := c.Recv(ctx)
	fmt.Printf("ack one -> recv offset=%d\n", r.Offset)

	// Drain the rest, returning one credit per delivery.
	for range 4 {
		c.Ack()
		r, _ := c.Recv(ctx)
		fmt.Printf("recv offset=%d\n", r.Offset)
	}

	fmt.Println("all 8 records consumed")
	fmt.Printf("max in-flight observed: %d\n", c.MaxInFlight())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
records=8 credits=3
recv offset=0
recv offset=1
recv offset=2
in-flight=3 (credits exhausted)
4th recv timed out: backpressure holds
ack one -> recv offset=3
recv offset=4
recv offset=5
recv offset=6
recv offset=7
all 8 records consumed
max in-flight observed: 3
```

### Tests

`TestCreditBoundsInFlight` is deterministic: it receives three records without acking, asserts `InFlight` is exactly 3, asserts a fourth `Recv` under a short context fails because the budget is spent, then acks once and asserts exactly one more record (offset 3) is delivered. `TestCreditConcurrentStress` is the `-race` proof: six worker goroutines each receive, hold the record for a millisecond (so several are in flight at once and the window is pushed toward the cap), then ack, while the test collects every offset exactly once and asserts `MaxInFlight` never exceeded the four-credit cap. The bound is a property of the semaphore, so it holds no matter how the scheduler interleaves the workers.

Create `credit_test.go`:

```go
package credit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func makeRecords(n int) []Record {
	recs := make([]Record, n)
	for i := range recs {
		recs[i] = Record{Offset: int64(i), Value: []byte{byte(i)}}
	}
	return recs
}

// TestCreditBoundsInFlight deterministically shows the credit window: with
// three credits the consumer delivers exactly three records before the fourth
// Recv blocks, and a single Ack releases exactly one more delivery.
func TestCreditBoundsInFlight(t *testing.T) {
	t.Parallel()
	c := NewCreditConsumer(makeRecords(8), 3)
	defer c.Close()

	ctx := context.Background()

	// Receive up to the full credit balance without acking.
	for i := range 3 {
		r, ok := c.Recv(ctx)
		if !ok {
			t.Fatalf("Recv %d: not ok", i)
		}
		if r.Offset != int64(i) {
			t.Fatalf("Recv %d: offset = %d, want %d", i, r.Offset, i)
		}
	}
	if got := c.InFlight(); got != 3 {
		t.Fatalf("InFlight = %d, want 3 (all credits spent)", got)
	}

	// The fourth Recv must block: every credit is spent and none returned.
	ctx4, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, ok := c.Recv(ctx4); ok {
		t.Fatal("fourth Recv returned a record while no credit was available")
	}

	// Returning one credit releases exactly one more delivery.
	c.Ack()
	r, ok := c.Recv(ctx)
	if !ok || r.Offset != 3 {
		t.Fatalf("after Ack: Recv = (%d,%v), want offset 3", r.Offset, ok)
	}

	if got := c.MaxInFlight(); got != 3 {
		t.Errorf("MaxInFlight = %d, want 3", got)
	}
}

// TestCreditConcurrentStress runs many workers acking under load and asserts
// the in-flight window never exceeds the advertised credit count, and that
// every record is delivered exactly once. Run under -race.
func TestCreditConcurrentStress(t *testing.T) {
	t.Parallel()
	const (
		total      = 200
		maxCredits = 4
		workers    = 6
	)
	c := NewCreditConsumer(makeRecords(total), maxCredits)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results := make(chan int64, total)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				r, ok := c.Recv(ctx)
				if !ok {
					return
				}
				results <- r.Offset
				// Hold the record briefly so several are in flight at once,
				// which pushes the in-flight window toward the credit cap.
				time.Sleep(time.Millisecond)
				c.Ack()
			}
		}()
	}

	seen := make(map[int64]bool, total)
	for range total {
		select {
		case off := <-results:
			if seen[off] {
				t.Fatalf("offset %d delivered twice", off)
			}
			seen[off] = true
		case <-ctx.Done():
			t.Fatalf("timed out after %d of %d records", len(seen), total)
		}
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("delivered %d distinct records, want %d", len(seen), total)
	}
	if hi := c.MaxInFlight(); hi > maxCredits {
		t.Errorf("MaxInFlight = %d, exceeds credit cap %d: bound violated", hi, maxCredits)
	}
}
```

## Review

The protocol is correct when the in-flight window -- records delivered but not acked -- never exceeds `MaxCredits`. That is guaranteed structurally: the token channel holds at most `MaxCredits` tokens, the fetcher takes one before each delivery and blocks when none remain, and `Ack` returns exactly one. `TestCreditBoundsInFlight` pins the exact behavior at the boundary (three out, the fourth blocks, one ack releases one), and `TestCreditConcurrentStress` confirms the high-water mark stays at or below the cap under racing workers. The delivery channel is deliberately unbuffered so that a record counts as in flight only once the application truly has it, not while it waits in a buffer.

The one rule the caller must honor is one `Ack` per delivered record. Ack twice and the balance climbs above the advertised credit, breaking the bound; never ack and the fetcher stalls forever once the budget is spent. This is the same discipline AMQP's prefetch count and Reactive Streams' `request(n)` demand impose: the producer may run exactly as far ahead as the consumer has explicitly permitted, and not one record further.

## Resources

- [Reactive Streams specification](https://www.reactive-streams.org/) -- the formal back-pressure protocol where a subscriber's `request(n)` is exactly the credit advertised here.
- [RabbitMQ consumer prefetch](https://www.rabbitmq.com/docs/consumer-prefetch) -- the `basic.qos` prefetch count, a production credit-based flow-control limit on unacknowledged messages.
- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) -- the standard weighted-semaphore package; the channel-of-tokens here is the unweighted special case.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-token-bucket-rate-limited-delivery.md](03-token-bucket-rate-limited-delivery.md)
