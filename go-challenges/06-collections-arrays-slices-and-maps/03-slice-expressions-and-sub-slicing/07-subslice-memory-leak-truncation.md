# Exercise 7: Trim a Huge Batch to a Small Head Without Pinning the Backing Array

A handler decodes a large `[]*Record` batch, keeps only the first N as a summary,
and returns it. Returning `batch[:N]` pins the *entire* multi-thousand-element
backing array in memory for the lifetime of that tiny summary — a silent leak that
shows up in pprof as a slow heap climb, never as an error. This exercise proves the
retention at the slice-expression level and fixes it with a right-sized
`slices.Clone`, verified by a finalizer that fires only in the copy path.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
logtrim/                   independent module: example.com/logtrim
  go.mod                   go 1.24
  logtrim.go               type Record; Head (leaky); HeadCopy (fixed)
  cmd/
    demo/
      main.go              runnable demo comparing cap and retention
  logtrim_test.go          cap test, zero-alloc test, finalizer reclamation tests
```

- Files: `logtrim.go`, `cmd/demo/main.go`, `logtrim_test.go`.
- Implement: `Head` returning `batch[:n]` (leaky), `HeadCopy` returning
  `slices.Clone(batch[:n])` (right-sized).
- Test: `Head`'s result keeps `cap == len(batch)` while `HeadCopy`'s has `cap == n`;
  `Head` is zero-alloc; a finalizer on the last element fires only under the copy
  path, proving the big array is reclaimed only when the head is copied.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logtrim/cmd/demo
cd ~/go-exercises/logtrim
go mod init example.com/logtrim
go mod edit -go=1.24
```

## Why a small head can pin a huge array

`batch[:n]` is a slice expression that produces a header of length `n` but capacity
`cap(batch)` — it inherits the *whole* backing array. The `n` elements you want are
at the front, but every element past them is still physically in the array, and the
array stays alive as long as any slice references it. So returning `batch[:4]` from a
handler and keeping it around keeps all thousand `*Record` pointers reachable, which
keeps every `Record` they point at alive. The memory you "trimmed away" is not freed;
it is pinned behind a four-element view. There is no error, no panic — just a heap
that does not come down, which you eventually chase in a profile.

The tell is `cap`. `cap(batch[:4])` equals `cap(batch)` (say 2000), not 4. A slice
whose capacity dwarfs its length is a slice that is holding a big array hostage.

The fix is to copy the head into its own right-sized array: `slices.Clone(batch[:n])`
allocates exactly `n` elements (`cap == n`) and copies the head into them. Now the
returned summary references only its own small array; the big decoded batch has no
remaining references and becomes collectable. (The three-index full-slice expression
`batch[:n:n]` bounds the capacity to `n` so a later `append` reallocates, but it does
*not* copy — it still points into the big array, so it does not release the memory. To
*free* the big array you must copy; to merely prevent `append` from clobbering, the
three-index form suffices. This exercise needs the memory released, so it clones.)

The test proves reclamation directly: it sets a finalizer on the *last* element of
the batch. Under the leaky `Head`, the returned head pins the whole array, so the
last element stays reachable and the finalizer never runs while the head is alive.
Under `HeadCopy`, only the first element is copied out, the big array is unreferenced,
and after `runtime.GC` the finalizer fires.

Create `logtrim.go`:

```go
package logtrim

import "slices"

// Record is one decoded item in a batch. Blob makes each record non-trivial so
// the retention of a large batch is real memory.
type Record struct {
	ID   int
	Blob [256]byte
}

// Head returns the first n records as batch[:n]. This is LEAKY: the result's
// capacity is cap(batch), so it pins the entire backing array (and every Record
// it points at) for as long as the head is referenced.
func Head(batch []*Record, n int) []*Record {
	return batch[:n]
}

// HeadCopy returns the first n records as an independent, right-sized copy. Its
// capacity is n, so the large source batch has no remaining references through it
// and becomes collectable.
func HeadCopy(batch []*Record, n int) []*Record {
	return slices.Clone(batch[:n])
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logtrim"
)

func main() {
	batch := make([]*logtrim.Record, 2000)
	for i := range batch {
		batch[i] = &logtrim.Record{ID: i}
	}

	leaky := logtrim.Head(batch, 4)
	fixed := logtrim.HeadCopy(batch, 4)

	fmt.Printf("leaky head:  len=%d cap=%d (pins the whole array)\n", len(leaky), cap(leaky))
	fmt.Printf("copied head: len=%d cap=%d (releases the array)\n", len(fixed), cap(fixed))
	fmt.Printf("same first record: %v\n", leaky[0] == fixed[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky head:  len=4 cap=2000 (pins the whole array)
copied head: len=4 cap=4 (releases the array)
same first record: true
```

## Tests

The cap test pins the retention at the expression level: `Head` inherits the source
capacity, `HeadCopy` is right-sized. The zero-alloc test confirms `Head` really is a
free reslice. The two finalizer tests are the proof of memory behavior: the leaky
head keeps the last element alive (finalizer must *not* fire), the copied head lets
it go (finalizer *must* fire after GC).

Create `logtrim_test.go`:

```go
package logtrim

import (
	"runtime"
	"testing"
	"time"
)

func makeBatch(n int) []*Record {
	batch := make([]*Record, n)
	for i := range batch {
		batch[i] = &Record{ID: i}
	}
	return batch
}

func TestCapExposesRetention(t *testing.T) {
	t.Parallel()
	batch := makeBatch(2000)

	leaky := Head(batch, 4)
	if cap(leaky) != cap(batch) {
		t.Fatalf("Head cap = %d; want %d (should inherit whole array)", cap(leaky), cap(batch))
	}

	fixed := HeadCopy(batch, 4)
	if cap(fixed) != 4 {
		t.Fatalf("HeadCopy cap = %d; want 4 (right-sized)", cap(fixed))
	}
	for i := range fixed {
		if fixed[i] != batch[i] {
			t.Fatalf("HeadCopy element %d differs from source", i)
		}
	}
}

func TestHeadZeroAlloc(t *testing.T) {
	batch := makeBatch(2000)
	n := testing.AllocsPerRun(1000, func() {
		_ = Head(batch, 4)
	})
	if n != 0 {
		t.Fatalf("Head allocated %v times per run; want 0", n)
	}
}

// buildLeakyHead returns batch[:1] and arms a finalizer on the last element. The
// leaky head pins the whole array, so the last element stays reachable.
func buildLeakyHead(n int, freed chan<- struct{}) []*Record {
	batch := makeBatch(n)
	runtime.SetFinalizer(batch[n-1], func(*Record) { freed <- struct{}{} })
	return Head(batch, 1)
}

// buildCopiedHead returns a right-sized copy of batch[:1]; the big array is then
// unreferenced and its last element becomes collectable.
func buildCopiedHead(n int, freed chan<- struct{}) []*Record {
	batch := makeBatch(n)
	runtime.SetFinalizer(batch[n-1], func(*Record) { freed <- struct{}{} })
	return HeadCopy(batch, 1)
}

func TestLeakyHeadPinsBackingArray(t *testing.T) {
	freed := make(chan struct{}, 1)
	head := buildLeakyHead(2000, freed)

	runtime.GC()
	runtime.GC()

	select {
	case <-freed:
		t.Fatal("leaky head should pin the backing array, but its last element was collected")
	case <-time.After(100 * time.Millisecond):
		// expected: still pinned while head is alive
	}
	runtime.KeepAlive(head)
}

func TestCopiedHeadReleasesBackingArray(t *testing.T) {
	freed := make(chan struct{}, 1)
	head := buildCopiedHead(2000, freed)

	runtime.GC()
	runtime.GC()

	select {
	case <-freed:
		// expected: the big array (and its last element) was reclaimed
	case <-time.After(3 * time.Second):
		t.Fatal("copied head did not release the backing array")
	}
	runtime.KeepAlive(head)
}
```

## Review

The trim is correct when the returned summary holds the right elements and, in the
copy path, lets the source batch go. The cap assertions are the fast, deterministic
proof (`cap` inherited vs right-sized); the finalizer tests are the direct proof that
the leaky head keeps the last element alive while the copied head frees it. The wrong
mental model is "I only returned four elements, so I only kept four" — you kept the
whole array, because a slice header carries a capacity that reaches the end of its
backing array. When a small slice must outlive a large one it was cut from, copy it.
Note the three-index `batch[:n:n]` fixes `append` clobbering but not this leak: only a
copy detaches from the big array. Run `go test -race`.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`runtime.SetFinalizer`](https://pkg.go.dev/runtime#SetFinalizer)
- [`runtime.KeepAlive`](https://pkg.go.dev/runtime#KeepAlive)
- [Go blog: Go Slices: usage and internals (memory sharing)](https://go.dev/blog/slices-intro)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-csv-line-field-slicing.md](06-csv-line-field-slicing.md) | Next: [08-repository-page-defensive-copy.md](08-repository-page-defensive-copy.md)
