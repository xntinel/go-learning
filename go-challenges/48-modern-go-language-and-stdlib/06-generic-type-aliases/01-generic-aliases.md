# Exercise 1: Generic Type Aliases

Go 1.24 lets a type alias carry its own type parameters and fix some of its target's. This exercise builds one small generic store, `KV[K, V]`, and then exercises every facet of generic aliases against it: an ergonomic partial-application alias, a plain-map alias, a cross-package migration shim, and tests that make the compiler and `reflect` both confirm "an alias is the same type."

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
aliasstore.go            KV[K,V] (methods), StringStore[V]=KV[string,V],
                         Set[T]=map[T]struct{}, constructors and helpers
internal/
  legacy/
    legacy.go            Cache[K,V]=aliasstore.KV[K,V] cross-package migration alias
    legacy_test.go       proves the migration alias crosses the package boundary
cmd/
  demo/
    main.go              passes a *StringStore[float64] where a *KV[string,float64] is asked
aliasstore_test.go       compile-time cross-assign, reflect identity, Set literal, Example
```

- Files: `aliasstore.go`, `internal/legacy/legacy.go`, `cmd/demo/main.go`, `aliasstore_test.go`, `internal/legacy/legacy_test.go`.
- Implement: the generic defined type `KV[K, V]` with its methods, the generic alias `StringStore[V] = KV[string, V]`, the plain-map alias `Set[T] = map[T]struct{}`, and the cross-package migration alias `legacy.Cache[K, V] = aliasstore.KV[K, V]`.
- Test: `aliasstore_test.go` cross-assigns alias and target at compile time, compares them under `reflect`, and uses a `Set` map literal; `internal/legacy/legacy_test.go` proves a value built through `legacy.Cache` is the same type as the canonical `KV` across the package boundary.
- Verify: `go test -count=1 -race ./...`

Set up the module. Generic aliases require the language version to be at least 1.24, so pin it explicitly:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/06-generic-type-aliases/01-generic-aliases/cmd/demo go-solutions/48-modern-go-language-and-stdlib/06-generic-type-aliases/01-generic-aliases/internal/legacy && cd go-solutions/48-modern-go-language-and-stdlib/06-generic-type-aliases/01-generic-aliases
go mod edit -go=1.24
```

### One defined type, several aliases

The design rule that makes this whole module coherent is: there is exactly one *defined* type, `KV[K comparable, V any]`, and it owns all the behavior. Everything else is an alias — a second name that adds zero new identity and zero new methods. `StringStore[V] = KV[string, V]` is the headline 1.24 feature, *partial application*: the alias's type-parameter list is shorter than the target's, pinning `K` to `string` and leaving `V` open, so a caller writes `StringStore[int]` and gets exactly `KV[string, int]`. `Set[T] = map[T]struct{}` aliases in the other direction, naming a plain map type so that a bare map literal is a valid `Set`.

The methods live on `KV` and nowhere else, and that is not an arbitrary style choice — it is forced. You cannot declare a method whose receiver is a generic or instantiated alias: `func (s *StringStore[V]) M()` is rejected with "cannot define new methods on generic alias type," and even fixing all the parameters (`type Counter = KV[string,int]; func (c *Counter) M()`) is rejected with "cannot define new methods on instantiated type." The legal case — a method on a plain, non-generic, same-package alias — does not arise here because `KV` is generic. So the methods sit on `KV`, and every alias exposes them transparently. Declare them once, get them everywhere.

Create `aliasstore.go`:

```go
package aliasstore

// KV is the one canonical, defined generic store. All methods live here, on the
// defined type, because a method receiver cannot be a generic or instantiated
// alias. Every alias below is the SAME type as its target and exposes these
// methods for free.
type KV[K comparable, V any] struct {
	m map[K]V
}

// NewKV constructs an empty KV.
func NewKV[K comparable, V any]() *KV[K, V] {
	return &KV[K, V]{m: make(map[K]V)}
}

func (s *KV[K, V]) Set(key K, value V) {
	s.m[key] = value
}

func (s *KV[K, V]) Get(key K) (V, bool) {
	v, ok := s.m[key]
	return v, ok
}

func (s *KV[K, V]) Len() int {
	return len(s.m)
}

// StringStore is a generic alias that fixes the key type to string and leaves the
// value type open (partial application). It is the SAME type as KV[string, V], so
// values are interchangeable with no conversion and it inherits KV's methods.
type StringStore[V any] = KV[string, V]

// NewStringStore is a convenience constructor. Its result is a *KV[string, V],
// which *StringStore[V] is just another name for.
func NewStringStore[V any]() *StringStore[V] {
	return NewKV[string, V]()
}

// Set is an alias whose target is a plain map type. A Set[T] is literally a
// map[T]struct{}, so a map literal can be passed wherever a Set is expected.
type Set[T comparable] = map[T]struct{}

func Add[T comparable](s Set[T], value T) {
	s[value] = struct{}{}
}

func Has[T comparable](s Set[T], value T) bool {
	_, ok := s[value]
	return ok
}
```

### The migration alias in another package

The most valuable real use of a generic alias is migration. When a generic type is renamed or relocated, an alias left at the old location keeps every caller compiling, and — because the alias is the *same* type — values cross the package boundary with no conversion, which a defined type could never allow. Here the canonical `KV` lives in the root package and `legacy.Cache[K, V] = aliasstore.KV[K, V]` is the compatibility shim a caller of the old `Cache` name still depends on.

The structural subtlety: the test for this shim must not sit in the root package. The `legacy` package imports the root; a root-package test importing `legacy` would close an import cycle. The fix used here is an internal test in `package legacy` itself, which keeps the import graph acyclic.

Create `internal/legacy/legacy.go`:

```go
package legacy

import store "example.com/generic-aliases"

// Cache is the old name for the store, kept as a generic alias so code that
// imported legacy.Cache keeps compiling after the type moved to the store
// package. Because it is an alias, values cross the package boundary with no
// conversion; the alias can be deleted once callers have migrated.
type Cache[K comparable, V any] = store.KV[K, V]

// NewCache forwards to the canonical constructor.
func NewCache[K comparable, V any]() *Cache[K, V] {
	return store.NewKV[K, V]()
}
```

### The runnable demo

A test proves a property in the abstract; a demo makes it concrete. This one builds a `*StringStore[float64]` and hands it to `total`, whose parameter is a `*KV[string, float64]`. That call compiles *only* because the alias and the target are the same type — a defined type would demand an explicit conversion and fail to build.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	aliasstore "example.com/generic-aliases"
)

// total accepts the canonical KV type. The caller passes a StringStore, which
// compiles only because StringStore[V] is the SAME type as KV[string, V].
func total(s *aliasstore.KV[string, float64]) float64 {
	sum := 0.0
	for _, key := range []string{"pi", "e"} {
		if v, ok := s.Get(key); ok {
			sum += v
		}
	}
	return sum
}

func main() {
	s := aliasstore.NewStringStore[float64]() // *StringStore[float64]
	s.Set("pi", 3.14)
	s.Set("e", 2.71)

	// Assignable in the other direction too, with no conversion.
	var alias *aliasstore.StringStore[float64] = aliasstore.NewKV[string, float64]()
	alias.Set("tau", 6.28)

	fmt.Printf("entries: %d, total: %.2f\n", s.Len(), total(s))
	fmt.Printf("alias entries: %d\n", alias.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
entries: 2, total: 5.85
alias entries: 1
```

### Tests

"Same type" is a claim you can verify two ways, and the tests use both. At *compile time*, assigning a `*StringStore[int]` to a `*KV[string, int]` variable and vice versa only builds if they are genuinely one type — a defined type would require a conversion and fail. At *run time*, `reflect.TypeOf` reports the alias and the target as identical. The `Set` test confirms a map literal is accepted where a `Set` is expected, and the `Example` pins observable behavior.

Create `aliasstore_test.go`:

```go
package aliasstore

import (
	"fmt"
	"reflect"
	"testing"
)

func TestAliasIsInterchangeable(t *testing.T) {
	t.Parallel()

	// A value built through the alias is assignable to the target type...
	var viaTarget *KV[string, int] = NewStringStore[int]()
	// ...and a value built through the target is assignable to the alias.
	var viaAlias *StringStore[int] = NewKV[string, int]()

	viaTarget.Set("a", 1)
	viaAlias.Set("b", 2)

	if v, ok := viaTarget.Get("a"); !ok || v != 1 {
		t.Fatalf("viaTarget.Get(a) = %d,%v", v, ok)
	}
	if v, ok := viaAlias.Get("b"); !ok || v != 2 {
		t.Fatalf("viaAlias.Get(b) = %d,%v", v, ok)
	}
}

func TestAliasReflectsAsSameType(t *testing.T) {
	t.Parallel()

	alias := reflect.TypeOf(NewStringStore[int]())
	target := reflect.TypeOf(NewKV[string, int]())
	if alias != target {
		t.Fatalf("alias type %v != target type %v; an alias must be the same type", alias, target)
	}
}

func TestSetAliasAcceptsMapLiteral(t *testing.T) {
	t.Parallel()

	// Because Set[string] is literally map[string]struct{}, a map literal is a
	// valid Set with no conversion.
	s := Set[string]{"x": {}}
	Add(s, "y")

	if !Has(s, "x") || !Has(s, "y") {
		t.Fatal("Set should contain x and y")
	}
	if Has(s, "z") {
		t.Fatal("Set should not contain z")
	}
}

func Example() {
	s := NewStringStore[int]()
	s.Set("answer", 42)
	v, ok := s.Get("answer")
	fmt.Println(v, ok)

	set := Set[string]{}
	Add(set, "go")
	fmt.Println(Has(set, "go"), Has(set, "rust"))

	// Output:
	// 42 true
	// true false
}
```

The migration shim is verified from inside the `legacy` package, where the import graph stays acyclic. A value built through `NewCache` is assigned to a canonical `*store.KV` handle with no conversion, and `reflect.TypeOf` confirms the two are one type across the package boundary.

Create `internal/legacy/legacy_test.go`:

```go
package legacy

import (
	"reflect"
	"testing"

	store "example.com/generic-aliases"
)

func TestMigrationAliasCrossesPackages(t *testing.T) {
	t.Parallel()

	// A value built through the legacy alias is the very same type as the
	// canonical KV: assignable with no conversion across the package boundary.
	var canonical *store.KV[string, int] = NewCache[string, int]()
	canonical.Set("a", 1)
	if v, ok := canonical.Get("a"); !ok || v != 1 {
		t.Fatalf("legacy-built store via canonical handle = %d,%v", v, ok)
	}
	if reflect.TypeOf(NewCache[string, int]()) != reflect.TypeOf(store.NewKV[string, int]()) {
		t.Fatal("legacy.Cache must be the same type as the canonical KV")
	}
}
```

## Review

The module is correct when the compiler and `reflect` agree that each alias is its target. The cross-assignments in `TestAliasIsInterchangeable` build only if `StringStore[int]` and `KV[string, int]` are one type; `TestAliasReflectsAsSameType` confirms the same at run time; and the demo's `total(s)` call, taking a `*KV[string, float64]` and fed a `*StringStore[float64]`, is the same proof in `cmd/demo`. The migration shim is sound when a `legacy.Cache`-built value drops into a `*store.KV` handle with no conversion, which `TestMigrationAliasCrossesPackages` checks from inside the `legacy` package so no import cycle forms.

Common mistakes for this feature. The first is trying to hang a method on an alias: a generic or instantiated alias receiver is rejected outright ("cannot define new methods on generic alias type" / "on instantiated type"), so all behavior must live on the defined `KV`. The precise rule matters — a method on a *plain, non-generic, same-package* alias is allowed; it just does not arise here because `KV` is generic. The second is expecting an alias to create a distinct type: it never will, so use a defined type when you want the compiler to keep two things apart. The third is testing the cross-package shim from the importing side: place that test in the `legacy` package or an external `_test` package, never in a package that would close the import cycle.

## Resources

- [The Go Programming Language Specification: Alias declarations](https://go.dev/ref/spec#Alias_declarations) — the normative rule that an alias denotes the same type, now including type-parameterized aliases.
- [Go 1.24 release notes: changes to the language](https://go.dev/doc/go1.24#language) — the release that enabled full support for generic type aliases.
- [Proposal #46477: generic type aliases](https://github.com/golang/go/issues/46477) — the design discussion and rationale for parameterized aliases and partial application.
- [An Introduction to Generics](https://go.dev/blog/intro-generics) — background on type parameters and constraints that the alias's target must satisfy.

---

Back to [00-concepts.md](00-concepts.md)
