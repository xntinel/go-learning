# Exercise 12: A [16]byte Session ID as a Zero-Allocation Idempotency Key

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An idempotency guard for a payments API or a message-queue consumer answers
one question fast: "have I already handled this request ID." The natural
storage is `map[SessionID]struct{}`, but the ID usually arrives as raw
bytes off the wire — a `[]byte`, which cannot be a map key at all, the
compiler rejects it outright. Most implementations paper over this with
`string(idBytes)`, allocating and copying on every single lookup in a hot
path, or they skip the length check before converting and let a truncated
request take down the goroutine handling it. This module builds the correct
fix: parse the ID once into a `[16]byte` array — comparable and hashable by
construction — guard the conversion against short input, and key directly
on the array with zero allocation per check, safely from any number of
concurrent handler goroutines at once.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
sessionidempotency/          module example.com/sessionidempotency
  go.mod                     go 1.24
  guard.go                   SessionID [16]byte; ParseSessionID; Guard; ErrShortID
  guard_test.go               naive-parse-panics contrast, parse table, first-seen semantics,
                              concurrent exactly-once, zero-allocation repeat lookup,
                              ExampleGuard_FirstSeen
```

- Files: `guard.go`, `guard_test.go`.
- Implement: `type SessionID [16]byte`; `ParseSessionID(b []byte) (SessionID, error)` guarding length before a slice-to-array conversion and returning `ErrShortID` on a short buffer; `Guard` backed by `map[SessionID]struct{}` and a `sync.Mutex`, with `FirstSeen(id SessionID) bool` and `Len() int`.
- Test: the unexported `parseSessionIDNaive` contrast proving the unguarded conversion panics; a parse table over exact length, longer, short, and empty input; `FirstSeen` first-seen semantics for one and for two distinct ids; many goroutines racing `FirstSeen` on overlapping ids, asserting under `-race` that exactly one caller per id ever observes true; a zero-allocation property on a repeated lookup; and `ExampleGuard_FirstSeen` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the array type is what makes the map possible at all

Go requires a map's key type to be comparable. Slices, maps, and functions
are explicitly *not* comparable, so `map[[]byte]struct{}` is not a type
error you can work around — it simply does not compile:
`invalid map key type []byte`. This is not a style nitpick; it is the
compiler refusing to let you build a data structure whose core operation
(hash the key, compare it for equality) is undefined for the type you gave
it. A slice's identity is a pointer, length, and capacity, none of which is
"the bytes" in the sense two independently-received copies of the same ID
would share.

A `[16]byte` array has none of that ambiguity. Its size is fixed at compile
time and every element is a plain `byte`, so the whole array is comparable:
`==` compares all sixteen bytes, and the runtime can hash it the same way.
`map[SessionID]struct{}` compiles and behaves exactly as you would want —
two session IDs with the same sixteen bytes, parsed independently from two
different requests, hash to the same bucket and are `==`. This is precisely
why UUIDs, trace IDs, and session tokens are almost always modeled as
`[16]byte` at the point they become map keys, even though they usually
arrive over the wire as a `[]byte` or a hyphenated string.

The naive fix many codebases reach for instead is `string(idBytes)` used as
the key. That compiles and works, but `string([]byte)` always allocates a
new backing array and copies every byte — on every single idempotency
check, in the hottest path of the request pipeline. Parsing into a
`SessionID` array does the copy exactly once, at ingestion, and every
lookup after that is a zero-allocation map access on a plain value type.

There is a second, sharper trap in that one-time parse: the Go 1.20+
slice-to-array conversion `SessionID(b[:16])` panics if `b` is shorter than
16 bytes. A version that skips the length check —

```go
func parseSessionIDNaive(b []byte) SessionID {
    return SessionID(b[:16]) // panics if len(b) < 16
}
```

— turns a truncated network read into a crashed goroutine instead of a
clean error. `ParseSessionID` guards `len(b) < 16` before ever attempting
the conversion; the naive version above is never exported, it exists only
in this module's test file to prove the panic is real.

Create `guard.go`:

```go
// Package sessionidempotency guards a payments API or a message-queue
// consumer against handling the same request twice.
//
// The natural storage for "have I seen this ID before" is
// map[SessionID]struct{}, but a session ID usually arrives off the wire as a
// []byte -- and a slice cannot be a map key at all; the compiler rejects it
// outright. This package parses the wire bytes once into a SessionID array,
// which is comparable and hashable by construction, and keys directly on
// that, with zero allocation per lookup after parsing.
package sessionidempotency

import (
	"errors"
	"sync"
)

// ErrShortID means the source bytes are too short to hold a SessionID.
var ErrShortID = errors.New("sessionidempotency: source shorter than 16 bytes")

// SessionID is a 16-byte session/request identifier, the same size as a
// UUID. Being a [16]byte array, it is comparable and hashable, so it can be
// used directly as a map key -- unlike a []byte, which is neither and is
// rejected by the compiler as a map key type. Callers that receive an id off
// the wire as a []byte convert it once with ParseSessionID and then carry
// the array value from there, with no further allocation.
type SessionID [16]byte

// ParseSessionID reads a SessionID from the first 16 bytes of b. It guards
// the length before the Go 1.20+ slice-to-array conversion, which panics if
// b is shorter than 16 bytes, so a truncated wire read returns ErrShortID
// instead of crashing the goroutine handling it.
func ParseSessionID(b []byte) (SessionID, error) {
	if len(b) < 16 {
		return SessionID{}, ErrShortID
	}
	return SessionID(b[:16]), nil
}

// Guard is an idempotency filter keyed directly on SessionID. It answers
// "have I already handled this request" the way a payments API or a
// message-queue consumer must before applying a side effect twice: the first
// call for a given id records it and reports true (safe to proceed); every
// later call for the same id reports false (already handled, skip).
//
// Keying map[SessionID]struct{} directly on the array avoids the classic
// string(id[:]) conversion many implementations reach for to get a
// comparable key -- that conversion allocates and copies on every single
// lookup. A [16]byte needs neither: it is already comparable and hashable,
// so it drops straight into the map key position with zero allocation.
//
// Guard is safe for concurrent use by multiple goroutines.
type Guard struct {
	mu   sync.Mutex
	seen map[SessionID]struct{}
}

// NewGuard returns an empty Guard.
func NewGuard() *Guard {
	return &Guard{seen: make(map[SessionID]struct{})}
}

// FirstSeen reports whether id has not been recorded before, recording it as
// a side effect so every later call for the same id returns false. Exactly
// one caller, out of any number racing on the same id, ever observes true;
// every other concurrent or later caller for that id observes false and
// must treat the request as an already-applied duplicate.
func (g *Guard) FirstSeen(id SessionID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.seen[id]; ok {
		return false
	}
	g.seen[id] = struct{}{}
	return true
}

// Len reports how many distinct session ids the Guard has recorded.
func (g *Guard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.seen)
}
```

### Using it

`ParseSessionID` is the one place a `[]byte` off the wire becomes a
`SessionID`; every handler downstream of it carries the array value, never
raw bytes again. `Guard` is constructed once per process (or per
idempotency scope) and shared by every request-handling goroutine — its
mutex is what makes `FirstSeen`'s "exactly once per id" promise hold even
when many requests for the same id race in at once, which
`TestConcurrentFirstSeenExactlyOncePerID` confirms under `-race`.

The module has no `main.go`, because an idempotency guard is a library, not
a tool. Its executable demonstration is `ExampleGuard_FirstSeen`: `go test`
runs it and compares its standard output against the `// Output:` comment,
so the usage shown below cannot drift away from the code.

Two contracts cross the package boundary and both are documented on the
type. `SessionID` is a plain value, not a pointer or a slice header, so
passing one around, storing one in a struct, or using one as a map key never
aliases another caller's copy — there is nothing to alias, the sixteen bytes
are the whole value. `Guard` is safe for concurrent use, which is the only
reason it is useful in a request handler at all: a payments API typically
runs one goroutine per inbound request, and every one of them needs to
consult the same idempotency state before applying a side effect.

### Tests

`TestNaiveParsePanicsOnShortBuffer` is the antipattern contrast: it calls
the unexported `parseSessionIDNaive`, which performs the same conversion
`ParseSessionID` does but with no length guard in front of it, and asserts
the panic actually happens on a 15-byte buffer. `TestParseSessionID` is the
table over the real function: exact length, longer, short, and empty,
checking both the returned bytes and the `ErrShortID` sentinel.

`TestFirstSeenOnce` and `TestFirstSeenDistinctIDsIndependent` pin the
ordinary idempotency contract for one and for two ids.
`TestConcurrentFirstSeenExactlyOncePerID` is the concurrency case: twenty
distinct ids, fifty goroutines racing on each, and the assertion that every
id's true-count across all its racing callers is exactly one — proving the
mutex closes the lookup-then-write race window rather than merely making it
rare. `TestFirstSeenAllocatesNothingOnRepeat` pins the zero-allocation claim
from the package doc comment with `testing.AllocsPerRun`; it deliberately
skips `t.Parallel`, since `AllocsPerRun` panics if invoked from a parallel
test.

Create `guard_test.go`:

```go
package sessionidempotency

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// parseSessionIDNaive mirrors the raw Go 1.20+ slice-to-array conversion
// with no length guard in front of it. It is unexported and unreachable
// from the package API; it exists only so the test below can demonstrate,
// rather than merely describe, why ParseSessionID checks len(b) first.
func parseSessionIDNaive(b []byte) SessionID {
	return SessionID(b[:16])
}

func TestNaiveParsePanicsOnShortBuffer(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("parseSessionIDNaive on a 15-byte buffer should panic; it did not")
		}
	}()
	_ = parseSessionIDNaive(make([]byte, 15))
}

func TestParseSessionID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		n       int
		wantErr error
	}{
		{name: "exactly 16 bytes", n: 16},
		{name: "more than 16 bytes", n: 20},
		{name: "15 bytes", n: 15, wantErr: ErrShortID},
		{name: "empty", n: 0, wantErr: ErrShortID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := make([]byte, tc.n)
			for i := range raw {
				raw[i] = byte(i)
			}

			id, err := ParseSessionID(raw)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseSessionID(%d bytes) err = %v, want %v", tc.n, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			for i := range id {
				if id[i] != byte(i) {
					t.Fatalf("id[%d] = %d, want %d", i, id[i], i)
				}
			}
		})
	}
}

func TestFirstSeenOnce(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	id := SessionID{1, 2, 3, 4}

	if !g.FirstSeen(id) {
		t.Fatal("FirstSeen on a new id should return true")
	}
	if g.FirstSeen(id) {
		t.Fatal("FirstSeen on a repeated id should return false")
	}
	if got := g.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestFirstSeenDistinctIDsIndependent(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	a := SessionID{1}
	b := SessionID{2}

	if !g.FirstSeen(a) {
		t.Fatal("FirstSeen(a) should return true the first time")
	}
	if !g.FirstSeen(b) {
		t.Fatal("FirstSeen(b) should return true the first time, independent of a")
	}
	if g.FirstSeen(a) {
		t.Fatal("FirstSeen(a) should return false the second time")
	}
	if got := g.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
}

// TestConcurrentFirstSeenExactlyOncePerID launches many goroutines that all
// race to call FirstSeen on a small, fixed set of ids, some overlapping.
// Under -race, exactly one caller per distinct id may ever observe true; the
// Guard's mutex is what keeps a duplicate delivery from slipping through a
// race window between the map lookup and the map write.
func TestConcurrentFirstSeenExactlyOncePerID(t *testing.T) {
	t.Parallel()

	const numIDs = 20
	const attemptsPerID = 50

	g := NewGuard()
	var trueCount [numIDs]int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for id := 0; id < numIDs; id++ {
		for attempt := 0; attempt < attemptsPerID; attempt++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				sid := SessionID{byte(id)}
				if g.FirstSeen(sid) {
					mu.Lock()
					trueCount[id]++
					mu.Unlock()
				}
			}(id)
		}
	}
	wg.Wait()

	for id, count := range trueCount {
		if count != 1 {
			t.Errorf("id %d: FirstSeen returned true %d times, want exactly 1", id, count)
		}
	}
	if got := g.Len(); got != numIDs {
		t.Fatalf("Len() = %d, want %d", got, numIDs)
	}
}

// TestFirstSeenAllocatesNothingOnRepeat pins the "zero allocation per
// lookup" claim in the package doc comment: once an id has been recorded,
// every later FirstSeen call for it is a plain map read on a value type,
// with no string conversion and no allocation.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestFirstSeenAllocatesNothingOnRepeat(t *testing.T) {
	g := NewGuard()
	id := SessionID{9, 9, 9}
	g.FirstSeen(id) // first call: records id, one map write

	allocs := testing.AllocsPerRun(100, func() {
		g.FirstSeen(id)
	})
	if allocs != 0 {
		t.Fatalf("FirstSeen on an already-recorded id allocated %v times, want 0", allocs)
	}
}

// ExampleGuard_FirstSeen is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment below.
func ExampleGuard_FirstSeen() {
	g := NewGuard()

	raw := make([]byte, 16)
	for i := range raw {
		raw[i] = byte(i)
	}
	id, err := ParseSessionID(raw)
	if err != nil {
		panic(err)
	}

	fmt.Println("first request:", g.FirstSeen(id))
	fmt.Println("retry of same request:", g.FirstSeen(id))

	if _, err := ParseSessionID(raw[:10]); errors.Is(err, ErrShortID) {
		fmt.Println("truncated id rejected:", err)
	}

	fmt.Println("distinct ids recorded:", g.Len())

	// Output:
	// first request: true
	// retry of same request: false
	// truncated id rejected: sessionidempotency: source shorter than 16 bytes
	// distinct ids recorded: 1
}
```

## Review

The design is correct when `FirstSeen` behaves as a pure idempotency gate —
true exactly once per distinct ID, false on every repeat, even under
concurrent racing callers — which `TestFirstSeenOnce`,
`TestFirstSeenDistinctIDsIndependent`, and
`TestConcurrentFirstSeenExactlyOncePerID` pin directly. The deeper point of
the exercise is what a passing happy-path test would not show on its own:
`map[[]byte]struct{}` would not have compiled at all,
`map[string(id[:])]struct{}` would compile but allocate on every lookup, and
an unguarded slice-to-array conversion would panic on any truncated wire
read. `SessionID`'s `[16]byte` type is what turns "comparable, hashable,
zero-allocation map key" from a hope into a compiler-enforced guarantee,
`ParseSessionID`'s length guard is what keeps a short ID from panicking
instead of returning a clean error, and `Guard`'s mutex is what keeps that
guarantee true when many handler goroutines call `FirstSeen` at once. Run
`go test -count=1 -race ./...`.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — which types are comparable, and therefore valid map key types.
- [Go Specification: Conversions from slice to array or array pointer](https://go.dev/ref/spec#Conversions_from_slice_to_array_or_array_pointer) — the Go 1.20+ conversion `ParseSessionID` guards before using.
- [testing.AllocsPerRun](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe used to pin the zero-allocation repeat lookup.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-array-value-semantics-config-snapshot.md](11-array-value-semantics-config-snapshot.md) | Next: [13-status-class-counter-array.md](13-status-class-counter-array.md)
