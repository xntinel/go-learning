# Exercise 16: Hash Ring Endpoint Aggregator

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sharded cache or partitioned queue needs to route a key to one of several
partition endpoints, and it needs to do so the same way every time and from
every process. Consistent hashing solves this: place each endpoint at a few
virtual positions ("replicas") on a ring of hash values, then walk clockwise
from a key's hash to find its owner. The natural constructor for "seed the
ring with any number of endpoints" is variadic, and it must forward whatever
slice a caller already has without copying or mutating it.

## What you'll build

```text
hashring/                  independent module: example.com/hashring
  go.mod                   go 1.24
  hashring.go              package hashring; type Ring; New(endpoints ...string), Add(endpoints ...string), Get(key string)
  cmd/
    demo/
      main.go              runnable demo: route keys, then grow the ring
  hashring_test.go         table tests: determinism, splat-equals-direct-args, no-mutation, empty ring, coverage
```

- Files: `hashring.go`, `cmd/demo/main.go`, `hashring_test.go`.
- Implement: `New(endpoints ...string) *Ring` and `(*Ring).Add(endpoints ...string)`, both placing `replicas` virtual nodes per endpoint on a sorted hash ring; `(*Ring).Get(key string) (string, bool)` walks clockwise to the owning endpoint.
- Test: `Get` is deterministic for a fixed ring; `New(a,b,c)` and `New(existingSlice...)` agree on every key; `Add` never mutates its input slice; an empty ring reports `ok=false`; every key resolves to a registered endpoint.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/hashring/cmd/demo
cd ~/go-exercises/hashring
go mod init example.com/hashring
go mod edit -go=1.24
```

### Why the constructor is variadic and `Add` must not copy

`New(endpoints ...string)` exists so a caller can write `hashring.New("a",
"b", "c")` for a literal list, or `hashring.New(existing...)` to splat a
slice it already built elsewhere — a config loader, say, that produced
`[]string{"partition-a", "partition-b"}`. Both call shapes funnel into the
same unexported ring-building logic through `Add`, which is the single place
that knows about virtual nodes and the sort order. Centralizing it here means
`New` is just "empty ring, then `Add`" — there is no second code path that
could drift from the first.

`Add` reads `endpoints` and only reads it: it never writes into the caller's
backing array. That matters because a caller who splats a slice — `Add(existing
...)` — is trusting that the callee will not corrupt it. Each endpoint is
hashed at `replicas` distinct virtual positions (`"partition-a#0"`,
`"partition-a#1"`, ...) so that ownership of the ring is spread across
several points rather than one, which is what keeps the ring balanced when a
node joins or leaves. `Get` hashes the key, binary-searches the sorted hash
list with `sort.Search` for the first virtual node at or past that hash, and
wraps around to index 0 if the key's hash is past every node — the ring is
circular, not a line.

Create `hashring.go`:

```go
// hashring.go
package hashring

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// replicas is the number of virtual nodes placed on the ring per endpoint.
// More replicas spread ownership more evenly across the ring.
const replicas = 3

// Ring is a consistent-hash ring mapping keys to endpoints. The zero value is
// not usable; construct one with New.
type Ring struct {
	sortedHashes []uint32
	owner        map[uint32]string
}

// New builds a Ring seeded with any number of partition endpoints. Passing an
// existing []string via splat (New(existing...)) forwards the slice directly
// to Add without allocating an intermediate copy in the caller.
func New(endpoints ...string) *Ring {
	r := &Ring{owner: make(map[uint32]string)}
	r.Add(endpoints...)
	return r
}

// Add registers any number of additional endpoints, each placed at `replicas`
// virtual positions on the ring. Add never mutates endpoints; it only reads it.
func (r *Ring) Add(endpoints ...string) {
	for _, ep := range endpoints {
		for i := 0; i < replicas; i++ {
			h := hashKey(virtualKey(ep, i))
			r.sortedHashes = append(r.sortedHashes, h)
			r.owner[h] = ep
		}
	}
	sort.Slice(r.sortedHashes, func(i, j int) bool { return r.sortedHashes[i] < r.sortedHashes[j] })
}

// Get returns the endpoint that owns key: the first virtual node clockwise
// from key's hash, wrapping around the ring. ok is false for an empty ring.
func (r *Ring) Get(key string) (endpoint string, ok bool) {
	if len(r.sortedHashes) == 0 {
		return "", false
	}
	h := hashKey(key)
	idx := sort.Search(len(r.sortedHashes), func(i int) bool { return r.sortedHashes[i] >= h })
	if idx == len(r.sortedHashes) {
		idx = 0
	}
	return r.owner[r.sortedHashes[idx]], true
}

func virtualKey(endpoint string, replica int) string {
	return fmt.Sprintf("%s#%d", endpoint, replica)
}

func hashKey(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/hashring"
)

func main() {
	ring := hashring.New("partition-a", "partition-b", "partition-c")

	for _, key := range []string{"order-1", "order-2", "order-3", "order-4"} {
		ep, _ := ring.Get(key)
		fmt.Printf("%s -> %s\n", key, ep)
	}

	existing := []string{"partition-d", "partition-e"}
	ring.Add(existing...)
	ep, _ := ring.Get("order-1")
	fmt.Printf("after growing the ring, order-1 -> %s\n", ep)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
order-1 -> partition-b
order-2 -> partition-b
order-3 -> partition-b
order-4 -> partition-a
after growing the ring, order-1 -> partition-e
```

### Tests

`TestSplatFromSliceMatchesDirectArgs` is the one that proves the variadic
constructor's two call shapes are truly equivalent: a ring built from literal
arguments and one built by splatting an equivalent slice must route every
test key identically.

Create `hashring_test.go`:

```go
// hashring_test.go
package hashring

import (
	"slices"
	"testing"
)

func TestGetIsDeterministic(t *testing.T) {
	t.Parallel()

	r := New("a", "b", "c")
	first, ok := r.Get("order-42")
	if !ok {
		t.Fatalf("Get returned ok=false for a non-empty ring")
	}
	for i := 0; i < 10; i++ {
		got, ok := r.Get("order-42")
		if !ok || got != first {
			t.Fatalf("Get(%q) call %d = %q, ok=%v; want %q, ok=true", "order-42", i, got, ok, first)
		}
	}
}

func TestSplatFromSliceMatchesDirectArgs(t *testing.T) {
	t.Parallel()

	direct := New("a", "b", "c")

	existing := []string{"a", "b", "c"}
	splatted := New(existing...)

	for _, key := range []string{"order-1", "order-2", "order-3", "order-4", "order-5"} {
		want, _ := direct.Get(key)
		got, _ := splatted.Get(key)
		if got != want {
			t.Fatalf("Get(%q): direct-args ring = %q, splatted-slice ring = %q; want equal", key, want, got)
		}
	}
}

func TestAddDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	endpoints := []string{"a", "b", "c"}
	original := slices.Clone(endpoints)

	r := New()
	r.Add(endpoints...)

	if !slices.Equal(endpoints, original) {
		t.Fatalf("Add mutated its input: got %v, want %v", endpoints, original)
	}
}

func TestEmptyRingReturnsNotOK(t *testing.T) {
	t.Parallel()

	r := New()
	if _, ok := r.Get("anything"); ok {
		t.Fatalf("Get on an empty ring: ok = true, want false")
	}
}

func TestEveryKeyMapsToARegisteredEndpoint(t *testing.T) {
	t.Parallel()

	endpoints := []string{"a", "b", "c"}
	r := New(endpoints...)
	valid := make(map[string]bool)
	for _, ep := range endpoints {
		valid[ep] = true
	}

	for i := 0; i < 50; i++ {
		key := "key-" + string(rune('A'+i))
		got, ok := r.Get(key)
		if !ok || !valid[got] {
			t.Fatalf("Get(%q) = %q, ok=%v; want one of %v", key, got, ok, endpoints)
		}
	}
}
```

## Review

The ring is correct when `Get` is a pure, deterministic function of the
ring's current membership and the key — the same key always resolves to the
same endpoint until the membership changes, every key resolves to some
registered endpoint, and an empty ring reports `ok=false` rather than
panicking or returning a zero-value endpoint that looks valid. The senior
point is the aliasing discipline on the variadic parameter: `Add` must only
read `endpoints`, never write into it, because callers routinely splat a
slice they still hold a reference to elsewhere. The mistake to avoid is
sorting or truncating `endpoints` in place to "save an allocation" — that
corrupts the caller's slice the moment `Add(existing...)` is called with
`existing` aliased by the caller for something else.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — the non-cryptographic hash used to place endpoints on the ring.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — binary search for the first hash at or past a key's position.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-notification-fanout-recipients.md](15-notification-fanout-recipients.md) | Next: [17-tenant-key-sharding-variadic.md](17-tenant-key-sharding-variadic.md)
