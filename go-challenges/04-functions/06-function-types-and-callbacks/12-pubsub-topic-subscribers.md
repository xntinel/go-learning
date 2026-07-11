# Exercise 12: Event Bus With Multiple Subscriber Callbacks Per Topic

**Nivel: Intermedio** — validacion rapida (un test corto).

A single dispatch table maps a kind to one handler; a pub/sub event bus is the
next step up — several independent subscribers react to the same topic, and
one subscriber's failure must not stop the others from being called. This
module builds a small `Bus` where callbacks are the subscriber contract.

## What you'll build

```text
eventbus/                   independent module: example.com/eventbus-topics
  go.mod                     go 1.24
  eventbus.go                type Handler, type Bus, Subscribe/Unsubscribe/Publish
  eventbus_test.go           table test: fan-out, error aggregation, unsubscribe
```

Files: `eventbus.go`, `eventbus_test.go`.
Implement: `type Handler func(payload string) error`, a `Bus` with
`Subscribe(topic string, h Handler) int`, `Unsubscribe(topic string, id int) bool`,
and `Publish(topic string, payload string) error`.
Test: a table covering all-succeed, one-fails, and both-fail, plus a
dedicated test for unsubscribe-then-republish.
Verify: `go test -count=1 ./...`

```bash
mkdir -p ~/go-exercises/eventbus-topics
cd ~/go-exercises/eventbus-topics
go mod init example.com/eventbus-topics
go mod edit -go=1.24
```

### Why a handle instead of the function value

Every subscriber for a topic must run on `Publish`, and a failing subscriber
must not block the rest — that fan-out-and-aggregate shape is the whole
design. The one wrinkle: Go permits comparing a function value only against
`nil`, so `Unsubscribe` cannot look up "this exact closure" in the slice.
`Subscribe` hands back an integer handle instead, and `Unsubscribe` matches on
that handle — the same reason a `context.CancelFunc` or an `http.Server`
shutdown hook is keyed by a token, not by re-supplying the original callback.

Create `eventbus.go`:

```go
package eventbus

import "errors"

// Handler is a subscriber callback invoked with the payload published to its topic.
type Handler func(payload string) error

type subscriber struct {
	id int
	fn Handler
}

// Bus fans a published payload out to every subscriber registered for a topic.
type Bus struct {
	subs   map[string][]subscriber
	nextID int
}

// New returns an empty Bus.
func New() *Bus {
	return &Bus{subs: make(map[string][]subscriber)}
}

// Subscribe registers h for topic and returns a handle. Function values cannot
// be compared for equality in Go, so the handle (not the func) is what
// Unsubscribe later matches against.
func (b *Bus) Subscribe(topic string, h Handler) int {
	b.nextID++
	id := b.nextID
	b.subs[topic] = append(b.subs[topic], subscriber{id: id, fn: h})
	return id
}

// Unsubscribe removes the subscriber with the given handle from topic. It
// reports whether a matching handle was found.
func (b *Bus) Unsubscribe(topic string, id int) bool {
	list := b.subs[topic]
	for i, s := range list {
		if s.id == id {
			b.subs[topic] = append(list[:i:i], list[i+1:]...)
			return true
		}
	}
	return false
}

// Publish calls every subscriber registered for topic with payload. It always
// calls all of them, even if an earlier one errors, and aggregates every
// failure with errors.Join so a caller can inspect each one via errors.Is.
func (b *Bus) Publish(topic string, payload string) error {
	var errs []error
	for _, s := range b.subs[topic] {
		if err := s.fn(payload); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### Tests

The first table drives two subscribers on the same topic through three
combinations of failure; `calls` proves both ran regardless of outcome, and
`errors.Is` proves the aggregated error still matches the sentinel. A second
test proves an unsubscribed handler stops receiving payloads and that
unsubscribing the same handle twice reports `false` the second time.

Create `eventbus_test.go`:

```go
package eventbus

import (
	"errors"
	"testing"
)

var errHandlerFailed = errors.New("handler failed")

func TestBus_PublishFansOutAndAggregatesErrors(t *testing.T) {
	tests := []struct {
		name       string
		failFirst  bool
		failSecond bool
		wantErr    bool
	}{
		{"all succeed", false, false, false},
		{"second fails", false, true, true},
		{"both fail", true, true, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bus := New()
			var calls int

			bus.Subscribe("order.created", func(payload string) error {
				calls++
				if tc.failFirst {
					return errHandlerFailed
				}
				return nil
			})
			bus.Subscribe("order.created", func(payload string) error {
				calls++
				if tc.failSecond {
					return errHandlerFailed
				}
				return nil
			})

			err := bus.Publish("order.created", "order-1")

			if calls != 2 {
				t.Errorf("calls = %d, want 2 (both subscribers must run)", calls)
			}
			if tc.wantErr && !errors.Is(err, errHandlerFailed) {
				t.Errorf("Publish() = %v, want to match errHandlerFailed", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Publish() = %v, want nil", err)
			}
		})
	}
}

func TestBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := New()
	var callsA, callsB int

	idA := bus.Subscribe("order.created", func(payload string) error {
		callsA++
		return nil
	})
	bus.Subscribe("order.created", func(payload string) error {
		callsB++
		return nil
	})

	if ok := bus.Unsubscribe("order.created", idA); !ok {
		t.Fatalf("Unsubscribe(idA) = false, want true")
	}

	if err := bus.Publish("order.created", "order-2"); err != nil {
		t.Fatalf("Publish() = %v, want nil", err)
	}

	if callsA != 0 {
		t.Errorf("callsA = %d, want 0 (unsubscribed)", callsA)
	}
	if callsB != 1 {
		t.Errorf("callsB = %d, want 1", callsB)
	}

	if ok := bus.Unsubscribe("order.created", idA); ok {
		t.Errorf("Unsubscribe(idA) again = true, want false (already removed)")
	}
}

func TestBus_PublishWithNoSubscribersIsNoop(t *testing.T) {
	bus := New()
	if err := bus.Publish("nobody.listening", "x"); err != nil {
		t.Errorf("Publish() = %v, want nil", err)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Publish` never short-circuits on the first error — every subscriber gets a
chance to react, and `errors.Join` keeps each failure inspectable instead of
collapsing them into one opaque message. The handle-based `Unsubscribe`
exists only because function values aren't comparable in Go; it's the same
trick a cancel-token or a event-listener-removal API always reaches for. Left
out on purpose: concurrent `Publish`/`Subscribe` calls, which would need a
mutex around the `subs` map — this module is single-threaded by design.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-retry-with-classifier-callback.md](11-retry-with-classifier-callback.md) | Next: [13-config-tree-visitor-early-stop.md](13-config-tree-visitor-early-stop.md)
