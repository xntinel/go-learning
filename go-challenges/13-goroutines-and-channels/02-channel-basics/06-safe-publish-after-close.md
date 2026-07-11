# Exercise 6: Pub/Sub Broker that Never Panics on Publish-After-Shutdown

Sending on a closed channel panics. In a pub/sub broker, that panic is not
hypothetical: during shutdown a publisher goroutine can call `Publish` a
microsecond after another goroutine called `Close`, and a naive broker crashes the
whole process. This exercise builds a broker whose `Publish` returns an error after
`Close` instead of panicking, using a guarded `closed` flag — the direct antidote to
the send-on-closed failure mode.

This module is self-contained: its own module, a `broker` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
broker/                      independent module: example.com/broker
  go.mod                     go 1.26
  broker.go                  type Msg, Broker; Subscribe, Publish, Close; ErrClosed
  cmd/demo/main.go           runnable demo: subscribe, publish, close
  broker_test.go             delivery, publish-after-close, double-close, race
```

- Files: `broker.go`, `cmd/demo/main.go`, `broker_test.go`.
- Implement: `New() *Broker`, `Subscribe() <-chan Msg`, `Publish(Msg) error`, `Close()`, with `Publish` after `Close` returning `ErrClosed` rather than panicking.
- Test: a subscriber receives a published message; publish after close returns `ErrClosed`; double close is safe; concurrent publishers racing a close never panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/broker/cmd/demo
cd ~/go-exercises/broker
go mod init example.com/broker
```

### The whole difficulty is the shutdown race

`close(ch)` is the correct way to tell subscribers "no more messages"; a subscriber
ranging its channel sees the close and exits. But `close` on the *broker's* side
creates a hazard on the *publish* side: once a subscriber channel is closed, any
`ch <- m` on it panics. Under load, `Publish` and `Close` run on different
goroutines, so a publisher can be mid-`Publish` when `Close` runs. Without
coordination that is a "send on closed channel" panic that takes down the service.

The fix is a single mutex that serializes the two operations plus a `closed` bool:

- `Close` takes the lock, sets `closed = true`, closes every subscriber channel, and
  drops its references. Guarding with the `closed` flag makes a second `Close` a
  no-op, so double-close never panics.
- `Publish` takes the *same* lock, returns `ErrClosed` immediately if `closed`, and
  otherwise sends to each subscriber. Because both hold the lock, `Publish` can
  never observe a half-closed broker — it either runs entirely before `Close`
  (delivering) or entirely after (`ErrClosed`). There is no window in which it sends
  on an already-closed channel.

Subscriber channels are buffered and the send is non-blocking (`select` with a
`default`): a slow or absent subscriber must not stall the publisher or, worse,
deadlock it while it holds the broker lock. Dropping a message to a full subscriber
is the right trade-off for a fan-out broker — backpressure on one subscriber must
not become head-of-line blocking for all of them.

`ErrClosed` is a package-level sentinel so callers branch with `errors.Is`.

Create `broker.go`:

```go
package broker

import (
	"errors"
	"sync"
)

// ErrClosed is returned by Publish after the broker has been closed.
var ErrClosed = errors.New("broker: closed")

// Msg is a published message.
type Msg struct {
	Topic string
	Body  string
}

// Broker is a minimal in-process fan-out pub/sub. Publish after Close returns
// ErrClosed instead of panicking on a send to a closed channel.
type Broker struct {
	mu     sync.Mutex
	closed bool
	subs   []chan Msg
}

// New returns an open Broker.
func New() *Broker {
	return &Broker{}
}

// Subscribe registers a new subscriber and returns its receive-only channel. The
// channel is closed when the broker is closed.
func (b *Broker) Subscribe() <-chan Msg {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Msg, 16)
	b.subs = append(b.subs, ch)
	return ch
}

// Publish delivers m to every current subscriber. It returns ErrClosed if the
// broker is closed. A subscriber whose buffer is full drops the message rather
// than blocking the publisher.
func (b *Broker) Publish(m Msg) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	for _, ch := range b.subs {
		select {
		case ch <- m:
		default: // subscriber full: drop rather than block
		}
	}
	return nil
}

// Close closes every subscriber channel and marks the broker closed. It is safe to
// call more than once.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/broker"
)

func main() {
	b := broker.New()
	sub := b.Subscribe()

	_ = b.Publish(broker.Msg{Topic: "orders", Body: "created"})
	fmt.Println("received:", (<-sub).Body)

	b.Close()

	err := b.Publish(broker.Msg{Topic: "orders", Body: "late"})
	fmt.Println("publish after close is ErrClosed:", errors.Is(err, broker.ErrClosed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
received: created
publish after close is ErrClosed: true
```

### Tests

`TestPublishDeliversToSubscriber` proves the happy path. `TestPublishAfterCloseReturnsErrNotPanic`
is the exercise's reason for existing: a naive broker panics here; this one returns
`ErrClosed`, asserted with `errors.Is`. `TestDoubleCloseIsSafe` calls `Close` twice.
`TestConcurrentPublishAndCloseNoPanic` is the race gate: many publisher goroutines
hammer `Publish` while one goroutine calls `Close`, and the test asserts no panic and
that every publish either succeeded or returned `ErrClosed`. Under `-race` this is
the real proof the mutex closes the send-on-closed window.

Create `broker_test.go`:

```go
package broker

import (
	"errors"
	"sync"
	"testing"
)

func TestPublishDeliversToSubscriber(t *testing.T) {
	t.Parallel()
	b := New()
	sub := b.Subscribe()

	if err := b.Publish(Msg{Topic: "t", Body: "hello"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := <-sub
	if got.Body != "hello" {
		t.Fatalf("received %q, want %q", got.Body, "hello")
	}
}

func TestPublishAfterCloseReturnsErrNotPanic(t *testing.T) {
	t.Parallel()
	b := New()
	b.Subscribe()
	b.Close()

	err := b.Publish(Msg{Topic: "t", Body: "late"})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
}

func TestDoubleCloseIsSafe(t *testing.T) {
	t.Parallel()
	b := New()
	b.Subscribe()
	b.Close()
	b.Close() // must not panic
}

func TestSubscriberSeesCloseViaCommaOk(t *testing.T) {
	t.Parallel()
	b := New()
	sub := b.Subscribe()
	b.Close()

	if _, ok := <-sub; ok {
		t.Fatal("receive on a closed subscriber returned ok=true")
	}
}

func TestConcurrentPublishAndCloseNoPanic(t *testing.T) {
	t.Parallel()
	b := New()
	// Drain subscribers so buffers never wedge the publishers.
	for range 4 {
		sub := b.Subscribe()
		go func() {
			for range sub {
			}
		}()
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := b.Publish(Msg{Topic: "t", Body: "x"}); err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("unexpected Publish error: %v", err)
					return
				}
			}
		}()
	}

	// Race a close against the publishers.
	go b.Close()

	wg.Wait()
}
```

## Review

The broker is correct when `Publish` and `Close` can never interleave in a way that
sends on a closed channel. The single mutex is what enforces that: both operations
hold it, so `Publish` sees the broker as fully open or fully closed, never in
between. `TestConcurrentPublishAndCloseNoPanic` under `-race` is the evidence — 50
publishers racing a `Close` with no panic and every error being `ErrClosed`. The
mistakes this exercise targets are exactly the ones that crash real services:
sending after close (guarded by the `closed` flag under the lock), and closing twice
(the `closed` flag makes the second `Close` a no-op). The non-blocking `select`
send is the subtler point — without it, a publisher holding the broker lock could
block on a full subscriber and deadlock every other operation.

## Resources

- [Go Language Spec: Send statements](https://go.dev/ref/spec#Send_statements) — sending on a closed channel panics; sending on a nil channel blocks.
- [Go Language Spec: Close](https://go.dev/ref/spec#Close) — closing a closed or nil channel panics.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock serializing publish against close.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-pipeline-stage-transform.md](05-pipeline-stage-transform.md) | Next: [07-fan-in-merge-health-checks.md](07-fan-in-merge-health-checks.md)
