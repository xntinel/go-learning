# Exercise 18: Cache Invalidation Broadcast to Multiple Subscriber Closures

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A distributed cache has many independent consumers of the same invalidation
event — edge nodes, a search index, a local in-process cache — each
registering its own callback. `NewBroadcaster` closes over a subscriber
registry keyed by ID; when a key is invalidated, every registered callback
runs in its own goroutine so one slow subscriber cannot delay the others, and
the closure waits for all of them before reporting who was notified.

## What you'll build

```text
broadcaster/               independent module: example.com/cache-invalidation-broadcast
  go.mod                   go 1.24
  broadcaster.go           NewBroadcaster returns (subscribe, invalidate)
  cmd/
    demo/
      main.go               registers three subscribers, invalidates one key
  broadcaster_test.go       table test: fan-out, empty case, isolation, concurrency
```

- Files: `broadcaster.go`, `cmd/demo/main.go`, `broadcaster_test.go`.
- Implement: `NewBroadcaster() (subscribe func(id string, cb func(key string)), invalidate func(key string) []string)`, both closing over a mutex-guarded subscriber map; `invalidate` runs every callback in parallel, waits for completion, and returns the sorted IDs notified.
- Test: three subscribers all see the invalidated key and are reported sorted; an empty broadcaster returns no notifications; two broadcasters never share subscribers; 50 subscribers all execute and `invalidate` genuinely waits for every one under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/18-cache-invalidation-subscriber-broadcast/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/18-cache-invalidation-subscriber-broadcast
go mod edit -go=1.24
```

### Snapshot before you fan out, then wait for everyone

`subscribe` and `invalidate` share one captured `subs` map, guarded by a
mutex. The subtle design decision is in `invalidate`: it takes the lock only
long enough to copy the current subscriber set into a local `snapshot`, then
releases the lock *before* calling a single callback. Holding the lock while
running arbitrary subscriber code would mean one slow or blocking subscriber
stalls every other goroutine that wants to `subscribe` or `invalidate` on the
same broadcaster for as long as that one callback runs — exactly the kind of
convoy a broadcast mechanism must avoid.

Once the lock is released, `invalidate` launches one goroutine per subscriber
in the snapshot, each running its callback and then appending its own ID to a
shared `notified` slice under a second, separate mutex (append is not safe
for concurrent use any more than the map is). A `sync.WaitGroup` makes
`invalidate` block until every goroutine has finished, so the function's
return value is not a "fire and forget" — the caller can rely on every
subscriber having actually run by the time `invalidate` returns. Sorting
`notified` before returning is what makes the result assertable in a test:
goroutine completion order is otherwise nondeterministic.

Create `broadcaster.go`:

```go
package broadcaster

import (
	"sort"
	"sync"
)

// NewBroadcaster returns a paired subscribe/invalidate closure sharing one
// captured subscriber registry, guarded by a mutex. subscribe registers a
// callback under an ID; invalidate runs every currently-registered callback
// concurrently (one goroutine per subscriber) so a slow subscriber cannot
// delay the others, waits for all of them to finish, and returns the sorted
// IDs of the subscribers that were notified.
//
// invalidate takes a snapshot of the subscriber map under the lock and then
// releases it before calling any callback: holding the lock while running
// arbitrary subscriber code would let one slow or blocking callback stall
// every future subscribe/invalidate call on the same broadcaster.
func NewBroadcaster() (subscribe func(id string, cb func(key string)), invalidate func(key string) []string) {
	var mu sync.Mutex
	subs := make(map[string]func(string))

	subscribe = func(id string, cb func(key string)) {
		mu.Lock()
		defer mu.Unlock()
		subs[id] = cb
	}

	invalidate = func(key string) []string {
		mu.Lock()
		snapshot := make(map[string]func(string), len(subs))
		for id, cb := range subs {
			snapshot[id] = cb
		}
		mu.Unlock()

		var wg sync.WaitGroup
		var resultMu sync.Mutex
		notified := make([]string, 0, len(snapshot))

		for id, cb := range snapshot {
			wg.Add(1)
			go func(id string, cb func(string)) {
				defer wg.Done()
				cb(key)
				resultMu.Lock()
				notified = append(notified, id)
				resultMu.Unlock()
			}(id, cb)
		}
		wg.Wait()

		sort.Strings(notified)
		return notified
	}

	return subscribe, invalidate
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/cache-invalidation-broadcast"
)

func main() {
	subscribe, invalidate := broadcaster.NewBroadcaster()

	var mu sync.Mutex
	seen := map[string]string{}

	subscribe("edge-node-1", func(key string) {
		mu.Lock()
		seen["edge-node-1"] = key
		mu.Unlock()
	})
	subscribe("edge-node-2", func(key string) {
		mu.Lock()
		seen["edge-node-2"] = key
		mu.Unlock()
	})
	subscribe("search-index", func(key string) {
		mu.Lock()
		seen["search-index"] = key
		mu.Unlock()
	})

	notified := invalidate("user:42")
	fmt.Println("notified:", notified)
	fmt.Println("edge-node-1 saw:", seen["edge-node-1"])
	fmt.Println("edge-node-2 saw:", seen["edge-node-2"])
	fmt.Println("search-index saw:", seen["search-index"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
notified: [edge-node-1 edge-node-2 search-index]
edge-node-1 saw: user:42
edge-node-2 saw: user:42
search-index saw: user:42
```

### Tests

Create `broadcaster_test.go`:

```go
package broadcaster

import (
	"reflect"
	"sync"
	"testing"
)

func TestInvalidateNotifiesAllSubscribersWithKey(t *testing.T) {
	subscribe, invalidate := NewBroadcaster()

	var mu sync.Mutex
	seen := map[string]string{}
	record := func(id string) func(string) {
		return func(key string) {
			mu.Lock()
			defer mu.Unlock()
			seen[id] = key
		}
	}

	subscribe("a", record("a"))
	subscribe("b", record("b"))
	subscribe("c", record("c"))

	notified := invalidate("cache-key-1")

	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(notified, want) {
		t.Fatalf("notified = %v, want %v", notified, want)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range want {
		if seen[id] != "cache-key-1" {
			t.Errorf("subscriber %q saw key %q, want %q", id, seen[id], "cache-key-1")
		}
	}
}

func TestInvalidateWithNoSubscribersReturnsEmpty(t *testing.T) {
	_, invalidate := NewBroadcaster()

	notified := invalidate("anything")
	if len(notified) != 0 {
		t.Fatalf("notified = %v, want empty", notified)
	}
}

func TestTwoBroadcastersDoNotShareSubscribers(t *testing.T) {
	subscribeA, invalidateA := NewBroadcaster()
	_, invalidateB := NewBroadcaster()

	subscribeA("only-a", func(string) {})

	if got := invalidateA("k"); len(got) != 1 {
		t.Fatalf("broadcaster A notified = %v, want 1 subscriber", got)
	}
	if got := invalidateB("k"); len(got) != 0 {
		t.Fatalf("broadcaster B notified = %v, want 0 — broadcasters must not share subscribers", got)
	}
}

func TestInvalidateRunsSubscribersConcurrentlyAndWaitsForAll(t *testing.T) {
	subscribe, invalidate := NewBroadcaster()

	const subscribers = 50
	var mu sync.Mutex
	count := 0

	for i := range subscribers {
		id := string(rune('a' + i%26))
		subscribe(id+string(rune('0'+i/26)), func(string) {
			mu.Lock()
			count++
			mu.Unlock()
		})
	}

	notified := invalidate("burst")
	if len(notified) != subscribers {
		t.Fatalf("notified count = %d, want %d", len(notified), subscribers)
	}

	mu.Lock()
	defer mu.Unlock()
	if count != subscribers {
		t.Fatalf("callbacks executed = %d, want %d (invalidate must wait for all subscribers)", count, subscribers)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first test proves the core contract: every subscriber sees the
invalidated key, and the returned ID list is sorted so the test does not have
to guess goroutine completion order. The empty-broadcaster test is the
trivial edge case a fan-out mechanism must not panic or hang on. The
isolation test repeats the structural guarantee every factory in this lesson
relies on. The concurrency test is the one that would fail if `invalidate`
returned before every goroutine finished, or if the shared `notified` slice
were appended to without its own lock — `-race` catches the second, and the
count assertion catches the first.

## Resources

- [pkg.go.dev: sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — how `invalidate` waits for every subscriber goroutine before returning.
- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding both the subscriber registry and the shared results slice.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how `subscribe` and `invalidate` share one captured registry.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-async-job-queue-backpressure-gate.md](17-async-job-queue-backpressure-gate.md) | Next: [19-tls-certificate-auto-refresh-rotation.md](19-tls-certificate-auto-refresh-rotation.md)
