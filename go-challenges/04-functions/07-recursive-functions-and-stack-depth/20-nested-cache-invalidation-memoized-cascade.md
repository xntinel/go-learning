# Exercise 20: Recursive Cache Invalidation with Memoized Dependencies

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A derived-data cache — a rendered dashboard built from a profile and
settings, both built from a user record — needs invalidation to cascade:
touching `user` must also invalidate `profile`, `settings`, and anything
built from either of those. Writing that cascade as plain recursion over a
dependency graph runs into the same diamond problem as the cycle-detection
exercise elsewhere in this lesson: if two different keys both depend on the
same downstream key, naive recursion invalidates that shared key once for
every path that leads to it — redundant work that gets worse the wider the
diamond is — and a cyclic dependency graph (a modeling bug, but one that
should not become an outage) would recurse forever. A single visited set,
used as a memo of "this subtree is already handled," fixes both problems
with the same line of code.

This module is fully self-contained: its own `go mod init`, the invalidator
inline, its own demo and tests.

## What you'll build

```text
cacheinvalidate/               independent module: example.com/cacheinvalidate
  go.mod                         go 1.24
  cacheinvalidate.go              type Invalidator; func New; method Invalidate
  cacheinvalidate_test.go          diamond dedup, cyclic termination, leaf-only, unknown key, wide diamond
  cmd/
    demo/
      main.go                      invalidates "user" through a profile/settings diamond into "dashboard"
```

- Files: `cacheinvalidate.go`, `cmd/demo/main.go`, `cacheinvalidate_test.go`.
- Implement: `type Invalidator struct { deps map[string][]string }`,
  `func New(deps map[string][]string) *Invalidator`, and
  `func (inv *Invalidator) Invalidate(root string) []string` cascading
  recursively with a visited-set memo.
- Test: a diamond dependency invalidates the shared downstream key exactly
  once; a cyclic dependency graph terminates instead of looping; a leaf key
  invalidates only itself; an unknown key is treated as itself with no
  dependents; a wide diamond (20 branches sharing one leaf) still
  invalidates that leaf exactly once.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/20-nested-cache-invalidation-memoized-cascade/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/20-nested-cache-invalidation-memoized-cascade
go mod edit -go=1.24
```

### The visited set is a memo, not just a cycle guard

It is tempting to read a `visited` set purely as a way to stop infinite
recursion on a cycle, the same role it plays in the ancestor-walk exercise
elsewhere in this lesson. Here it is doing a second job that matters even
when the graph has no cycles at all: in a diamond — `user` depends into
both `profile` and `settings`, and both of those depend into `dashboard` —
the recursive cascade reaches `dashboard` twice, once through each parent.
Without memoization, `dashboard`'s own dependents (and theirs, and so on)
would each be re-walked once per path that reaches `dashboard`, and in a
graph with several stacked diamonds that redundant work multiplies instead
of adding, the same combinatorial blowup the regex-memoization exercise in
this lesson hits for an unrelated reason.

Marking a key visited *before* recursing into its children (not after
returning) is what makes the memo effective for both jobs at once: the
first path to reach a key claims it immediately, so a second path arriving
at the same key — whether that second path exists because of a diamond or
because of an actual cycle — sees it already visited and stops, without
needing to distinguish "seen because of a cycle" from "seen because of a
diamond." That symmetry is exactly why one data structure serves as both
the cycle guard and the work-avoidance memo: recursive cache invalidation
does not need to tell those two situations apart, only to guarantee it
never processes the same key twice.

Create `cacheinvalidate.go`:

```go
// Package cacheinvalidate cascades cache invalidation through a graph of
// dependent keys: invalidating a key must also invalidate everything that
// was derived from it, recursively. A diamond-shaped dependency graph (two
// keys both derived from, and both feeding into, a shared downstream key)
// means the naive recursive cascade would visit that shared key twice; a
// visited set memoizes "this subtree is already invalidated" so each key is
// processed exactly once no matter how many paths lead to it, and a cyclic
// dependency graph cannot send the cascade into an infinite loop.
package cacheinvalidate

import "sort"

// Invalidator holds the dependency graph: deps[key] lists the keys that
// depend on (are derived from) key, and so must be invalidated whenever key
// is.
type Invalidator struct {
	deps map[string][]string
}

// New builds an Invalidator from a dependency graph mapping each key to the
// keys that depend on it.
func New(deps map[string][]string) *Invalidator {
	return &Invalidator{deps: deps}
}

// Invalidate cascades invalidation starting at root and returns every key
// invalidated, root included, in the order first reached. A key already
// invalidated during this call (because two different upstream keys both
// depend on it — a diamond) is not processed again: the visited set is the
// memo that turns "already invalidated this subtree" into a no-op instead of
// redundant work, and it is also what stops a cyclic dependency graph from
// cascading forever.
func (inv *Invalidator) Invalidate(root string) []string {
	visited := make(map[string]bool)
	var order []string

	var cascade func(key string)
	cascade = func(key string) {
		if visited[key] {
			return
		}
		visited[key] = true
		order = append(order, key)

		children := append([]string(nil), inv.deps[key]...)
		sort.Strings(children)
		for _, child := range children {
			cascade(child)
		}
	}

	cascade(root)
	return order
}
```

### The runnable demo

The demo builds a diamond — `user` feeds both `profile` and `settings`,
both of which feed `dashboard` — and confirms `dashboard` is invalidated
once, not twice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/cacheinvalidate"
)

func main() {
	// A diamond: "user" feeds both "profile" and "settings", and both of
	// those feed "dashboard". Without memoization, invalidating "user"
	// would process "dashboard" twice.
	deps := map[string][]string{
		"user":      {"profile", "settings"},
		"profile":   {"dashboard"},
		"settings":  {"dashboard"},
		"dashboard": {},
	}

	inv := cacheinvalidate.New(deps)
	invalidated := inv.Invalidate("user")
	fmt.Printf("invalidated: %s\n", strings.Join(invalidated, ", "))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
invalidated: user, profile, dashboard, settings
```

### Tests

`TestInvalidateDiamondVisitsSharedKeyOnce` checks the exact order and that
`dashboard` appears only once. `TestInvalidateCyclicDependencyTerminates`
is the safety net: a genuine cycle must not hang the test suite.
`TestInvalidateLeafKeyOnly` and `TestInvalidateUnknownKeyIsJustItself`
cover the small-input edges. `TestInvalidateDoesNotProcessSharedKeyTwice`
is the strongest check: a wide diamond with 20 branches all sharing one
leaf must still invalidate that leaf exactly once, which a version without
the memo would fail by a wide margin (20 occurrences instead of 1).

Create `cacheinvalidate_test.go`:

```go
package cacheinvalidate

import (
	"reflect"
	"testing"
)

func TestInvalidateDiamondVisitsSharedKeyOnce(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"user":      {"profile", "settings"},
		"profile":   {"dashboard"},
		"settings":  {"dashboard"},
		"dashboard": {},
	}

	inv := New(deps)
	got := inv.Invalidate("user")
	want := []string{"user", "profile", "dashboard", "settings"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invalidate() = %v, want %v", got, want)
	}
}

func TestInvalidateCyclicDependencyTerminates(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}

	inv := New(deps)
	got := inv.Invalidate("a")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invalidate() = %v, want %v", got, want)
	}
}

func TestInvalidateLeafKeyOnly(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"root": {"leaf"},
		"leaf": {},
	}

	inv := New(deps)
	got := inv.Invalidate("leaf")
	want := []string{"leaf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invalidate() = %v, want %v", got, want)
	}
}

func TestInvalidateUnknownKeyIsJustItself(t *testing.T) {
	t.Parallel()

	inv := New(map[string][]string{"a": {"b"}})
	got := inv.Invalidate("does-not-exist")
	want := []string{"does-not-exist"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Invalidate() = %v, want %v", got, want)
	}
}

func TestInvalidateDoesNotProcessSharedKeyTwice(t *testing.T) {
	t.Parallel()

	// A wide diamond: root depends on 20 middle keys, all 20 of which
	// depend on the same "shared" leaf. Without memoization, "shared" would
	// appear 20 times in the result.
	deps := map[string][]string{"root": {}}
	var middles []string
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		middles = append(middles, name)
		deps["root"] = append(deps["root"], name)
		deps[name] = []string{"shared"}
	}
	deps["shared"] = []string{}

	inv := New(deps)
	got := inv.Invalidate("root")

	count := 0
	for _, k := range got {
		if k == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared key appeared %d times, want exactly 1", count)
	}
	if len(got) != 1+len(middles)+1 {
		t.Fatalf("len(got) = %d, want %d", len(got), 1+len(middles)+1)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Invalidate` is correct when every reachable key appears in its result
exactly once, regardless of how many paths in the dependency graph lead to
it — `TestInvalidateDiamondVisitsSharedKeyOnce` and
`TestInvalidateDoesNotProcessSharedKeyTwice` both check this, at a small
scale and a wide one. `TestInvalidateCyclicDependencyTerminates` confirms
the same mechanism protects against a modeling bug that would otherwise
hang the process. The mistake this exercise targets is marking a key
visited *after* recursing into its children instead of before: that
ordering still terminates on a simple cycle in some cases, but it fails to
prevent the diamond case from doing redundant work, since the check happens
too late to catch the second path arriving at a key whose subtree is still
being processed by the first.

## Resources

- [Go Specification: Function literals and closures](https://go.dev/ref/spec#Function_literals)
- [sort package (sort.Strings)](https://pkg.go.dev/sort#Strings)
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-even-odd-mutual-recursion-depth-guard.md](19-even-odd-mutual-recursion-depth-guard.md) | Next: [21-audit-log-pii-redaction-recursive.md](21-audit-log-pii-redaction-recursive.md)
