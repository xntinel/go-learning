# Exercise 4: Fixing an Append-Aliasing Bug in a Cache/Config Layer

This is the single most common slice bug in production Go: a cache stores a slice
that points into a buffer the caller still owns and reuses. The cached value looks
correct at store time and then silently changes when the caller overwrites the
buffer — a corruption that surfaces far from its cause, often as a "why is this
cached config wrong" mystery. You will reproduce the corruption with the buggy
aliasing version, then fix it with `slices.Clone` so the cached value owns
isolated storage.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
cfgcache/                  independent module: example.com/cfgcache
  go.mod                   go 1.26
  cfgcache.go              Cache; StoreAliased (buggy), Store (fixed via Clone), Get
  cmd/
    demo/
      main.go              store both ways, reuse the buffer, print corruption vs stability
  cfgcache_test.go         reproduce corruption; assert the fixed value survives, Example
```

Files: `cfgcache.go`, `cmd/demo/main.go`, `cfgcache_test.go`.
Implement: `StoreAliased` (stores the caller's slice directly, sharing storage) and `Store` (stores `slices.Clone`, owning storage), plus `Get`.
Test: reproduce corruption of the aliased value after the caller reuses its buffer; assert the cloned value is unchanged.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The corruption, precisely

Picture a parser with a reusable scratch buffer of capacity 16. It reads the first
record — the bytes `alice` — into `buf`, so `buf` is `len 5, cap 16`. It caches
that value. Then it reads the next record by reusing the same buffer:
`buf = append(buf[:0], "bob"...)`. That reset-and-append writes `b`, `o`, `b` into
indices 0, 1, 2 of the *same backing array* — `buf[:0]` kept the array, and the
three new bytes fit in the spare capacity.

If the cache stored the slice by aliasing (`c.m[key] = buf`), its stored header
still points at that backing array with length 5. Reading it back now yields
`bobce`: the first three bytes were overwritten by the parser's reuse, the last
two (`c`, `e`) survive. The cached value changed even though nobody touched the
cache. This is not a rare corner case — it is what happens any time you retain a
slice into storage the producer recycles (a scratch buffer, a pooled `[]byte`, a
`bufio` window).

The fix is ownership: the cache must copy the bytes into storage it alone controls
before retaining them. `slices.Clone(buf)` allocates a fresh backing array and
copies the elements, so the parser's later reuse of `buf` cannot reach the cached
value. Note that a three-index slice (`buf[:5:5]`) does *not* fix this particular
bug — three-index caps prevent a *consumer's append* from spilling past the view,
but here the corruption comes from overwriting the very bytes the view covers, and
only an independent copy defends against that. Reach for `Clone` whenever the
producer will reuse the storage; reach for three-index caps (Exercise 5) when a
consumer will append into a view of shared storage.

Create `cfgcache.go`:

```go
package cfgcache

import "slices"

// Cache maps a key to a byte value (a rendered config blob, a serialized record).
type Cache struct {
	m map[string][]byte
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{m: make(map[string][]byte)}
}

// StoreAliased is the BUGGY version, kept to document the trap: it retains the
// caller's slice directly. If the caller later reuses or overwrites the backing
// array, the cached value changes underneath the cache.
func (c *Cache) StoreAliased(key string, value []byte) {
	c.m[key] = value
}

// Store is the CORRECT version: it clones value into storage the cache owns, so
// the cached bytes are immune to the caller's later reuse of its buffer.
func (c *Cache) Store(key string, value []byte) {
	c.m[key] = slices.Clone(value)
}

// Get returns the stored value and whether the key was present.
func (c *Cache) Get(key string) ([]byte, bool) {
	v, ok := c.m[key]
	return v, ok
}
```

### The runnable demo

The demo stores the same `alice` bytes twice — once aliased, once cloned — then
reuses the buffer to hold `bob` and reads both back. The aliased entry is
corrupted; the cloned entry is intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/cfgcache"
)

func main() {
	buf := make([]byte, 0, 16)
	buf = append(buf, []byte("alice")...)

	c := cfgcache.New()
	c.StoreAliased("aliased", buf)
	c.Store("cloned", buf)

	// The producer reuses its scratch buffer for the next record.
	buf = append(buf[:0], []byte("bob")...)
	_ = buf

	aliased, _ := c.Get("aliased")
	cloned, _ := c.Get("cloned")
	fmt.Printf("aliased=%q\n", aliased)
	fmt.Printf("cloned=%q\n", cloned)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
aliased="bobce"
cloned="alice"
```

### Tests

`TestAliasedValueCorrupted` documents the bug: after the producer reuses the
buffer, the aliased entry is no longer `alice`. `TestClonedValueSurvivesReuse` is
the fix's proof: the cloned entry still reads `alice`. `TestCloneIndependentAfterMutation`
mutates individual bytes of the source directly and confirms the clone is
untouched, covering in-place mutation as well as reuse.

Create `cfgcache_test.go`:

```go
package cfgcache

import (
	"fmt"
	"testing"
)

func TestAliasedValueCorrupted(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 16)
	buf = append(buf, []byte("alice")...)

	c := New()
	c.StoreAliased("k", buf)

	buf = append(buf[:0], []byte("bob")...) // producer reuses the buffer
	_ = buf

	got, ok := c.Get("k")
	if !ok {
		t.Fatal("key missing")
	}
	if string(got) == "alice" {
		t.Fatal("expected the aliased value to be corrupted by buffer reuse, but it was intact")
	}
	if string(got) != "bobce" {
		t.Fatalf("aliased value = %q, want the corrupted %q", got, "bobce")
	}
}

func TestClonedValueSurvivesReuse(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 16)
	buf = append(buf, []byte("alice")...)

	c := New()
	c.Store("k", buf)

	buf = append(buf[:0], []byte("bob")...) // same reuse as the buggy case
	_ = buf

	got, ok := c.Get("k")
	if !ok {
		t.Fatal("key missing")
	}
	if string(got) != "alice" {
		t.Fatalf("cloned value corrupted: got %q, want %q", got, "alice")
	}
}

func TestCloneIndependentAfterMutation(t *testing.T) {
	t.Parallel()
	src := []byte("secret")
	c := New()
	c.Store("k", src)

	for i := range src {
		src[i] = 'x' // mutate every byte of the source in place
	}

	got, _ := c.Get("k")
	if string(got) != "secret" {
		t.Fatalf("clone changed with source: got %q, want %q", got, "secret")
	}
}

func ExampleCache_Store() {
	buf := make([]byte, 0, 8)
	buf = append(buf, []byte("v1")...)
	c := New()
	c.Store("k", buf)
	buf = append(buf[:0], []byte("v2")...)
	_ = buf
	got, _ := c.Get("k")
	fmt.Printf("%s\n", got)
	// Output: v1
}
```

## Review

The fix is correct when the cached value is independent of any later change to the
caller's buffer — reuse (`append(buf[:0], ...)`) or in-place mutation. The two
tests separate the concern: `TestAliasedValueCorrupted` proves the bug is real (it
*fails* if the value somehow stayed intact, which would mean the reuse did not
actually share storage and the lesson would be teaching a non-bug), and
`TestClonedValueSurvivesReuse` proves `Store` defends against it. The trap to
internalize: aliasing looks correct at store time and every read *right after*
store returns the right bytes — the corruption only appears once the producer
reuses its storage, which is why this bug survives casual testing. When you retain
a caller's slice, clone it. Run `-race` to confirm no shared mutation slips
through.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices)
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-event-batcher-reuse-backing-array.md](03-event-batcher-reuse-backing-array.md) | Next: [05-three-index-slice-protocol-framing.md](05-three-index-slice-protocol-framing.md)
