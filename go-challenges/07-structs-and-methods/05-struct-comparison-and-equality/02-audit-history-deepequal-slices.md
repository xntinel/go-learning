# Exercise 2: Recent-Takes Audit Log: When a Slice Field Kills ==

Rate limiters in production rarely stay pure counters — someone always wants a
bounded audit trail of the last few decisions for debugging or metrics. The moment
you add a `history []int` field, the struct stops being comparable: `==` no longer
compiles. This exercise builds that `BucketV2`, implements `Equal` with
`slices.Equal` on the history, and contrasts it with `reflect.DeepEqual` so the
`nil`-vs-empty-slice trap is concrete rather than folklore.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
auditbucket/                independent module: example.com/auditbucket
  go.mod                    go 1.26
  auditbucket.go            type BucketV2{tokens,max int; history []int}; Take, Equal (slices.Equal)
  cmd/
    demo/
      main.go               runnable demo: take, inspect history, compare
  auditbucket_test.go       Equal vs reflect.DeepEqual; nil-vs-empty slice contrast
```

- Files: `auditbucket.go`, `cmd/demo/main.go`, `auditbucket_test.go`.
- Implement: a `BucketV2` with a bounded `history []int` of recent take sizes; `Equal` using `slices.Equal` on `history` plus scalar checks.
- Test: positive/negative `Equal`; `reflect.DeepEqual` and `Equal` agree on populated histories; `reflect.DeepEqual(nil, empty) == false` while `slices.Equal(nil, empty) == true`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/02-audit-history-deepequal-slices/cmd/demo
cd go-solutions/07-structs-and-methods/05-struct-comparison-and-equality/02-audit-history-deepequal-slices
```

### Why `==` will not compile, and what replaces it

`BucketV2` adds `history []int`. A slice field is never comparable, so it makes the
whole struct non-comparable: writing `a == b` on two `BucketV2` values is a
*compile error*, `invalid operation: a == b (struct containing []int cannot be
compared)`. This is the compiler doing you a favor — it refuses to let a comparison
that has no meaning slip into the code. Because the offending line does not compile,
this exercise documents it in a plain (non-assembled) fence rather than a `Create`
block; the gate would reject a source file that does not build.

```text
// This does NOT compile once BucketV2 has a slice field:
//   invalid operation: a == b (struct containing []int cannot be compared)
if a == b { ... }
```

The replacement is an `Equal` that compares each part with the right tool: the two
`int` fields with `==`, and the `history` slice with `slices.Equal`, which reports
whether both slices have the same length and equal elements in order.
`slices.Equal` is type-safe (its element type is fixed by the slice type),
allocation-free, and — importantly for this artifact — treats a `nil` history and
an empty-but-non-nil history as equal, because both have length zero. A limiter that
has recorded no takes yet should compare equal whether its history field was left
`nil` or initialized to `[]int{}`; `slices.Equal` gives that for free.

Contrast that with `reflect.DeepEqual`, which many engineers reach for out of habit
when a struct "has a slice in it". On two fully populated `BucketV2` values with
matching histories, `DeepEqual` and `Equal` agree. But `DeepEqual(nil, []int{})`
returns **false** — it distinguishes a nil slice from an empty one — so a
`DeepEqual`-based bucket comparison would call two just-constructed, no-takes-yet
buckets unequal purely because one field was `nil` and the other `[]int{}`. That is
the flaky-test generator the concepts file warns about, made concrete.

Create `auditbucket.go`:

```go
package auditbucket

import (
	"errors"
	"slices"
)

// ErrEmpty is returned when a Take asks for more tokens than are available.
var ErrEmpty = errors.New("bucket is empty")

const maxHistory = 3

// BucketV2 is a token bucket that keeps a bounded audit trail of recent take
// sizes. The history []int field makes BucketV2 non-comparable: == will not
// compile, so equality goes through Equal.
type BucketV2 struct {
	tokens  int
	max     int
	history []int // recent take sizes, most-recent last, capped at maxHistory
}

// New returns a full BucketV2 with capacity max (clamped to at least 1) and an
// empty (nil) history.
func New(max int) *BucketV2 {
	if max <= 0 {
		max = 1
	}
	return &BucketV2{tokens: max, max: max}
}

// Take removes n tokens and records n in the bounded history.
func (b *BucketV2) Take(n int) error {
	if n <= 0 || b.tokens < n {
		return ErrEmpty
	}
	b.tokens -= n
	b.history = append(b.history, n)
	if len(b.history) > maxHistory {
		b.history = b.history[len(b.history)-maxHistory:]
	}
	return nil
}

// Available reports the tokens currently in the bucket.
func (b *BucketV2) Available() int { return b.tokens }

// History returns a copy of the recorded take sizes.
func (b *BucketV2) History() []int { return slices.Clone(b.history) }

// Equal reports whether two buckets have the same state: scalar fields via ==
// and the history via slices.Equal (which treats nil and empty as equal).
func (b BucketV2) Equal(other BucketV2) bool {
	return b.tokens == other.tokens &&
		b.max == other.max &&
		slices.Equal(b.history, other.history)
}
```

### The runnable demo

The demo takes twice, shows the recorded history, builds a second bucket that
replays the same takes, and confirms they are `Equal`; then it shows that a
different history makes them unequal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/auditbucket"
)

func main() {
	a := auditbucket.New(10)
	_ = a.Take(2)
	_ = a.Take(3)
	fmt.Printf("a history: %v available: %d\n", a.History(), a.Available())

	b := auditbucket.New(10)
	_ = b.Take(2)
	_ = b.Take(3)
	fmt.Printf("replayed equal: %v\n", a.Equal(*b))

	_ = b.Take(1)
	fmt.Printf("diverged equal: %v\n", a.Equal(*b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a history: [2 3] available: 5
replayed equal: true
diverged equal: false
```

### Tests

`TestEqualOverHistories` is table-driven over matching and mismatching histories.
`TestEqualAgreesWithDeepEqualWhenPopulated` asserts that on fully populated,
non-nil histories `Equal` and `reflect.DeepEqual` return the same answer — they
agree in the common case. `TestNilVersusEmptySlice` is the motivating contrast: it
asserts directly that `reflect.DeepEqual(nil, []int{})` is `false` while
`slices.Equal(nil, []int{})` is `true`, and that two no-takes buckets differing only
in nil-vs-empty history are `Equal` under our method but would be judged unequal by
a `DeepEqual`-based comparison.

Create `auditbucket_test.go`:

```go
package auditbucket

import (
	"reflect"
	"slices"
	"testing"
)

func TestEqualOverHistories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		a, b   []int
		scalar bool // whether to also match tokens/max
		want   bool
	}{
		{"same history", []int{1, 2}, []int{1, 2}, true, true},
		{"different order", []int{1, 2}, []int{2, 1}, true, false},
		{"different length", []int{1, 2}, []int{1}, true, false},
		{"nil vs empty history", nil, []int{}, true, true},
		{"same history different scalar", []int{1}, []int{1}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := BucketV2{tokens: 5, max: 10, history: tt.a}
			bTokens := 5
			if !tt.scalar {
				bTokens = 4
			}
			b := BucketV2{tokens: bTokens, max: 10, history: tt.b}
			if got := a.Equal(b); got != tt.want {
				t.Fatalf("Equal = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEqualAgreesWithDeepEqualWhenPopulated(t *testing.T) {
	t.Parallel()

	a := BucketV2{tokens: 5, max: 10, history: []int{1, 2, 3}}
	b := BucketV2{tokens: 5, max: 10, history: []int{1, 2, 3}}
	c := BucketV2{tokens: 5, max: 10, history: []int{1, 2, 9}}

	if a.Equal(b) != reflect.DeepEqual(a, b) {
		t.Fatal("Equal and DeepEqual disagree on identical populated buckets")
	}
	if a.Equal(c) != reflect.DeepEqual(a, c) {
		t.Fatal("Equal and DeepEqual disagree on differing populated buckets")
	}
}

func TestNilVersusEmptySlice(t *testing.T) {
	t.Parallel()

	// The motivating gotcha, asserted directly.
	if reflect.DeepEqual([]int(nil), []int{}) {
		t.Fatal("expected DeepEqual(nil, empty) == false")
	}
	if !slices.Equal([]int(nil), []int{}) {
		t.Fatal("expected slices.Equal(nil, empty) == true")
	}

	// Two no-takes buckets differing only in nil vs empty history.
	a := BucketV2{tokens: 10, max: 10, history: nil}
	b := BucketV2{tokens: 10, max: 10, history: []int{}}
	if !a.Equal(b) {
		t.Fatal("our Equal must treat nil and empty history as equal")
	}
	if reflect.DeepEqual(a, b) {
		t.Fatal("DeepEqual would (wrongly) call these unequal; that is the trap")
	}
}
```

## Review

`BucketV2` is correct when equality is defined field-appropriately: scalars by `==`,
the history by `slices.Equal`. The exercise's whole point is the split between the
two comparison tools, and `TestNilVersusEmptySlice` is what pins it — if you had
implemented `Equal` as a single `reflect.DeepEqual(b, other)`, that test's final
assertion (`DeepEqual` calls the nil/empty pair unequal) would instead be your
`Equal` failing on two freshly constructed buckets. The other trap is order:
`slices.Equal` is order-sensitive, which is correct for an audit trail (the sequence
of takes is the signal), and `TestEqualOverHistories` locks that in with the
"different order" case. `History` returns a clone so callers cannot mutate the
internal slice.

## Resources

- [slices.Equal](https://pkg.go.dev/slices#Equal) — type-safe, order-sensitive slice comparison; nil and empty compare equal.
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — the recursive comparator and its nil-vs-empty-slice rule.
- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — why a slice field makes a struct non-comparable.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-composite-idempotency-map-key.md](03-composite-idempotency-map-key.md)
