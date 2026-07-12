# Exercise 7: A Panic-Free `Equal(a, b any)` for Idempotency and Change Detection

`==` on two `any` values panics when the dynamic type is uncomparable — a slice, a
map, a func. That is a live hazard anywhere you compare untyped values: deduplicating
repeated webhook deliveries, detecting config drift, checking whether a cached value
changed. This module builds `Equal(a, b any)` that guards comparability before `==`
and falls back to `reflect.DeepEqual`, and a control test that proves the raw `==`
would have panicked.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
safeequal/                 independent module: example.com/safeequal
  go.mod                   go 1.26
  safeequal.go             Equal(a, b any) bool: guard Comparable, fast ==, DeepEqual fallback
  cmd/
    demo/
      main.go              runnable demo: compare comparables and slices without panicking
  safeequal_test.go        comparable fast path, uncomparable via DeepEqual, raw == panics (control)
```

- Files: `safeequal.go`, `cmd/demo/main.go`, `safeequal_test.go`.
- Implement: `Equal(a, b any) bool` that returns false for differing dynamic types, uses `==` when the type is comparable, and falls back to `reflect.DeepEqual` for uncomparable types — never panicking.
- Test: correct results for comparable types via the fast path and for uncomparable types via `DeepEqual`, without panicking; a control test proving raw `==` on the same uncomparable inputs panics; nil vs typed-nil and differing dynamic types return false.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why guard before comparing

`==` on two interface values compares their dynamic types first and their values
second. If the types differ, the result is `false` with no panic — that part is safe.
The hazard is when the two values share a dynamic type that is not comparable: slices,
maps, and functions have no defined `==`, so the runtime panics with "comparing
uncomparable type". You cannot know statically whether an `any` holds a slice, so an
unguarded `a == b` on untyped values is a latent panic waiting for the first slice.

`Equal` guards up front. It reads the dynamic types with `reflect.TypeOf`. If both are
`nil` (both are the nil interface), they are equal; if exactly one is nil, they differ.
If the types differ, the answer is `false` without touching `==`. If the types match
and the type reports `Comparable()`, the fast `==` path is safe and cheap. Only when
the shared type is uncomparable does it fall back to `reflect.DeepEqual`, which handles
slices and maps structurally. This ordering matters for performance: `DeepEqual`
allocates and reflects, so it is reserved for the cases that genuinely need it, and the
common comparable case stays a single machine comparison.

A subtlety the tests pin: `nil` (the nil interface) versus a typed nil like
`(*int)(nil)`. The first has no dynamic type; the second's dynamic type is `*int`.
`Equal` treats them as unequal, which is correct — they are not the same value — and it
does so without a panic because it compares the types before anything else.

Create `safeequal.go`:

```go
package safeequal

import "reflect"

// Equal reports whether a and b are equal without ever panicking on an uncomparable
// dynamic type. Differing dynamic types are unequal; a comparable shared type uses
// fast ==; an uncomparable shared type falls back to reflect.DeepEqual.
func Equal(a, b any) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta == nil || tb == nil {
		// Equal only if BOTH are the nil interface.
		return ta == nil && tb == nil
	}
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}
```

### The runnable demo

The demo compares a few pairs — equal ints, unequal ints, differing types, and two
equal byte slices — to show that the slice comparison, which would panic under raw
`==`, returns cleanly through `Equal`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/safeequal"
)

func main() {
	fmt.Println("ints equal:  ", safeequal.Equal(42, 42))
	fmt.Println("ints differ: ", safeequal.Equal(42, 43))
	fmt.Println("type differ: ", safeequal.Equal(42, "42"))
	fmt.Println("slices equal:", safeequal.Equal([]byte("abc"), []byte("abc")))
	fmt.Println("maps equal:  ", safeequal.Equal(
		map[string]int{"a": 1}, map[string]int{"a": 1}))
	fmt.Println("nil vs typed:", safeequal.Equal(nil, (*int)(nil)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ints equal:   true
ints differ:  false
type differ:  false
slices equal: true
maps equal:   true
nil vs typed: false
```

### Tests

`TestEqualComparable` covers the fast `==` path over ints, strings, and a struct of
comparables. `TestEqualUncomparable` covers slices and maps through `DeepEqual` — and
the whole test would panic if `Equal` were not guarding. `TestRawEqualPanics` is the
control: it runs the raw `a == b` on the same uncomparable inputs inside a `recover` and
asserts it panics, which is the justification for the guard. `TestEqualEdgeCases` pins
nil vs typed-nil and differing dynamic types.

Create `safeequal_test.go`:

```go
package safeequal

import (
	"testing"
)

type point struct {
	X, Y int
}

func TestEqualComparable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"ints equal", 42, 42, true},
		{"ints differ", 42, 43, false},
		{"strings equal", "x", "x", true},
		{"struct equal", point{1, 2}, point{1, 2}, true},
		{"struct differ", point{1, 2}, point{1, 3}, false},
		{"type differ", 42, int64(42), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Equal(tc.a, tc.b); got != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestEqualUncomparable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"byte slices equal", []byte("abc"), []byte("abc"), true},
		{"byte slices differ", []byte("abc"), []byte("abd"), false},
		{"maps equal", map[string]int{"a": 1}, map[string]int{"a": 1}, true},
		{"maps differ", map[string]int{"a": 1}, map[string]int{"a": 2}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Equal(tc.a, tc.b); got != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestRawEqualPanics(t *testing.T) {
	t.Parallel()

	// Control: prove the raw == on uncomparable dynamic types panics, which is why
	// Equal must guard with Comparable().
	var a any = []byte("abc")
	var b any = []byte("abc")

	didPanic := func() (p bool) {
		defer func() {
			if recover() != nil {
				p = true
			}
		}()
		_ = a == b // panics: comparing uncomparable type []uint8
		return false
	}()

	if !didPanic {
		t.Fatal("expected raw == on []byte to panic")
	}
	// And Equal handles the very same inputs safely.
	if !Equal(a, b) {
		t.Fatal("Equal on equal byte slices returned false")
	}
}

func TestEqualEdgeCases(t *testing.T) {
	t.Parallel()

	if !Equal(nil, nil) {
		t.Fatal("Equal(nil, nil) = false, want true")
	}
	if Equal(nil, (*int)(nil)) {
		t.Fatal("Equal(nil, typed-nil) = true, want false")
	}
	if Equal(42, nil) {
		t.Fatal("Equal(int, nil) = true, want false")
	}
}
```

## Review

`Equal` is correct when it never panics and still returns the right answer: differing
dynamic types are unequal, a comparable shared type uses `==`, and an uncomparable
shared type uses `DeepEqual`. `TestRawEqualPanics` is the load-bearing test — it proves
the raw `==` genuinely panics on the same inputs `Equal` handles, which is the entire
reason the guard exists. The mistakes this module prevents are comparing `any` values
with `==` without knowing the dynamic type (fine for ints, a panic for slices) and
reaching for `reflect.DeepEqual` on every comparison "to be safe" — correct but slow, so
the `Comparable()` fast path matters. A related landmine outside this code: never use an
`any` of uncomparable dynamic type as a map key; it panics on insert for the same reason.
Run `go test -race` to confirm every branch behaves.

## Resources

- [`reflect.Type.Comparable`](https://pkg.go.dev/reflect#Type.Comparable) — whether values of a type may be compared with `==`.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — structural equality for slices, maps, and the rest.
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — which types are comparable and when interface comparison panics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-generics-over-any-store.md](08-generics-over-any-store.md)
