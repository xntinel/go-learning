# Exercise 4: Per-tenant client registry with thundering-herd-safe lazy init

Multi-tenant backends lazily build one expensive resource per tenant — a database
pool, an upstream gRPC client, a signed API session — and share it across every
request for that tenant. The bug that shows up under load is the thundering herd:
a burst of concurrent first-requests for a cold tenant each build their own
client, opening dozens of connections when one was wanted. This module builds the
registry that fixes it: a cold tenant triggers exactly one construction no matter
how many callers arrive at once.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tenantreg/                    independent module: example.com/tenantreg
  go.mod                      go 1.26
  registry.go                 type Client, type Registry; NewRegistry, Client
  cmd/
    demo/
      main.go                 runnable demo: same tenant shares, distinct tenants differ
  registry_test.go            200-goroutine single-build race test, distinct-tenant test, Example
```

- Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
- Implement: `Registry` with `Client(tenant string) *Client`, building each tenant's client at most once and sharing it.
- Test: 200 goroutines call `Client(sameTenant)`; a build counter equals exactly 1 and every returned pointer is identical; distinct tenants build independently.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/04-connection-registry && cd go-solutions/15-sync-primitives/04-sync-map/04-connection-registry
```

### Why LoadOrStore a *sync.Once, not the client

The tempting one-liner is `LoadOrStore(tenant, buildClient(tenant))`. It is wrong
for the exact reason spelled out in the concepts: Go evaluates `buildClient(tenant)`
*before* `LoadOrStore` runs, so the expensive construction happens on every call,
even a cache hit — the map then throws away all but one result, but you already
paid to build them all. That is the thundering herd, not a fix for it.

The correct structure separates "who inserted the entry" from "who runs the build".
Each map value is a small `*lazyEntry` holding a `sync.Once` and an
`atomic.Pointer[Client]`. `Client(tenant)` does
`LoadOrStore(tenant, &lazyEntry{})` — cheap, allocates only an empty entry — then
calls `entry.once.Do(func(){ ... build ... })`. `sync.Once` guarantees the build
runs exactly once and, critically, that every other goroutine calling `once.Do`
*blocks until that one build completes* before returning. So the second through
two-hundredth caller for a cold tenant all wait on the same `Once`, and when they
proceed the client is already built and stored. Every caller then reads the shared
client from the `atomic.Pointer` and returns the identical pointer.

Two synchronization tools are doing distinct jobs here. `LoadOrStore` makes "insert
this tenant's entry" atomic so all callers agree on one `*lazyEntry`. `sync.Once`
makes "build this tenant's client" happen exactly once and publishes it with a
happens-before edge to every waiter. The `atomic.Pointer` is how the built client
crosses from the building goroutine to the readers; because the store happens
inside `once.Do` and the loads happen after `once.Do` returns, the read always sees
a fully-constructed client, never a nil or half-built one.

Create `registry.go`:

```go
package tenantreg

import (
	"sync"
	"sync/atomic"
)

// Client is a stand-in for an expensive per-tenant resource: a DB pool, an
// upstream client, a signed session. Building one is costly, so we build at
// most one per tenant and share it.
type Client struct {
	Tenant string
	id     int64
}

// ID returns the unique build id assigned when this client was constructed.
func (c *Client) ID() int64 { return c.id }

type lazyEntry struct {
	once   sync.Once
	client atomic.Pointer[Client]
}

// Registry lazily builds and caches one Client per tenant. It is safe for
// concurrent use and guarantees exactly one build per tenant even under a burst
// of concurrent first-requests (no thundering herd).
type Registry struct {
	entries sync.Map // map[string]*lazyEntry
	build   func(tenant string) *Client
}

// NewRegistry returns a Registry that constructs clients with build. build is
// called at most once per tenant.
func NewRegistry(build func(tenant string) *Client) *Registry {
	return &Registry{build: build}
}

// Client returns the shared Client for tenant, constructing it on first use.
// Concurrent first-callers for the same cold tenant all receive the one client
// that a single build produced.
func (r *Registry) Client(tenant string) *Client {
	actual, _ := r.entries.LoadOrStore(tenant, &lazyEntry{})
	e := actual.(*lazyEntry)
	e.once.Do(func() {
		e.client.Store(r.build(tenant))
	})
	return e.client.Load()
}
```

### The runnable demo

Because `Client.id` is unexported, a separate `package main` cannot set it. Expose
construction through an exported constructor. Append to `registry.go`:

```go
// NewClient builds a Client with an explicit id. It exists so external callers
// (and the demo) can construct clients without touching unexported fields.
func NewClient(tenant string, id int64) *Client {
	return &Client{Tenant: tenant, id: id}
}
```

The demo builds a registry whose `build` assigns a monotonically increasing id, so
you can see that two calls for the same tenant return the same id (one build) while
a different tenant gets a fresh id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/tenantreg"
)

func main() {
	var next atomic.Int64
	reg := tenantreg.NewRegistry(func(tenant string) *tenantreg.Client {
		return tenantreg.NewClient(tenant, next.Add(1))
	})

	a1 := reg.Client("acme")
	a2 := reg.Client("acme")
	b1 := reg.Client("globex")

	fmt.Printf("acme first build id:  %d\n", a1.ID())
	fmt.Printf("acme second call id:  %d (same client: %v)\n", a2.ID(), a1 == a2)
	fmt.Printf("globex build id:      %d\n", b1.ID())
	fmt.Printf("total builds:         %d\n", next.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme first build id:  1
acme second call id:  1 (same client: true)
globex build id:      2
total builds:         2
```

### Tests

`TestSingleBuildUnderHerd` is the contract: 200 goroutines call `Client` for the
same cold tenant concurrently; the build function increments an `atomic.Int64`, and
after `wg.Wait` that counter must be exactly 1 and every one of the 200 returned
pointers must be identical. `TestDistinctTenantsBuildIndependently` confirms two
tenants each build once. `-race` proves the whole path is clean.

Create `registry_test.go`:

```go
package tenantreg

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSingleBuildUnderHerd(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	reg := NewRegistry(func(tenant string) *Client {
		return NewClient(tenant, builds.Add(1))
	})

	const goroutines = 200
	got := make([]*Client, goroutines)
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = reg.Client("acme")
		}()
	}
	wg.Wait()

	if n := builds.Load(); n != 1 {
		t.Fatalf("build ran %d times, want exactly 1 (thundering herd)", n)
	}
	first := got[0]
	if first == nil {
		t.Fatal("got a nil client")
	}
	for i, c := range got {
		if c != first {
			t.Fatalf("goroutine %d got a different client pointer; want all identical", i)
		}
	}
}

func TestDistinctTenantsBuildIndependently(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	reg := NewRegistry(func(tenant string) *Client {
		return NewClient(tenant, builds.Add(1))
	})

	a := reg.Client("a")
	b := reg.Client("b")
	if a == b {
		t.Fatal("distinct tenants returned the same client")
	}
	if a.Tenant != "a" || b.Tenant != "b" {
		t.Fatalf("tenants = %q,%q, want a,b", a.Tenant, b.Tenant)
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("build ran %d times for 2 tenants, want 2", n)
	}
	// A repeat call for an existing tenant does not rebuild.
	if again := reg.Client("a"); again != a {
		t.Fatal("second Client(a) rebuilt instead of sharing")
	}
	if n := builds.Load(); n != 2 {
		t.Fatalf("build ran %d times after a repeat call, want still 2", n)
	}
}

func ExampleRegistry() {
	var next atomic.Int64
	reg := NewRegistry(func(tenant string) *Client {
		return NewClient(tenant, next.Add(1))
	})
	c1 := reg.Client("acme")
	c2 := reg.Client("acme")
	fmt.Println(c1 == c2, next.Load())
	// Output: true 1
}
```

## Review

The registry is correct when a cold tenant triggers exactly one build regardless of
how many callers race in, and every caller returns the same client pointer. The
mechanism is two-layered and both layers matter: `LoadOrStore` makes all callers
agree on one `*lazyEntry`, and `sync.Once` makes the build run once while blocking
every other caller until it finishes, so no one reads a half-built client. The
central trap — the whole reason this exercise exists — is building the client as
the `LoadOrStore` argument: that runs the expensive constructor on every call and
defeats the point. Store a cheap `*lazyEntry` and build inside `once.Do`. The
`atomic.Pointer` publishes the built client with a happens-before edge so readers
never see nil. `TestSingleBuildUnderHerd` under `-race` is the proof: build count
exactly 1, all pointers identical, no data race.

## Resources

- [sync.Once](https://pkg.go.dev/sync#Once) — `Do` runs once and blocks concurrent callers until it returns.
- [sync.Map.LoadOrStore](https://pkg.go.dev/sync#Map.LoadOrStore) — the atomic insert of the lazy entry.
- [sync/atomic Pointer](https://pkg.go.dev/sync/atomic#Pointer) — publishing the built client across goroutines.

---

Back to [03-generic-typed-map.md](03-generic-typed-map.md) | Next: [05-metrics-registry.md](05-metrics-registry.md)
