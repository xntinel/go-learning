# Exercise 1: Object Interning Pool

The evaluator allocates one object per literal and per intermediate result, so a tight numeric loop drowns the garbage collector in short-lived integer objects. This exercise builds the fix in isolation: an integer interning pool that hands back a shared pointer for the common small values and a fresh allocation for everything else. It is a pure data structure with no dependency on the rest of the interpreter, which is exactly why it goes first — every later evaluator change can lean on it immediately, and its correctness can be proven without a lexer or a parser in sight.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
object/
  pool.go             Integer, NewInteger, the [-5, 256] intern pool
  pool_test.go        pooled-vs-unpooled pointer identity + boundaries
cmd/
  demo/
    main.go           print interned and non-interned pointer identity
```

- Files: `object/pool.go`, `object/pool_test.go`, `cmd/demo/main.go`.
- Implement: `Integer` with `ObjectType` and `Inspect`, the `intPoolMin`/`intPoolMax` bounds, the package-level pool array initialized in `init`, and `NewInteger`.
- Test: `pool_test.go` asserts pooled values return the same pointer, unpooled values return distinct pointers, every value round-trips, and the pool boundaries intern correctly.
- Verify: `go test -race ./...`

### Why interning, and why a fixed array

The cost the pool attacks is allocation volume, not allocation size. An `Integer` is a single `int64`, so each one is cheap to create — but `1 + 2` makes three of them, and a pass over a 100,000-element array makes hundreds of thousands. Each is a separate object the garbage collector must trace and reclaim, and that tracing is the bottleneck, not the bytes. Interning replaces the allocation entirely for the values that occur most: the constructor returns a pointer into a pre-built table instead of calling the allocator.

The pool is a Go array, not a slice and not a map, and that choice is load-bearing. A `[262]Integer` value is one contiguous block of memory; the garbage collector treats the whole array as a single object and scans it in one step regardless of how many of its elements are handed out. A map of cached integers, by contrast, would itself allocate, would need a lock, and would add a hash lookup to the hot path — defeating the purpose. Indexing the array by `value - intPoolMin` turns the lookup into a single subtraction and a bounds check, which is as cheap as object construction gets.

The range [-5, 256] is the empirically useful one. It spans every ASCII byte value and the small counters and offsets that dominate real loops; CPython interns exactly this range. Values outside it fall through to a normal heap allocation, so correctness never depends on a value being pooled — only performance does. The one rule the pool imposes on its callers is that pointer identity is a valid equality check only inside the range: two `NewInteger(1000)` calls return different pointers with the same value, so general equality must compare the value field and use the pointer check only as a fast path.

Create `object/pool.go`:

```go
package object

import "fmt"

// Integer is the Monkey integer value.
type Integer struct {
	Value int64
}

// ObjectType returns the type tag used by the evaluator's type switch.
func (i *Integer) ObjectType() string { return "INTEGER" }

// Inspect returns a printable representation for the REPL.
func (i *Integer) Inspect() string { return fmt.Sprintf("%d", i.Value) }

const (
	intPoolMin = -5
	intPoolMax = 256
)

// intPool holds pre-allocated Integer objects for the most common values.
// It is indexed by (value - intPoolMin): intPool[0] == -5, intPool[5] == 0,
// intPool[261] == 256.
var intPool [intPoolMax - intPoolMin + 1]Integer

func init() {
	for i := range intPool {
		intPool[i].Value = int64(i + intPoolMin)
	}
}

// NewInteger returns an interned *Integer for values in [intPoolMin, intPoolMax]
// and a fresh heap allocation for all other values. Interned values return the
// same pointer on every call, so pointer equality (p == q) is a valid fast-path
// equality check within the pool range.
func NewInteger(v int64) *Integer {
	if v >= intPoolMin && v <= intPoolMax {
		return &intPool[v-intPoolMin]
	}
	return &Integer{Value: v}
}
```

### The runnable demo

The demo makes the two halves of the contract visible side by side: a pooled value yields one shared pointer no matter how many times you ask for it, and an unpooled value yields a distinct allocation every time. The boolean it prints is the exact property the evaluator relies on when it uses `==` as a fast path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/object-interning-pool/object"
)

func main() {
	a := object.NewInteger(42)
	b := object.NewInteger(42)
	fmt.Printf("pooled 42:      same pointer = %t\n", a == b)

	c := object.NewInteger(1000)
	d := object.NewInteger(1000)
	fmt.Printf("unpooled 1000:  same pointer = %t\n", c == d)

	fmt.Printf("inspect:        %s\n", object.NewInteger(7).Inspect())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pooled 42:      same pointer = true
unpooled 1000:  same pointer = false
inspect:        7
```

### Tests

The tests pin both halves of the interning contract and, crucially, the two boundaries where it is easiest to be off by one. The pooled cases assert pointer identity; the unpooled cases assert pointer distinctness; the boundary test checks that `intPoolMin` and `intPoolMax` themselves are interned while one step past each is not. The value-preservation test guards against an indexing bug that would intern the wrong slot.

Create `object/pool_test.go`:

```go
package object

import (
	"fmt"
	"testing"
)

func TestNewIntegerInternsPooledValues(t *testing.T) {
	t.Parallel()

	cases := []int64{-5, -1, 0, 1, 42, 100, 200, 256}
	for _, v := range cases {
		a := NewInteger(v)
		b := NewInteger(v)
		if a != b {
			t.Errorf("NewInteger(%d): got different pointers for pooled value", v)
		}
	}
}

func TestNewIntegerAllocatesOutsidePool(t *testing.T) {
	t.Parallel()

	cases := []int64{-6, 257, 1000, -100, 1 << 30, -1 << 30}
	for _, v := range cases {
		a := NewInteger(v)
		b := NewInteger(v)
		if a == b {
			t.Errorf("NewInteger(%d): expected distinct pointers for non-pooled value", v)
		}
	}
}

func TestNewIntegerPreservesValue(t *testing.T) {
	t.Parallel()

	cases := []int64{-5, 0, 42, 256, 300, -10}
	for _, v := range cases {
		got := NewInteger(v).Value
		if got != v {
			t.Errorf("NewInteger(%d).Value = %d, want %d", v, got, v)
		}
	}
}

func TestNewIntegerPoolBoundary(t *testing.T) {
	t.Parallel()

	// One below pool min must not be interned.
	lo := NewInteger(intPoolMin - 1)
	lo2 := NewInteger(intPoolMin - 1)
	if lo == lo2 {
		t.Errorf("NewInteger(%d) below pool: expected distinct pointers", intPoolMin-1)
	}

	// One above pool max must not be interned.
	hi := NewInteger(intPoolMax + 1)
	hi2 := NewInteger(intPoolMax + 1)
	if hi == hi2 {
		t.Errorf("NewInteger(%d) above pool: expected distinct pointers", intPoolMax+1)
	}

	// Boundary values themselves must be interned.
	if NewInteger(intPoolMin) != NewInteger(intPoolMin) {
		t.Errorf("NewInteger(%d) at pool min: expected same pointer", intPoolMin)
	}
	if NewInteger(intPoolMax) != NewInteger(intPoolMax) {
		t.Errorf("NewInteger(%d) at pool max: expected same pointer", intPoolMax)
	}
}

func ExampleNewInteger() {
	a := NewInteger(42)
	b := NewInteger(42)
	fmt.Println(a == b) // same pointer from pool
	// Output:
	// true
}

func ExampleNewInteger_outsidePool() {
	a := NewInteger(1000)
	b := NewInteger(1000)
	fmt.Println(a == b) // distinct heap allocations
	// Output:
	// false
}
```

## Review

The pool is correct when pooled values are pointer-identical and unpooled values are not, and when both pool boundaries land on the right side of the line: `intPoolMin` and `intPoolMax` interned, one step past each freshly allocated. The value-preservation cases confirm the index arithmetic maps each value to its own slot rather than a neighbor's. Keep in mind what the pool does not promise: nothing outside [-5, 256] is interned, so any equality path the evaluator builds on top of `NewInteger` must compare the value field for the general case and treat the pointer comparison only as a fast path inside the pool. The array-of-values layout is the detail that makes the whole thing free for the garbage collector to trace; switching it to a slice of pointers or a map would reintroduce the allocations the pool exists to remove.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — chapter 3 introduces the object system this pool optimizes.
- [pkg.go.dev/fmt](https://pkg.go.dev/fmt) — `fmt.Sprintf` used in `Inspect`.
- [go.dev/ref/spec#Comparison_operators](https://go.dev/ref/spec#Comparison_operators) — the pointer-equality semantics that interning depends on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-module-cache-circular-import.md](02-module-cache-circular-import.md)
