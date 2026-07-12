# Exercise 7: Generic Type Alias vs Defined Generic Type for a Set Utility

Sets show up everywhere in backend code — allowlists, dedup, "seen" tracking — and
Go 1.24 gives you two ways to name one generically. A *generic type alias*
`type Set[T comparable] = map[T]struct{}` is a shared shape usable interchangeably
with a bare map literal, but it carries no methods. A *defined generic type*
`type SetT[T comparable] map[T]struct{}` carries `Add`/`Has`/`Union` but is a
distinct type. This exercise builds both and shows when each is the right tool.

This module is fully self-contained: its own `go mod init`, demo, and tests. It
requires Go 1.24 for generic type aliases.

## What you'll build

```text
genset/                   independent module: example.com/genset
  go.mod                  go 1.24 (generic type aliases need it)
  genset.go               type Set[T] = map[T]struct{} (alias);
                          type SetT[T] map[T]struct{} (defined) + Add/Has/Union
  cmd/
    demo/
      main.go             an allowlist as SetT; the alias interchanged with a map
  genset_test.go          alias assignability, defined-type methods, union tests
```

- Files: `genset.go`, `cmd/demo/main.go`, `genset_test.go`.
- Implement: the generic alias `Set[T]`, the defined generic type `SetT[T]` with `NewSetT`, `Add`, `Has`, and `Union`, and a `Sorted` helper for deterministic output.
- Test: alias values are assignable to/from a plain `map[T]struct{}` literal with no conversion; `SetT` supports `Add`/`Has`/`Union` while a bare map must be converted to gain them.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Shared shape vs shared behavior

Before Go 1.24 a type alias could not take type parameters; 1.24 makes
`type Set[T comparable] = map[T]struct{}` legal. Being an alias, `Set[T]` is not a
new type — it is a second spelling of `map[T]struct{}`. That is exactly what you
want when several packages should agree on a common *shape* without minting
incompatible types: a `Set[string]` value is a `map[string]struct{}` value, so it
flows into any function taking the bare map and vice versa, with no conversion.
The catch is the unchanging alias rule: an alias carries no methods, so
`Set[T]` has no `Add` or `Has`. You use it as a readable name for a plain map you
manipulate with `s[k] = struct{}{}` and `_, ok := s[k]`.

When the set needs *behavior*, you need a defined type. `type SetT[T comparable]
map[T]struct{}` is a distinct generic type that carries methods: `Add`, `Has`,
`Union`. Because it is defined, a bare `map[T]struct{}` is not a `SetT[T]` — you
must convert (`SetT[string](m)`) to gain the methods, which is the price of the
extra capability. The method receivers are value receivers, but the mutation still
sticks because a map value is a reference to the same underlying data.

The rule of thumb: reach for the generic *alias* when you want a shared spelling
that stays interchangeable with the underlying map; reach for the defined generic
*type* when the set needs methods. You cannot have both from one declaration,
because aliases never carry methods.

The `Sorted` helper below is generic over `cmp.Ordered` so its output is
deterministic; it takes the alias `Set[T]`, which is the plain map, and works for
either shape after a conversion.

Create `genset.go`:

```go
package genset

import (
	"cmp"
	"maps"
	"slices"
)

// Set is a generic type ALIAS (Go 1.24). Set[T] is the identical type as
// map[T]struct{}, so the two interchange with no conversion. It carries NO
// methods, because an alias never does.
type Set[T comparable] = map[T]struct{}

// SetT is a DEFINED generic type over the same shape. It is a distinct type that
// carries methods; a bare map must be converted to become a SetT.
type SetT[T comparable] map[T]struct{}

// NewSetT builds a SetT from the given values.
func NewSetT[T comparable](vals ...T) SetT[T] {
	s := make(SetT[T], len(vals))
	for _, v := range vals {
		s[v] = struct{}{}
	}
	return s
}

// Add inserts v. The value receiver still mutates because a map is a reference.
func (s SetT[T]) Add(v T) { s[v] = struct{}{} }

// Has reports whether v is present.
func (s SetT[T]) Has(v T) bool {
	_, ok := s[v]
	return ok
}

// Union returns a new SetT containing every element of s and o.
func (s SetT[T]) Union(o SetT[T]) SetT[T] {
	out := make(SetT[T], len(s)+len(o))
	for v := range s {
		out[v] = struct{}{}
	}
	for v := range o {
		out[v] = struct{}{}
	}
	return out
}

// Sorted returns the elements of any set-shaped map in sorted order, for
// deterministic output. It accepts the alias Set (== a plain map).
func Sorted[T cmp.Ordered](s Set[T]) []T {
	return slices.Sorted(maps.Keys(s))
}
```

### The interchangeability the alias buys, and the conversion the definition costs

Because `Set[T]` is an alias, this compiles with no conversion:

```go
var a Set[string] = map[string]struct{}{"x": {}} // map literal -> alias
var m map[string]struct{} = a                     // alias -> map literal
```

Because `SetT[T]` is a definition, a bare map does *not* satisfy it and must be
converted to gain the methods:

```go
raw := map[string]struct{}{"x": {}}
// raw.Has("x")           // does not compile: map has no method Has
s := SetT[string](raw)    // explicit conversion
_ = s.Has("x")            // now the methods are available
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/genset"
)

func main() {
	// An allowlist wants behavior, so it is the defined type.
	allow := genset.NewSetT("read", "write")
	allow.Add("admin")
	fmt.Println("has write:", allow.Has("write"))
	fmt.Println("has delete:", allow.Has("delete"))

	extra := genset.NewSetT("read", "audit")
	fmt.Println("union:", genset.Sorted(genset.Set[string](allow.Union(extra))))

	// The alias is just a map; it interchanges with a bare map literal.
	var shape genset.Set[int] = map[int]struct{}{1: {}, 2: {}}
	fmt.Println("alias sorted:", genset.Sorted(shape))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
has write: true
has delete: false
union: [admin audit read write]
alias sorted: [1 2]
```

### Tests

The tests prove the alias interchanges with a plain map with no conversion, and
that the defined type's methods work while a bare map must be converted to reach
them. `Union` and `Sorted` give a deterministic, comparable result.

Create `genset_test.go`:

```go
package genset

import (
	"fmt"
	"slices"
	"testing"
)

func TestAliasInterchangesWithMap(t *testing.T) {
	t.Parallel()

	// map literal assigns to the alias with no conversion...
	var a Set[string] = map[string]struct{}{"x": {}, "y": {}}
	// ...and the alias assigns back to a plain map with no conversion.
	var m map[string]struct{} = a

	if len(m) != 2 {
		t.Fatalf("len = %d, want 2", len(m))
	}
	// A function taking the bare map accepts the alias directly.
	if got := len(a); got != 2 {
		t.Fatalf("alias len = %d, want 2", got)
	}
}

func TestDefinedTypeMethods(t *testing.T) {
	t.Parallel()

	s := NewSetT("a", "b")
	s.Add("c")

	if !s.Has("b") || !s.Has("c") {
		t.Fatal("Has should report added elements")
	}
	if s.Has("z") {
		t.Fatal("Has should be false for absent element")
	}
}

func TestConvertMapToGainMethods(t *testing.T) {
	t.Parallel()

	raw := map[string]struct{}{"a": {}}
	// raw.Has("a") would not compile; convert to the defined type first.
	s := SetT[string](raw)
	if !s.Has("a") {
		t.Fatal("after conversion, methods should be available")
	}
}

func TestUnion(t *testing.T) {
	t.Parallel()

	u := NewSetT(1, 2).Union(NewSetT(2, 3))
	got := Sorted(Set[int](u))
	want := []int{1, 2, 3}
	if !slices.Equal(got, want) {
		t.Fatalf("Union sorted = %v, want %v", got, want)
	}
}

func ExampleSetT_Union() {
	u := NewSetT("read", "write").Union(NewSetT("write", "admin"))
	fmt.Println(Sorted(Set[string](u)))
	// Output: [admin read write]
}
```

## Review

The distinction is correct when the alias assigns to and from a bare
`map[T]struct{}` with no conversion, and when the defined `SetT` carries `Add`/
`Has`/`Union` that a plain map only gets after an explicit `SetT[...]` conversion.
The mistake is expecting the alias to have methods — it never will, because an
alias is the same type as the map, which has none. The second mistake is pinning
go.mod below 1.24: generic type aliases were not legal before then, and the
declaration fails to compile. Choose the alias for a shared shape you keep
interchangeable with maps, and the defined type when the set needs behavior.

## Resources

- [Go 1.24 Release Notes: generic type aliases](https://go.dev/doc/go1.24#language) — the feature this exercise depends on.
- [Go Language Spec: Alias declarations](https://go.dev/ref/spec#Alias_declarations) — type parameters on aliases.
- [`maps.Keys`](https://pkg.go.dev/maps#Keys) and [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — deterministic iteration over a set.

---

Prev: [06-trusted-untrusted-string-safety-types.md](06-trusted-untrusted-string-safety-types.md) | Next: [08-json-marshal-recursion-guard.md](08-json-marshal-recursion-guard.md)
