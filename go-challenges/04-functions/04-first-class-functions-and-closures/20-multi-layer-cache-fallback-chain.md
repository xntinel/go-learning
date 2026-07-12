# Exercise 20: Multi-Layer Cache Fallback Chain (Memory → Redis → DB)

**Nivel: Intermedio** — validacion rapida (un test corto).

A read path that checks an in-process map, then Redis, then the database is
one of the most common shapes in backend caching: try the cheapest tier
first and fall through only on a miss. `NewLookup` compiles the ordered list
of tiers exactly once into a captured slice, and returns a single closure
that tries each in turn and returns the first hit — no per-call branching on
which tiers exist, because the chain itself is fixed at construction time.

## What you'll build

```text
cachechain/                independent module: example.com/multi-layer-cache-fallback
  go.mod                   go 1.24
  cachechain.go            Layer type, NewLookup returns func(key) (string, bool)
  cmd/
    demo/
      main.go               memory/redis/db layers over three keys
  cachechain_test.go        table test: hit order, empty chain, short-circuit
```

- Files: `cachechain.go`, `cmd/demo/main.go`, `cachechain_test.go`.
- Implement: `type Layer func(key string) (string, bool)` and `NewLookup(layers ...Layer) func(key string) (string, bool)`, closing over a defensively-copied slice of layers tried in order.
- Test: a table proves a hit in each tier short-circuits the rest; a zero-layer chain always misses; a layer after the first hit is never called.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Compile the chain once, walk it on every call

`NewLookup` takes a variadic `...Layer` and copies it into `chain` exactly
once, at construction time — not on every lookup. This is the "compile the
rules once" idea in its simplest form: the ordering decision (which tier is
cheapest, which is authoritative) is made a single time when the lookup
pipeline is built, and every subsequent call is just a loop over the already-
decided order. The returned closure captures `chain`, not the original
`layers` slice, so a caller who kept a reference to the slice it passed in
and later appended to it cannot silently change an already-built lookup's
behavior.

The loop itself is a short-circuit: it calls each `Layer` in order and
returns on the first one that reports a hit (`ok == true`), never touching
the remaining layers. This is what makes the ordering matter operationally —
memory first because it's free, Redis second because it's a network hop but
still cheap, the database last because it's the expensive, always-correct
source of truth. A chain with zero layers is a valid, if useless, lookup that
always misses; the tests cover it as the boundary case.

Create `cachechain.go`:

```go
package cachechain

// Layer looks up key in one cache tier and reports whether it was found.
type Layer func(key string) (string, bool)

// NewLookup compiles an ordered chain of layers once and returns a single
// closure that, on every call, tries each layer in order and returns the
// first hit. layers is captured by the closure (copied defensively) so the
// caller cannot mutate the chain after NewLookup returns.
//
// A typical chain is (in-process memory, Redis, database): the closure tries
// memory first because it's free, then Redis, then falls all the way through
// to the database, which is assumed to always have an answer or explicitly
// report a miss.
func NewLookup(layers ...Layer) func(key string) (string, bool) {
	chain := make([]Layer, len(layers))
	copy(chain, layers)

	return func(key string) (string, bool) {
		for _, layer := range chain {
			if value, ok := layer(key); ok {
				return value, true
			}
		}
		return "", false
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/multi-layer-cache-fallback"
)

func main() {
	memory := map[string]string{"user:1": "alice"}
	redis := map[string]string{"user:2": "bob"}
	db := map[string]string{"user:1": "alice", "user:2": "bob", "user:3": "carol"}

	lookup := cachechain.NewLookup(
		func(key string) (string, bool) { v, ok := memory[key]; return v, ok },
		func(key string) (string, bool) { v, ok := redis[key]; return v, ok },
		func(key string) (string, bool) { v, ok := db[key]; return v, ok },
	)

	for _, key := range []string{"user:1", "user:2", "user:3", "user:404"} {
		value, ok := lookup(key)
		fmt.Printf("%s: value=%q found=%v\n", key, value, ok)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:1: value="alice" found=true
user:2: value="bob" found=true
user:3: value="carol" found=true
user:404: value="" found=false
```

### Tests

Create `cachechain_test.go`:

```go
package cachechain

import "testing"

func TestLookupReturnsFirstHitInOrder(t *testing.T) {
	memory := map[string]string{"a": "from-memory"}
	redis := map[string]string{"a": "from-redis", "b": "from-redis"}
	db := map[string]string{"a": "from-db", "b": "from-db", "c": "from-db"}

	memoryLayer := func(key string) (string, bool) { v, ok := memory[key]; return v, ok }
	redisLayer := func(key string) (string, bool) { v, ok := redis[key]; return v, ok }
	dbLayer := func(key string) (string, bool) { v, ok := db[key]; return v, ok }

	lookup := NewLookup(memoryLayer, redisLayer, dbLayer)

	tests := []struct {
		name      string
		key       string
		wantValue string
		wantFound bool
	}{
		{"hit in first layer (memory)", "a", "from-memory", true},
		{"miss memory, hit redis", "b", "from-redis", true},
		{"miss memory and redis, hit db", "c", "from-db", true},
		{"miss all layers", "z", "", false},
	}

	for _, tc := range tests {
		value, found := lookup(tc.key)
		if value != tc.wantValue || found != tc.wantFound {
			t.Fatalf("%s: lookup(%q) = (%q, %v), want (%q, %v)",
				tc.name, tc.key, value, found, tc.wantValue, tc.wantFound)
		}
	}
}

func TestLookupWithNoLayersAlwaysMisses(t *testing.T) {
	lookup := NewLookup()

	if _, found := lookup("anything"); found {
		t.Fatal("lookup with no layers reported found, want a miss")
	}
}

func TestLookupDoesNotCallLayersAfterFirstHit(t *testing.T) {
	calledThird := false

	first := func(key string) (string, bool) { return "hit", true }
	third := func(key string) (string, bool) { calledThird = true; return "unused", true }

	lookup := NewLookup(first, third)
	value, found := lookup("x")

	if value != "hit" || !found {
		t.Fatalf("lookup(%q) = (%q, %v), want (%q, %v)", "x", value, found, "hit", true)
	}
	if calledThird {
		t.Fatal("layer after the first hit was called, want short-circuit")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The main table proves the fallback order end to end: a hit in memory never
touches Redis or the database, a miss in memory but a hit in Redis never
touches the database, and a miss everywhere returns `false`. The empty-chain
test is the trivial boundary a compiled pipeline must still handle instead of
panicking on an empty slice. The short-circuit test is the one that catches a
subtle bug: a naive implementation that calls every layer to check for
correctness (say, to record a metric per tier) and only then decides which
result to return would call the third layer even after the first layer hit,
wasting a database round trip on every single request.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `Layer` type used to keep the chain self-documenting.
- [pkg.go.dev: Variadic functions](https://go.dev/ref/spec#Passing_arguments_to_..._parameters) — how `layers ...Layer` accepts any number of tiers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-tls-certificate-auto-refresh-rotation.md](19-tls-certificate-auto-refresh-rotation.md) | Next: [21-oauth-token-auto-refresh-guard.md](21-oauth-token-auto-refresh-guard.md)
