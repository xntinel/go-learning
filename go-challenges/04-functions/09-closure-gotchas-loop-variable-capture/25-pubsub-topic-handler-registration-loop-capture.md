# Exercise 25: Pub/Sub Handler Registration: Topic Variable Captured in Handler Closure

**Nivel: Intermedio** — validacion rapida (un test corto).

A message broker registers one handler per topic in a loop, each handler
closure recording which topic it was invoked for. This is the textbook
loop-capture shape, and it now works correctly by default: on a module whose
`go.mod` declares `go 1.22` or later, the `topic` range variable is a fresh
instance per iteration, so each handler closes over its own value. On an
older module — or a module that never bumped its `go` directive after
upgrading toolchains — this exact code silently reverts to the pre-1.22 bug,
where every handler shares one variable and all of them report whatever
topic the loop last reached.

## What you'll build

```text
pubsub/                      independent module: example.com/pubsub
  go.mod                     go 1.24
  pubsub.go                  Broker, DeliveryLog, RegisterTopics
  cmd/
    demo/
      main.go                runnable demo: register topics, publish, print deliveries
  pubsub_test.go             each handler keeps its own topic; unknown-topic edge case
```

- Files: `pubsub.go`, `cmd/demo/main.go`, `pubsub_test.go`.
- Implement: `Broker.Subscribe`/`Publish`; `RegisterTopics(b, topics, log)` subscribing one handler per topic that closes directly over the loop's `topic` variable.
- Test: publish to every registered topic and assert each handler recorded exactly its own topic, not a later iteration's.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why this code's correctness depends on the `go` directive, not just the toolchain

`for _, topic := range topics { b.Subscribe(topic, func(t, payload string) { ... }) }`
looks identical whether it is safe or buggy. What decides its behavior is the
`go` line in `go.mod`: Go 1.22 changed the *language semantics* of `for`
loops so that `topic` is a new variable each iteration, but that change is
gated by the module's declared language version, not by which toolchain
happens to compile it. A newer toolchain compiling a module that still
declares `go 1.21` reproduces the OLD semantics exactly, on purpose, so
upgrading your Go installation can never silently change a program's
behavior. That means the fix for this class of bug is sometimes not a code
change at all — it is bumping the `go` directive — and that this module's
`go.mod` declaring `go 1.24` is doing real, load-bearing work, not just
picking a version to compile with.

Create `pubsub.go`:

```go
package pubsub

import "sync"

// Broker routes published messages to the handler subscribed for a topic.
type Broker struct {
	mu       sync.Mutex
	handlers map[string]func(topic, payload string)
}

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{handlers: make(map[string]func(topic, payload string))}
}

// Subscribe registers handler as the receiver for topic.
func (b *Broker) Subscribe(topic string, handler func(topic, payload string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = handler
}

// Publish delivers payload to topic's handler, if one is registered.
func (b *Broker) Publish(topic, payload string) bool {
	b.mu.Lock()
	h, ok := b.handlers[topic]
	b.mu.Unlock()
	if !ok {
		return false
	}
	h(topic, payload)
	return true
}

// RegisterTopics subscribes one handler per topic that appends every
// delivery it receives to log. Each handler closes directly over the loop's
// `topic` variable -- on Go 1.22+ (this module's `go` directive is 1.24)
// that variable is a fresh instance per iteration, so each handler correctly
// keeps its OWN topic and never sees a later iteration's value. Before Go
// 1.22, or in a module whose go.mod still declares an older language
// version, this exact code would have been the classic loop-capture bug:
// every handler would close over the SAME shared `topic` variable and all
// of them would report whatever topic the loop last reached.
func RegisterTopics(b *Broker, topics []string, log *DeliveryLog) {
	for _, topic := range topics {
		b.Subscribe(topic, func(t, payload string) {
			log.record(t, payload)
		})
	}
}

// DeliveryLog records topic/payload pairs handlers were actually invoked
// with, safe for concurrent use.
type DeliveryLog struct {
	mu   sync.Mutex
	Rows []Delivery
}

// Delivery is one handler invocation observed by a DeliveryLog.
type Delivery struct {
	Topic   string
	Payload string
}

func (l *DeliveryLog) record(topic, payload string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Rows = append(l.Rows, Delivery{Topic: topic, Payload: payload})
}

// Snapshot returns a copy of the recorded deliveries.
func (l *DeliveryLog) Snapshot() []Delivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Delivery, len(l.Rows))
	copy(out, l.Rows)
	return out
}
```

### The runnable demo

The demo registers three topics and publishes to each, printing the handler
that actually fired.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pubsub"
)

func main() {
	b := pubsub.NewBroker()
	log := &pubsub.DeliveryLog{}
	pubsub.RegisterTopics(b, []string{"orders", "payments", "shipping"}, log)

	b.Publish("orders", "order-1")
	b.Publish("payments", "payment-1")
	b.Publish("shipping", "shipment-1")

	for _, d := range log.Snapshot() {
		fmt.Printf("handler for %s received topic=%s payload=%s\n", d.Topic, d.Topic, d.Payload)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handler for orders received topic=orders payload=order-1
handler for payments received topic=payments payload=payment-1
handler for shipping received topic=shipping payload=shipment-1
```

### Tests

`TestRegisterTopicsEachHandlerKeepsOwnTopic` publishes to every registered
topic and asserts each handler recorded exactly its own topic and payload.
`TestPublishUnknownTopicReturnsFalse` covers the edge case of publishing to a
topic nobody subscribed to.

Create `pubsub_test.go`:

```go
package pubsub

import "testing"

func TestRegisterTopicsEachHandlerKeepsOwnTopic(t *testing.T) {
	b := NewBroker()
	log := &DeliveryLog{}
	topics := []string{"orders", "payments", "shipping"}
	RegisterTopics(b, topics, log)

	for _, topic := range topics {
		if ok := b.Publish(topic, "payload-"+topic); !ok {
			t.Fatalf("no handler registered for topic %q", topic)
		}
	}

	rows := log.Snapshot()
	if len(rows) != len(topics) {
		t.Fatalf("len(rows) = %d, want %d", len(rows), len(topics))
	}
	for i, topic := range topics {
		want := Delivery{Topic: topic, Payload: "payload-" + topic}
		if rows[i] != want {
			t.Fatalf("rows[%d] = %+v, want %+v (handler captured the wrong topic)", i, rows[i], want)
		}
	}
}

func TestPublishUnknownTopicReturnsFalse(t *testing.T) {
	b := NewBroker()
	log := &DeliveryLog{}
	RegisterTopics(b, []string{"orders"}, log)

	if ok := b.Publish("unknown", "x"); ok {
		t.Fatal("Publish on an unregistered topic returned true, want false")
	}
	if len(log.Snapshot()) != 0 {
		t.Fatal("no handler should have been invoked for an unknown topic")
	}
}
```

## Review

The broker is correct when every topic's handler reports exactly the topic
it was registered for, no matter how many topics are registered in the same
loop. What makes this exercise different from most loop-capture bugs is that
the SOURCE CODE for the buggy and fixed versions is identical — the fix here
is not a code change but a `go.mod` line. `RegisterTopics` closes directly
over the loop's `topic` variable; whether that is safe depends entirely on
whether the module declares `go 1.22` or later. `go vet` and the Go
compiler will not warn you if a `go.mod` still declares an old language
version and quietly keeps you on the pre-1.22 behavior forever, even under a
brand new toolchain — this is the one loop-capture case where reading the
top of `go.mod` matters as much as reading the loop body.

## Resources

- [Go blog: Fixing for loops in Go 1.22](https://go.dev/blog/loopvar-preview) — per-iteration loop variables and how the `go` directive gates the change.
- [Go spec: For statements](https://go.dev/ref/spec#For_statements) — the language-level semantics of range variables.
- [`go.mod` reference: the `go` directive](https://go.dev/ref/mod#go-mod-file-go) — how the declared language version affects compiled semantics.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-nested-transaction-savepoint-defer-timing-violation.md](24-nested-transaction-savepoint-defer-timing-violation.md) | Next: [26-observability-span-context-value-capture-propagation.md](26-observability-span-context-value-capture-propagation.md)
