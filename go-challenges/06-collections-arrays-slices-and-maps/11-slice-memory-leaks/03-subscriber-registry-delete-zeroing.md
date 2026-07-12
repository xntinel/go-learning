# Exercise 3: Unsubscribe From a Pub/Sub Hub Without Leaking Connection Pointers

A pub/sub hub keeps a slice of live subscribers, each holding a connection and its
buffers. When a client disconnects you remove it from the slice — and the obvious
one-line removal leaves a live pointer to the deregistered subscriber in the
slice's unused tail, pinning the connection it was supposed to release. This
module builds the hub, shows the tail-pin the naive removal causes, and fixes it
with `slices.Delete`, which zeroes the vacated tail.

## What you'll build

```text
hub/                         independent module: example.com/hub
  go.mod                     go 1.24
  hub.go                     type Subscriber, Hub; Add, Remove, RemoveLeaky, IDs, Len
  cmd/
    demo/
      main.go                add three subscribers, remove one, show the surviving set
  hub_test.go                reclaimable-after-Remove, pinned-after-RemoveLeaky, set-preserved; -race
```

Files: `hub.go`, `cmd/demo/main.go`, `hub_test.go`.
Implement: a `Hub` over `[]*Subscriber` with `Remove` (uses `slices.Delete`) and a contrasting `RemoveLeaky` (naive swap-remove).
Test: prove `Remove` lets the subscriber be reclaimed and `RemoveLeaky` pins it, using a `weak.Pointer`; assert the surviving set is preserved.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Why the naive removal leaks

Removing element `i` from a slice without preserving order is usually written as a
swap-remove: move the last element into the hole, then shrink the length.

```go
// RemoveLeaky's body — the bug.
s[i] = s[len(s)-1]
s = s[:len(s)-1]
```

For a `[]int` this is fine. For a `[]*Subscriber` it leaks. Shrinking the length
does not clear the slot at the old `len(s)-1` index — that slot is now beyond
`len` but still inside `cap`, and it still holds a pointer. When you remove the
*last* element this way, its pointer stays sitting in that tail slot: the
subscriber is no longer a member of the hub, but the backing array (which the hub
still references) keeps a live pointer to it, so the GC cannot reclaim it. In a
long-lived hub churning subscribers, every removed connection can linger in a tail
slot until the array is next grown or overwritten — a steady leak that a heap
profile shows as `*Subscriber` retained by the hub's slice.

`slices.Delete(s, i, i+1)` fixes this precisely. Its contract is to remove the
elements in `[i, i+1)` *and zero the slice's tail* between the new and old length,
so no dead pointer survives in the spare capacity. It preserves order (it shifts
the tail down rather than swapping), which is a bonus but not the point; the point
is the zeroing. For a pointer-bearing element type it is a correctness fix, not a
style preference.

The `Subscriber` here carries a kilobyte-sized `buf` standing in for a real
connection's read buffer, so that dropping the last strong reference to a
subscriber really does make it individually collectible — the runtime can batch
tiny pointer-free objects, so a leak test needs a non-tiny, pointer-containing
value to observe reclamation honestly.

Create `hub.go`:

```go
package hub

import "slices"

// Subscriber holds a connection's identity and its buffers. The buf field makes
// it a non-tiny, pointer-containing allocation so collection is observable.
type Subscriber struct {
	ID  string
	buf []byte
}

// NewSubscriber returns a subscriber with a kilobyte read buffer.
func NewSubscriber(id string) *Subscriber {
	return &Subscriber{ID: id, buf: make([]byte, 1024)}
}

// Hub holds the currently registered subscribers.
type Hub struct {
	subs []*Subscriber
}

// Add registers s.
func (h *Hub) Add(s *Subscriber) { h.subs = append(h.subs, s) }

// Remove deregisters the subscriber with the given id and reports whether one was
// found. It uses slices.Delete, which zeroes the vacated tail slot so no dead
// *Subscriber pointer survives to pin a released connection.
func (h *Hub) Remove(id string) bool {
	i := slices.IndexFunc(h.subs, func(s *Subscriber) bool { return s.ID == id })
	if i < 0 {
		return false
	}
	h.subs = slices.Delete(h.subs, i, i+1)
	return true
}

// RemoveLeaky deregisters by naive swap-remove. It shrinks the length but never
// clears the old tail slot, so the removed *Subscriber stays reachable through the
// backing array's spare capacity. Kept only to contrast with Remove.
func (h *Hub) RemoveLeaky(id string) bool {
	i := slices.IndexFunc(h.subs, func(s *Subscriber) bool { return s.ID == id })
	if i < 0 {
		return false
	}
	h.subs[i] = h.subs[len(h.subs)-1]
	h.subs = h.subs[:len(h.subs)-1]
	return true
}

// IDs returns the ids of the registered subscribers, in order.
func (h *Hub) IDs() []string {
	out := make([]string, 0, len(h.subs))
	for _, s := range h.subs {
		out = append(out, s.ID)
	}
	return out
}

// Len reports the number of registered subscribers.
func (h *Hub) Len() int { return len(h.subs) }
```

## The runnable demo

The demo registers three subscribers, removes the middle one, and prints the
surviving set — order-preserving `slices.Delete` keeps the rest in place.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/hub"
)

func main() {
	h := &hub.Hub{}
	for _, id := range []string{"alice", "bob", "carol"} {
		h.Add(hub.NewSubscriber(id))
	}

	removed := h.Remove("bob")
	fmt.Printf("removed bob: %v\n", removed)
	fmt.Printf("survivors:   %v\n", h.IDs())
	fmt.Printf("count:       %d\n", h.Len())
	fmt.Printf("remove ghost: %v\n", h.Remove("dave"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
removed bob: true
survivors:   [alice carol]
count:       2
remove ghost: false
```

## Tests

The reclaimability tests use a `weak.Pointer`: take a weak reference to a
subscriber, drop the strong one, remove it, and force GC. After `Remove` the weak
pointer goes `nil` (reclaimed); after `RemoveLeaky` it stays non-`nil` (pinned in
the tail). GC is asynchronous, so the reclaim test polls `runtime.GC()` under a
bounded deadline rather than assuming one call suffices. The set-preservation test
is a plain, parallel-safe assertion.

Create `hub_test.go`:

```go
package hub

import (
	"runtime"
	"slices"
	"testing"
	"time"
	"weak"
)

func TestRemoveLetsSubscriberBeReclaimed(t *testing.T) {
	h := &Hub{}
	h.Add(NewSubscriber("a"))
	s := NewSubscriber("b")
	h.Add(s)
	wp := weak.Make(s)

	s = nil // drop the only strong reference outside the hub
	// slices.Delete zeroes the vacated tail slot, so no dead pointer survives.
	if !h.Remove("b") {
		t.Fatal("Remove(b) = false, want true")
	}

	deadline := time.Now().Add(2 * time.Second)
	for wp.Value() != nil {
		if time.Now().After(deadline) {
			t.Fatal("subscriber not reclaimed after Remove; tail slot still pins it")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

func TestRemoveLeakyPinsSubscriber(t *testing.T) {
	h := &Hub{}
	h.Add(NewSubscriber("a"))
	s := NewSubscriber("b")
	h.Add(s)
	wp := weak.Make(s)

	s = nil
	// RemoveLeaky removes the last element but leaves its pointer in the tail.
	if !h.RemoveLeaky("b") {
		t.Fatal("RemoveLeaky(b) = false, want true")
	}

	runtime.GC()
	runtime.GC()
	if wp.Value() == nil {
		t.Fatal("RemoveLeaky unexpectedly reclaimed the subscriber; tail should pin it")
	}
	runtime.KeepAlive(h)
}

func TestRemovePreservesSet(t *testing.T) {
	t.Parallel()

	h := &Hub{}
	for _, id := range []string{"a", "b", "c", "d"} {
		h.Add(NewSubscriber(id))
	}

	if !h.Remove("b") {
		t.Fatal("Remove(b) = false")
	}

	got := h.IDs()
	for _, want := range []string{"a", "c", "d"} {
		if !slices.Contains(got, want) {
			t.Fatalf("survivor %q missing from %v", want, got)
		}
	}
	if slices.Contains(got, "b") {
		t.Fatalf("removed id still present in %v", got)
	}
	if h.Len() != 3 {
		t.Fatalf("Len = %d, want 3", h.Len())
	}
}

func TestRemoveMissing(t *testing.T) {
	t.Parallel()

	h := &Hub{}
	h.Add(NewSubscriber("a"))
	if h.Remove("zzz") {
		t.Fatal("Remove of absent id returned true")
	}
}
```

## Review

The hub is correct when `Remove` both preserves the surviving set and lets the
removed subscriber be reclaimed. The `weak.Pointer` tests are the proof: after
`slices.Delete` the tail slot is nil, so the deregistered subscriber becomes
unreachable and the weak pointer eventually goes `nil`; after the naive
`RemoveLeaky` the subscriber's pointer survives in the backing array's spare
capacity, so it stays pinned. The mistake this module exists to prevent is
swap-remove or truncation on a slice of pointers without zeroing the vacated slot —
it "works" by every functional test and leaks in production. If you must remove by
hand, nil the slot before shrinking; otherwise use `slices.Delete`. Run
`go test -race` to confirm the removal path is safe under concurrent readers of
`IDs`.

## Resources

- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — removes a range and zeroes the vacated tail elements.
- [`slices.IndexFunc`](https://pkg.go.dev/slices#IndexFunc) — locating the element to remove.
- [`weak` package](https://pkg.go.dev/weak) — `weak.Make` and `Value()`, used here to prove reclaimability.

---

Back to [02-memstats-pinning-leak-test.md](02-memstats-pinning-leak-test.md) | Next: [04-clip-token-out-of-request-buffer.md](04-clip-token-out-of-request-buffer.md)
