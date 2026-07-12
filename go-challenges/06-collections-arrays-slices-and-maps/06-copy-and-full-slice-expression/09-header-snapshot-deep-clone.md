# Exercise 9: Deep-Clone a `map[string][]string` Header Snapshot

Snapshotting a request-header-like `map[string][]string` needs a *deep* clone.
`maps.Clone` copies the map but shares the value slices, so a caller appending to
or overwriting a value slice in the "snapshot" mutates the original. This exercise
builds a `Clone` that mirrors `http.Header.Clone` — copying the map and re-cloning
each value slice — and demonstrates the shallow-clone trap explicitly.

Self-contained module: own `go mod init`, own demo, own tests. This is the last
exercise in the lesson.

## What you'll build

```text
header/                    independent module: example.com/header
  go.mod                   go 1.26
  header.go                type Header; Add, Get, Clone (deep), shallowClone (bug)
  cmd/
    demo/
      main.go              snapshot, mutate the snapshot, original unchanged
  header_test.go           deep-clone isolation, shallow-clone value-sharing leak
```

Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
Implement: `Header.Clone` copying the map and `slices.Clone`-ing each value; a `shallowClone` using only `maps.Clone`.
Test: clone, append to a value slice and add a key in the clone, assert the original is byte-for-byte unchanged; a negative sub-test uses `maps.Clone` and shows a shared-value-slice mutation leaking into the original.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/09-header-snapshot-deep-clone/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/06-copy-and-full-slice-expression/09-header-snapshot-deep-clone
```

### Why `maps.Clone` is not enough

`maps.Clone(m)` allocates a new map and copies every key and value by assignment.
For a `map[string]int` that is a full, independent copy. For a
`map[string][]string` it is not, because the values are slices, and assigning a
slice copies only its three-word header — pointer, length, capacity — not the
backing array it points at. So the clone's `"Authorization"` value slice and the
original's are two headers over the *same* array. Writing `clone["Authorization"][0] = "x"`
changes the original's element too, and appending into a shared value slice that
has spare capacity writes into the original's backing array. The map spine is
isolated (adding a brand-new key to the clone does not appear in the original), but
the values leak — a partial isolation that is worse than none because it looks safe.

The fix is exactly what `http.Header.Clone` does: copy the map spine, then clone
each value slice so every entry gets its own backing array. `slices.Clone(v)` per
value makes the snapshot fully independent — a caller can `Add`, append, overwrite,
or delete entries in the snapshot without the original observing anything. This is
the deep-clone-one-level-down discipline: `maps.Clone` and `slices.Clone` are each
shallow with respect to what their elements point at, so a container of containers
needs a clone at every level that shares mutable state.

Create `header.go`:

```go
package header

import "slices"

// Header is a multi-valued header/label set, like http.Header.
type Header map[string][]string

// Add appends value under key, creating the slice if needed.
func (h Header) Add(key, value string) {
	h[key] = append(h[key], value)
}

// Get returns the first value for key, or "" if absent.
func (h Header) Get(key string) string {
	v := h[key]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// Clone returns a deep, independent snapshot: the map spine and every value
// slice are copied, mirroring http.Header.Clone. A nil header clones to nil.
func (h Header) Clone() Header {
	if h == nil {
		return nil
	}
	c := make(Header, len(h))
	for k, v := range h {
		c[k] = slices.Clone(v)
	}
	return c
}
```

The buggy shallow clone lives beside it so the test can contrast them. It uses
`maps.Clone`, which shares the value slices.

Create `shallow.go`:

```go
package header

import "maps"

// shallowClone copies the map spine but shares every value slice with the
// source. Used only in tests to demonstrate the leak.
func shallowClone(h Header) Header {
	return maps.Clone(h)
}
```

### The runnable demo

The demo builds a header, snapshots it, then mutates the snapshot every way a
caller might — appends a value, overwrites a value, adds a new key — and prints the
original to show it is untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/header"
)

func main() {
	h := header.Header{}
	h.Add("X-Trace", "t1")
	h.Add("X-Trace", "t2")
	h.Add("Authorization", "secret")

	snap := h.Clone()
	snap.Add("X-Trace", "t3")      // append to a value slice
	snap["Authorization"][0] = "x" // overwrite a value
	snap.Add("X-Debug", "on")      // add a new key

	fmt.Println("original X-Trace:      ", h["X-Trace"])
	fmt.Println("original Authorization:", h["Authorization"])
	fmt.Println("original has X-Debug:  ", h.Get("X-Debug") != "")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
original X-Trace:       [t1 t2]
original Authorization: [secret]
original has X-Debug:   false
```

### Tests

`TestDeepCloneIsolatesValues` mutates the snapshot in every way and asserts the
original is byte-for-byte unchanged. `TestShallowCloneLeaksValues` uses
`shallowClone` and asserts a value overwrite in the clone leaks into the original,
while a new key does not — pinning the "spine isolated, values shared" nature of
`maps.Clone`.

Create `header_test.go`:

```go
package header

import (
	"maps"
	"slices"
	"testing"
)

func seeded() Header {
	h := Header{}
	h.Add("X-Trace", "t1")
	h.Add("X-Trace", "t2")
	h.Add("Authorization", "secret")
	return h
}

func TestDeepCloneIsolatesValues(t *testing.T) {
	t.Parallel()

	h := seeded()
	snap := h.Clone()

	snap.Add("X-Trace", "t3")
	snap["Authorization"][0] = "tampered"
	snap.Add("X-Debug", "on")

	if !slices.Equal(h["X-Trace"], []string{"t1", "t2"}) {
		t.Fatalf("original X-Trace leaked: %v", h["X-Trace"])
	}
	if h.Get("Authorization") != "secret" {
		t.Fatalf("original Authorization overwritten: %q", h.Get("Authorization"))
	}
	if _, ok := h["X-Debug"]; ok {
		t.Fatal("original gained X-Debug from clone")
	}
}

func TestShallowCloneLeaksValues(t *testing.T) {
	t.Parallel()

	h := seeded()
	snap := shallowClone(h)

	// The map spine is isolated: a new key does not reach the original.
	snap.Add("X-Debug", "on")
	if _, ok := h["X-Debug"]; ok {
		t.Fatal("maps.Clone should isolate the map spine, but new key leaked")
	}

	// The value slices are shared: overwriting one leaks into the original.
	snap["Authorization"][0] = "tampered"
	if h.Get("Authorization") != "tampered" {
		t.Fatalf("expected shallow clone to leak value mutation, original = %q", h.Get("Authorization"))
	}
}

func TestCloneNilIsNil(t *testing.T) {
	t.Parallel()

	var h Header
	if h.Clone() != nil {
		t.Fatal("Clone of nil header should be nil")
	}
}

func TestCloneMatchesMapsEqual(t *testing.T) {
	t.Parallel()

	h := seeded()
	snap := h.Clone()
	if !maps.EqualFunc(h, snap, slices.Equal) {
		t.Fatal("deep clone should be value-equal to the source")
	}
}
```

## Review

The snapshot is correct when a caller cannot reach the original through it:
`TestDeepCloneIsolatesValues` appends, overwrites, and adds a key in the clone and
the original stays byte-for-byte identical, while `TestCloneMatchesMapsEqual`
confirms the clone starts out equal. The negative `TestShallowCloneLeaksValues`
makes the trap concrete — `maps.Clone` isolates the map spine (a new key does not
leak) but shares the value slices (an overwrite does leak), which is exactly why
`http.Header.Clone` re-clones each value rather than trusting a map copy. The
general rule: `maps.Clone` and `slices.Clone` are shallow one level down; a
container whose elements are themselves mutable containers must be cloned at every
level.

## Resources

- [`maps.Clone`](https://pkg.go.dev/maps#Clone)
- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`http.Header.Clone` (source/behavior)](https://pkg.go.dev/net/http#Header.Clone)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-length-prefixed-frame-reader.md](08-length-prefixed-frame-reader.md) | Next: [10-frame-writer-copy-sized-by-length.md](10-frame-writer-copy-sized-by-length.md)
