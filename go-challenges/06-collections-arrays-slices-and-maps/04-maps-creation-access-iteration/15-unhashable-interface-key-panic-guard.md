# Exercise 15: map[any]V and the Runtime Panic a Comparable Static Type Cannot Prevent

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A request-dedup cache, a generic memoizer, or anything keyed by a decoded
gRPC `Any`, a JSON value, or a config field whose shape is not known until
runtime ends up with a map keyed by `any`. The compiler happily accepts
`map[any]V` — `any` satisfies the comparable constraint at the *static* type
level, because every interface type is technically comparable. What the
compiler cannot check is the *dynamic* type stuffed into that interface at
runtime: if it turns out to be a slice, a map, a function, or a struct or
array embedding one of those, hashing it blows up with a runtime panic the
instant the map tries to use it as a key. This module reproduces that panic,
then builds the real fix — a bounded cache keyed by a normalized, always-
comparable key type that makes the panic structurally impossible rather
than merely caught.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
keyguard/                     module example.com/keyguard
  go.mod                      go 1.24
  keyguard.go                  Key, Normalize; Store with NewStore/Put/Get/Len; sentinel errors
  keyguard_test.go              raw panic reproduced and recovered; per-call-site recover
                               patch contrasted; Store never panics across a type table;
                               capacity boundary; concurrency; ExampleStore
```

- Files: `keyguard.go`, `keyguard_test.go`.
- Implement: `Key string` built by `Normalize(v any) (Key, error)`, which canonicalizes `v` via `json.Marshal` so the result is always a comparable string; `NewStore(maxEntries int) (*Store, error)` rejecting a non-positive capacity with `ErrNonPositiveMaxEntries`; `(*Store).Put(key any, value int) error` normalizing the key and rejecting a new key beyond capacity with `ErrStoreFull`; `(*Store).Get(key any) (value int, ok bool, err error)`; `(*Store).Len() int`.
- Test: a raw `map[any]int` write with a slice key panics and is caught by a bare `defer`/`recover` (proving it is a real, recoverable panic and not an unrecoverable fatal error); a per-call-site `recover`-based patch catching the same panic, contrasted against `Store`; `Store` never panicking across a table of slice, map, struct-embedding-a-slice, and plain string keys, tested with no `recover` present at all; two distinct-but-equal slices normalizing to the same `Key`; a channel key (unmarshalable, not just unhashable) reporting an error from `Normalize`; the capacity boundary rejecting a new key while still accepting an update to an existing one; concurrent `Put`/`Get` under `-race`; and `ExampleStore` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a comparable static type is not a comparable runtime guarantee

Go's spec requires a map's key type to be comparable, and it checks that at
compile time — but for an interface type, comparability is a promise about
the *interface*, not about whatever concrete value ends up stored inside it.
Every interface type, including `any`, is technically "comparable" as far as
the compiler is concerned, because comparing two interface values is defined
as comparing their dynamic types first and, if those match, their dynamic
values second. The catch is in that second step: if the dynamic type turns
out to be a slice, a map, a function, or a struct or array that embeds one of
those, comparing (or hashing, which a map key requires) panics at runtime
with `runtime error: hash of unhashable type ...`. `map[any]V` therefore
defers the entire hashability question from compile time to the exact moment
a specific key is inserted or looked up.

That panic is worth contrasting with the other map failure mode this
lesson's concepts cover: an unsynchronized concurrent write calls
`fatal("concurrent map writes")`, which `recover` cannot catch because it is
not a panic at all, it is a hard process abort. The unhashable-key case is
different — it is a genuine `panic`, triggered by the runtime's type-assertion
and hashing machinery, and a `defer`/`recover` sitting anywhere up the call
stack catches it cleanly, with no partial mutation left behind (the map
either gets the entry or it does not; there is no half-written state). A
per-call-site guard built on that fact looks like a fix:

```go
func safePutRecover(m map[any]int, key any, value int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered from panic: %v", r)
		}
	}()
	m[key] = value
	return nil
}
```

It works, for the one call site that remembers to use it. Every other
function anywhere in the codebase that writes into a `map[any]V` still has
the bug, because the underlying hash attempt still runs — and still
panics internally — every single time; `recover` only stops that panic from
unwinding past the wrapper. It is a patch applied at the call site, not a
property of the map's type.

The structural fix removes the interface key from the map entirely. `Key` is
a plain `string`, unconditionally comparable regardless of what value it was
derived from, and `Normalize` produces one by marshaling the input to JSON —
which has no concept of "unhashable," because it serializes structure rather
than comparing identity, and `encoding/json` canonicalizes map key order by
sorting, so two structurally-equal values always normalize to the same `Key`
no matter what dynamic type or field order they arrived with. `Store` then
never holds an `any`-keyed map at all: every `Put`/`Get` normalizes first, so
there is no code path left in `Store` that could reach the runtime's
hash-of-unhashable-type panic — not "the panic is caught," but "the panic has
no map key type it could ever fire against." `Store` also bounds itself with
a `maxEntries` capacity, because a cache keyed by arbitrary caller-supplied
values has no natural size limit and is otherwise a memory-exhaustion vector.

Create `keyguard.go`:

```go
// Package keyguard demonstrates the runtime panic hiding inside any map
// keyed by an interface type -- the shape of a request-dedup cache or a
// generic memoizer keyed by decoded JSON, gRPC Any, or config values whose
// concrete type is not known at compile time -- and removes it structurally
// by never storing a caller's dynamic key in a map at all.
package keyguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by NewStore and Store's methods. Callers should
// test for them with errors.Is.
var (
	// ErrNonPositiveMaxEntries means NewStore was called with a capacity of
	// zero or less.
	ErrNonPositiveMaxEntries = errors.New("keyguard: max entries must be positive")
	// ErrStoreFull means Put would add a new key beyond the Store's
	// configured capacity.
	ErrStoreFull = errors.New("keyguard: store is at capacity")
)

// Key is a normalized, always-comparable stand-in for an arbitrary value.
// Because Key is a string, it can be used as a map key regardless of what
// the original value's dynamic type was -- there is no runtime hashing
// decision left to make, so storing one can never panic.
type Key string

// Normalize converts v into a canonical Key by marshaling it to JSON. This
// works for the values that would otherwise panic as map keys -- slices,
// maps, and structs containing them -- because JSON has no notion of
// "unhashable": it serializes structure, not identity, and encoding/json
// sorts map keys before encoding, so two maps with the same content always
// normalize to the same Key regardless of iteration order. Values that
// cannot be marshaled at all (channels, functions) report an error instead
// of panicking.
func Normalize(v any) (Key, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("keyguard: cannot normalize key of type %T: %w", v, err)
	}
	return Key(b), nil
}

// Store is a bounded cache keyed by an arbitrary comparable-or-not value.
// Every entry point normalizes its incoming key to a Key before touching
// the internal map, so no caller of Store can trigger the Go runtime's
// hash-of-unhashable-type panic no matter what dynamic type they pass in --
// the guarantee is structural, not a recover wrapper applied per call site.
//
// Store is safe for concurrent use by multiple goroutines.
type Store struct {
	mu         sync.RWMutex
	data       map[Key]int
	maxEntries int
}

// NewStore returns an empty Store that holds at most maxEntries distinct
// keys. It returns ErrNonPositiveMaxEntries if maxEntries is not positive;
// the bound exists because a store keyed by arbitrary normalized values has
// no natural size limit of its own, and an unbounded cache keyed by
// caller-supplied data is a memory-exhaustion vector.
func NewStore(maxEntries int) (*Store, error) {
	if maxEntries <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrNonPositiveMaxEntries, maxEntries)
	}
	return &Store{data: make(map[Key]int), maxEntries: maxEntries}, nil
}

// Put normalizes key and stores value under the resulting Key. It returns
// an error if key cannot be marshaled to JSON at all (a channel or a
// function), or ErrStoreFull if key is new and the Store is already at
// capacity. Put never panics regardless of key's dynamic type.
func (s *Store) Put(key any, value int) error {
	k, err := Normalize(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[k]; !exists && len(s.data) >= s.maxEntries {
		return fmt.Errorf("%w: capacity %d", ErrStoreFull, s.maxEntries)
	}
	s.data[k] = value
	return nil
}

// Get normalizes key and looks it up. Like Put, it never panics regardless
// of key's dynamic type.
func (s *Store) Get(key any) (value int, ok bool, err error) {
	k, err := Normalize(key)
	if err != nil {
		return 0, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok = s.data[k]
	return value, ok, nil
}

// Len reports the number of distinct keys currently stored.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}
```

### Using it

Construct one `Store` per cache with the capacity your workload can afford,
then call `Put`/`Get` with any value, comparable or not — a slice, a nested
struct, a plain string — and let `Normalize` handle the conversion. `Store`
carries a `sync.RWMutex` internally and its doc comment promises safety for
concurrent use, so a single value can be shared across every request-handling
goroutine. The one input `Put`/`Get` legitimately reject is a value
`encoding/json` cannot marshal at all — a channel or a function — which
surfaces as an ordinary error rather than a panic, exactly like every other
input `Store` validates.

The module has no `main.go`, because a keyed cache is a library, not a tool.
Its executable demonstration is `ExampleStore`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

### Tests

`TestRawMapPanicsOnUnhashableKey` reproduces the failure with the barest
possible reproduction — a plain `map[any]int` and a `[]string` key — wrapped
in a bare `defer`/`recover`, establishing that this really is a catchable
panic and not a `fatal` abort. `safePutRecover` and
`TestSafePutRecoverPatchCatchesPanic` are the contrast the whole module is
built around: the helper is unexported and lives only in this test file,
demonstrating the per-call-site patch that catches the panic for exactly one
write path — nothing in the package API works this way.

`TestStorePutGetNeverPanics` is written deliberately without any `recover`
in sight — it runs a table of key shapes (slice, map, struct embedding a
slice, plain string) through `Store` and would crash the test binary
outright if `Store` could still panic, which is the sharpest possible proof
that the guarantee is structural. `TestStoreNormalizesByContentNotIdentity`
confirms `Normalize` canonicalizes by content, and
`TestNormalizeRejectsUnmarshalableValue` covers the one input `Normalize`
legitimately cannot handle — a channel. `TestStoreRejectsWhenFull` pins the
capacity boundary: a brand-new key beyond `maxEntries` is rejected, but
updating a key already present never counts against the limit.
`TestStoreConcurrentAccess` drives `Put` and `Get` from many goroutines under
`-race`.

Create `keyguard_test.go`:

```go
package keyguard

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestRawMapPanicsOnUnhashableKey reproduces the failure mode this whole
// package exists to guard against: writing an unhashable dynamic value
// (here, a []string) into a map[any]int panics at the assignment. The
// panic is caught here with a plain defer/recover to prove it is a real,
// recoverable panic -- not the unrecoverable "fatal error: concurrent map
// writes" that a data race produces.
func TestRawMapPanicsOnUnhashableKey(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic writing an unhashable key, got none")
		}
	}()

	m := map[any]int{}
	m[[]string{"tenant-a", "tenant-b"}] = 1
	t.Fatal("unreachable: the assignment above must panic")
}

// safePutRecover is the per-call-site patch an engineer reaches for before
// discovering the structural fix: it wraps a raw map[any]int write in
// defer/recover, converting the runtime panic into an error. It still
// requires every write site to remember this wrapper, and the underlying
// hash attempt still runs (and still panics internally) on every unhashable
// key -- recover only stops the panic from unwinding past this function. It
// is never exported and never reachable from the package API: Store's
// normalized Key removes the hazard from the type system's reach entirely
// instead of catching it here, one call site at a time.
func safePutRecover(m map[any]int, key any, value int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("keyguard: recovered from panic: %v", r)
		}
	}()
	m[key] = value
	return nil
}

// TestSafePutRecoverPatchCatchesPanic shows the per-call-site patch working
// -- for this one call site. It is the contrast the module is built around:
// Store's tests below prove the same input never panics in the first place,
// with no recover anywhere in sight.
func TestSafePutRecoverPatchCatchesPanic(t *testing.T) {
	t.Parallel()

	m := map[any]int{}
	err := safePutRecover(m, []string{"tenant-a"}, 1)
	if err == nil {
		t.Fatal("safePutRecover with a slice key should return an error, got nil")
	}
	if len(m) != 0 {
		t.Fatalf("len(m) = %d, want 0 (the panicking write must not have partially succeeded)", len(m))
	}
}

func TestNewStoreRejectsNonPositiveMaxEntries(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1} {
		if _, err := NewStore(n); !errors.Is(err, ErrNonPositiveMaxEntries) {
			t.Fatalf("NewStore(%d) err = %v, want ErrNonPositiveMaxEntries", n, err)
		}
	}
}

// TestStorePutGetNeverPanics is the guarded case, run without any recover:
// every key type that would panic against a raw map[any]V is normalized to
// a comparable Key first, so Store.Put/Get simply cannot reach the
// runtime's hash-of-unhashable-type panic.
func TestStorePutGetNeverPanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  any
	}{
		{"slice key", []string{"tenant-a", "tenant-b"}},
		{"map key", map[string]int{"a": 1, "b": 2}},
		{"struct embedding a slice", struct {
			Tags []string
		}{Tags: []string{"x", "y"}}},
		{"plain comparable key", "tenant-c"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := NewStore(10)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			if err := s.Put(tc.key, 99); err != nil {
				t.Fatalf("Put(%v) = %v, want nil", tc.key, err)
			}
			value, ok, err := s.Get(tc.key)
			if err != nil || !ok || value != 99 {
				t.Fatalf("Get(%v) = (%d, %v, %v), want (99, true, nil)", tc.key, value, ok, err)
			}
		})
	}
}

// TestStoreNormalizesByContentNotIdentity proves two distinct slice values
// with identical contents normalize to the same Key -- Normalize is
// canonicalizing structure, not comparing pointer identity.
func TestStoreNormalizesByContentNotIdentity(t *testing.T) {
	t.Parallel()

	s, err := NewStore(10)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put([]string{"a", "b"}, 5); err != nil {
		t.Fatalf("Put = %v, want nil", err)
	}
	value, ok, err := s.Get([]string{"a", "b"}) // a different slice, same contents
	if err != nil || !ok || value != 5 {
		t.Fatalf("Get(distinct-but-equal slice) = (%d, %v, %v), want (5, true, nil)", value, ok, err)
	}
}

func TestNormalizeRejectsUnmarshalableValue(t *testing.T) {
	t.Parallel()

	_, err := Normalize(make(chan int))
	if err == nil {
		t.Fatal("Normalize(chan int) should return an error, got nil")
	}
}

// TestStoreRejectsWhenFull covers the capacity boundary: a new key beyond
// maxEntries is rejected, but updating an already-present key never counts
// against the limit.
func TestStoreRejectsWhenFull(t *testing.T) {
	t.Parallel()

	s, err := NewStore(2)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put("a", 1); err != nil {
		t.Fatalf("Put(a): %v", err)
	}
	if err := s.Put("b", 2); err != nil {
		t.Fatalf("Put(b): %v", err)
	}
	if err := s.Put("a", 100); err != nil {
		t.Fatalf("Put(a) update at capacity: %v", err)
	}
	if err := s.Put("c", 3); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("Put(c) beyond capacity: err = %v, want ErrStoreFull", err)
	}
	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
}

// TestStoreConcurrentAccess drives Put and Get on shared keys from many
// goroutines at once, under -race: Store's doc comment promises safety for
// concurrent use, and this is what holds it to that.
func TestStoreConcurrentAccess(t *testing.T) {
	t.Parallel()

	s, err := NewStore(100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := []string{"tenant", fmt.Sprint(i % 10)}
			if err := s.Put(key, i); err != nil {
				t.Errorf("Put: %v", err)
			}
			if _, _, err := s.Get(key); err != nil {
				t.Errorf("Get: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// ExampleStore is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleStore() {
	s, err := NewStore(10)
	if err != nil {
		panic(err)
	}

	sliceKey := []string{"tenant-a", "tenant-b"}
	if err := s.Put(sliceKey, 7); err != nil {
		panic(err)
	}
	value, ok, err := s.Get([]string{"tenant-a", "tenant-b"}) // a distinct slice, same contents
	if err != nil {
		panic(err)
	}
	fmt.Printf("slice key: value=%d ok=%v\n", value, ok)

	_, ok, err = s.Get([]string{"never-stored"})
	if err != nil {
		panic(err)
	}
	fmt.Println("unknown key present:", ok)

	if _, err := Normalize(make(chan int)); err != nil {
		fmt.Println("channel key rejected:", err)
	}

	// Output:
	// slice key: value=7 ok=true
	// unknown key present: false
	// channel key rejected: keyguard: cannot normalize key of type chan int: json: unsupported type: chan int
}
```

## Review

The exercise is correct exactly when the guarantee it claims for `Store`
holds: no key shape, no matter how deeply it embeds a slice or a map, can
make `Store.Put`/`Store.Get` panic. `TestStorePutGetNeverPanics` is written
with no `recover` anywhere in it deliberately, because that is the only way
to prove a negative — if `Normalize` ever regressed to skip a case, this
test would fail by crashing the whole test binary, loudly, rather than by a
soft assertion. `safePutRecover` is kept only in the test file, as a
contrast, not a competing solution: it is the per-call-site patch an
engineer reaches for when stuck with a `map[any]V` that cannot be redesigned,
and it never appears in `Store`'s API because `Store`'s normalized `Key`
removes the hazard from the type system's reach entirely instead of catching
it after the fact. `Store` also bounds its own size with `maxEntries`, since
a cache keyed by arbitrary caller-supplied values would otherwise have no
natural limit. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — interface comparability is checked against the dynamic type at runtime, not statically.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — the map key type must be comparable, which `any` satisfies only at the static level.
- [runtime package: panic values](https://pkg.go.dev/runtime#Error) — the `runtime.Error` interface behind `hash of unhashable type`, distinct from the unrecoverable `fatal("concurrent map writes")`.
- [encoding/json: Marshal](https://pkg.go.dev/encoding/json#Marshal) — map keys are sorted before encoding, which is what makes `Normalize` canonical regardless of a source map's iteration order.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-map-of-slices-append-grouping.md](14-map-of-slices-append-grouping.md) | Next: [16-sharded-map-vs-rwmutex-contention.md](16-sharded-map-vs-rwmutex-contention.md)
