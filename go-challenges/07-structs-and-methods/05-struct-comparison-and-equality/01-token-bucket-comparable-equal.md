# Exercise 1: Rate Limiter: Comparable Struct and a Custom Equal Contract

A token-bucket rate limiter is the smallest real artifact where struct equality
matters: you compare limiter snapshots in tests, and you want a stated equality
contract rather than an accident of field order. Because every field of this bucket
is comparable, `==` already works — so this exercise proves that a hand-written
`Equal` and the built-in `==` agree, and pins the contract that `Equal` compares
state, not pointer identity.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
tbucket/                    independent module: example.com/tbucket
  go.mod                    go 1.26
  tbucket.go                type Bucket{tokens,max int}; New, Take, Available, Max, Refill, Equal
  cmd/
    demo/
      main.go               runnable demo: take, compare snapshots, refill
  tbucket_test.go           equality-contract tests; asserts (a==b) == a.Equal(b)
```

- Files: `tbucket.go`, `cmd/demo/main.go`, `tbucket_test.go`.
- Implement: a `Bucket` with `New(max)`, `Take(n)`, `Available()`, `Max()`, `Refill()`, and a value-receiver `Equal(other Bucket) bool`.
- Test: fresh buckets equal; `Take` diverges state; pointer identity irrelevant; different `max` not equal; `(a == b) == a.Equal(b)` for the comparable case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tbucket/cmd/demo
cd ~/go-exercises/tbucket
go mod init example.com/tbucket
```

### Why a custom Equal when `==` already works

`Bucket` holds two `int` fields, so it is fully comparable and `a == b` is legal,
fast, and compile-checked. The custom `Equal` here is not compensating for a
missing capability — it is stating a contract. Three things fall out of writing it
as a value method that compares the fields:

- It is *reflexive, symmetric, transitive* by construction, because it delegates to
  `int` `==`. Those three properties are what dedup maps and sets silently rely on;
  a bucket used as a metrics key or in a snapshot-comparison must not violate them.
- Because the receiver is a *value* (`func (b Bucket) Equal`), it compares state and
  is blind to pointer identity. `New` returns a `*Bucket`, and comparing two
  `*Bucket` with `==` would compare *addresses* — always false for distinct
  allocations even when the state is identical. `a.Equal(*b)` asks the question you
  actually mean: do these two limiters have the same state?
- It documents which fields define identity. Today that is every field, so `Equal`
  and `==` coincide — and the test asserts exactly that: `(a == b) == a.Equal(b)`.
  The moment a future field is *derived* (a cached rate, a last-seen timestamp),
  `Equal` is the place to exclude it, and the test that pins the current agreement
  is what tells you the contract changed on purpose.

`Take` is `O(1)`. Two sentinel errors, wrapped and asserted with `errors.Is`, keep
the failure modes explicit: taking more than is available is `ErrEmpty`, taking a
non-positive amount is `ErrOverdraw`.

Create `tbucket.go`:

```go
package tbucket

import "errors"

// ErrEmpty is returned when a Take asks for more tokens than are available.
var ErrEmpty = errors.New("bucket is empty")

// ErrOverdraw is returned when a Take asks for a non-positive number of tokens.
var ErrOverdraw = errors.New("cannot take a non-positive number of tokens")

// Bucket is a token-bucket rate limiter. Both fields are comparable, so Bucket
// values may be compared with == directly; Equal states the same contract by hand.
type Bucket struct {
	tokens int
	max    int
}

// New returns a full bucket with capacity max (clamped to at least 1).
func New(max int) *Bucket {
	if max <= 0 {
		max = 1
	}
	return &Bucket{tokens: max, max: max}
}

// Take removes n tokens, or returns a sentinel error without mutating state.
func (b *Bucket) Take(n int) error {
	if n <= 0 {
		return ErrOverdraw
	}
	if b.tokens < n {
		return ErrEmpty
	}
	b.tokens -= n
	return nil
}

// Available reports the tokens currently in the bucket.
func (b *Bucket) Available() int { return b.tokens }

// Max reports the bucket capacity.
func (b *Bucket) Max() int { return b.max }

// Refill returns the bucket to full capacity.
func (b *Bucket) Refill() { b.tokens = b.max }

// Equal reports whether two buckets have the same state. It is a value receiver,
// so it compares state regardless of pointer identity.
func (b Bucket) Equal(other Bucket) bool {
	return b.tokens == other.tokens && b.max == other.max
}
```

### The runnable demo

The demo takes from one bucket, shows that a fresh identical bucket is `Equal`,
then diverges the state and shows they stop being equal, and finally that `Refill`
restores equality.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tbucket"
)

func main() {
	a := tbucket.New(10)
	b := tbucket.New(10)

	fmt.Printf("fresh equal: %v\n", a.Equal(*b))

	_ = a.Take(3)
	fmt.Printf("after Take(3) available: %d\n", a.Available())
	fmt.Printf("diverged equal: %v\n", a.Equal(*b))

	a.Refill()
	fmt.Printf("refilled equal: %v\n", a.Equal(*b))

	// == agrees with Equal for this fully comparable type.
	fmt.Printf("== agrees: %v\n", (*a == *b) == a.Equal(*b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fresh equal: true
after Take(3) available: 7
diverged equal: false
refilled equal: true
== agrees: true
```

### Tests

`TestEqualComparesState` is the main contract test: two fresh buckets of the same
capacity are equal, and a `Take` makes them unequal. `TestEqualIgnoresPointerIdentity`
proves a value `Equal` ignores the fact that `a` and `b` are distinct allocations.
`TestEqualWithDifferentMax` pins the "all identity fields count" contract — same
`tokens`, different `max`, not equal. `TestEqualAgreesWithDoubleEquals` is the
property that ties it together: for this fully comparable type, `a.Equal(b)` and
`a == b` must return the same answer for every state.

Create `tbucket_test.go`:

```go
package tbucket

import (
	"errors"
	"fmt"
	"testing"
)

func TestTakeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want error
	}{
		{"overdraw beyond capacity", 11, ErrEmpty},
		{"zero is rejected", 0, ErrOverdraw},
		{"negative is rejected", -1, ErrOverdraw},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := New(10)
			if err := b.Take(tt.n); !errors.Is(err, tt.want) {
				t.Fatalf("Take(%d) err = %v, want %v", tt.n, err, tt.want)
			}
			if b.Available() != 10 {
				t.Fatalf("failed Take mutated state: Available = %d, want 10", b.Available())
			}
		})
	}
}

func TestEqualComparesState(t *testing.T) {
	t.Parallel()

	a := New(10)
	b := New(10)
	if !a.Equal(*b) {
		t.Fatal("two fresh buckets with the same max should be equal")
	}
	if err := a.Take(3); err != nil {
		t.Fatal(err)
	}
	if a.Equal(*b) {
		t.Fatal("buckets with different available should not be equal")
	}
}

func TestEqualIgnoresPointerIdentity(t *testing.T) {
	t.Parallel()

	a := New(5)
	b := New(5)
	if a == b {
		t.Fatal("distinct allocations should not be pointer-equal")
	}
	if !a.Equal(*b) {
		t.Fatal("Equal should compare values, not pointer identity")
	}
}

func TestEqualWithDifferentMax(t *testing.T) {
	t.Parallel()

	a := New(10)
	b := New(20)
	// Force identical token counts but keep different capacities.
	if err := b.Take(10); err != nil {
		t.Fatal(err)
	}
	if a.Available() != b.Available() {
		t.Fatalf("precondition: tokens differ (%d vs %d)", a.Available(), b.Available())
	}
	if a.Equal(*b) {
		t.Fatal("equal tokens but different max must not be Equal")
	}
}

func TestEqualAgreesWithDoubleEquals(t *testing.T) {
	t.Parallel()

	states := []struct{ takeA, takeB int }{
		{0, 0}, {3, 0}, {3, 3}, {10, 1},
	}
	for _, s := range states {
		a, b := New(10), New(10)
		_ = a.Take(s.takeA)
		_ = b.Take(s.takeB)
		if got, want := a.Equal(*b), *a == *b; got != want {
			t.Fatalf("Equal=%v but ==%v for takes %+v", got, want, s)
		}
	}
}

func Example() {
	a := New(4)
	b := New(4)
	_ = a.Take(1)
	fmt.Println(a.Equal(*b), a.Available())
	// Output: false 3
}
```

## Review

The bucket is correct when `Equal` is a pure function of the two identity fields and
nothing else touches the answer. The load-bearing assertion is
`TestEqualAgreesWithDoubleEquals`: because every field is comparable, a correct
`Equal` must return exactly what `==` returns for every state, and any divergence
means the method compares the wrong subset of fields. The two traps this exercise
exists to prevent: comparing `*Bucket` values with `==` (that compares addresses,
so `TestEqualIgnoresPointerIdentity` would fail if `Equal` were a pointer method
delegating to pointer `==`), and forgetting a field in `Equal` (which
`TestEqualWithDifferentMax` catches). Run `go test -race` to confirm nothing here
depends on ordering.

## Resources

- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the field-by-field rule for struct `==`.
- [Effective Go: Constructors](https://go.dev/doc/effective_go#composite_literals) — why `New` returns a fully initialized value.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-audit-history-deepequal-slices.md](02-audit-history-deepequal-slices.md)
