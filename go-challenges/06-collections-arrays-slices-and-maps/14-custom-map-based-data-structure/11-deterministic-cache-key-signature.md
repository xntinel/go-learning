# Exercise 11: Deterministic Cache-Key Signatures From a Build-Args Map

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A build system in the shape of Docker BuildKit or Bazel derives a
content-addressed cache key from the inputs to a step: the build arguments,
the target platform, a handful of labels. The whole point of the key is that
identical inputs produce the identical key on every machine and every run, so
a cache hit is a cache hit, and different inputs produce a different key, so a
stale artifact is never served for a changed input. Those inputs almost always
arrive as a `map[string]string` -- build args are exactly that shape in every
BuildKit frontend -- and the natural way to fold a map into a hash is to range
it and write each pair to the hasher as you go.

That natural way is broken by a property of Go maps that has nothing to do
with the build system: `range` over a map visits entries in an order the
runtime randomizes per iteration, on purpose, so that code never grows a
dependency on it. A function that hashes `for k, v := range args { h.Write(...) }`
directly does not compute one digest for a given set of args -- it computes
whichever of `n!` possible byte orderings the runtime happened to choose for
that particular call. Call it twice in the same process, over the identical
map, and it can disagree with itself. Downstream, that means a cache that
never converges: the build farm reports a miss for a target it built five
minutes ago with byte-for-byte identical arguments, because the digest of "the
same inputs" changed between the two invocations.

The fix is one line: sort the keys before writing anything. `slices.Sorted(maps.Keys(fields))`
turns "whatever order range chose this time" into "lexicographic order, every
time", which is the only property a content-addressed key can be built on.
This module builds that as a package: a `Signer` with exactly one exported
method, `Sign`, that folds a map into a hex digest deterministically, encodes
each pair so that no concatenation of adjacent fields can collide with a
different set of fields, and treats a nil map as the well-defined digest of
zero pairs rather than a special case to guard against.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
cachekey/                module example.com/cachekey
  go.mod                 go 1.24
  cachekey.go            Signer; NewSigner, Sign
  cachekey_test.go       stability table, the unsorted-range contrast, content-vs-order,
                         collision resistance, nil/empty, concurrency, ExampleSigner_Sign
```

- Files: `cachekey.go`, `cachekey_test.go`.
- Implement: `NewSigner() *Signer` returning a ready-to-use, stateless `Signer`; `(*Signer) Sign(fields map[string]string) string` returning a stable lowercase hex SHA-256 digest, built by writing each `(key, value)` pair in `slices.Sorted(maps.Keys(fields))` order, each field length-prefixed to avoid concatenation collisions.
- Test: `Sign` returns the identical digest across 50 calls on the same map; the unexported `signatureUnsorted` contrast shows the naive direct-range version disagreeing with itself across 50 calls on that same map; two maps built by inserting the identical pairs in reverse order sign identically, and a map with one field changed signs differently; `{"a":"bc"}` and `{"ab":"c"}` do not collide; a nil map and an empty map sign identically and a non-empty map does not; a map with an empty key or empty value does not collide with a different placement of the same characters; `Signer` is safe for concurrent use; and `ExampleSigner_Sign` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Range order is randomized on purpose; a hash built on it is not a hash

A Go `map` is a hash table, and since Go 1.24 the runtime backs it with a
Swiss-table implementation, but the property that matters here predates that
change: `range` over a map visits its entries in an order the runtime
deliberately randomizes, perturbing the start position on every range
statement so that no program can come to depend on a particular order. That
randomization is invisible to almost all code, because almost all code either
doesn't care about order or extracts values one key at a time. Hashing is the
one operation where it matters, because a hash function is order-sensitive by
construction -- it folds bytes in sequence, and a different sequence of the
same bytes produces a different digest almost certainly.

```go
// signatureUnsorted — the version everyone writes first.
func signatureUnsorted(fields map[string]string) string {
    h := sha256.New()
    for k, v := range fields {          // order: randomized, per call
        h.Write([]byte(k))
        h.Write([]byte(v))
    }
    return hex.EncodeToString(h.Sum(nil))
}
```

Call `signatureUnsorted` twice on the same `fields` value, with nothing
changed in between, and the two results can differ. That is not a
hypothetical: this module's test calls it fifty times on one map and asserts
that more than one distinct digest comes out, which is what actually happens
in practice on a map with a handful of entries. A cache key built this way
does not identify "these inputs" -- it identifies "these inputs, hashed in
whichever order happened this time", which is not a useful thing to key a
cache on.

`slices.Sorted(maps.Keys(fields))` fixes it by imposing an order that has
nothing to do with the runtime's internal bucket layout: lexicographic order
on the keys, which is a total order and therefore always the same for a given
set of keys. `Sign` also writes each field length-prefixed rather than
concatenated, because sorted order alone does not prevent a different
ambiguity: `"a"+"bc"` and `"ab"+"c"` are the same bytes even though they come
from different (key, value) pairs. A build system that hashed one key's
value directly against the next key's name would occasionally alias two
genuinely different sets of build args onto the same key.

Create `cachekey.go`:

```go
// Package cachekey derives a stable, content-addressed cache key from a map
// of build arguments and labels, the way a build system (Docker BuildKit,
// Bazel) fingerprints a step so that identical inputs hit the cache and
// different inputs miss it.
//
// The one property that matters is that the digest depends only on the set
// of (key, value) pairs, never on the order the caller built the map in or
// the order Go's runtime happens to range it in on a given call.
package cachekey

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"maps"
	"slices"
)

// Signer computes stable hex digests over a map of string fields.
//
// A Signer holds no mutable state; NewSigner returns a value that is ready
// to use immediately. It is safe for concurrent use by multiple goroutines:
// Sign allocates a fresh hash.Hash on every call and never touches shared
// state.
type Signer struct{}

// NewSigner returns a Signer ready to compute cache-key signatures.
func NewSigner() *Signer {
	return &Signer{}
}

// Sign returns a stable, lowercase hex-encoded SHA-256 digest of fields.
//
// The digest is a pure function of the (key, value) pairs present in
// fields. Sign sorts the keys with slices.Sorted(maps.Keys(fields)) before
// writing anything to the hash, so two calls over maps holding the
// identical set of pairs always produce the identical digest -- regardless
// of insertion order, regardless of process, and regardless of the
// iteration order Go's runtime chooses for any single range statement,
// which is randomized per call and must never leak into a cache key.
//
// Each pair is written as a length-prefixed (key, value) block, so that no
// concatenation of adjacent fields can collide with a different set of
// fields: {"a": "bc"} and {"ab": "c"} hash to different digests even though
// naive concatenation of "a"+"bc" and "ab"+"c" would produce the same bytes.
//
// A nil or empty fields map is valid input; it produces the well-defined
// digest of zero pairs, not an error. Sign does not retain or mutate
// fields.
func (s *Signer) Sign(fields map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		writeBlock(h, k)
		writeBlock(h, fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeBlock writes s to h prefixed with its length as an 8-byte
// big-endian integer, which is what makes the length-prefixed encoding in
// Sign collision-resistant against concatenation ambiguity.
func writeBlock(h hash.Hash, s string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(s)))
	h.Write(length[:])
	h.Write([]byte(s))
}
```

### Using it

A `Signer` carries no configuration and no mutable state, so `NewSigner`
never fails and the returned value can be shared freely across every build
worker in a process -- there is nothing for concurrent calls to contend over,
because `Sign` allocates its own `hash.Hash` on every call rather than
reusing one. Call `Sign` once per build step with that step's resolved args
and labels, and compare the result against whatever the cache already has
under that key: equal digests mean equal inputs, by construction, not by
convention. `Sign` never retains or mutates the map it is given, so the
caller is free to keep building on the same map after signing it.

The `Example` below is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift from the code that actually
runs.

```go
func ExampleSigner_Sign() {
	s := NewSigner()

	insertedForward := map[string]string{
		"GO_VERSION": "1.24",
		"TARGET":     "linux/amd64",
	}
	insertedReverse := map[string]string{}
	insertedReverse["TARGET"] = "linux/amd64"
	insertedReverse["GO_VERSION"] = "1.24"

	sig := s.Sign(insertedForward)
	fmt.Println("same content, same signature:", sig == s.Sign(insertedReverse))

	changed := map[string]string{
		"GO_VERSION": "1.25",
		"TARGET":     "linux/amd64",
	}
	fmt.Println("different content, different signature:", sig != s.Sign(changed))

	fmt.Println(sig)

	// Output:
	// same content, same signature: true
	// different content, different signature: true
	// cf28b8f1ba697c2fcdf23989e19c5fef3aa8552198eeb432d0d8c7f53f0fd0d3
}
```

### Tests

`TestSignIsStableAcrossCalls` calls `Sign` fifty times on the same map and
asserts every call returns the identical digest -- the property the whole
package exists to guarantee. `TestUnsortedRangeIsUnstable` is the module's
core test: `signatureUnsorted` is unexported and unreachable from the
package API, and exists only so the test can pin, numerically, what it gets
wrong -- fifty calls on the same map produce more than one distinct digest --
immediately followed by the same fifty-call loop through `Sign` on the
identical map producing exactly one. `TestSignDependsOnlyOnContent` builds
two maps by inserting the same three pairs in reverse order and asserts they
sign identically, then changes one value and asserts the digest changes.
`TestSignAvoidsConcatenationCollision` and
`TestSignEmptyStringFieldsAreNotAmbiguous` pin the length-prefix encoding
against the two concatenation ambiguities it is meant to prevent.
`TestSignHandlesEmptyAndNil` checks that `nil` and an empty map sign
identically, and that the resulting digest is a well-formed 64-character hex
string. `TestSignerIsSafeForConcurrentUse` runs twenty goroutines signing
copies of the same fields under `-race` and asserts every result agrees.

Create `cachekey_test.go`:

```go
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
)

// signatureUnsorted is the mapper as it is usually written the first time:
// it hashes the fields map by ranging it directly, in whatever order Go's
// runtime happens to choose for that particular range statement. It is
// never exported and never reachable from the package API; it exists only
// so the tests can pin the instability it produces.
func signatureUnsorted(fields map[string]string) string {
	h := sha256.New()
	for k, v := range fields {
		h.Write([]byte(k))
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func buildArgs() map[string]string {
	return map[string]string{
		"GO_VERSION":  "1.24",
		"TARGET":      "linux/amd64",
		"BASE_IMAGE":  "golang:1.24-alpine",
		"BUILD_STAGE": "release",
		"VCS_REF":     "a1b2c3d",
		"LABEL_TEAM":  "platform",
		"CACHE_EPOCH": "7",
		"FEATURE_SET": "lambda,server",
	}
}

func TestSignIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	s := NewSigner()
	fields := buildArgs()

	seen := map[string]struct{}{}
	for range 50 {
		seen[s.Sign(fields)] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("Sign produced %d distinct digests over 50 calls on the same map, want 1", len(seen))
	}
}

// TestUnsortedRangeIsUnstable is the heart of the module: it pins the exact
// defect signatureUnsorted has and Sign does not. Ranging a map directly
// visits its entries in an order the Go runtime randomizes per range
// statement, so hashing them as encountered can produce a different digest
// on different calls over the identical map, even within one process.
func TestUnsortedRangeIsUnstable(t *testing.T) {
	t.Parallel()

	fields := buildArgs()

	seen := map[string]struct{}{}
	for range 50 {
		seen[signatureUnsorted(fields)] = struct{}{}
	}
	if len(seen) <= 1 {
		t.Fatalf("signatureUnsorted produced %d distinct digest(s) over 50 calls on the same map; "+
			"want more than 1 -- either range order stopped being randomized, or this environment "+
			"got unlucky and the test should be rerun", len(seen))
	}

	s := NewSigner()
	sorted := map[string]struct{}{}
	for range 50 {
		sorted[s.Sign(fields)] = struct{}{}
	}
	if len(sorted) != 1 {
		t.Fatalf("Sign produced %d distinct digests over 50 calls on the same map, want exactly 1", len(sorted))
	}
}

func TestSignDependsOnlyOnContent(t *testing.T) {
	t.Parallel()

	s := NewSigner()

	// Two maps built with the same pairs inserted in reverse order must
	// sign identically: content, not insertion order, is what the digest
	// depends on.
	a := map[string]string{}
	a["GO_VERSION"] = "1.24"
	a["TARGET"] = "linux/amd64"
	a["VCS_REF"] = "a1b2c3d"

	b := map[string]string{}
	b["VCS_REF"] = "a1b2c3d"
	b["TARGET"] = "linux/amd64"
	b["GO_VERSION"] = "1.24"

	if s.Sign(a) != s.Sign(b) {
		t.Fatal("Sign disagreed on two maps holding the identical pairs")
	}

	c := map[string]string{"GO_VERSION": "1.25", "TARGET": "linux/amd64", "VCS_REF": "a1b2c3d"}
	if s.Sign(a) == s.Sign(c) {
		t.Fatal("Sign agreed on two maps holding different pairs")
	}
}

func TestSignAvoidsConcatenationCollision(t *testing.T) {
	t.Parallel()

	s := NewSigner()

	left := map[string]string{"a": "bc"}
	right := map[string]string{"ab": "c"}
	if s.Sign(left) == s.Sign(right) {
		t.Fatal(`Sign({"a":"bc"}) collided with Sign({"ab":"c"}); the length-prefix encoding failed`)
	}
}

func TestSignHandlesEmptyAndNil(t *testing.T) {
	t.Parallel()

	s := NewSigner()

	nilDigest := s.Sign(nil)
	emptyDigest := s.Sign(map[string]string{})
	if nilDigest != emptyDigest {
		t.Fatalf("Sign(nil) = %q, Sign(empty map) = %q, want equal", nilDigest, emptyDigest)
	}
	if len(nilDigest) != sha256.Size*2 {
		t.Fatalf("digest length = %d, want %d hex characters", len(nilDigest), sha256.Size*2)
	}

	nonEmpty := s.Sign(map[string]string{"k": "v"})
	if nonEmpty == nilDigest {
		t.Fatal("a non-empty map signed the same as an empty one")
	}
}

func TestSignEmptyStringFieldsAreNotAmbiguous(t *testing.T) {
	t.Parallel()

	s := NewSigner()

	withEmptyValue := map[string]string{"k": ""}
	withEmptyKey := map[string]string{"": "k"}
	if s.Sign(withEmptyValue) == s.Sign(withEmptyKey) {
		t.Fatal(`Sign({"k":""}) collided with Sign({"":"k"})`)
	}
}

func TestSignerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	s := NewSigner()
	want := s.Sign(buildArgs())

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fields := buildArgs()
			if got := s.Sign(fields); got != want {
				t.Errorf("concurrent Sign = %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
}

// ExampleSigner_Sign is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleSigner_Sign() {
	s := NewSigner()

	insertedForward := map[string]string{
		"GO_VERSION": "1.24",
		"TARGET":     "linux/amd64",
	}
	insertedReverse := map[string]string{}
	insertedReverse["TARGET"] = "linux/amd64"
	insertedReverse["GO_VERSION"] = "1.24"

	sig := s.Sign(insertedForward)
	fmt.Println("same content, same signature:", sig == s.Sign(insertedReverse))

	changed := map[string]string{
		"GO_VERSION": "1.25",
		"TARGET":     "linux/amd64",
	}
	fmt.Println("different content, different signature:", sig != s.Sign(changed))

	fmt.Println(sig)

	// Output:
	// same content, same signature: true
	// different content, different signature: true
	// cf28b8f1ba697c2fcdf23989e19c5fef3aa8552198eeb432d0d8c7f53f0fd0d3
}
```

## Review

`Sign` is correct when the same set of `(key, value)` pairs always produces
the same digest, regardless of how the caller built the map or which order
the runtime chose to range it in on a given call -- that is the entire
contract a content-addressed cache key exists to uphold. The trap is that
`range` over a map is randomized per call by design, so folding it directly
into a hash produces a function that disagrees with itself, which
`TestUnsortedRangeIsUnstable` demonstrates by calling the unexported,
unreachable `signatureUnsorted` fifty times on one map and observing more
than one result. `slices.Sorted(maps.Keys(fields))` fixes the order; the
length-prefix encoding in `writeBlock` fixes a second, independent trap where
sorted-but-concatenated fields can alias a different set of fields onto the
same bytes. `Signer` carries no mutable state, so `NewSigner` cannot fail and
the result is shared safely across goroutines, each call to `Sign` owning
its own `hash.Hash`. Run `go test -count=1 -race ./...` to confirm the
stability table, the instability contrast, the content-vs-order and
collision-resistance cases, and the concurrent-use test.

## Resources

- [`maps.Keys`](https://pkg.go.dev/maps#Keys) — the iterator over a map's keys that `slices.Sorted` consumes.
- [`slices.Sorted`](https://pkg.go.dev/slices#Sorted) — collects an iterator into a sorted slice; the fix for map range's randomized order.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — the digest algorithm `Sign` uses.
- [Go Wiki: Why does Go not have deterministic map iteration?](https://go.dev/blog/maps#iteration-order) — the design rationale for the randomization this module works around.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-maphash-comparable-index.md](10-maphash-comparable-index.md) | Next: [12-tenant-override-resolver.md](12-tenant-override-resolver.md)
