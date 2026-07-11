# Exercise 26: GraphQL Schema Definitions Loaded and Validated at init with Cross-Reference Resolution

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde de referencia circular).

A GraphQL-style schema built from reusable field fragments -- shared blocks
like "every node has an id and timestamps" that other types `$ref` to avoid
repeating themselves -- has exactly the same failure modes as any other
static configuration graph: a fragment can reference another fragment name
that does not exist, and two or more fragments can reference each other in a
cycle that would recurse forever if resolved naively. This exercise loads
such a schema and resolves every `$ref` chain into a flat field list at
package initialization, panicking immediately on either mistake.

## What you'll build

```text
gqlschema/                  independent module: example.com/gqlschema
  go.mod                     module example.com/gqlschema
  gqlschema.go                 FragmentDef, ResolveFragments (cycle + missing-ref detection), FieldsFor
  cmd/
    demo/
      main.go                  prints resolved fields, then both failure modes on ad-hoc schemas
  gqlschema_test.go             flatten/dedupe table + missing-reference + circular + self-reference cases
```

Files: `gqlschema.go`, `cmd/demo/main.go`, `gqlschema_test.go`.
Implement: `ResolveFragments(raw map[string]FragmentDef) (map[string][]string, error)` that flattens each fragment's `Includes` into its own field list (dedup, include-fields-then-own-fields order), detects an include naming a fragment absent from `raw`, and detects a circular `Includes` chain (including a fragment including itself) via a white/gray/black visit-state walk; `init()` calling it on the package's own schema and panicking on error.
Test: a chain of includes flattens and dedupes correctly; a missing referenced fragment returns an error naming the reference chain; a two-fragment cycle and a direct self-reference both return a "circular" error; the package's own schema (built at init) resolves as expected.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gqlschema/cmd/demo
cd ~/go-exercises/gqlschema
go mod init example.com/gqlschema
go mod edit -go=1.24
```

### Why this must fail at init, not on first query

A schema's fragments are static: they are baked into the binary as Go data,
never mutated at runtime. If `User` includes `BaseNode` and `BaseNode`
includes `Timestamps`, resolving `User`'s full field list is deterministic
and can be computed exactly once. Deferring that resolution to the first
GraphQL query that touches `User` means the mistake -- a typo'd fragment
name, or two fragments accidentally referencing each other -- surfaces as a
confusing runtime error deep inside query execution, possibly hours after
deploy, instead of as an immediate, unambiguous panic the moment the process
starts. `init()` is exactly the place to pay this cost once and fail loudly.

The cycle detection is the part naive code gets wrong. A recursive resolver
that does not track "am I currently in the middle of resolving this
fragment" will recurse forever -- or, in Go, blow the goroutine stack --
the moment two fragments `$ref` each other, or a fragment `$ref`s itself.
The standard fix is a three-color walk: each fragment starts **white**
(unvisited); when resolution begins for it, it turns **gray** (in progress,
on the current path); when resolution finishes, it turns **black** (fully
resolved, safe to reuse). Encountering a **gray** fragment again means the
current path has looped back on itself -- a cycle -- and that is reported
with the full reference chain rather than merely "cycle detected somewhere."
A **black** fragment is memoized: `ResolveFragments` never re-walks a
fragment's includes twice, so overlapping fragments (`User` and `Order` both
including `BaseNode`) cost no more than resolving `BaseNode` once.

As in earlier init-validation exercises, the resolution logic lives in a
plain function, `ResolveFragments`, that returns an error; `init()` is the
only place that panics. That is what lets the test file construct small,
deliberately broken schemas -- a missing reference, a two-fragment cycle, a
fragment including itself -- and assert on the exact error, instead of
having to fork a process to observe an init-time panic.

Create `gqlschema.go`:

```go
// gqlschema.go
// Package gqlschema loads a small GraphQL-like schema made of reusable field
// fragments at package initialization, resolving each fragment's $ref
// includes into a flat field list and panicking on a missing include or a
// circular $ref chain -- both of which are static configuration mistakes
// that should fail the instant the binary starts, not on first query.
package gqlschema

import (
	"fmt"
	"sort"
	"strings"
)

// FragmentDef is one named schema fragment: its own fields, plus other
// fragment names it includes via $ref (their fields are merged in).
type FragmentDef struct {
	Fields   []string
	Includes []string
}

// rawFragments is the static schema source. Timestamps and BaseNode are
// shared building blocks; User and Order each $ref BaseNode.
var rawFragments = map[string]FragmentDef{
	"Timestamps": {Fields: []string{"createdAt", "updatedAt"}},
	"BaseNode":   {Fields: []string{"id"}, Includes: []string{"Timestamps"}},
	"User":       {Fields: []string{"name", "email"}, Includes: []string{"BaseNode"}},
	"Order":      {Fields: []string{"total"}, Includes: []string{"BaseNode"}},
}

// resolved maps each fragment name to its fully flattened, deduplicated
// field list, computed once at init.
var resolved map[string][]string

func init() {
	r, err := ResolveFragments(rawFragments)
	if err != nil {
		panic("gqlschema: " + err.Error())
	}
	resolved = r
}

// ResolveFragments flattens every fragment's $ref includes into its own
// field list, in include-order then own-fields-order with duplicates
// dropped. It reports an error naming the reference chain if a fragment
// includes a name not present in raw, or if includes form a cycle. It is
// extracted from init so tests can exercise both failure modes directly.
func ResolveFragments(raw map[string]FragmentDef) (map[string][]string, error) {
	const (
		white = iota // not yet visited
		gray         // on the current resolution path (would be a cycle)
		black        // fully resolved
	)
	state := make(map[string]int, len(raw))
	out := make(map[string][]string, len(raw))

	var resolve func(name string, chain []string) ([]string, error)
	resolve = func(name string, chain []string) ([]string, error) {
		if state[name] == black {
			return out[name], nil
		}
		if state[name] == gray {
			return nil, fmt.Errorf("circular $ref: %s", strings.Join(append(append([]string{}, chain...), name), " -> "))
		}
		def, ok := raw[name]
		if !ok {
			return nil, fmt.Errorf("fragment %q not found (referenced via %s)", name, strings.Join(chain, " -> "))
		}
		state[name] = gray
		var fields []string
		seen := make(map[string]struct{})
		for _, inc := range def.Includes {
			incFields, err := resolve(inc, append(chain, name))
			if err != nil {
				return nil, err
			}
			for _, f := range incFields {
				if _, dup := seen[f]; !dup {
					seen[f] = struct{}{}
					fields = append(fields, f)
				}
			}
		}
		for _, f := range def.Fields {
			if _, dup := seen[f]; !dup {
				seen[f] = struct{}{}
				fields = append(fields, f)
			}
		}
		state[name] = black
		out[name] = fields
		return fields, nil
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic resolution order and error reporting

	for _, name := range names {
		if _, err := resolve(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FieldsFor returns the fully resolved field list for a fragment name, and
// whether that fragment exists.
func FieldsFor(name string) ([]string, bool) {
	f, ok := resolved[name]
	if !ok {
		return nil, false
	}
	cp := make([]string, len(f))
	copy(cp, f)
	return cp, true
}

// FragmentNames returns every known fragment name, sorted.
func FragmentNames() []string {
	names := make([]string, 0, len(resolved))
	for name := range resolved {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/gqlschema"
)

func main() {
	for _, name := range gqlschema.FragmentNames() {
		fields, _ := gqlschema.FieldsFor(name)
		fmt.Printf("%s: %v\n", name, fields)
	}

	// Show the two failure modes ResolveFragments guards against, using a
	// separate ad-hoc schema (the package's own schema at init already
	// proved to resolve cleanly, or this demo would never have run).
	broken := map[string]gqlschema.FragmentDef{
		"A": {Includes: []string{"B"}},
		"B": {Includes: []string{"A"}},
	}
	if _, err := gqlschema.ResolveFragments(broken); err != nil {
		fmt.Println("circular schema rejected:", err)
	}

	missingRef := map[string]gqlschema.FragmentDef{
		"User": {Includes: []string{"Ghost"}},
	}
	if _, err := gqlschema.ResolveFragments(missingRef); err != nil {
		fmt.Println("missing fragment rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
BaseNode: [createdAt updatedAt id]
Order: [createdAt updatedAt id total]
Timestamps: [createdAt updatedAt]
User: [createdAt updatedAt id name email]
circular schema rejected: circular $ref: A -> B -> A
missing fragment rejected: fragment "Ghost" not found (referenced via User)
```

### Tests

Create `gqlschema_test.go`:

```go
// gqlschema_test.go
package gqlschema

import (
	"slices"
	"strings"
	"testing"
)

func TestResolveFragmentsFlattensIncludes(t *testing.T) {
	t.Parallel()

	raw := map[string]FragmentDef{
		"Timestamps": {Fields: []string{"createdAt", "updatedAt"}},
		"BaseNode":   {Fields: []string{"id"}, Includes: []string{"Timestamps"}},
		"User":       {Fields: []string{"name"}, Includes: []string{"BaseNode"}},
	}
	got, err := ResolveFragments(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"createdAt", "updatedAt", "id", "name"}
	if !slices.Equal(got["User"], want) {
		t.Fatalf("User fields = %v, want %v", got["User"], want)
	}
}

func TestResolveFragmentsDedupesSharedFields(t *testing.T) {
	t.Parallel()

	raw := map[string]FragmentDef{
		"Shared": {Fields: []string{"id"}},
		"A":      {Fields: []string{"id", "name"}, Includes: []string{"Shared"}},
	}
	got, err := ResolveFragments(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"id", "name"}
	if !slices.Equal(got["A"], want) {
		t.Fatalf("A fields = %v, want %v (duplicate id from Shared should be dropped)", got["A"], want)
	}
}

func TestResolveFragmentsMissingReference(t *testing.T) {
	t.Parallel()

	raw := map[string]FragmentDef{
		"User": {Includes: []string{"Ghost"}},
	}
	_, err := ResolveFragments(raw)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want containing %q", err, "not found")
	}
}

func TestResolveFragmentsCircularReference(t *testing.T) {
	t.Parallel()

	raw := map[string]FragmentDef{
		"A": {Includes: []string{"B"}},
		"B": {Includes: []string{"A"}},
	}
	_, err := ResolveFragments(raw)
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("err = %v, want containing %q", err, "circular")
	}
}

func TestResolveFragmentsSelfReference(t *testing.T) {
	t.Parallel()

	raw := map[string]FragmentDef{
		"A": {Includes: []string{"A"}},
	}
	_, err := ResolveFragments(raw)
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("err = %v, want containing %q", err, "circular")
	}
}

func TestPackageSchemaResolvedAtInit(t *testing.T) {
	t.Parallel()

	fields, ok := FieldsFor("User")
	if !ok {
		t.Fatal("FieldsFor(\"User\") ok = false")
	}
	want := []string{"createdAt", "updatedAt", "id", "name", "email"}
	if !slices.Equal(fields, want) {
		t.Fatalf("User fields = %v, want %v", fields, want)
	}

	if _, ok := FieldsFor("Nonexistent"); ok {
		t.Fatal("FieldsFor(\"Nonexistent\") ok = true, want false")
	}
}
```

## Review

`ResolveFragments` is correct when three things hold: a chain of includes
flattens into the right field list with duplicates from shared fragments
dropped (`TestResolveFragmentsFlattensIncludes`,
`TestResolveFragmentsDedupesSharedFields`); a fragment naming a nonexistent
include fails with an error identifying both the missing name and the
reference chain that led to it; and a cycle -- whether a direct
self-reference or a longer loop through several fragments -- is caught by
the gray-state check before it can recurse infinitely, also naming the full
chain. `TestPackageSchemaResolvedAtInit` is the proof that the package's own
production schema is itself one of the "good" cases: if it were not, the
test binary would never have started, because `init()` would have panicked
before any test ran.

The mistake to avoid is a resolver that only tracks "have I fully resolved
this fragment yet" (a single visited set) without also tracking "am I
currently in the middle of resolving it": that collapses gray and white
into one state and either infinite-loops on a cycle or, worse, silently
returns a partial (empty) field list for a fragment caught mid-cycle instead
of reporting the cycle at all.

## Resources

- [GraphQL spec: Fragments](https://spec.graphql.org/October2021/#sec-Language.Fragments) — the reusable-field-set concept this exercise's `FragmentDef` models.
- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why resolving and validating the schema belongs in `init()`.
- [Wikipedia — Topological sorting: depth-first search](https://en.wikipedia.org/wiki/Topological_sorting#Depth-first_search) — the white/gray/black cycle-detection technique `ResolveFragments` uses.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-structured-logging-format-string-parser.md](25-structured-logging-format-string-parser.md) | Next: [27-request-correlation-id-entropy-generator.md](27-request-correlation-id-entropy-generator.md)
