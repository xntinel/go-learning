# Exercise 6: Stateful Model-Based Testing of an LRU Cache

A bounded LRU cache is stateful code whose correctness is a property of the *whole
operation history*, not of any single call. `Get` mutates recency; `Put` may evict;
whether the right key is evicted depends on every access that came before. No
point assertion can test that. Model-based testing runs a random sequence of
operations against both the real cache and a simple, obviously-correct reference
model, and asserts they agree after every step — so the bug in the interleaving,
which is where cache bugs live, cannot hide. This exercise builds the cache and
tests it with `pgregory.net/rapid`'s state-machine support.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
lrucache/                   independent module: example.com/lrucache
  go.mod                    go 1.26, requires pgregory.net/rapid
  lrucache.go               type LRU[K,V]; New, Get, Put, Len (container/list based)
  cmd/
    demo/
      main.go               runnable demo: fill, access, evict
  lrucache_test.go          rapid StateMachine model-based test + inline t.Repeat form
```

Files: `lrucache.go`, `cmd/demo/main.go`, `lrucache_test.go`.
Implement: a generic `LRU[K,V]` with O(1) `Get`/`Put` over `container/list` plus a `map`, capacity eviction of the least-recently-used entry.
Test: a `rapid.StateMachine` driving random `Get`/`Put` sequences against a slow-but-obvious slice+map model, checking length, capacity, values, and eviction order after every action; plus the same test written with the inline `t.Repeat(map[string]func)` form.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lrucache/cmd/demo
cd ~/go-exercises/lrucache
go mod init example.com/lrucache
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### The real cache, the model, and the invariant checked after every step

The production cache is the fast one: a `container/list` holds entries in
recency order (front is most-recently-used, back is least), and a `map` indexes
list elements for O(1) lookup. `Get` moves the element to the front and returns its
value; `Put` updates and moves to front if present, otherwise inserts at the front
and, if at capacity, evicts the back element. This is the standard O(1) LRU, and it
is easy to get the eviction order subtly wrong.

The model is the opposite: correct by inspection, speed irrelevant. It keeps a
`[]int` of keys ordered least-to-most recently used and a `map[int]int` of values.
Every operation is O(n) — a linear scan to move a key to the end — but there is
nothing to get wrong. The model *is* the specification of what an LRU must do.

The state machine drives both. Each action (`Get`, `Put`) draws its arguments from
`rapid` and applies the *same* operation to both the real cache and the model, so
they stay in lockstep; the actions that return a value assert the real and model
results match. The `Check` method — run by rapid before and after every action — is
where the whole-history invariant lives: real length equals model length, length
never exceeds capacity, every key the model holds is present in the real cache with
the same value (checked with a non-mutating `peek`, so the invariant check does not
itself disturb recency), and — the property that catches eviction bugs — the real
cache's keys in recency order exactly match the model's order slice. Keys are drawn
from a small range (0–9) so collisions, updates, and evictions actually happen; a
wide key range would rarely re-access a key and never exercise eviction order.

When a bug exists — say `Put` evicts from the front instead of the back, or `Get`
forgets to move the element — rapid finds a violating sequence and *shrinks* it to
the minimal sequence of operations that still breaks the invariant, often three or
four calls you can trace by hand.

Create `lrucache.go`:

```go
package lrucache

import "container/list"

type node[K comparable, V any] struct {
	key K
	val V
}

// LRU is a bounded least-recently-used cache with O(1) Get and Put. The list holds
// entries in recency order (front = most recent, back = least); the map indexes
// list elements for O(1) lookup.
type LRU[K comparable, V any] struct {
	cap   int
	ll    *list.List
	index map[K]*list.Element
}

// New returns an empty cache holding at most capacity entries (minimum 1).
func New[K comparable, V any](capacity int) *LRU[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU[K, V]{cap: capacity, ll: list.New(), index: make(map[K]*list.Element)}
}

// Put inserts or updates key, marking it most-recently-used and evicting the
// least-recently-used entry if the cache is at capacity.
func (c *LRU[K, V]) Put(k K, v V) {
	if el, ok := c.index[k]; ok {
		el.Value.(*node[K, V]).val = v
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= c.cap {
		if back := c.ll.Back(); back != nil {
			delete(c.index, back.Value.(*node[K, V]).key)
			c.ll.Remove(back)
		}
	}
	c.index[k] = c.ll.PushFront(&node[K, V]{key: k, val: v})
}

// Get returns the value for key and marks it most-recently-used.
func (c *LRU[K, V]) Get(k K) (V, bool) {
	if el, ok := c.index[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*node[K, V]).val, true
	}
	var zero V
	return zero, false
}

// Len reports the number of entries currently held.
func (c *LRU[K, V]) Len() int { return c.ll.Len() }
```

The test needs two read-only views the public API does not expose: a `peek` that
reads a value without touching recency (so the invariant check is side-effect free)
and the keys in recency order. Because the test lives in the same package, add them
as unexported methods.

Create `lrucache_peek.go`:

```go
package lrucache

// peek returns the value for key without changing recency; used only by invariant
// checks in tests.
func (c *LRU[K, V]) peek(k K) (V, bool) {
	if el, ok := c.index[k]; ok {
		return el.Value.(*node[K, V]).val, true
	}
	var zero V
	return zero, false
}

// keysInOrder returns keys from least- to most-recently-used, matching the model's
// order slice.
func (c *LRU[K, V]) keysInOrder() []K {
	out := make([]K, 0, c.ll.Len())
	for e := c.ll.Back(); e != nil; e = e.Prev() {
		out = append(out, e.Value.(*node[K, V]).key)
	}
	return out
}
```

### The runnable demo

The demo fills a cache of capacity two, touches the older key to make it recent,
inserts a third key, and shows which entry was evicted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lrucache"
)

func main() {
	c := lrucache.New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")    // "a" is now most-recently-used; "b" is the eviction victim
	c.Put("c", 3) // evicts "b"

	_, aOK := c.Get("a")
	_, bOK := c.Get("b")
	_, cOK := c.Get("c")
	fmt.Printf("len=%d a=%v b=%v c=%v\n", c.Len(), aOK, bOK, cOK)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len=2 a=true b=false c=true
```

### The model-based tests

The `machine` struct wraps the real cache and the model. Its `Get` and `Put`
methods are the actions (each `Name(t *rapid.T)`); `Check` holds the invariants.
`rapid.StateMachineActions(sm)` reflects those methods into an action map, and
`t.Repeat` runs a random, shrinkable sequence of them. The second test shows the
equivalent inline form — `t.Repeat(map[string]func(*rapid.T){...})` with the `""`
key holding the invariant check — which is handier for a small machine and makes
explicit what `StateMachineActions` does by reflection.

Create `lrucache_test.go`:

```go
package lrucache

import (
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// modelLRU is the slow-but-obviously-correct reference: keys ordered LRU..MRU plus
// a value map.
type modelLRU struct {
	cap   int
	order []int
	vals  map[int]int
}

func newModel(cap int) *modelLRU { return &modelLRU{cap: cap, vals: map[int]int{}} }

func (m *modelLRU) touch(k int) {
	if i := slices.Index(m.order, k); i >= 0 {
		m.order = slices.Delete(m.order, i, i+1)
	}
	m.order = append(m.order, k)
}

func (m *modelLRU) put(k, v int) {
	if _, ok := m.vals[k]; ok {
		m.vals[k] = v
		m.touch(k)
		return
	}
	if len(m.order) >= m.cap {
		evict := m.order[0]
		m.order = m.order[1:]
		delete(m.vals, evict)
	}
	m.vals[k] = v
	m.touch(k)
}

func (m *modelLRU) get(k int) (int, bool) {
	v, ok := m.vals[k]
	if ok {
		m.touch(k)
	}
	return v, ok
}

type machine struct {
	real  *LRU[int, int]
	model *modelLRU
}

func (s *machine) Get(t *rapid.T) {
	k := rapid.IntRange(0, 9).Draw(t, "k")
	rv, rok := s.real.Get(k)
	mv, mok := s.model.get(k)
	if rok != mok || (rok && rv != mv) {
		t.Fatalf("Get(%d): real=(%d,%v) model=(%d,%v)", k, rv, rok, mv, mok)
	}
}

func (s *machine) Put(t *rapid.T) {
	k := rapid.IntRange(0, 9).Draw(t, "k")
	v := rapid.IntRange(0, 100).Draw(t, "v")
	s.real.Put(k, v)
	s.model.put(k, v)
}

// Check runs before and after every action: the whole-history invariant.
func (s *machine) Check(t *rapid.T) {
	if s.real.Len() != len(s.model.order) {
		t.Fatalf("len: real=%d model=%d", s.real.Len(), len(s.model.order))
	}
	if s.real.Len() > s.real.cap {
		t.Fatalf("len %d exceeds capacity %d", s.real.Len(), s.real.cap)
	}
	if got := s.real.keysInOrder(); !slices.Equal(got, s.model.order) {
		t.Fatalf("eviction order: real=%v model=%v", got, s.model.order)
	}
	for _, k := range s.model.order {
		rv, rok := s.real.peek(k)
		if !rok || rv != s.model.vals[k] {
			t.Fatalf("peek(%d): real=(%d,%v) model=%d", k, rv, rok, s.model.vals[k])
		}
	}
}

func TestLRUModelReflection(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		capacity := rapid.IntRange(1, 4).Draw(t, "cap")
		sm := &machine{real: New[int, int](capacity), model: newModel(capacity)}
		t.Repeat(rapid.StateMachineActions(sm))
	})
}

func TestLRUModelInline(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		capacity := rapid.IntRange(1, 4).Draw(t, "cap")
		sm := &machine{real: New[int, int](capacity), model: newModel(capacity)}
		t.Repeat(map[string]func(*rapid.T){
			"get": sm.Get,
			"put": sm.Put,
			"":    sm.Check, // the "" action runs before/after every other action
		})
	})
}

func ExampleLRU() {
	c := New[string, int](1)
	c.Put("a", 1)
	c.Put("b", 2) // evicts "a"
	_, ok := c.Get("a")
	fmt.Println(c.Len(), ok)
	// Output: 1 false
}
```

## Review

The cache is correct when a random sequence of `Get`/`Put` operations keeps it in
lockstep with the model on length, capacity, per-key values, and — the decisive one
— eviction order after every step. Model-based testing is the only honest way to
test stateful code: the bug is almost never in one operation, it is in the
interleaving, and running the real object beside a trivially-correct model over a
shrinkable random history is what surfaces it as a minimal, debuggable sequence.

The mistakes to avoid are specific to stateful properties. First, do not let the
invariant check mutate state: reading values through `Get` in `Check` would move
elements to the front and change the very recency the check is verifying — that is
why `peek` exists. Second, do not draw keys from a wide range: with keys unlikely
to repeat, you never re-access, never evict a still-referenced key, and never test
eviction order, so the model agrees trivially and the test proves nothing — a small
key space is what forces the interesting collisions. Third, keep the real and model
operations truly parallel: applying `Put` to one but not the other, or in a
different order, desynchronizes them and produces a failure that is a test bug, not
a cache bug. Run `go test -race`; a single cache instance is used per property run,
but the race detector still guards against accidental shared state.

## Resources

- [`container/list`](https://pkg.go.dev/container/list) — the doubly-linked list backing the O(1) recency order.
- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid#StateMachineActions) — `StateMachineActions`, `T.Repeat`, and the state-machine model.
- [rapid state-machine example](https://github.com/flyingmutant/rapid/blob/master/example_statemachine_test.go) — the canonical model-based test in the rapid source.

---

Back to [05-metamorphic-query-pipeline.md](05-metamorphic-query-pipeline.md) | Next: [07-custom-generators-and-shrinking.md](07-custom-generators-and-shrinking.md)
