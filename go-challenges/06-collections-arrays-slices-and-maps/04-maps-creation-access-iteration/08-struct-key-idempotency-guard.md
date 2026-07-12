# Exercise 8: Comparable Struct Keys: an idempotency guard keyed by (tenant, requestID)

At-least-once delivery — webhooks, message queues, retried client calls — means
the same request can arrive twice, and a write handler must process it once. The
right data structure is a map keyed by a comparable struct. This exercise builds an
idempotency guard keyed by `struct{ Tenant, RequestID string }`, using comma-ok to
short-circuit duplicates, and explains the map-key comparability rules.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
idemguard/                 independent module: example.com/idemguard
  go.mod
  guard.go                 RequestKey struct key, Result; Guard.Process (comma-ok dedup), Seen
  cmd/
    demo/
      main.go              runnable demo: first delivery charges, duplicate is cached, other tenant distinct
  guard_test.go            first processes, duplicate cached, tenant isolation, key equality, -race
```

- Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
- Implement: `Process(key, handler)` that runs the handler once per key and returns the cached result on duplicates, keyed by a comparable `RequestKey` struct.
- Test: first delivery processes and records, a duplicate returns the cached result without reprocessing (side-effect counter unchanged), a different tenant with the same request ID is distinct, struct keys compare with `==`, and concurrent duplicate delivery is race-free.
- Verify: `go test -count=1 -race ./...`

### Why a struct key, and the comparability rule

The dedup question is "have I already processed this (tenant, requestID) pair?".
A struct key answers it directly: `RequestKey{Tenant, RequestID string}` is a
valid map key because both fields are comparable, and two keys are equal under
`==` exactly when both fields match. That gives an O(1) composite lookup with clean
value semantics — no delimiter-joined string like `tenant + ":" + requestID`, which
is slower, allocates, and breaks the moment a tenant name contains the delimiter.

The comparability rule is the load-bearing fact. A map key type must be comparable
with `==`. Structs and arrays qualify when *all* their fields/elements are
comparable; slices, maps, and functions never are. So `struct{ Tenant, RequestID string }`
is a fine key, but adding a `Tags []string` field to it would make the struct
non-comparable and fail to compile the moment you used it as a map key — a
compile-time guarantee, not a runtime surprise:

```text
// Won't compile if used as a map key:
type BadKey struct {
	Tenant string
	Tags   []string // slice field makes BadKey non-comparable
}
// var m map[BadKey]int  // error: invalid map key type BadKey
```

`Process` takes the request key and a handler thunk. It reads the cache with
comma-ok: on a hit it returns the stored `Result` and `true` (cached) without
calling the handler at all — that is the idempotency; on a miss it runs the
handler exactly once, records the result, and returns it with `false`. The whole
guard is serialized by a mutex, so even a duplicate that races itself runs the
handler once.

Create `guard.go`:

```go
package idemguard

import "sync"

// RequestKey is a comparable composite key. Both fields are comparable, so the
// struct is a valid map key and two keys are equal exactly when both match.
type RequestKey struct {
	Tenant    string
	RequestID string
}

// Result is the recorded outcome of processing a request.
type Result struct {
	Status int
	Body   string
}

// Guard deduplicates at-least-once deliveries keyed by RequestKey.
type Guard struct {
	mu   sync.Mutex
	seen map[RequestKey]Result
}

// New returns an empty Guard.
func New() *Guard {
	return &Guard{seen: make(map[RequestKey]Result)}
}

// Process runs handler at most once per key. On a duplicate it returns the
// cached result and cached=true without invoking handler; on first sight it runs
// handler, records the result, and returns cached=false.
func (g *Guard) Process(key RequestKey, handler func() Result) (result Result, cached bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.seen[key]; ok {
		return r, true
	}
	r := handler()
	g.seen[key] = r
	return r, false
}

// Seen reports whether key has already been processed.
func (g *Guard) Seen(key RequestKey) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.seen[key]
	return ok
}
```

### The runnable demo

The demo processes a webhook, then a duplicate of it (showing the handler's side
effect runs only once), then the same request ID under a different tenant (showing
the composite key keeps them distinct).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idemguard"
)

func main() {
	g := idemguard.New()
	key := idemguard.RequestKey{Tenant: "acme", RequestID: "evt-1"}

	charges := 0
	handler := func() idemguard.Result {
		charges++ // the real side effect we must not repeat
		return idemguard.Result{Status: 200, Body: "charged"}
	}

	r1, cached1 := g.Process(key, handler)
	r2, cached2 := g.Process(key, handler) // duplicate delivery
	fmt.Printf("first:  status=%d cached=%v\n", r1.Status, cached1)
	fmt.Printf("second: status=%d cached=%v\n", r2.Status, cached2)
	fmt.Printf("charges executed: %d\n", charges)

	other := idemguard.RequestKey{Tenant: "globex", RequestID: "evt-1"}
	_, cached3 := g.Process(other, handler)
	fmt.Printf("other tenant same id: cached=%v\n", cached3)
	fmt.Printf("charges executed: %d\n", charges)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first:  status=200 cached=false
second: status=200 cached=true
charges executed: 1
other tenant same id: cached=false
charges executed: 2
```

The duplicate delivery returns the cached result and does not re-run the handler,
so `charges` stays at `1`. The same request ID under `globex` is a different key,
so it processes independently and `charges` becomes `2`.

### Tests

`TestFirstProcesses` proves the first delivery runs the handler and records the
result. `TestDuplicateCached` is the core contract: a second delivery of the same
key returns the cached result and leaves the side-effect counter untouched.
`TestDifferentTenantDistinct` proves the composite key isolates tenants.
`TestKeyEquality` shows struct keys compare with `==`. `TestConcurrentDuplicate`
fires many goroutines at the same key under `-race` and asserts the handler ran
exactly once.

Create `guard_test.go`:

```go
package idemguard

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFirstProcesses(t *testing.T) {
	t.Parallel()

	g := New()
	key := RequestKey{Tenant: "t", RequestID: "r"}
	calls := 0
	r, cached := g.Process(key, func() Result {
		calls++
		return Result{Status: 201, Body: "ok"}
	})
	if cached {
		t.Fatal("first delivery should not be cached")
	}
	if r.Status != 201 || calls != 1 {
		t.Fatalf("status=%d calls=%d, want 201 and 1", r.Status, calls)
	}
}

func TestDuplicateCached(t *testing.T) {
	t.Parallel()

	g := New()
	key := RequestKey{Tenant: "t", RequestID: "r"}
	calls := 0
	handler := func() Result {
		calls++
		return Result{Status: 200, Body: "done"}
	}

	g.Process(key, handler)
	r, cached := g.Process(key, handler) // duplicate
	if !cached {
		t.Fatal("duplicate should be cached")
	}
	if r.Body != "done" {
		t.Fatalf("cached body = %q, want done", r.Body)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1 (side effect must not repeat)", calls)
	}
}

func TestDifferentTenantDistinct(t *testing.T) {
	t.Parallel()

	g := New()
	a := RequestKey{Tenant: "acme", RequestID: "evt-1"}
	b := RequestKey{Tenant: "globex", RequestID: "evt-1"}

	calls := 0
	handler := func() Result { calls++; return Result{Status: 200} }

	g.Process(a, handler)
	_, cached := g.Process(b, handler)
	if cached {
		t.Fatal("same request ID under a different tenant must be distinct")
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times, want 2", calls)
	}
}

func TestKeyEquality(t *testing.T) {
	t.Parallel()

	a := RequestKey{Tenant: "t", RequestID: "r"}
	b := RequestKey{Tenant: "t", RequestID: "r"}
	c := RequestKey{Tenant: "t", RequestID: "s"}
	if a != b {
		t.Fatal("equal struct keys should compare equal")
	}
	if a == c {
		t.Fatal("differing struct keys should compare unequal")
	}
}

func TestConcurrentDuplicate(t *testing.T) {
	t.Parallel()

	g := New()
	key := RequestKey{Tenant: "t", RequestID: "r"}
	var calls atomic.Int64

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Process(key, func() Result {
				calls.Add(1)
				return Result{Status: 200}
			})
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", got)
	}
}

func ExampleGuard_Process() {
	g := New()
	key := RequestKey{Tenant: "acme", RequestID: "evt-1"}
	h := func() Result { return Result{Status: 200} }
	_, c1 := g.Process(key, h)
	_, c2 := g.Process(key, h)
	fmt.Println(c1, c2)
	// Output: false true
}
```

## Review

The guard is correct when the handler runs exactly once per distinct key and every
duplicate returns the recorded result: `TestDuplicateCached` asserts the side-effect
counter stays at one, which is the entire point of idempotency. The composite struct
key is what makes "(tenant, requestID) seen before?" an O(1) comma-ok lookup with
`==` equality, and `TestDifferentTenantDistinct` proves the two fields together form
the identity. The comparability rule is the design constraint to remember: this key
works because both fields are comparable; a slice or map field would make the struct
non-comparable and fail to compile as a key, and the fix is to design a comparable
key, never to stringify. Run `go test -race` to confirm the mutex makes concurrent
duplicate deliveries collapse to a single handler run.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — struct comparability rules.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — key types must be comparable.
- [sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — the race-free counter in the concurrency test.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-nil-map-failure-modes.md](07-nil-map-failure-modes.md) | Next: [09-map-value-addressability-pointers.md](09-map-value-addressability-pointers.md)
