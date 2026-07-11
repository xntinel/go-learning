# Exercise 2: A Content-Addressed Blob Store Keyed by [32]byte SHA-256

Content-addressed storage keys a blob by the hash of its bytes, so identical
payloads collapse to one entry and writes are idempotent by construction. Because
`sha256.Sum256` returns a `[32]byte` value and a fixed-size array is comparable,
you use the digest *directly* as a map key with no hex-string allocation. This
exercise builds that store and proves the value-semantics of the array key make
deduplication trivial.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
blobstore/                   independent module: example.com/blobstore
  go.mod
  store.go                   type Store; Put(blob) (key [32]byte, existed bool); Get(key) ([]byte, bool); Len
  cmd/
    demo/
      main.go                runnable demo: put twice, dedup, get by key
  store_test.go              dedup, distinct keys, round-trip, zero-key miss, key stability
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a `Store` over `map[[32]byte][]byte`, with `Put(blob []byte) (key [32]byte, existed bool)` and `Get(key [32]byte) ([]byte, bool)`.
- Test: putting the same blob twice dedups; two blobs give two keys; round-trip through the map; a zero key misses; keys are stable across calls.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/blobstore/cmd/demo
cd ~/go-exercises/blobstore
go mod init example.com/blobstore
```

### Why the [32]byte key needs no hex string

`sha256.Sum256(blob)` returns a `[32]byte` *value*, not a slice. A fixed-size
array of comparable elements is comparable, so `map[[32]byte][]byte` is a legal
map type and the digest is a legal key with no further ceremony. Compare this to
the common but wasteful pattern of `hex.EncodeToString(sum[:])` to get a
`map[string][]byte` key: that allocates a 64-byte string on every write and read,
purely to make the key printable. The array key skips all of it — the 32 bytes ARE
the key, hashed and compared in place.

The value semantics are what make deduplication correct. Two calls to
`sha256.Sum256` on equal bytes return two `[32]byte` values that are `==`, so they
land in the same bucket. `Put` computes the key, checks `_, ok := s.blobs[key]`,
and if the key is already present returns `existed = true` without overwriting.
Identical payloads therefore collapse to a single entry, and re-putting a blob is
idempotent: same key, `existed = true`, one stored copy.

One correctness detail: `Put` stores a *copy* of the incoming slice
(`append([]byte(nil), blob...)`), not the caller's slice. Otherwise a caller who
mutates their buffer after `Put` would corrupt the stored blob — the store must own
its data. This is the slice-aliasing hazard the concepts file warns about, in
miniature: the `[32]byte` key is safely copied by value automatically, but the
`[]byte` value must be copied by hand.

Create `store.go`:

```go
package blobstore

import (
	"crypto/sha256"
)

// Store is a content-addressed blob store. Identical payloads deduplicate to one
// entry because the SHA-256 digest is the map key.
type Store struct {
	blobs map[[sha256.Size]byte][]byte
}

// New returns an empty Store.
func New() *Store {
	return &Store{blobs: make(map[[sha256.Size]byte][]byte)}
}

// Put stores blob under its SHA-256 digest and returns the key. If a blob with
// the same digest is already present, it is not overwritten and existed is true.
func (s *Store) Put(blob []byte) (key [sha256.Size]byte, existed bool) {
	key = sha256.Sum256(blob)
	if _, ok := s.blobs[key]; ok {
		return key, true
	}
	// Own the data: copy the caller's slice so later mutations cannot corrupt us.
	stored := append([]byte(nil), blob...)
	s.blobs[key] = stored
	return key, false
}

// Get returns the blob stored under key, if present.
func (s *Store) Get(key [sha256.Size]byte) ([]byte, bool) {
	blob, ok := s.blobs[key]
	return blob, ok
}

// Len reports the number of distinct blobs stored.
func (s *Store) Len() int {
	return len(s.blobs)
}
```

Note `sha256.Size` is the constant `32`, so `[sha256.Size]byte` is `[32]byte` — the
same type `sha256.Sum256` returns. Using the constant keeps the key type and the
digest type provably identical.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/hex"
	"fmt"

	"example.com/blobstore"
)

func main() {
	s := blobstore.New()

	k1, existed1 := s.Put([]byte("config-v1"))
	fmt.Printf("put #1: existed=%v key=%s\n", existed1, hex.EncodeToString(k1[:8]))

	k2, existed2 := s.Put([]byte("config-v1")) // same payload
	fmt.Printf("put #2: existed=%v key=%s\n", existed2, hex.EncodeToString(k2[:8]))
	fmt.Printf("same key: %v\n", k1 == k2)

	k3, _ := s.Put([]byte("config-v2")) // different payload
	fmt.Printf("distinct key: %v\n", k1 != k3)

	blob, ok := s.Get(k1)
	fmt.Printf("get k1: %q ok=%v\n", string(blob), ok)
	fmt.Printf("entries: %d\n", s.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
put #1: existed=false key=e3155b20e1346328
put #2: existed=true key=e3155b20e1346328
same key: true
distinct key: true
get k1: "config-v1" ok=true
entries: 2
```

### Tests

`TestPutDeduplicates` puts the same blob twice and asserts the second returns
`existed = true` with `Len` unchanged at one. `TestPutDistinctBlobs` puts two
different payloads and asserts two distinct keys, both retrievable.
`TestZeroKeyMisses` asserts a `[32]byte{}` never-stored key returns `ok = false`.
`TestKeyStability` is the fuzz-ish table: over a range of payloads, the key from
`Put` equals a fresh `sha256.Sum256` of the same bytes, pinning the array key as a
pure function of content.

Create `store_test.go`:

```go
package blobstore

import (
	"crypto/sha256"
	"testing"
)

func TestPutDeduplicates(t *testing.T) {
	t.Parallel()

	s := New()
	k1, existed1 := s.Put([]byte("payload"))
	if existed1 {
		t.Fatal("first Put should report existed=false")
	}
	k2, existed2 := s.Put([]byte("payload"))
	if !existed2 {
		t.Fatal("second Put of identical payload should report existed=true")
	}
	if k1 != k2 {
		t.Fatalf("identical payloads must produce equal keys: %x vs %x", k1, k2)
	}
	if s.Len() != 1 {
		t.Fatalf("store should hold one entry after dedup, got %d", s.Len())
	}
}

func TestPutDistinctBlobs(t *testing.T) {
	t.Parallel()

	s := New()
	ka, _ := s.Put([]byte("alpha"))
	kb, _ := s.Put([]byte("beta"))
	if ka == kb {
		t.Fatal("distinct payloads must produce distinct keys")
	}
	if a, ok := s.Get(ka); !ok || string(a) != "alpha" {
		t.Fatalf("Get(ka) = %q,%v; want alpha,true", a, ok)
	}
	if b, ok := s.Get(kb); !ok || string(b) != "beta" {
		t.Fatalf("Get(kb) = %q,%v; want beta,true", b, ok)
	}
	if s.Len() != 2 {
		t.Fatalf("store should hold two entries, got %d", s.Len())
	}
}

func TestZeroKeyMisses(t *testing.T) {
	t.Parallel()

	s := New()
	s.Put([]byte("something"))
	var zero [sha256.Size]byte
	if _, ok := s.Get(zero); ok {
		t.Fatal("a zero key must miss")
	}
}

func TestKeyStability(t *testing.T) {
	t.Parallel()

	payloads := [][]byte{
		nil,
		{},
		[]byte("a"),
		[]byte("the quick brown fox"),
		make([]byte, 1000),
	}
	s := New()
	for _, p := range payloads {
		key, _ := s.Put(p)
		want := sha256.Sum256(p)
		if key != want {
			t.Fatalf("Put key %x != Sum256 %x for payload len %d", key, want, len(p))
		}
	}
}
```

The `nil` and `{}` payloads both hash to the SHA-256 of the empty input, so they
share a key — a real property of content addressing that the store handles without
special-casing.

## Review

The store is correct when the key is a pure function of the blob's bytes and the
map deduplicates on that key. The `[32]byte` array key is the crux: it is
comparable, so it works as a map key with no hex encoding, and its value semantics
mean two independently-hashed equal payloads produce `==` keys that collide to one
bucket. The one hand-written safeguard is copying the incoming slice in `Put` — the
array key copies itself, but the `[]byte` value would otherwise alias the caller's
buffer and corrupt on later mutation. Common mistakes to avoid: keying on
`hex.EncodeToString(sum[:])` (needless per-op allocation) and storing `blob`
directly instead of a copy. Run `go test -race` to confirm the round-trips and the
zero-key miss, then note `TestKeyStability` proving the key never drifts from
`sha256.Sum256`.

## Resources

- [crypto/sha256](https://pkg.go.dev/crypto/sha256) — `Sum256` returns `[32]byte`; `Size` is the constant 32.
- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — array comparability and map-key requirements.
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — key types must be comparable.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-block-checksum-fixed-arrays.md](01-block-checksum-fixed-arrays.md) | Next: [03-keymaterial-defensive-copy.md](03-keymaterial-defensive-copy.md)
