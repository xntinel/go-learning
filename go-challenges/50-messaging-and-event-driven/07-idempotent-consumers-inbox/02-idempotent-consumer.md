# Exercise 2: Idempotent Consumer Middleware

This is the on-the-job piece: a generic middleware that wraps a business handler
and makes at-least-once delivery safe without the handler knowing anything about
dedup. It claims the message id and applies the side effect in one atomic step, and
returns the *cached* result on a redelivery instead of re-running the handler.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
consumer/                  independent module: example.com/consumer
  go.mod                   go 1.26
  consumer.go              Message[T], Result, InboxStore, MemStore, Consumer[T]; ErrHandler
  cmd/
    demo/
      main.go              runnable demo: first delivery vs cached redelivery
  consumer_test.go         exactly-once (-race), retry-safe, distinct, Example
```

- Files: `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`.
- Implement: a generic `Consumer[T any]` over an `InboxStore` interface whose `RecordAndApply` atomically claims a key and runs `apply` only on first sighting, caches the `Result`, and returns the cached one on a duplicate; a `MemStore` implementing it with a `sync.Mutex`; a sentinel `ErrHandler` wrapping handler failures with `%w`.
- Test: deliver one message from M concurrent goroutines and assert the side effect ran once and every caller saw the same `Result`; a handler that fails the first time leaves the id un-recorded so a later delivery succeeds; N distinct ids each run once; the error path is asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/50-messaging-and-event-driven/07-idempotent-consumers-inbox/02-idempotent-consumer/cmd/demo
cd go-solutions/50-messaging-and-event-driven/07-idempotent-consumers-inbox/02-idempotent-consumer
go mod edit -go=1.26
```

### The atomic mark-and-apply boundary

The invariant from the concepts file is that *marking* the id and *applying* the
side effect commit together. In a database that is one transaction; here it is one
closure executed under one lock. The `InboxStore` interface exposes exactly that
boundary:

```go
RecordAndApply(ctx, key, apply func() (Result, error)) (Result, bool, error)
```

`RecordAndApply` is the transaction. On a first sighting it runs `apply` (which
calls the business handler) and, *only if `apply` returns no error*, records the
key together with the result — then returns `(result, true, nil)`. If `apply`
returns an error it records nothing and returns `(zero, true, err)`: the rollback.
On a duplicate it never calls `apply` at all; it returns the cached result as
`(cached, false, nil)`. Because the whole thing runs under the store's lock, a
concurrent redelivery cannot squeeze between the claim and the apply — the second
goroutine blocks, then finds the key already done and gets the cached result.

Holding the lock across `apply` is the deliberate modelling choice. A real database
does not lock the whole table; it relies on the `UNIQUE` constraint so only per-row
contention serializes. The in-memory `MemStore` uses one mutex for simplicity,
which serializes all handlers — correct, but a real system would key the lock per
id or lean on the constraint. The lesson keeps the guarantee visible and honest
about the mapping.

### The consumer wraps the handler

`Consumer[T]` holds the store and a handler `func(context.Context, Message[T])
(Result, error)`. Its `Deliver` builds the composite key from the message's group
and id, then calls `RecordAndApply` with a closure that invokes the handler. A
handler error is wrapped with the package sentinel `ErrHandler` using `%w`, so a
caller can test `errors.Is(err, ErrHandler)` for "the handler failed" while still
unwrapping the original cause. Because `apply` returned an error, the store rolled
back and the id was not recorded: the broker's next redelivery re-invokes the
handler cleanly.

Create `consumer.go`:

```go
package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrHandler wraps any error returned by a business handler, so callers can
// distinguish a handler failure from a store failure with errors.Is.
var ErrHandler = errors.New("consumer: handler failed")

// Message is one delivery. Group is the consumer group; ID is the producer's
// stable message id. Together they form the dedup key.
type Message[T any] struct {
	ID    string
	Group string
	Body  T
}

// Result is the outcome the consumer caches and returns verbatim on a
// redelivery, so request/reply flows see the same response every time.
type Result struct {
	Response string
}

// InboxStore is the atomic mark-and-apply boundary: the interface a Consumer
// depends on. Implementations must run apply at most once per key and cache its
// Result, recording nothing when apply fails.
type InboxStore interface {
	// RecordAndApply claims key. On the first sighting it runs apply and, only
	// if apply succeeds, records key with the produced Result, returning
	// (result, true, nil); if apply fails it records nothing and returns
	// (zero, true, err). On a duplicate it returns (cachedResult, false, nil)
	// without calling apply.
	RecordAndApply(ctx context.Context, key string, apply func() (Result, error)) (Result, bool, error)
}

// MemStore is an in-memory InboxStore. A single mutex serializes claims and, by
// holding across apply, guarantees a concurrent redelivery of the same key sees
// the cached result instead of re-running the handler.
type MemStore struct {
	mu   sync.Mutex
	done map[string]Result
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{done: make(map[string]Result)}
}

// RecordAndApply implements InboxStore.
func (m *MemStore) RecordAndApply(ctx context.Context, key string, apply func() (Result, error)) (Result, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.done[key]; ok {
		return r, false, nil
	}
	r, err := apply()
	if err != nil {
		return Result{}, true, err // rollback: record nothing so a redelivery retries
	}
	m.done[key] = r
	return r, true, nil
}

// Len reports how many keys are recorded (test/inspection helper).
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.done)
}

// Consumer wraps a business handler with at-least-once-safe delivery.
type Consumer[T any] struct {
	store   InboxStore
	handler func(context.Context, Message[T]) (Result, error)
}

// New builds a Consumer over store, dispatching first-time deliveries to handler.
func New[T any](store InboxStore, handler func(context.Context, Message[T]) (Result, error)) *Consumer[T] {
	return &Consumer[T]{store: store, handler: handler}
}

// Deliver processes msg. On the first delivery it runs the handler and commits
// the inbox record and the result atomically; on a redelivery it returns the
// cached Result without re-running the handler. A handler error is wrapped with
// ErrHandler and leaves the id un-recorded so the broker can redeliver.
func (c *Consumer[T]) Deliver(ctx context.Context, msg Message[T]) (Result, error) {
	key := msg.Group + "\x00" + msg.ID
	res, _, err := c.store.RecordAndApply(ctx, key, func() (Result, error) {
		r, herr := c.handler(ctx, msg)
		if herr != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrHandler, herr)
		}
		return r, nil
	})
	return res, err
}
```

### The runnable demo

The demo delivers the same message twice. A side-effect counter proves the handler
runs once; the second delivery returns the cached response.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/consumer"
)

func main() {
	var sends int
	handler := func(ctx context.Context, m consumer.Message[string]) (consumer.Result, error) {
		sends++ // the external side effect: "send a welcome email"
		return consumer.Result{Response: "emailed " + m.Body}, nil
	}

	c := consumer.New(consumer.NewMemStore(), handler)
	msg := consumer.Message[string]{ID: "evt-1", Group: "welcome", Body: "alice"}

	ctx := context.Background()
	r1, _ := c.Deliver(ctx, msg)
	fmt.Printf("first delivery: %s\n", r1.Response)

	r2, _ := c.Deliver(ctx, msg) // redelivery
	fmt.Printf("redelivery: %s\n", r2.Response)

	fmt.Printf("handler ran %d time(s)\n", sends)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first delivery: emailed alice
redelivery: emailed alice
handler ran 1 time(s)
```

### Tests

`TestExactlyOnceUnderConcurrency` is the headline: M goroutines deliver one
message, the handler increments an `atomic.Int64`, and the test asserts the counter
is one and every caller observed the same `Result`. `TestRetryLeavesIdUnrecorded`
gives a handler that fails on its first call and succeeds after; the first
`Deliver` returns an `ErrHandler`-wrapped error and records nothing, so a second
`Deliver` re-runs the handler and succeeds. `TestDistinctMessages` delivers N unique
ids and asserts each ran once. The error assertion uses `errors.Is` against both
`ErrHandler` and the underlying business sentinel, proving the double-`%w` chain.

Create `consumer_test.go`:

```go
package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var errBusiness = errors.New("charge declined")

func TestExactlyOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	var sideEffects atomic.Int64
	handler := func(ctx context.Context, m Message[int]) (Result, error) {
		sideEffects.Add(1)
		return Result{Response: fmt.Sprintf("applied-%d", m.Body)}, nil
	}
	c := New(NewMemStore(), handler)
	msg := Message[int]{ID: "evt-1", Group: "billing", Body: 7}

	const m = 100
	results := make([]Result, m)
	var wg sync.WaitGroup
	for i := range m {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Deliver(t.Context(), msg)
			if err != nil {
				t.Errorf("Deliver: %v", err)
			}
			results[i] = r
		}()
	}
	wg.Wait()

	if got := sideEffects.Load(); got != 1 {
		t.Fatalf("side effect ran %d times; want 1", got)
	}
	for i, r := range results {
		if r != (Result{Response: "applied-7"}) {
			t.Fatalf("caller %d saw %+v; want cached applied-7", i, r)
		}
	}
}

func TestRetryLeavesIdUnrecorded(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	handler := func(ctx context.Context, m Message[int]) (Result, error) {
		if attempts.Add(1) == 1 {
			return Result{}, errBusiness // transient first failure
		}
		return Result{Response: "ok"}, nil
	}
	store := NewMemStore()
	c := New(store, handler)
	msg := Message[int]{ID: "evt-9", Group: "billing", Body: 1}

	_, err := c.Deliver(t.Context(), msg)
	if !errors.Is(err, ErrHandler) || !errors.Is(err, errBusiness) {
		t.Fatalf("first Deliver err = %v; want ErrHandler wrapping errBusiness", err)
	}
	if store.Len() != 0 {
		t.Fatalf("id recorded after failure; want un-recorded so broker retries")
	}

	r, err := c.Deliver(t.Context(), msg) // redelivery retries cleanly
	if err != nil {
		t.Fatalf("retry Deliver err = %v; want nil", err)
	}
	if r.Response != "ok" {
		t.Fatalf("retry result = %q; want ok", r.Response)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("handler ran %d times; want 2 (failed then retried)", got)
	}
}

func TestDistinctMessages(t *testing.T) {
	t.Parallel()
	var sideEffects atomic.Int64
	handler := func(ctx context.Context, m Message[int]) (Result, error) {
		sideEffects.Add(1)
		return Result{Response: "done"}, nil
	}
	c := New(NewMemStore(), handler)

	const n = 50
	for i := range n {
		msg := Message[int]{ID: fmt.Sprintf("evt-%d", i), Group: "billing", Body: i}
		if _, err := c.Deliver(t.Context(), msg); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}
	if got := sideEffects.Load(); got != n {
		t.Fatalf("side effects = %d; want %d (each id once)", got, n)
	}
}

func Example() {
	var runs int
	handler := func(ctx context.Context, m Message[string]) (Result, error) {
		runs++
		return Result{Response: "charged " + m.Body}, nil
	}
	c := New(NewMemStore(), handler)
	msg := Message[string]{ID: "evt-1", Group: "billing", Body: "alice"}

	r1, _ := c.Deliver(context.Background(), msg)
	r2, _ := c.Deliver(context.Background(), msg) // duplicate returns cached result
	fmt.Println(r1.Response, r2.Response, runs)
	// Output: charged alice charged alice 1
}

// Your turn: add a test where the handler returns ctx.Err() under a cancelled
// context and assert the id is left un-recorded (store.Len() == 0) so a later
// delivery with a live context can still succeed.
```

## Review

The consumer is correct when the side effect runs exactly once per key across any
number of redeliveries, concurrent or serial, and when a failure leaves no trace so
the broker can retry. `TestExactlyOnceUnderConcurrency` proves the first half: 100
concurrent deliveries, one side effect, and identical cached results — if
`RecordAndApply` released the lock between the claim and the apply, the counter
would exceed one under `-race`. `TestRetryLeavesIdUnrecorded` proves the second
half: a failed handler records nothing, so the redelivery re-runs it.

The mistakes to avoid: do not record the id before the handler succeeds — that
swallows a retryable message permanently, which is why `RecordAndApply` records
only after `apply` returns nil. Do not re-run the handler on a duplicate "because it
is idempotent" — return the cached `Result`, or a request/reply caller can observe
changed state or a second external call. And keep the mark and apply in one
critical section; splitting them reopens the double-apply gap. The single mutex here
serializes all handlers for clarity — a production store would rely on a per-row
`UNIQUE` constraint instead. Run `go test -race` to confirm the boundary holds.

## Resources

- [Idempotent Consumer pattern - microservices.io](https://microservices.io/patterns/communication-style/idempotent-consumer.html) — the mark-and-apply-in-one-transaction invariant.
- [Transactional Outbox pattern - microservices.io](https://microservices.io/patterns/data/transactional-outbox.html) — the producer-side companion that can itself double-publish.
- [errors package](https://pkg.go.dev/errors) — `errors.Is` and wrapping with `%w`, including multiple `%w` in one `fmt.Errorf`.

---

Back to [01-inbox-store.md](01-inbox-store.md) | Next: [03-idempotency-keys.md](03-idempotency-keys.md)
