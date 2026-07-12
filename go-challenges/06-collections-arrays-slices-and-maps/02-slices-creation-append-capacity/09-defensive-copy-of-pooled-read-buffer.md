# Exercise 9: Defensive Copy Before Retaining Bytes From a Reused Read Buffer

Readers reuse buffers. `bufio.Reader`, `net.Conn.Read`, and any `sync.Pool`-backed
`[]byte` hand you a window into memory that is valid only until the next read, then
overwrite it. Storing that window — in a slice, a cache, a channel to another
goroutine — is a use-after-overwrite bug: the data changes on the next read. The
fix is to copy the bytes into storage you own before retaining them. This is the
same corruption family as Exercises 3 and 4, in the shape where it most often bites
in network code.

This module is self-contained: its own module, demo, and tests. It is the last in
this lesson.

## What you'll build

```text
readretain/                independent module: example.com/readretain
  go.mod                   go 1.26
  readretain.go            Reader (reuses one buffer); RetainAliased (buggy), RetainCopied
  cmd/
    demo/
      main.go              read two messages, retain both ways, show corruption
  readretain_test.go       corruption vs isolation, concurrent copies under -race, Example
```

Files: `readretain.go`, `cmd/demo/main.go`, `readretain_test.go`.
Implement: a `Reader` whose `Next` reads into one reused buffer and returns a view; `RetainAliased` (stores the view directly) and `RetainCopied` (stores `bytes.Clone`).
Test: overwrite the buffer with the next read and assert the aliased store is corrupted while the copied store is stable; a concurrent test proving cloned retention is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/09-defensive-copy-of-pooled-read-buffer/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/09-defensive-copy-of-pooled-read-buffer
go mod edit -go=1.26
```

### Why the read view is valid only until the next read

The `Reader` here models exactly what a pooled or buffered reader does: it keeps
one backing array and, on each `Next`, refills it with the new message
(`buf = append(buf[:0], payload...)`) and returns a view into it. Reusing the
array is the whole point — it is why buffered readers do not allocate per read.
The unavoidable consequence is that the returned slice is a *loan*, not a gift: it
is only valid until the next `Next`, which overwrites the same array.

`RetainAliased` stores that loaned slice directly. Every read after the store
overwrites the bytes the stored slice points at, so a collection built with it ends
up full of slices that all alias the same array and all read whatever the *last*
message was. The bug is invisible right after the store — the bytes are still
correct — and only appears on the next read, which is what makes it so common and
so hard to spot in review.

`RetainCopied` calls `bytes.Clone(msg)`, which allocates a fresh backing array and
copies the bytes, so the stored value is independent of the reader's buffer and
survives every subsequent read. `bytes.Clone` is the idiomatic form; the
equivalents are `append([]byte(nil), msg...)` and `dst := make([]byte, len(msg));
copy(dst, msg)`. All three produce storage you own. The rule is unconditional: any
time you retain bytes past the call that gave them to you, and those bytes came
from a reader or pool that reuses its buffer, copy first.

Create `readretain.go`:

```go
package readretain

import "bytes"

// Reader models a pooled/buffered reader: it reuses a single backing array
// across reads. The slice returned by Next is valid only until the next Next,
// which overwrites the same array.
type Reader struct {
	buf []byte
}

// Next reads the next message into the reused buffer and returns a view into it.
// The returned slice must be copied before it is retained past the next call.
func (r *Reader) Next(payload []byte) []byte {
	r.buf = append(r.buf[:0], payload...)
	return r.buf
}

// RetainAliased is the BUGGY retention: it stores the reader's loaned slice
// directly, so the next read corrupts every previously stored entry.
func RetainAliased(dst [][]byte, msg []byte) [][]byte {
	return append(dst, msg)
}

// RetainCopied is the correct retention: it stores an independent copy so the
// stored bytes survive the reader's buffer reuse.
func RetainCopied(dst [][]byte, msg []byte) [][]byte {
	return append(dst, bytes.Clone(msg))
}
```

### The runnable demo

The demo reads two equal-length messages, retaining each both ways, then prints the
stored first entry from each collection: the aliased one has been overwritten by
the second read; the copied one is intact.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/readretain"
)

func main() {
	r := &readretain.Reader{}

	var aliased, copied [][]byte
	m1 := r.Next([]byte("alpha"))
	aliased = readretain.RetainAliased(aliased, m1)
	copied = readretain.RetainCopied(copied, m1)

	// The next read overwrites the reader's shared buffer.
	r.Next([]byte("omega"))

	fmt.Printf("aliased[0]=%q\n", aliased[0])
	fmt.Printf("copied[0]=%q\n", copied[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
aliased[0]="omega"
copied[0]="alpha"
```

### Tests

`TestAliasedRetentionCorrupted` reads a second message and asserts the aliased
first entry was overwritten. `TestCopiedRetentionSurvives` runs the same sequence
and asserts the copied entry is stable. `TestConcurrentCopiesAreIsolated` gives
each goroutine its own reader, clones every message before storing under a mutex,
and asserts all entries are present and correct — a `-race`-clean proof that copied
retention is safe under concurrent reuse.

Create `readretain_test.go`:

```go
package readretain

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestAliasedRetentionCorrupted(t *testing.T) {
	t.Parallel()
	r := &Reader{}
	var store [][]byte

	m1 := r.Next([]byte("alpha"))
	store = RetainAliased(store, m1)

	r.Next([]byte("omega")) // overwrites the shared buffer

	if string(store[0]) == "alpha" {
		t.Fatal("expected the aliased entry to be corrupted by the next read, but it was intact")
	}
	if string(store[0]) != "omega" {
		t.Fatalf("aliased entry = %q, want the corrupted %q", store[0], "omega")
	}
}

func TestCopiedRetentionSurvives(t *testing.T) {
	t.Parallel()
	r := &Reader{}
	var store [][]byte

	m1 := r.Next([]byte("alpha"))
	store = RetainCopied(store, m1)

	r.Next([]byte("omega")) // same overwrite

	if string(store[0]) != "alpha" {
		t.Fatalf("copied entry corrupted: got %q, want %q", store[0], "alpha")
	}
}

func TestConcurrentCopiesAreIsolated(t *testing.T) {
	t.Parallel()
	const g = 50
	var mu sync.Mutex
	var store [][]byte
	var wg sync.WaitGroup

	for i := range g {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Reader{} // each goroutine owns its buffer
			msg := r.Next([]byte(fmt.Sprintf("msg%02d", i)))
			clone := bytes.Clone(msg)
			mu.Lock()
			store = append(store, clone)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(store) != g {
		t.Fatalf("stored %d messages, want %d", len(store), g)
	}
	seen := make(map[string]bool, g)
	for _, m := range store {
		seen[string(m)] = true
	}
	for i := range g {
		want := fmt.Sprintf("msg%02d", i)
		if !seen[want] {
			t.Fatalf("missing message %q", want)
		}
	}
}

func ExampleRetainCopied() {
	r := &Reader{}
	var store [][]byte
	store = RetainCopied(store, r.Next([]byte("v1")))
	r.Next([]byte("v2"))
	fmt.Printf("%s\n", store[0])
	// Output: v1
}
```

## Review

Copied retention is correct when a stored entry is independent of any later read
from the reader. `TestAliasedRetentionCorrupted` proves the bug is real — it
*fails* if the entry stays intact, which would mean the reader was not actually
reusing its buffer and the lesson would be teaching a non-bug — and
`TestCopiedRetentionSurvives` proves the fix. The concurrent test matters because
this is where the aliasing bug turns from a correctness problem into a data race:
several goroutines retaining slices into a shared reused buffer is undefined
behavior the race detector will flag; cloning first makes each stored value
independent and the whole thing race-free. The rule to carry forward: bytes from a
reader or pool that reuses its buffer must be copied before you keep them —
`bytes.Clone`, `append([]byte(nil), b...)`, or `make`+`copy`. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone)
- [`bufio.Reader`](https://pkg.go.dev/bufio#Reader) — its returned slices are valid only until the next read.
- [Go Wiki: SliceTricks (copy idioms)](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-trim-response-buffer-grow-clip.md](08-trim-response-buffer-grow-clip.md) | Next: [10-make-len-vs-cap-mapper-bug.md](10-make-len-vs-cap-mapper-bug.md)
