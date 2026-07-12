# Exercise 14: Backup Chunk Store Integrity Check with EqualFunc

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A content-addressable backup tool like restic splits every file into chunks,
hashes each one, and stores it once under that hash -- identical chunks across
different backups share the same entry. Verifying a snapshot means comparing
two chunk stores, local against remote, or this run against the last known-good
one, for bit-for-bit equality: same hashes, same bytes behind every hash. That
comparison forces a compile-time wall `00-concepts.md` documents but that most
code never actually runs into until it tries to write exactly this check:
`maps.Equal` requires its value type to be `comparable`, and `map[string][]byte`
never satisfies that, because a slice is never comparable. `maps.EqualFunc`
with `bytes.Equal` is not a nicer way to write the comparison; for this map
shape, it is the only one the compiler accepts at all.

The trap this exposes is not the compile error itself -- that one is loud and
immediate, and nobody ships past it by accident. The trap is what a developer
reaches for once `maps.Equal` refuses to compile and they go looking for a
workaround that avoids `EqualFunc` entirely: compare `len(a) == len(b)`, or
compare the two stores' key sets, and call that "close enough" to equality.
Both checks pass on a corrupted store that has every hash the good one has,
with the wrong bytes behind one of them -- exactly the failure a backup
integrity check exists to catch, silently missed by a comparison that never
looks at a single byte of chunk data.

This module builds `chunkstore`, a `ChunkStore` type and an `Identical`
function built on `maps.EqualFunc`, plus a `Corrupted` helper that names which
hashes disagree once `Identical` has already said no.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
chunkstore/                module example.com/chunkstore
  go.mod                   go 1.24
  chunkstore.go            ChunkStore; Identical; Corrupted
  chunkstore_test.go       Identical table, Corrupted table, identicalCountOnly
                          contrast, concurrent-reads test, ExampleIdentical
```

- Files: `chunkstore.go`, `chunkstore_test.go`.
- Implement: `type ChunkStore map[string][]byte`; `Identical(a, b ChunkStore) bool` built on `maps.EqualFunc(a, b, bytes.Equal)`; `Corrupted(a, b ChunkStore) []string` returning the sorted hashes present on both sides with differing bytes.
- Test: the `Identical` table (identical content behind distinct slice headers, one corrupted chunk, a missing hash on either side, nil/empty equivalence); the `Corrupted` table (no mismatch, one mismatch, several mismatches sorted, a missing-not-corrupted hash); the `identicalCountOnly` contrast proving a key-set-only check misses the exact corruption case `Identical` catches; a concurrent-reads test; `ExampleIdentical` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/14-chunk-store-integrity-equalfunc
cd go-solutions/06-collections-arrays-slices-and-maps/10-maps-package/14-chunk-store-integrity-equalfunc
go mod edit -go=1.24
```

### comparable is a compiler question, not a runtime one

`maps.Equal[M1, M2 ~map[K]V, K, V comparable](m1 M1, m2 M2) bool` states its
constraint directly in the type parameter: `V comparable`. `ChunkStore` is
`map[string][]byte`, so `V` is `[]byte`, and a slice type is never
`comparable` -- not sometimes, not depending on what's in it, never. A call
to `maps.Equal(a, b)` where `a, b ChunkStore` does not type-check at all; the
build fails before a single test runs. This is the point `00-concepts.md`
makes about `maps.Equal`'s constraint being enforced by the compiler rather
than by a runtime panic, made concrete: there is no "it compiles but panics
on a slice value" case to guard against, because the offending program never
compiles in the first place.

`maps.EqualFunc[M1, M2 ~map[K]V, K comparable, V1, V2 any](m1 M1, m2 M2, eq
func(V1, V2) bool) bool` drops the `comparable` requirement on the value type
entirely and asks for an explicit equality function instead. For raw chunk
bytes, that function is `bytes.Equal`: length first, then every byte. The
workaround that avoids writing an equality function at all is the one this
module isolates:

```go
func identicalCountOnly(a, b ChunkStore) bool {
    if len(a) != len(b) {
        return false
    }
    for hash := range a {
        if _, ok := b[hash]; !ok {
            return false
        }
    }
    return true
}
```

This reports `true` for two stores that agree on every hash and disagree on
what one of those hashes contains. It is not a smaller version of the real
check; it is a different, weaker question -- "do these stores name the same
chunks" instead of "do these stores agree on what those chunks contain" --
and a backup tool that asks the weaker question has no integrity check at
all, just an inventory match.

Create `chunkstore.go`:

```go
// Package chunkstore models a content-addressable backup chunk store like
// restic: a map from content hash to the raw bytes of the chunk it
// addresses. It exists to show the compile-time wall maps.Equal hits here:
// []byte is not comparable, so ChunkStore's value type cannot satisfy
// maps.Equal's comparable constraint, and maps.EqualFunc with an explicit
// equality function is not a stylistic choice but the only option that
// compiles.
package chunkstore

import (
	"bytes"
	"maps"
	"slices"
)

// ChunkStore maps a content hash to the raw bytes of the chunk it
// addresses.
//
// ChunkStore is not safe for concurrent use while being written to; the
// caller must synchronize any goroutine that assigns into or deletes from a
// shared ChunkStore. Concurrent calls to Identical against a ChunkStore that
// no goroutine is writing to are safe, the same guarantee any read-only map
// access carries.
type ChunkStore map[string][]byte

// Identical reports whether a and b contain exactly the same set of hashes,
// each mapping to bit-for-bit identical bytes. A nil ChunkStore compares
// equal to an empty one; both have zero entries.
//
// []byte is not comparable, so a call to maps.Equal(a, b) does not compile
// for a ChunkStore: maps.Equal requires its value type to satisfy
// comparable, and a slice never does. maps.EqualFunc accepts an explicit
// equality function instead, and bytes.Equal is exactly the right one for
// raw chunk data: it compares length and every byte, which is the
// definition of "these two backups agree on this chunk."
func Identical(a, b ChunkStore) bool {
	return maps.EqualFunc(a, b, bytes.Equal)
}

// Corrupted returns the hashes present in both a and b whose bytes differ,
// sorted for deterministic output. This is Identical's diagnostic
// counterpart: where Identical answers "do these snapshots agree", the
// Corrupted list answers "which chunks do they disagree about", the next
// question a backup tool asks once Identical has already said no.
//
// Corrupted reports only hashes present on both sides with differing
// content; it does not report a hash missing from one store entirely. A
// caller that also needs to catch missing chunks should compare the two
// stores' key sets, or their lengths, separately.
//
// The returned slice is freshly allocated on every call and never aliases
// a or b.
func Corrupted(a, b ChunkStore) []string {
	var bad []string
	for hash, want := range a {
		if got, ok := b[hash]; ok && !bytes.Equal(want, got) {
			bad = append(bad, hash)
		}
	}
	slices.Sort(bad)
	return bad
}
```

### Using it

Both functions take two `ChunkStore` values and return a plain result, so
there is nothing to construct: build a `ChunkStore` however chunks are read
in -- from disk, from a manifest, from a remote listing -- and call
`Identical` to decide whether a snapshot verification passes. When it does
not, `Corrupted` narrows the failure to the exact hashes that disagree, which
is what a repair or re-upload step needs to target instead of re-transferring
the whole snapshot. Neither function mutates or retains either input.

`ExampleIdentical` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleIdentical() {
	local := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshot"),
	}
	remote := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshot"),
	}
	fmt.Println(Identical(local, remote))

	corrupted := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshoT"), // one byte differs
	}
	fmt.Println(Identical(local, corrupted))
	fmt.Println(Corrupted(local, corrupted))

	// Output:
	// true
	// false
	// [c3d4]
}
```

### Tests

`TestIdentical` is the table: content-equal stores built from distinct slice
headers (proving the comparison looks at bytes, not slice identity), one
corrupted chunk, a hash missing from either side, and the nil/empty
equivalences a real snapshot walk will eventually produce. `TestCorrupted`
covers the same shape from the diagnostic side, including that a hash
missing from one store is reported as a mismatch by `Identical` but not
listed by `Corrupted`, which only names hashes present on both sides with
different bytes. `identicalCountOnly` is the unexported antipattern, and
`TestCountOnlyMissesCorruptionThatIdenticalCatches` is the module's center of
gravity: two stores share every hash, so the count-only check says they
match, and `Identical` says they don't, because one chunk's bytes silently
differ. `TestConcurrentReadsDoNotRace` exercises the concurrency contract in
the type's doc comment under `-race`.

Create `chunkstore_test.go`:

```go
package chunkstore

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestIdentical(t *testing.T) {
	t.Parallel()

	base := ChunkStore{
		"hash-a": []byte("the quick brown fox"),
		"hash-b": []byte("jumps over the lazy dog"),
	}

	tests := []struct {
		name string
		a, b ChunkStore
		want bool
	}{
		{
			name: "identical stores, distinct slice headers",
			a:    base,
			b: ChunkStore{
				"hash-a": []byte("the quick brown fox"),
				"hash-b": []byte("jumps over the lazy dog"),
			},
			want: true,
		},
		{
			name: "one chunk silently corrupted",
			a:    base,
			b: ChunkStore{
				"hash-a": []byte("the quick brown fox"),
				"hash-b": []byte("jumps over the lazy dof"), // one byte flipped
			},
			want: false,
		},
		{
			name: "b is missing a hash entirely",
			a:    base,
			b: ChunkStore{
				"hash-a": []byte("the quick brown fox"),
			},
			want: false,
		},
		{
			name: "b has an extra hash a does not",
			a: ChunkStore{
				"hash-a": []byte("the quick brown fox"),
			},
			b:    base,
			want: false,
		},
		{name: "both nil", a: nil, b: nil, want: true},
		{name: "both empty", a: ChunkStore{}, b: ChunkStore{}, want: true},
		{
			name: "a nil chunk's bytes equal an empty chunk's bytes",
			a:    ChunkStore{"hash-a": nil},
			b:    ChunkStore{"hash-a": []byte{}},
			want: true,
		},
		{name: "nil compares equal to empty", a: nil, b: ChunkStore{}, want: true},
		{
			name: "nil against a populated store",
			a:    nil,
			b:    ChunkStore{"hash-a": []byte("x")},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Identical(tc.a, tc.b); got != tc.want {
				t.Fatalf("Identical() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCorrupted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b ChunkStore
		want []string
	}{
		{
			name: "no shared hashes differ",
			a:    ChunkStore{"h1": []byte("a"), "h2": []byte("b")},
			b:    ChunkStore{"h1": []byte("a"), "h2": []byte("b")},
			want: nil,
		},
		{
			name: "one of two shared hashes differs",
			a:    ChunkStore{"h1": []byte("a"), "h2": []byte("b")},
			b:    ChunkStore{"h1": []byte("a"), "h2": []byte("B")},
			want: []string{"h2"},
		},
		{
			name: "multiple corrupted hashes come back sorted",
			a:    ChunkStore{"z": []byte("1"), "a": []byte("2"), "m": []byte("3")},
			b:    ChunkStore{"z": []byte("X"), "a": []byte("Y"), "m": []byte("3")},
			want: []string{"a", "z"},
		},
		{
			name: "a hash missing from b is not reported as corrupted",
			a:    ChunkStore{"h1": []byte("a"), "h2": []byte("only-in-a")},
			b:    ChunkStore{"h1": []byte("a")},
			want: nil,
		},
		{name: "both empty", a: ChunkStore{}, b: ChunkStore{}, want: nil},
		{name: "both nil", a: nil, b: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Corrupted(tc.a, tc.b)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Corrupted() = %v, want %v", got, tc.want)
			}
		})
	}
}

// identicalCountOnly is the shortcut a first draft reaches for: compare the
// two stores' sizes and key sets, and declare them identical if the
// metadata lines up. It never looks at a single byte of chunk data, so the
// exact failure mode a backup integrity check exists to catch -- silent
// bit-rot in one chunk while every hash is still present on both sides --
// passes right through it undetected. It is never exported and never
// reachable from the package API; it exists so the tests can pin what it
// misses.
func identicalCountOnly(a, b ChunkStore) bool {
	if len(a) != len(b) {
		return false
	}
	for hash := range a {
		if _, ok := b[hash]; !ok {
			return false
		}
	}
	return true
}

// TestCountOnlyMissesCorruptionThatIdenticalCatches is the heart of the
// module: two stores share every hash, so identicalCountOnly says they
// match, but one chunk's bytes silently differ -- exactly the corruption a
// restic-style integrity check is run to catch -- and Identical, backed by
// maps.EqualFunc(a, b, bytes.Equal), reports the mismatch.
func TestCountOnlyMissesCorruptionThatIdenticalCatches(t *testing.T) {
	t.Parallel()

	a := ChunkStore{
		"hash-a": []byte("snapshot chunk one"),
		"hash-b": []byte("snapshot chunk two"),
	}
	b := ChunkStore{
		"hash-a": []byte("snapshot chunk one"),
		"hash-b": []byte("snapshot chunk TWO"), // same length, different bytes
	}

	if !identicalCountOnly(a, b) {
		t.Fatal("identicalCountOnly = false, want true (same key set, same lengths)")
	}
	if Identical(a, b) {
		t.Fatal("Identical = true, want false: hash-b's bytes differ between stores")
	}
}

// TestConcurrentReadsDoNotRace exercises the concurrency contract in the
// ChunkStore doc comment: Identical holds no state of its own, so many
// goroutines may compare the same stores at once as long as nothing writes
// to either concurrently.
func TestConcurrentReadsDoNotRace(t *testing.T) {
	t.Parallel()

	a := ChunkStore{"hash-a": []byte("payload"), "hash-b": []byte("more payload")}
	b := ChunkStore{"hash-a": []byte("payload"), "hash-b": []byte("more payload")}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !Identical(a, b) {
				t.Error("Identical = false under concurrent reads, want true")
			}
		}()
	}
	wg.Wait()
}

// ExampleIdentical is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleIdentical() {
	local := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshot"),
	}
	remote := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshot"),
	}
	fmt.Println(Identical(local, remote))

	corrupted := ChunkStore{
		"a1b2": []byte("first chunk of the snapshot"),
		"c3d4": []byte("second chunk of the snapshoT"), // one byte differs
	}
	fmt.Println(Identical(local, corrupted))
	fmt.Println(Corrupted(local, corrupted))

	// Output:
	// true
	// false
	// [c3d4]
}
```

## Review

`Identical` is correct when it agrees with a byte-for-byte comparison of
every shared hash and treats any hash present on only one side as a mismatch
-- the table pins both. The mistake this module isolates is not a wrong
implementation of that check but a different, weaker one substituted for
it: `identicalCountOnly` compares metadata -- sizes, key sets -- and never
opens a single chunk, so it reports two stores as identical when they share
every hash and disagree about what's behind one of them, exactly the
scenario `TestCountOnlyMissesCorruptionThatIdenticalCatches` pins.
`maps.EqualFunc(a, b, bytes.Equal)` is not a workaround for that gap; it is
the only signature that compiles at all once `V` is `[]byte`, because
`maps.Equal`'s `comparable` constraint rules out a slice value type before
the program can even build. `Corrupted` narrows a `false` from `Identical`
down to the specific hashes that disagree, reporting only real content
mismatches and never a chunk that is simply missing from one side. Neither
function synchronizes access to its inputs; a `ChunkStore` shared across
goroutines still needs its own lock around a concurrent writer. Run
`go test -count=1 -race ./...`.

## Resources

- [`maps` package: Equal is a compile-time constraint, not a runtime panic](00-concepts.md) — the concept this module builds directly on.
- [`maps.Equal`](https://pkg.go.dev/maps#Equal) and [`maps.EqualFunc`](https://pkg.go.dev/maps#EqualFunc) — the two comparison functions, and the constraint that separates them.
- [`bytes.Equal`](https://pkg.go.dev/bytes#Equal) — the equality function `Identical` supplies to `maps.EqualFunc`.
- [restic: Design](https://restic.readthedocs.io/en/stable/100_references.html#terminology) — the content-addressable chunk store this module models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-tenant-rate-limiter-pointwise-lock.md](13-tenant-rate-limiter-pointwise-lock.md) | Next: [15-wal-log-compaction-tombstones.md](15-wal-log-compaction-tombstones.md)
