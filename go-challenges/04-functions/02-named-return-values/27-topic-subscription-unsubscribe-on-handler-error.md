# Exercise 27: Pub/Sub Unsubscribe Guard on Handler Failure

A retry-driven delivery loop resubscribes and redelivers whenever a handler
fails, on the assumption that the failure was transient. But if the handler
itself is what's broken — a poison message it can never process — leaving
the subscription live means every retry redelivers to the same broken
handler, processing (and re-failing) the same message over and over. This
exercise builds a `Deliver` that uses a deferred closure keyed on the named
`err` result to drop the subscription the moment a handler fails, so a retry
sees "not subscribed" instead of delivering twice.

**Nivel: Intermedio** — validacion rapida (tres pruebas cortas).

## What you'll build

```text
pubsub/                     independent module: example.com/pubsub
  go.mod
  pubsub.go                 Broker; Subscribe/Unsubscribe; Deliver (deferred unsubscribe on error)
  cmd/demo/
    main.go                 runnable demo: a healthy delivery, a poison one, then a retry
  pubsub_test.go             keeps subscription on success, drops it on handler error, blocks the retry
```

- Files: `pubsub.go`, `cmd/demo/main.go`, `pubsub_test.go`.
- Implement: `(*Broker) Deliver(id int, msg string) (delivered bool, err error)` whose deferred closure calls `Unsubscribe(id)` whenever the named `err` is non-nil.
- Test: a handler that succeeds keeps its subscription; one that fails loses it; redelivering to the same id afterward reports "not found" instead of calling the handler again.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/27-topic-subscription-unsubscribe-on-handler-error/cmd/demo
cd go-solutions/04-functions/02-named-return-values/27-topic-subscription-unsubscribe-on-handler-error
go mod edit -go=1.24
```

### One failure, one unsubscribe, driven by the named result

```go
defer func() {
    if err != nil {
        b.Unsubscribe(id)
    }
}()

if err = h(msg); err != nil {
    return false, err
}
return true, nil
```

`Deliver` looks up the handler under the broker's mutex, then registers a
deferred closure before ever calling it. Because `err` is a named result,
that closure runs after `h(msg)`'s return value has been copied into `err`
— regardless of which of `Deliver`'s two return statements produced it — and
can act on the outcome uniformly: non-nil `err` means drop the subscription,
nil means leave it alone. A caller retrying with the same `id` after a
failure gets `"deliver: subscription %d not found"` instead of a second call
into the same broken handler, which is exactly the duplicate-processing bug
this guard exists to prevent.

Create `pubsub.go`:

```go
package pubsub

import (
	"fmt"
	"sync"
)

// Handler processes one delivered message.
type Handler func(msg string) error

// Broker is a minimal in-memory pub/sub topic: subscribers register a
// Handler and receive an id they can use to unsubscribe.
type Broker struct {
	mu   sync.Mutex
	subs map[int]Handler
	next int
}

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[int]Handler)}
}

// Subscribe registers h and returns the subscription id.
func (b *Broker) Subscribe(h Handler) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := b.next
	b.subs[id] = h
	return id
}

// Unsubscribe removes a subscription. It is safe to call on an id that is
// already gone.
func (b *Broker) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}

// Subscribed reports whether id still has a live subscription.
func (b *Broker) Subscribed(id int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.subs[id]
	return ok
}

// Deliver hands msg to the subscriber registered under id.
//
// delivered and err are named results: a single deferred closure checks err
// once Deliver is about to return and, whenever the handler failed,
// unsubscribes id from the broker. A retry driver that resubscribes and
// redelivers on failure would otherwise redeliver to a handler that already
// saw (and rejected) the message, processing it twice; removing the
// subscription on the first failure prevents that duplicate delivery.
func (b *Broker) Deliver(id int, msg string) (delivered bool, err error) {
	b.mu.Lock()
	h, ok := b.subs[id]
	b.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("deliver: subscription %d not found", id)
	}

	defer func() {
		if err != nil {
			b.Unsubscribe(id)
		}
	}()

	if err = h(msg); err != nil {
		return false, err
	}
	return true, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/pubsub"
)

func main() {
	broker := pubsub.NewBroker()

	id := broker.Subscribe(func(msg string) error {
		if msg == "poison" {
			return errors.New("cannot process poison message")
		}
		fmt.Println("handled:", msg)
		return nil
	})

	delivered, err := broker.Deliver(id, "hello")
	fmt.Printf("deliver hello: delivered=%v err=%v subscribed=%v\n", delivered, err, broker.Subscribed(id))

	delivered, err = broker.Deliver(id, "poison")
	fmt.Printf("deliver poison: delivered=%v err=%v subscribed=%v\n", delivered, err, broker.Subscribed(id))

	// A naive retry loop would redeliver here; the broker already dropped
	// the subscription, so it reports "not found" instead of processing
	// the poison message twice.
	delivered, err = broker.Deliver(id, "poison")
	fmt.Printf("retry poison: delivered=%v err=%v\n", delivered, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handled: hello
deliver hello: delivered=true err=<nil> subscribed=true
deliver poison: delivered=false err=cannot process poison message subscribed=false
retry poison: delivered=false err=deliver: subscription 1 not found
```

### Tests

Create `pubsub_test.go`:

```go
package pubsub

import (
	"errors"
	"testing"
)

func TestDeliverKeepsSubscriptionOnSuccess(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	var got string
	id := b.Subscribe(func(msg string) error {
		got = msg
		return nil
	})

	delivered, err := b.Deliver(id, "hello")
	if err != nil {
		t.Fatalf("Deliver: unexpected error: %v", err)
	}
	if !delivered {
		t.Fatal("delivered = false, want true on success")
	}
	if got != "hello" {
		t.Fatalf("handler saw %q, want hello", got)
	}
	if !b.Subscribed(id) {
		t.Fatal("Subscribed = false after success, want true")
	}
}

func TestDeliverUnsubscribesOnHandlerError(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	wantErr := errors.New("boom")
	id := b.Subscribe(func(msg string) error {
		return wantErr
	})

	delivered, err := b.Deliver(id, "poison")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if delivered {
		t.Fatal("delivered = true, want false on handler error")
	}
	if b.Subscribed(id) {
		t.Fatal("Subscribed = true after handler error, want false")
	}
}

func TestDeliverRetryAfterFailureIsNotFound(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	calls := 0
	id := b.Subscribe(func(msg string) error {
		calls++
		return errors.New("boom")
	})

	_, _ = b.Deliver(id, "poison")
	_, err := b.Deliver(id, "poison")
	if err == nil {
		t.Fatal("Deliver after unsubscribe: want error, got nil")
	}
	if calls != 1 {
		t.Fatalf("handler called %d times, want exactly 1 (no duplicate processing)", calls)
	}
}
```

## Review

`Deliver` is correct when a successful handler keeps its subscription and a
failing one loses it before the function returns — the property the tests
pin down by redelivering to the same id and asserting the handler runs
exactly once. The deferred closure is what guarantees this: it inspects the
named `err` after either return statement has set it, so the unsubscribe
logic exists once regardless of how many failure exits `Deliver` grows in
the future. The mistake to avoid is calling `Unsubscribe` inline at the one
failure return statement that exists today — it works until a second
failure exit is added (a validation error before the handler even runs, say)
and that one forgets the cleanup, silently reintroducing the duplicate-
delivery bug.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)
- [Google Cloud Pub/Sub: Handling failures](https://cloud.google.com/pubsub/docs/subscriber#handling_failures)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-graceful-shutdown-worker-drain-with-timeout.md](26-graceful-shutdown-worker-drain-with-timeout.md) | Next: [28-operation-duration-percentile-metric.md](28-operation-duration-percentile-metric.md)
