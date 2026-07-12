# Exercise 12: A Named Route Table Through the ~map[K]V Constraint

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A reverse proxy's routing table is the Envoy/Traefik pattern that never stops
running: host name to backend address, held in memory, swapped for a new one
atomically on every config push, and read on every single request in between.
The merge step -- take the currently live table, apply the incoming push on top
-- is the layered-config pattern `00-concepts.md` already covers for
`maps.Copy`. What that merge function is *typed* as turns out to matter as much
as what it does, and that is the sharp edge this module isolates: every
function in the standard `maps` package is generic over `~map[K]V`, "any type
whose underlying type is `map[K]V`", specifically so that a named domain type
like a routing table keeps its identity -- and its methods -- through the
operation instead of decaying into a bare map.

The trap looks harmless because it compiles cleanly and runs correctly. A merge
function written as `func(map[string]string, map[string]string) map[string]string`
does merge two routing tables just fine. But `map[string]string` is an
*unnamed* type, and Go's assignability rule lets any named type with that exact
underlying type -- a `RouteTable`, but just as easily an unrelated `Headers`
map, a feature-flag set, anything shaped `map[string]string` -- flow into that
parameter with no conversion and no warning. The function cannot tell a routing
table from a header set, because by the time it sees the value, the type that
would have told it apart is already gone. The `~` in `~map[K]V` is what puts
that distinction back: it is a constraint the compiler enforces, not a comment
a future editor can outgrow.

This module builds `routetable`, a `RouteTable` named type with a `Lookup`
method, and a generic `MergeOverride` built on `maps.Clone` and `maps.Copy`
that is provably reusable across any named map type while remaining unable to
mix two different ones by accident.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
routetable/                module example.com/routetable
  go.mod                   go 1.24
  routetable.go            RouteTable, Lookup; generic MergeOverride
  routetable_test.go       Lookup table, MergeOverride table, aliasing test,
                          mergeMapsNonGeneric contrast, ExampleMergeOverride
```

- Files: `routetable.go`, `routetable_test.go`.
- Implement: `type RouteTable map[string]string`; `(RouteTable) Lookup(host string) (string, bool)`; `MergeOverride[M ~map[K]V, K comparable, V any](base, override M) M` built on `maps.Clone` + `maps.Copy`, with `override` winning on key collision.
- Test: the `Lookup` table (hit, miss, empty, nil); the `MergeOverride` table (collision, nil base, nil override, both nil); an aliasing test proving the merged map is independent of both inputs; the `mergeMapsNonGeneric` contrast showing a bare `map[string]string` signature accepts an unrelated named type with no compile error, while `MergeOverride`'s result is directly usable as `RouteTable` with no conversion; a test that `MergeOverride` works unchanged for a second named map type; and `ExampleMergeOverride` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The tilde is a compile-time promise, not decoration

A merge function written the obvious way, without generics, looks entirely
reasonable:

```go
func mergeMapsNonGeneric(base, override map[string]string) map[string]string {
    merged := make(map[string]string, len(base)+len(override))
    for k, v := range base {
        merged[k] = v
    }
    for k, v := range override {
        merged[k] = v
    }
    return merged
}
```

Nothing about this signature stops a caller from writing
`mergeMapsNonGeneric(routeTable, headers)` where `headers` is a completely
unrelated `type Headers map[string]string`. Go's assignability rule allows it:
a value is assignable to a parameter of an *unnamed* type as long as the
underlying types match, and `map[string]string` is unnamed. The function
compiles, runs, and merges a routing table with an HTTP header set into a
plain `map[string]string` -- data from two different domains, silently fused,
with the compiler having offered no opinion on whether that made sense.

`MergeOverride[M ~map[K]V, K comparable, V any](base, override M) M` closes
that hole by making `M` itself the type parameter: both arguments must be the
*same* `M`, so `MergeOverride(routeTable, headers)` is a compile error --
`RouteTable` and `Headers`, not `map[string]string` twice. The tilde matters
independently of that: `~map[K]V` (as opposed to the bare `map[K]V`) means
"any type whose underlying type is `map[K]V`", which is what lets `RouteTable`
satisfy the constraint at all. Drop the tilde and `RouteTable` -- a distinct
named type -- would not satisfy `map[K]V` even though its underlying type is
identical, because type identity, not just underlying-type equality, is what
a bare type constraint checks.

Create `routetable.go`:

```go
// Package routetable models a reverse proxy's hot-reloadable routing table:
// host name to backend address, swapped atomically on every config push. It
// exists to show that the ~map[K]V constraint used throughout the standard
// maps package lets a named map type keep its identity -- and its methods --
// through a generic merge, instead of decaying to a bare map.
package routetable

import "maps"

// RouteTable maps a request host to the backend address it should be routed
// to.
//
// RouteTable is not safe for concurrent use while being written to; the
// caller must synchronize any goroutine that assigns into or deletes from a
// shared RouteTable. Concurrent calls to Lookup on a RouteTable that no
// goroutine is writing to are safe, the same guarantee any read-only map
// access carries.
type RouteTable map[string]string

// Lookup returns the backend host is routed to. ok is false if host has no
// entry, including when rt is nil or empty.
func (rt RouteTable) Lookup(host string) (string, bool) {
	backend, ok := rt[host]
	return backend, ok
}

// MergeOverride returns a new map containing every entry of base, with every
// entry of override written on top, so override wins on a key collision.
// This is the layered-config merge pattern: base is the previous routing
// table, override is the incoming config push.
//
// M is constrained to ~map[K]V, not the bare map[K]V the maps package's own
// signatures use as shorthand in prose: the tilde means "any type whose
// underlying type is map[K]V", so MergeOverride accepts a named map type --
// RouteTable, or any other -- and returns that same named type, not a bare
// map. Drop the tilde and RouteTable would not satisfy the constraint at
// all, because RouteTable's type identity differs from map[string]string
// even though its underlying type is identical. A caller working with
// RouteTable gets a RouteTable back and can call RouteTable's methods on
// the result with no conversion.
//
// The result is a new top-level map built with maps.Clone and maps.Copy;
// mutating it never mutates base or override, and mutating base or override
// after the call never mutates the result. That independence is only as
// deep as V: if V is itself a pointer, slice, or map, the cloned entries
// still share the same underlying value, exactly as maps.Clone documents.
// A nil base and a nil override both merge to an empty, non-nil M.
func MergeOverride[M ~map[K]V, K comparable, V any](base, override M) M {
	merged := maps.Clone(base)
	if merged == nil {
		merged = make(M, len(override))
	}
	maps.Copy(merged, override)
	return merged
}
```

### Using it

`RouteTable` needs no constructor: build one with a map literal or `make`,
same as any map, and swap the whole value atomically when a new config push
arrives -- readers holding the old value keep working against it, since
`MergeOverride` never mutates `base`. The one contract that crosses the
package boundary is concurrency: `RouteTable` synchronizes nothing itself, so
a routing table shared across request-handling goroutines still needs its own
lock around the swap, exactly as `00-concepts.md` states for every `maps`
function.

`ExampleMergeOverride` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleMergeOverride() {
	live := RouteTable{"api.example.com": "backend-1", "www.example.com": "backend-2"}
	push := RouteTable{"api.example.com": "backend-3"}

	next := MergeOverride(live, push)
	backend, ok := next.Lookup("api.example.com")
	fmt.Println(backend, ok)

	backend, ok = next.Lookup("www.example.com")
	fmt.Println(backend, ok)

	// live is untouched by the push: MergeOverride never mutates its inputs.
	backend, ok = live.Lookup("api.example.com")
	fmt.Println(backend, ok)

	// Output:
	// backend-3 true
	// backend-2 true
	// backend-1 true
}
```

### Tests

`TestLookup` and `TestMergeOverride` are the tables: every host outcome, and
every combination of nil and populated `base`/`override`, including the case
where both are nil and the result must still be an empty, non-nil map.
`TestMergeOverrideDoesNotAliasInputs` pins the aliasing contract in both
directions -- mutating the result must not touch `base`, and mutating `base`
after the call must not touch the result. `mergeMapsNonGeneric` is the
unexported antipattern: `TestNonGenericMergeAcceptsTheWrongDomainType` proves
it silently accepts a `headers` value where a `RouteTable` was intended, and
`TestGenericMergeReturnsUsableRouteTable` proves the payoff -- `MergeOverride`'s
result calls `.Lookup` directly, while the non-generic result needs an
explicit `RouteTable(...)` conversion before `.Lookup` is even callable on it.
`TestMergeOverrideWorksForAnyNamedMapType` confirms the constraint is not
special-cased to `RouteTable`: the same generic function merges a second,
unrelated named map type without any change.

Create `routetable_test.go`:

```go
package routetable

import (
	"fmt"
	"testing"
)

func TestLookup(t *testing.T) {
	t.Parallel()

	rt := RouteTable{"api.example.com": "backend-1", "www.example.com": "backend-2"}
	tests := []struct {
		name   string
		rt     RouteTable
		host   string
		want   string
		wantOK bool
	}{
		{"known host", rt, "api.example.com", "backend-1", true},
		{"unknown host", rt, "unknown.example.com", "", false},
		{"empty table", RouteTable{}, "api.example.com", "", false},
		{"nil table", nil, "api.example.com", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tc.rt.Lookup(tc.host)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Lookup(%q) = (%q, %v), want (%q, %v)", tc.host, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestMergeOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		base     RouteTable
		override RouteTable
		want     RouteTable
	}{
		{
			name:     "override wins on collision",
			base:     RouteTable{"a.com": "old", "b.com": "keep"},
			override: RouteTable{"a.com": "new"},
			want:     RouteTable{"a.com": "new", "b.com": "keep"},
		},
		{
			name:     "nil base",
			base:     nil,
			override: RouteTable{"a.com": "new"},
			want:     RouteTable{"a.com": "new"},
		},
		{
			name:     "nil override",
			base:     RouteTable{"a.com": "old"},
			override: nil,
			want:     RouteTable{"a.com": "old"},
		},
		{
			name:     "both nil merge to empty, non-nil",
			base:     nil,
			override: nil,
			want:     RouteTable{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MergeOverride(tc.base, tc.override)
			if got == nil {
				t.Fatal("MergeOverride returned nil, want a non-nil map")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("MergeOverride() = %v, want %v", got, tc.want)
			}
			for host, want := range tc.want {
				if backend, ok := got.Lookup(host); !ok || backend != want {
					t.Errorf("merged.Lookup(%q) = (%q, %v), want (%q, true)", host, backend, ok, want)
				}
			}
		})
	}
}

// TestMergeOverrideDoesNotAliasInputs pins the aliasing contract on the doc
// comment: the merged map is independent of both base and override in
// either direction.
func TestMergeOverrideDoesNotAliasInputs(t *testing.T) {
	t.Parallel()

	base := RouteTable{"a.com": "old"}
	override := RouteTable{"b.com": "new"}
	merged := MergeOverride(base, override)

	merged["c.com"] = "added-after-merge"
	if _, ok := base.Lookup("c.com"); ok {
		t.Fatal("mutating merged changed base")
	}

	base["d.com"] = "added-to-base-after-merge"
	if _, ok := merged.Lookup("d.com"); ok {
		t.Fatal("mutating base after MergeOverride changed the merged result")
	}
}

// headers is a second named type sharing RouteTable's exact underlying
// type, map[string]string. It stands in for any unrelated domain map --
// HTTP headers, a set of feature flags -- that a generic function typed
// over ~map[K]V will correctly refuse to accept in place of a RouteTable,
// but a non-generic helper typed over the literal map[string]string cannot
// tell apart from one.
type headers map[string]string

// mergeMapsNonGeneric is the merge function a first draft reaches for: the
// same clone-then-copy logic as MergeOverride, but typed over the bare
// map[string]string instead of the ~map[K]V constraint. Because
// map[string]string is an unnamed type, both RouteTable and headers are
// assignable to its parameters with no explicit conversion -- the function
// happily accepts either one wherever a RouteTable was intended, and always
// returns a bare map[string]string rather than the caller's named type. It
// is never exported and never reachable from the package API; it exists so
// the tests can pin what the erased type costs a caller.
func mergeMapsNonGeneric(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// TestNonGenericMergeAcceptsTheWrongDomainType shows the hole directly: a
// headers value flows into a parameter meant for a RouteTable with no
// compile error at all, because the parameter type is the unnamed
// map[string]string that both named types share.
func TestNonGenericMergeAcceptsTheWrongDomainType(t *testing.T) {
	t.Parallel()

	rt := RouteTable{"api.example.com": "backend-1"}
	h := headers{"content-type": "application/json"}

	mixed := mergeMapsNonGeneric(rt, h) // compiles: nothing here says this is a mistake
	if len(mixed) != 2 {
		t.Fatalf("mergeMapsNonGeneric mixed a RouteTable and a headers value into %v", mixed)
	}
}

// TestGenericMergeReturnsUsableRouteTable is the payoff: MergeOverride's
// result is already a RouteTable, so RouteTable's own method set is
// available immediately, with no conversion. The non-generic helper's
// result, by contrast, has static type map[string]string, and a call like
// mergeMapsNonGeneric(rt, override).Lookup(...) does not compile at all --
// only RouteTable(mergeMapsNonGeneric(rt, override)).Lookup(...) does,
// exactly the conversion tax a bare map[K]V signature imposes on every
// caller.
func TestGenericMergeReturnsUsableRouteTable(t *testing.T) {
	t.Parallel()

	base := RouteTable{"api.example.com": "backend-1"}
	override := RouteTable{"www.example.com": "backend-2"}

	merged := MergeOverride(base, override) // merged's static type is RouteTable
	if backend, ok := merged.Lookup("api.example.com"); !ok || backend != "backend-1" {
		t.Fatalf("merged.Lookup(api) = (%q, %v), want (backend-1, true)", backend, ok)
	}

	nonGeneric := mergeMapsNonGeneric(base, override)
	if backend, ok := RouteTable(nonGeneric).Lookup("www.example.com"); !ok || backend != "backend-2" {
		t.Fatalf("RouteTable(nonGeneric).Lookup(www) = (%q, %v), want (backend-2, true)", backend, ok)
	}
}

// TestMergeOverrideWorksForAnyNamedMapType shows MergeOverride is not
// special-cased to RouteTable: the ~map[K]V constraint accepts headers just
// as well, and the result comes back typed as headers, not RouteTable and
// not a bare map[string]string.
func TestMergeOverrideWorksForAnyNamedMapType(t *testing.T) {
	t.Parallel()

	base := headers{"content-type": "application/json"}
	override := headers{"accept": "text/plain"}

	merged := MergeOverride(base, override) // merged's static type is headers
	if len(merged) != 2 {
		t.Fatalf("MergeOverride(headers) = %v, want 2 entries", merged)
	}
}

// ExampleMergeOverride is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleMergeOverride() {
	live := RouteTable{"api.example.com": "backend-1", "www.example.com": "backend-2"}
	push := RouteTable{"api.example.com": "backend-3"}

	next := MergeOverride(live, push)
	backend, ok := next.Lookup("api.example.com")
	fmt.Println(backend, ok)

	backend, ok = next.Lookup("www.example.com")
	fmt.Println(backend, ok)

	// live is untouched by the push: MergeOverride never mutates its inputs.
	backend, ok = live.Lookup("api.example.com")
	fmt.Println(backend, ok)

	// Output:
	// backend-3 true
	// backend-2 true
	// backend-1 true
}
```

## Review

`MergeOverride` is correct when it produces every input entry exactly once,
`override` breaking every tie, and never mutates either input -- the table
test and the aliasing test pin both. What makes it worth choosing over
`mergeMapsNonGeneric` is not that operation, which both perform identically,
but what the signature does and does not let compile: a bare
`map[string]string` parameter accepts any named type with that underlying
type, silently, which is how a routing table and a header set end up merged
together with no error at all. `~map[K]V` closes that door -- both arguments
of a single `MergeOverride` call must be the same named type -- while still
letting the function work, unchanged, for any map type a caller defines. The
result comes back as that same named type, so a `RouteTable` stays a
`RouteTable` all the way through the merge, methods included, with no
conversion the non-generic version forces on every caller. `RouteTable`
synchronizes nothing on its own; a shared instance still needs its own lock
around a concurrent writer. Run `go test -count=1 -race ./...`.

## Resources

- [`maps` package: The ~map[K]V constraint preserves your domain types](00-concepts.md) — the concept this module builds directly on.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) and [`maps.Copy`](https://pkg.go.dev/maps#Copy) — the two primitives `MergeOverride` composes.
- [Go spec: Assignability](https://go.dev/ref/spec#Assignability) — the rule that lets a named type flow into an unnamed-typed parameter with no conversion.
- [Go spec: Type constraints](https://go.dev/ref/spec#Type_constraints) — the `~` (approximation element) syntax used in `~map[K]V`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-quorum-capacity-lazy-iterator-scan.md](11-quorum-capacity-lazy-iterator-scan.md) | Next: [13-tenant-rate-limiter-pointwise-lock.md](13-tenant-rate-limiter-pointwise-lock.md)
