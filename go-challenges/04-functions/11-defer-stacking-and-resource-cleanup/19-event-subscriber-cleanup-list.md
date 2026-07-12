# Exercise 19: Event Subscriber Registration — Defer Unsubscribe

**Nivel: Intermedio** — validacion rapida (un test corto).

Registering a callback with a shared event hub for the lifetime of one
request is the same acquire/release shape as a connection pool borrow —
except the "resource" here is a slot in someone else's subscriber list, and
forgetting to release it leaks a closure that keeps firing for every event
published after the request that created it has already responded. `defer
h.Unsubscribe(id)` right after `Subscribe` makes the removal unconditional.

## What you'll build

```text
subhub/                      independent module: example.com/subhub
  go.mod
  subhub/subhub.go            Hub (mutex-guarded); Subscribe/Unsubscribe/Publish; WithSubscription
  subhub/subhub_test.go       table test: unsubscribe on success/error; a panic case; a two-subscriber Publish test
  cmd/demo/main.go            runnable demo: subscribe for one request, publish, watch the count drop
```

- Files: `subhub/subhub.go`, `subhub/subhub_test.go`, `cmd/demo/main.go`.
- Implement: a mutex-guarded `Hub` with `Subscribe(fn func(Event)) int`, `Unsubscribe(id int)`, `Count() int`, and `Publish(e Event)` that snapshots the callback list under the lock before invoking any of them; and `WithSubscription(h *Hub, fn func(Event), work func() error) (err error)` that subscribes, defers the matching unsubscribe, and runs `work`.
- Test: a table test asserting `Count()` is 1 during `work` and 0 afterward, for both a `nil` and a non-nil `work` error; a separate test asserting a panic mid-`work` still leaves `Count()` at 0; a test that two independent subscribers each receive published events until their own `Unsubscribe`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/19-event-subscriber-cleanup-list/subhub go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/19-event-subscriber-cleanup-list/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/19-event-subscriber-cleanup-list
go mod edit -go=1.24
```

### The leak this prevents is invisible until someone else pays for it

An un-deferred `Unsubscribe` — one written as an explicit call at the end of
`work`, after the "normal" logic — looks fine until the first early `return`
is added six months later by someone who has forgotten (or never knew) that
a subscription needs cleanup. The callback stays registered forever: it
keeps firing on every subsequent `Publish`, closing over whatever request-scoped
state it captured, which is a memory leak and, if that closure writes an
HTTP response or increments a per-request counter, a correctness bug in a
completely different request that just happens to publish an event later.
`defer h.Unsubscribe(id)`, written immediately after `Subscribe` returns its
id, cannot be skipped by a later refactor of `work`'s body — it runs however
`work` exits, panic included, which is exactly the guarantee this module's
panic test checks.

### Publish snapshots before it calls anything

`Publish` copies the current subscriber callbacks into a slice while holding
the lock, then releases the lock before invoking any of them. Calling
callbacks *while* holding the lock would deadlock the moment a callback
itself calls `h.Subscribe` or `h.Unsubscribe` — exactly the kind of
self-referential call a real event-driven system tends to produce sooner or
later. Snapshotting also means a callback that unsubscribes itself mid-`Publish`
still finishes receiving the event it was already being called for, and a
callback subscribed by another goroutine after the snapshot was taken simply
waits for the next `Publish` instead of racing this one.

Create `subhub/subhub.go`:

```go
package subhub

import "sync"

// Event is one published notification.
type Event struct {
	Name    string
	Payload any
}

// Hub is a concurrency-safe pub/sub registry. Subscribe registers a
// callback and returns an id used to remove it again with Unsubscribe.
type Hub struct {
	mu     sync.Mutex
	subs   map[int]func(Event)
	nextID int
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int]func(Event))}
}

// Subscribe registers fn and returns an id that later removes it.
func (h *Hub) Subscribe(fn func(Event)) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	h.subs[id] = fn
	return id
}

// Unsubscribe removes the callback registered under id. Removing an id that
// is already gone (a double-unsubscribe) is a no-op, not an error.
func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, id)
}

// Count reports how many callbacks are currently registered.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Publish calls every currently-registered callback with e. The snapshot is
// taken under the lock so a callback that subscribes or unsubscribes during
// Publish cannot corrupt the map being ranged over, and cannot itself be
// invoked or skipped within this same Publish call.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	fns := make([]func(Event), 0, len(h.subs))
	for _, fn := range h.subs {
		fns = append(fns, fn)
	}
	h.mu.Unlock()

	for _, fn := range fns {
		fn(e)
	}
}

// WithSubscription registers fn for the duration of work and defers its
// removal so the subscription cannot outlive the caller on any exit path --
// a normal return, an error return, or a panic. Without the defer, an early
// return (or a panic) would leave fn registered forever, receiving events
// for a request that has long since finished responding.
func WithSubscription(h *Hub, fn func(Event), work func() error) (err error) {
	id := h.Subscribe(fn)
	defer h.Unsubscribe(id)
	return work()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/subhub/subhub"
)

func main() {
	h := subhub.NewHub()

	_ = subhub.WithSubscription(h, func(e subhub.Event) {
		fmt.Println("received:", e.Name)
	}, func() error {
		fmt.Println("subscribers during request:", h.Count())
		h.Publish(subhub.Event{Name: "order.created"})
		return nil
	})

	fmt.Println("subscribers after request:", h.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subscribers during request: 1
received: order.created
subscribers after request: 0
```

### Tests

Create `subhub/subhub_test.go`:

```go
package subhub

import (
	"errors"
	"testing"
)

func TestWithSubscriptionUnsubscribesOnEveryReturnPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		workErr error
	}{
		{name: "work succeeds", workErr: nil},
		{name: "work fails", workErr: errors.New("handler failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub()

			var receivedDuringWork int
			var countDuringWork int
			err := WithSubscription(h, func(Event) { receivedDuringWork++ }, func() error {
				countDuringWork = h.Count()
				h.Publish(Event{Name: "tick"})
				return tt.workErr
			})

			if !errors.Is(err, tt.workErr) {
				t.Fatalf("err = %v, want %v", err, tt.workErr)
			}
			if countDuringWork != 1 {
				t.Fatalf("Count during work = %d, want 1", countDuringWork)
			}
			if receivedDuringWork != 1 {
				t.Fatalf("receivedDuringWork = %d, want 1", receivedDuringWork)
			}
			if got := h.Count(); got != 0 {
				t.Fatalf("Count after WithSubscription = %d, want 0", got)
			}
		})
	}
}

func TestWithSubscriptionUnsubscribesOnPanic(t *testing.T) {
	t.Parallel()

	h := NewHub()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = WithSubscription(h, func(Event) {}, func() error {
			panic("handler blew up")
		})
	}()

	if got := h.Count(); got != 0 {
		t.Fatalf("Count after panic = %d, want 0", got)
	}
}

func TestPublishReachesOnlyCurrentlySubscribedCallbacks(t *testing.T) {
	t.Parallel()

	h := NewHub()
	var a, b int
	idA := h.Subscribe(func(Event) { a++ })
	idB := h.Subscribe(func(Event) { b++ })

	h.Publish(Event{Name: "first"})
	if a != 1 || b != 1 {
		t.Fatalf("a=%d b=%d, want 1 and 1", a, b)
	}

	h.Unsubscribe(idA)
	h.Publish(Event{Name: "second"})
	if a != 1 || b != 2 {
		t.Fatalf("a=%d b=%d, want 1 and 2", a, b)
	}

	h.Unsubscribe(idB)
	if got := h.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

`Count()` returning to `0` after `WithSubscription` is the invariant every
test in this module protects, from three different angles: a `nil` return,
an `error` return, and a `panic`. Any of those exiting `WithSubscription`
without running the deferred `Unsubscribe` would leave the hub's subscriber
map growing by one entry per request forever — a leak that is easy to miss
in development, where a handful of test requests never accumulate enough
stale subscribers to matter, and only shows up under sustained production
traffic. The two-subscriber test is there for a different reason: it proves
`Unsubscribe` removes exactly the id it was given, not "the last subscriber"
or "the first one" — a distinction that matters the moment more than one
request is subscribed to the same hub at once, which is the normal case.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Go Blog: Go Concurrency Patterns](https://go.dev/blog/pipelines) — the broader pub/sub-over-channels family this Hub simplifies.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [18-pending-webhook-drain-timeout.md](18-pending-webhook-drain-timeout.md) | Next: [20-distributed-lock-unlock-timeout.md](20-distributed-lock-unlock-timeout.md)
