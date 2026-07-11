# Exercise 19: Multi-Value Header Set: Cloning a Map of Slices All the Way Down

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

`net/http.Header` and gRPC's `metadata.MD` are both, underneath their
convenience methods, the same shape: `map[string][]string`, one or more
values per canonicalized key. A middleware chain that passes headers through
several stages -- an auth layer, a tracing layer, a retry layer -- routinely
needs each stage to hold its own copy, free to add a header, strip one, or
rewrite a value list without corrupting what the next stage (or the original
request) sees. That is exactly the scenario this lesson's earlier exercises
built the two tools for: `maps.Clone` for the map, `slices.Clone` for a
slice. The trap this module exists to catch is believing the first one is
enough on its own.

`maps.Clone` is, by its own documentation, a shallow copy: it allocates a new
map and copies each key and value into it, and for a `map[string][]string`
the *value* being copied is a slice header -- a pointer, a length, a
capacity -- not the elements the pointer refers to. `return maps.Clone(h)` as
a `Clone` method compiles, passes any test that only checks the cloned map's
keys and top-level equality, and then corrupts the original the first time a
caller appends to one of the clone's value slices with spare capacity to
absorb the write. This is the plain-slice aliasing bug from earlier in this
lesson, reappearing one level down inside a structure that looks, at the top
level, like it was already copied.

This module builds `mvheaders`, a small header-set type with a `Clone` that
gets both levels right: a fresh map, and a fresh backing array for every
value slice it holds. The one-level-shallow version never appears in the
package's own API -- it lives in the test file, as `shallowClone`, the thing
the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
mvheaders/                module example.com/mvheaders
  go.mod                  go 1.24
  mvheaders.go             Set; NewSet, Add, Values, Del, Len, Clone
  mvheaders_test.go        construction/canonicalization table, nil-Set safety, the
                           shallowClone contrast, deep-independence proof, ExampleSet_Clone
```

- Files: `mvheaders.go`, `mvheaders_test.go`.
- Implement: `NewSet(pairs ...string) (Set, error)` rejecting an odd-length pair list with `ErrOddPairCount`; `(Set).Add(key, value string)`, `(Set).Values(key string) []string`, `(Set).Del(key string)`, `(Set).Len() int`, all canonicalizing key with `textproto.CanonicalMIMEHeaderKey`; `(Set).Clone() Set` cloning the map and every value slice.
- Test: construction and canonicalization across `Add`/`Values`/`Del`/`Len`; nil-`Set` reads (`Values`, `Clone`, `Len`) staying safe while `Add` panics, matching a nil map's own read/write asymmetry; a `shallowClone` contrast proving a map-only clone still shares (and lets a caller corrupt) the original's value slices; `Clone` proven independent at both the map level and the slice level; and `ExampleSet_Clone` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/mvheaders
cd ~/go-exercises/mvheaders
go mod init example.com/mvheaders
go mod edit -go=1.24
```

### Why a shallow clone of a map of slices still aliases

`maps.Clone(m)` does exactly what its name says at exactly one level: it
allocates a new map, then for every key copies the value across with `=`.
For `map[string]int` that is a full copy, because an `int` *is* its own
value. For `map[string][]string` the value being assigned is a slice header,
and assigning a slice header copies the header -- pointer, length, capacity
-- not the array the pointer addresses. The clone and the original end up
with two different maps whose entries, key by key, point at the *same*
backing arrays:

```go
func shallowClone(s Set) Set {
    out := make(Set, len(s))
    for k, v := range s {
        out[k] = v   // copies the slice header; the backing array is shared
    }
    return out
}
```

Nothing about this is visibly wrong from the outside: `len(out) == len(s)`,
`out["X-Trace"][0] == s["X-Trace"][0]`, every equality check a hasty test
would write passes. The corruption needs one more step to appear --
`out["X-Trace"] = append(out["X-Trace"], "new")` -- and only shows up if
that key's slice happened to have spare capacity, which is exactly the
condition that makes `append` reuse the existing backing array instead of
allocating a new one. When it does, the original's slice, unwidened, sits
right in front of a value it never wrote, invisible until something reslices
past its own length to look. The fix composes the two tools this lesson
built earlier: clone the map with `maps.Clone`, then walk the result and
replace each value with `slices.Clone` of itself, so every slice gets its
own backing array before anything can be appended to it.

Create `mvheaders.go`:

```go
// Package mvheaders implements a multi-value header set shaped exactly like
// net/http.Header or gRPC's metadata.MD: map[string][]string, canonicalized
// by key, passed through a middleware chain where each stage must be free to
// mutate its own copy without corrupting the request's original headers.
//
// The package exists to get one detail right that a hand-rolled Clone
// routinely gets wrong: maps.Clone alone is a shallow copy, so a cloned
// header set still shares every []string value with the original, and
// mutating one of those slices -- even via a spare-capacity append -- writes
// through to the source. See the package tests for a side-by-side
// demonstration.
package mvheaders

import (
	"errors"
	"fmt"
	"maps"
	"net/textproto"
	"slices"
)

// ErrOddPairCount is returned by NewSet when the variadic key/value list has
// an odd number of elements.
var ErrOddPairCount = errors.New("mvheaders: pairs must have an even number of elements")

// Set is a multi-value header set. Keys are canonicalized with
// textproto.CanonicalMIMEHeaderKey, the same rule net/http.Header uses, so
// "content-type" and "Content-Type" address the same entry.
//
// Set is not safe for concurrent use: the caller must synchronize Add,
// Values, and Clone calls, typically by giving each goroutine or middleware
// stage its own Set (obtained via Clone) rather than sharing one.
type Set map[string][]string

// NewSet builds a Set from a flat key, value, key, value, ... list. It
// returns ErrOddPairCount if pairs has an odd length. NewSet() with no
// arguments returns an empty, non-nil Set ready for Add.
func NewSet(pairs ...string) (Set, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("%w: got %d elements", ErrOddPairCount, len(pairs))
	}
	s := make(Set, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		s.Add(pairs[i], pairs[i+1])
	}
	return s, nil
}

// Add canonicalizes key and appends value to that key's list, initializing
// it if necessary. Add panics if s is nil, exactly as writing to any nil map
// does; construct with NewSet or make(Set) first.
func (s Set) Add(key, value string) {
	k := textproto.CanonicalMIMEHeaderKey(key)
	s[k] = append(s[k], value)
}

// Values returns the slice stored under key's canonical form, or nil if key
// is absent. Reading from a nil Set is safe and returns nil, the same as any
// nil map read.
//
// The returned slice aliases the Set's internal storage: mutating it,
// including by appending into spare capacity, mutates the Set. Call Clone
// first if the caller needs an independent copy.
func (s Set) Values(key string) []string {
	return s[textproto.CanonicalMIMEHeaderKey(key)]
}

// Del removes key's canonical entry entirely. Del on a nil Set, or on a key
// that is not present, is a no-op: deleting from a nil map is as safe as
// reading from one.
func (s Set) Del(key string) {
	delete(s, textproto.CanonicalMIMEHeaderKey(key))
}

// Len reports the number of distinct keys in s.
func (s Set) Len() int {
	return len(s)
}

// Clone returns a Set that shares no storage with s: a fresh map, and a
// fresh backing array for every value slice. Mutating the clone -- adding
// keys, or appending to or writing through any of its value slices -- never
// affects s, and vice versa. Clone(nil) returns nil.
func (s Set) Clone() Set {
	out := maps.Clone(s)
	for k, v := range out {
		out[k] = slices.Clone(v)
	}
	return out
}
```

### Using it

Build a `Set` with `NewSet` (or `make(Set)` for an empty one), populate it
with `Add`, and hand a `Clone` to any stage of a pipeline that needs to
mutate headers without disturbing the original -- the auth layer that adds
`X-User-Id`, the retry layer that rewrites `X-Request-Id`, the logging layer
that strips a sensitive header with `Del` before writing an access log. Two
contracts are documented on the type and its methods and both matter here:
`Values` returns a slice that *aliases* the `Set`'s internal storage,
matching `net/http.Header.Values`'s own behavior, so it is a read path, not
a copy path; `Clone` is the one method in the package that returns something
guaranteed independent, at both the map level and every value slice inside
it.

The module has no `main.go`; a header set is a package another service
imports, not a program run on its own. Its executable demonstration is
`ExampleSet_Clone`: `go test` runs it and compares its standard output
against the `// Output:` comment, so the usage shown here cannot drift away
from the code.

```go
func ExampleSet_Clone() {
	original, err := NewSet("X-Request-Id", "req-1")
	if err != nil {
		panic(err)
	}

	// A middleware stage clones before mutating, so its changes never touch
	// the request's original headers.
	stage := original.Clone()
	stage.Add("X-Request-Id", "req-1-retry")
	stage.Add("X-Stage", "auth")

	fmt.Println("original:", original.Values("X-Request-Id"))
	fmt.Println("stage:   ", stage.Values("X-Request-Id"))
	fmt.Println("original X-Stage:", original.Values("X-Stage"))
	fmt.Println("stage    X-Stage:", stage.Values("X-Stage"))

	// Output:
	// original: [req-1]
	// stage:    [req-1 req-1-retry]
	// original X-Stage: []
	// stage    X-Stage: [auth]
}
```

### Tests

`TestNewSetAndValues` and `TestAddAccumulatesUnderCanonicalKey` pin
canonicalization: `"content-type"` and `"Content-Type"` must address the same
entry, and repeated `Add` calls accumulate rather than overwrite.
`TestNewSetRejectsOddPairCount` is the constructor's edge case.
`TestNilSetReadsAreSafe` and `TestNilSetAddPanics` together pin the same
read/write asymmetry `00-concepts.md` describes for maps in general: `Values`,
`Clone`, and `Len` all tolerate a nil `Set`, while `Add` panics, because a
nil map is readable but not writable. `TestDelRemovesKey` checks that
`Del` is canonicalization-aware and does not disturb other keys.

`TestShallowCloneSharesValueSlices` is the antipattern contrast at the
center of this module. `shallowClone` is unexported and unreachable from the
package's own `Clone`; the test builds a value slice with spare capacity,
clones it the shallow way, appends into the clone, and then reslices the
*original* past its own length to show the append landed in memory the
original still points at. `TestCloneIsFullyIndependent` runs the identical
scenario through the real `Clone` and proves neither the appended value nor
a key added to the clone ever becomes visible through the source.

Create `mvheaders_test.go`:

```go
package mvheaders

import (
	"errors"
	"fmt"
	"testing"
)

// shallowClone is the Clone a maps.Clone one-liner produces: a fresh map,
// but every value is still the original []string, shared with the source.
// It is never exported and never reachable from the package API; it exists
// so the tests can pin exactly what it corrupts.
func shallowClone(s Set) Set {
	out := make(Set, len(s))
	for k, v := range s {
		out[k] = v // same slice header: same pointer, same backing array
	}
	return out
}

func TestNewSetAndValues(t *testing.T) {
	t.Parallel()

	s, err := NewSet("content-type", "application/json", "X-Trace-Id", "abc")
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if got := s.Values("Content-Type"); len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("Values(Content-Type) = %v, want [application/json]", got)
	}
	if got := s.Values("x-trace-id"); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("Values(x-trace-id) = %v, want [abc] (canonicalization is case-insensitive)", got)
	}
	if got := s.Values("Missing"); got != nil {
		t.Fatalf("Values(Missing) = %v, want nil", got)
	}
}

func TestNewSetRejectsOddPairCount(t *testing.T) {
	t.Parallel()

	if _, err := NewSet("only-key"); !errors.Is(err, ErrOddPairCount) {
		t.Fatalf("NewSet(odd) error = %v, want ErrOddPairCount", err)
	}
}

func TestAddAccumulatesUnderCanonicalKey(t *testing.T) {
	t.Parallel()

	s, err := NewSet()
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	s.Add("accept", "text/html")
	s.Add("Accept", "application/json")

	got := s.Values("ACCEPT")
	want := []string{"text/html", "application/json"}
	if len(got) != len(want) {
		t.Fatalf("Values(ACCEPT) = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("Values(ACCEPT)[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestNilSetReadsAreSafe(t *testing.T) {
	t.Parallel()

	var s Set
	if got := s.Values("Anything"); got != nil {
		t.Fatalf("nil Set Values = %v, want nil", got)
	}
	if got := s.Clone(); got != nil {
		t.Fatalf("nil Set Clone() = %v, want nil", got)
	}
	if got := s.Len(); got != 0 {
		t.Fatalf("nil Set Len() = %d, want 0", got)
	}
	s.Del("Anything") // must not panic: delete on a nil map is a safe no-op
}

func TestDelRemovesKey(t *testing.T) {
	t.Parallel()

	s, err := NewSet("X-Trace-Id", "abc", "X-Stage", "auth")
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}

	s.Del("x-trace-id") // canonicalization applies to Del too
	if s.Len() != 1 {
		t.Fatalf("Len() after Del = %d, want 1", s.Len())
	}
	if got := s.Values("X-Trace-Id"); got != nil {
		t.Fatalf("Values(X-Trace-Id) after Del = %v, want nil", got)
	}
	if got := s.Values("X-Stage"); len(got) != 1 || got[0] != "auth" {
		t.Fatalf("Values(X-Stage) = %v, want [auth]: Del must not disturb other keys", got)
	}

	s.Del("Never-Present") // deleting an absent key is a no-op
	if s.Len() != 1 {
		t.Fatalf("Len() after deleting an absent key = %d, want 1", s.Len())
	}
}

func TestNilSetAddPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("Add on a nil Set did not panic; want assignment to entry in nil map")
		}
	}()
	var s Set
	s.Add("X-Trace-Id", "abc")
}

// TestShallowCloneSharesValueSlices is the antipattern contrast. shallowClone
// mirrors what "return maps.Clone(s)" alone produces: a new map, but every
// value slice is still the original, backing array and all. Appending into
// the clone's spare capacity writes into memory the source slice can still
// see through a wider reslice -- corrupting a header the source never
// touched.
func TestShallowCloneSharesValueSlices(t *testing.T) {
	t.Parallel()

	s := Set{"X-Trace": make([]string, 1, 4)}
	s["X-Trace"][0] = "req-1"

	shallow := shallowClone(s)
	shallow["X-Trace"] = append(shallow["X-Trace"], "corrupted")

	// s's own length is untouched, but the corruption sits right behind it
	// in the backing array both slices still share.
	widened := s["X-Trace"][:2]
	if widened[1] != "corrupted" {
		t.Fatalf("shallow clone's append did not corrupt the shared backing array: %v", widened)
	}
}

// TestCloneIsFullyIndependent proves Clone does not have the defect
// shallowClone has, at both levels: the map itself, and every value slice.
func TestCloneIsFullyIndependent(t *testing.T) {
	t.Parallel()

	s := Set{"X-Trace": make([]string, 1, 4)}
	s["X-Trace"][0] = "req-1"

	deep := s.Clone()
	deep["X-Trace"] = append(deep["X-Trace"], "unrelated")
	deep.Add("X-New", "only-on-clone")

	if len(s["X-Trace"]) != 1 {
		t.Fatalf("len(s[X-Trace]) = %d, want 1: appending to the clone must not change the source's length", len(s["X-Trace"]))
	}
	widened := s["X-Trace"][:cap(s["X-Trace"])]
	for _, v := range widened[1:] {
		if v == "unrelated" {
			t.Fatal("Clone: appending to the clone leaked into the source's backing array")
		}
	}
	if s.Values("X-New") != nil {
		t.Fatal("Clone: adding a key to the clone leaked into the source map")
	}
}

// ExampleSet_Clone is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleSet_Clone() {
	original, err := NewSet("X-Request-Id", "req-1")
	if err != nil {
		panic(err)
	}

	// A middleware stage clones before mutating, so its changes never touch
	// the request's original headers.
	stage := original.Clone()
	stage.Add("X-Request-Id", "req-1-retry")
	stage.Add("X-Stage", "auth")

	fmt.Println("original:", original.Values("X-Request-Id"))
	fmt.Println("stage:   ", stage.Values("X-Request-Id"))
	fmt.Println("original X-Stage:", original.Values("X-Stage"))
	fmt.Println("stage    X-Stage:", stage.Values("X-Stage"))

	// Output:
	// original: [req-1]
	// stage:    [req-1 req-1-retry]
	// original X-Stage: []
	// stage    X-Stage: [auth]
}
```

## Review

`Clone` is correct when the returned `Set` shares no storage with its
source at either level: a fresh map from `maps.Clone`, and then a fresh
backing array for every value slice from `slices.Clone`. Stopping after the
first level is the mistake this module isolates -- `maps.Clone` alone
produces a map that looks cloned by every top-level check, and only fails
the moment a caller appends into one of its value slices with spare
capacity, corrupting the source through a shared backing array exactly like
the plain-slice aliasing bug earlier in this lesson, one field deeper.
`NewSet` rejects an odd-length pair list with `ErrOddPairCount`, checkable
with `errors.Is`. `Values`, `Clone`, and `Len` all tolerate a nil `Set` the
way any map read does; `Add` panics on one, the way any map write does --
`Del` is the one write-shaped operation that stays safe, because deleting
from a nil map, like reading from one, never touches memory that isn't
there. `Set` is documented as not safe for concurrent use, so each pipeline
stage is expected to hold its own `Set`, typically obtained via `Clone`.
`ExampleSet_Clone` is the executable documentation: `go test` verifies its
output. Run `go test -count=1 -race ./...`.

## Resources

- [maps.Clone](https://pkg.go.dev/maps#Clone) — documented explicitly as a shallow copy: "The elements are copied using assignment."
- [slices.Clone](https://pkg.go.dev/slices#Clone) — the deep-enough copy this module applies to every value slice after `maps.Clone`.
- [net/http.Header](https://pkg.go.dev/net/http#Header) — the production type this module's `Set` mirrors, including `Values` and canonicalization.
- [net/textproto.CanonicalMIMEHeaderKey](https://pkg.go.dev/net/textproto#CanonicalMIMEHeaderKey) — the canonicalization rule `net/http.Header` and this module both use.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-csv-null-marker-importer.md](18-csv-null-marker-importer.md) | Next: [../06-copy-and-full-slice-expression/00-concepts.md](../06-copy-and-full-slice-expression/00-concepts.md)
