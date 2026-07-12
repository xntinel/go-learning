# Exercise 2: Prove Pointer Aliasing — Same Address vs Distinct Address

Before you can debug an aliasing race in a repository or a slice buffer, you need
the minimal reproduction firmly in hand: two pointers to the same variable behave
as one, and two pointers to distinct variables are independent. This module builds
that reproduction as a testable artifact and proves both properties with pointer
identity comparisons and mutation-observation.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
alias/                     independent module: example.com/alias
  go.mod                   module example.com/alias
  alias.go                 BadAlias(n) (a,b same address); NoAlias(m,n) (distinct); mutation helpers
  cmd/
    demo/
      main.go              runnable demo: same-address share vs distinct-address independence
  alias_test.go            identity assertions (a==b vs a!=b) + mutation-through-one-observed-by-other
```

- Files: `alias.go`, `cmd/demo/main.go`, `alias_test.go`.
- Implement: `BadAlias(n int) (a, b *int)` returning two pointers to the same local variable, and `NoAlias(m, n int) (a, b *int)` returning two pointers to distinct variables.
- Test: assert `a == b` and `*a == n` for `BadAlias`; `a != b` with independent mutation for `NoAlias`; mutate through one pointer and observe the other.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/09-pointer-aliasing-and-data-races/02-pointer-aliasing-fundamentals/cmd/demo
cd go-solutions/09-pointers/09-pointer-aliasing-and-data-races/02-pointer-aliasing-fundamentals
```

### What aliasing is, at the level of a single variable

`BadAlias(n)` declares one local variable `x := n`, then returns `&x` twice. Both
returned pointers hold the same address, so `a == b` is true and a write through
`*a` is immediately visible through `*b` — they are two names for one memory
location. This is the atom of every aliasing bug in the rest of the lesson: when a
repository returns the stored `*User` to two callers, or when two sub-slices share
a backing array, the underlying situation is exactly `BadAlias` — one location,
two handles.

`NoAlias(m, n)` returns `&m` and `&n`, the addresses of two distinct parameters
(each parameter is its own variable), so `a != b` and mutating `*a` leaves `*b`
untouched. That is the *safe* shape: independent storage, independent mutation.

A subtle Go detail lives here: returning `&x` from a function is fine even though
`x` is a local. Go's escape analysis sees that `x`'s address outlives the function
and allocates it on the heap. So `BadAlias` does not return a dangling pointer; it
returns two valid pointers to one heap-allocated `int`. The hazard is not
lifetime — it is that two handles to one mutable location, combined with
concurrency later, is a race.

These tests are single-goroutine and therefore `-race` clean: aliasing itself is
not a race. It becomes a race only when the aliased location is written by one
goroutine while another accesses it. Keeping this module single-goroutine isolates
the aliasing concept from the concurrency concept; later exercises combine them.

Create `alias.go`:

```go
package alias

// BadAlias returns two pointers to the SAME variable. Both point at one heap-
// allocated int (escape analysis moves x to the heap because its address
// escapes), so a == b and a write through one is visible through the other.
// This is the atom of every aliasing bug: one location, two handles.
func BadAlias(n int) (a, b *int) {
	x := n
	a = &x
	b = &x
	return a, b
}

// NoAlias returns two pointers to DISTINCT variables (the two parameters, each
// its own storage), so a != b and mutation through one does not affect the other.
func NoAlias(m, n int) (a, b *int) {
	a = &m
	b = &n
	return a, b
}

// addOne writes *p+1 through the pointer. Used to demonstrate that a mutation
// through an aliased pointer is observed through its alias.
func addOne(p *int) {
	*p = *p + 1
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/alias"
)

func main() {
	a, b := alias.BadAlias(10)
	fmt.Printf("BadAlias: same address = %v, *a=%d *b=%d\n", a == b, *a, *b)

	c, d := alias.NoAlias(1, 2)
	fmt.Printf("NoAlias: same address = %v, *c=%d *d=%d\n", c == d, *c, *d)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BadAlias: same address = true, *a=10 *b=10
NoAlias: same address = false, *c=1 *d=2
```

### Tests

The tests assert pointer identity directly (`==` on `*int` compares addresses, no
`unsafe` needed) and then prove the memory-sharing consequence by mutating through
one handle and reading the other. For `BadAlias`, a write through `a` must be seen
through `b`. For `NoAlias`, a write through one must *not* be seen through the
other.

Create `alias_test.go`:

```go
package alias

import (
	"fmt"
	"testing"
)

func TestBadAliasSameAddress(t *testing.T) {
	t.Parallel()

	a, b := BadAlias(42)
	if a != b {
		t.Fatal("BadAlias must return two pointers to the same variable (a == b)")
	}
	if *a != 42 {
		t.Fatalf("*a = %d, want 42", *a)
	}
}

func TestBadAliasMutationIsShared(t *testing.T) {
	t.Parallel()

	a, b := BadAlias(0)
	*a = 5
	if *b != 5 {
		t.Fatalf("*b = %d after *a=5; aliased pointers must share memory, want 5", *b)
	}
}

func TestNoAliasDistinctAddress(t *testing.T) {
	t.Parallel()

	a, b := NoAlias(1, 2)
	if a == b {
		t.Fatal("NoAlias must return two distinct pointers (a != b)")
	}
	if *a != 1 || *b != 2 {
		t.Fatalf("*a=%d *b=%d, want 1 2", *a, *b)
	}
}

func TestNoAliasMutationIsIndependent(t *testing.T) {
	t.Parallel()

	a, b := NoAlias(1, 2)
	addOne(a)
	if *a != 2 {
		t.Fatalf("*a = %d after addOne, want 2", *a)
	}
	if *b != 2 {
		t.Fatalf("*b = %d, distinct pointer must be unaffected, want 2", *b)
	}
}

func Example() {
	a, b := BadAlias(7)
	*a = 99
	fmt.Println(a == b, *b)
	// Output: true 99
}
```

## Review

The reproduction is correct when `BadAlias` yields `a == b` with shared mutation
and `NoAlias` yields `a != b` with independent mutation. The conceptual trap to
avoid is concluding "aliasing is a bug" — it is not; it is a legal and useful
property that Go relies on to pass large values cheaply. What turns it into a
defect is (1) concurrency on the aliased location with at least one writer, or (2)
an API that leaks an aliased pointer into internal state to a caller. Those two are
what the remaining exercises weaponize. Note the tests are single-goroutine on
purpose, so `-race` is clean here even though the same aliasing under concurrency
would light the detector up.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) — the formal setting for when aliased access becomes a race.
- [Go FAQ: pointers to local variables](https://go.dev/doc/faq#stack_or_heap) — why returning `&x` is safe (escape analysis).
- [Effective Go: pointers vs values](https://go.dev/doc/effective_go#pointers_vs_values) — when sharing through a pointer is the intended design.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-race-detector-as-ci-gate.md](03-race-detector-as-ci-gate.md)
