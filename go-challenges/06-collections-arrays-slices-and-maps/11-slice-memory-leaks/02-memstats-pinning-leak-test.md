# Exercise 2: Prove Non-Pinning With a runtime.ReadMemStats Delta Harness

The independent-copy test in Exercise 1 proves a snapshot does not *alias* the
buffer. It does not prove the snapshot does not *pin* it — aliasing and pinning
are different failures, and only the second is the memory leak. This module builds
the harness that proves the pin, or its absence: allocate a large backing array,
take a one-byte snapshot two ways, drop the source, force GC, and read `HeapAlloc`.
The copy path lets the array be reclaimed; the sub-slice path keeps all of it
alive. This is the exact technique for turning "I think this leaks" into a test.

## What you'll build

```text
pinprobe/                    independent module: example.com/pinprobe
  go.mod                     go 1.24
  pinprobe.go                ReadHeap, Fill, CopySnapshot, SubSnapshot
  cmd/
    demo/
      main.go                measures both paths, prints reclaimed/pinned booleans
  pinprobe_test.go           copy-path-reclaims, subslice-path-pins, benchmark, example
```

Files: `pinprobe.go`, `cmd/demo/main.go`, `pinprobe_test.go`.
Implement: `ReadHeap()` (GC twice, then `HeapAlloc`), `Fill(n)` (a committed `[]byte`), `CopySnapshot` (non-pinning), `SubSnapshot` (pinning).
Test: assert the copy path's `HeapAlloc` returns near baseline after GC and the sub-slice path's stays a large-array higher.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/02-memstats-pinning-leak-test/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/02-memstats-pinning-leak-test
go mod edit -go=1.24
```

## Why the harness looks the way it does

Four details separate a real leak test from a flaky one, and each is a line in
`ReadHeap` or `Fill`.

First, **force GC before reading, twice.** `HeapAlloc` counts the bytes of live
heap objects, but sweeping is incremental — a single `runtime.GC()` may not have
finished reclaiming everything from the previous cycle. Calling it twice means the
second collection completes the sweep of the first, so the reading is stable. Read
`HeapAlloc` only after both.

Second, **commit the pages by writing.** `make([]byte, n)` reserves the allocation,
and the Go runtime does zero it, so the accounting is real — but to be certain the
array is a genuine, individually-reclaimable large object and not something the
optimizer can elide, `Fill` writes a byte pattern across it. The write also
mirrors reality: a read buffer you are worried about pinning is a buffer that
holds data.

Third, **compare magnitudes, never exact bytes.** The test asserts the post-GC
`HeapAlloc` delta is *below half the array* for the copy path and *above half* for
the sub-slice path. An 8 MiB array with a 4 MiB threshold leaves megabytes of
margin on both sides, which absorbs the background noise of the test runtime and
the race detector. Asserting an exact byte count would flake on the first
unrelated allocation.

Fourth, **keep the source alive across the baseline read with `runtime.KeepAlive`.**
The compiler is free to end an object's lifetime at its last use, which can be
*before* the line you think measures it "while live." `runtime.KeepAlive(src)`
placed after the read forces the source to remain reachable through that read, so
the "array is live" baseline is honest. `KeepAlive` extends liveness for the
measurement; it never severs a pin — that distinction is the whole lesson.

The two snapshot functions are the contrast. `CopySnapshot(src, n)` returns
`append([]byte(nil), src[:n]...)` — a fresh one-element-array copy, no pin.
`SubSnapshot(src, n)` returns `src[:n]` — a header into `src`'s array, which pins
all of it. Same visible bytes; opposite lifetime consequences.

Create `pinprobe.go`:

```go
// Package pinprobe measures whether a small slice keeps a large backing array
// alive, using runtime.ReadMemStats deltas around a forced GC.
package pinprobe

import "runtime"

// ReadHeap returns HeapAlloc after two full GC cycles. The second GC completes
// the sweep started by the first, so the reading is stable rather than mid-sweep.
func ReadHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Fill allocates an n-byte slice and writes a pattern across it so the pages are
// committed and the object is an individually reclaimable large allocation.
func Fill(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// CopySnapshot returns the first n bytes in a freshly allocated array. It does
// not pin src's backing array: once the caller drops src, the array is reclaimable.
func CopySnapshot(src []byte, n int) []byte {
	return append([]byte(nil), src[:n]...)
}

// SubSnapshot returns the first n bytes as a sub-slice of src. The returned header
// points into src's array, so keeping it pins the entire array — the leak this
// harness detects.
func SubSnapshot(src []byte, n int) []byte {
	return src[:n]
}
```

## The runnable demo

The demo runs both paths and prints whether the large array was reclaimed (copy)
or pinned (sub-slice). The booleans are deterministic because they compare against
a half-array threshold with megabytes of margin.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"

	"example.com/pinprobe"
)

func main() {
	const n = 8 << 20 // 8 MiB
	const half = n / 2

	base := pinprobe.ReadHeap()

	// Copy path: snapshot is independent, so dropping src frees the array.
	src := pinprobe.Fill(n)
	snap := pinprobe.CopySnapshot(src, 1)
	runtime.KeepAlive(src)
	src = nil
	afterCopy := pinprobe.ReadHeap()
	copyReclaimed := int64(afterCopy)-int64(base) < half
	fmt.Printf("copy path reclaimed:  %v\n", copyReclaimed)
	runtime.KeepAlive(snap)

	// Sub-slice path: snapshot pins the array, so dropping src frees nothing.
	src2 := pinprobe.Fill(n)
	sub := pinprobe.SubSnapshot(src2, 1)
	src2 = nil
	afterSub := pinprobe.ReadHeap()
	subPinned := int64(afterSub)-int64(base) >= half
	fmt.Printf("subslice path pinned: %v\n", subPinned)
	runtime.KeepAlive(sub)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
copy path reclaimed:  true
subslice path pinned: true
```

## Tests

The tests assert the two paths' `HeapAlloc` deltas. They are deliberately *not*
`t.Parallel()`: `HeapAlloc` is process-global, so a concurrent test allocating in
another goroutine would perturb the reading. Go runs non-parallel tests one at a
time, so each memory test measures in isolation. The benchmark exists to quantify
the copy's single per-call allocation under `-benchmem`, and the example pins the
copy's length.

Create `pinprobe_test.go`:

```go
package pinprobe

import (
	"fmt"
	"runtime"
	"testing"
)

const probeSize = 8 << 20 // 8 MiB; half = 4 MiB threshold leaves wide margin.

func TestCopyPathReclaims(t *testing.T) {
	base := ReadHeap()

	src := Fill(probeSize)
	live := ReadHeap()

	snap := CopySnapshot(src, 1)
	runtime.KeepAlive(src)
	src = nil
	after := ReadHeap()

	half := int64(probeSize) / 2
	if liveDelta := int64(live) - int64(base); liveDelta < half {
		t.Fatalf("array not accounted live: delta %d bytes, want >= %d", liveDelta, half)
	}
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("copy path pinned the array: delta %d bytes, want < %d", delta, half)
	}
	if len(snap) != 1 {
		t.Fatalf("len(snap) = %d, want 1", len(snap))
	}
	runtime.KeepAlive(snap)
}

func TestSubslicePathPins(t *testing.T) {
	base := ReadHeap()

	src := Fill(probeSize)
	sub := SubSnapshot(src, 1)
	src = nil
	after := ReadHeap()

	half := int64(probeSize) / 2
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("sub-slice did not pin the array: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(sub)
}

func BenchmarkCopySnapshot(b *testing.B) {
	src := Fill(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		snap := CopySnapshot(src, 64)
		runtime.KeepAlive(snap)
	}
}

func ExampleCopySnapshot() {
	src := Fill(1024)
	snap := CopySnapshot(src, 4)
	fmt.Println(len(snap), src[0], snap[0])
	// Output: 4 0 0
}
```

## Review

The harness is correct when the two tests split cleanly: the copy path returns to
within half an array of baseline after GC (the 8 MiB was reclaimed), and the
sub-slice path stays at least half an array above baseline (the 8 MiB is pinned by
the one-byte view). The four disciplines are what keep it from flaking — GC twice
before every read, write to commit the array, compare magnitudes with a wide
margin, and `KeepAlive` the source across the baseline read. The mistakes this
module guards against are a MemStats test with no writes and no forced GC
(flaky), and the belief that `runtime.KeepAlive` could *fix* the sub-slice leak: it
only extends liveness, so it makes the pin *last longer*, never shorter. The fix
is always the copy. Run `go test -race` to confirm both measurements hold under
the race detector's allocation noise.

## Resources

- [`runtime.MemStats` and `ReadMemStats`](https://pkg.go.dev/runtime#MemStats) — `HeapAlloc`, `HeapInuse`, and how to read them.
- [`runtime.GC`](https://pkg.go.dev/runtime#GC) — forcing a complete collection before a measurement.
- [`runtime.KeepAlive`](https://pkg.go.dev/runtime#KeepAlive) — extending an object's reachability across a measurement point.

---

Back to [01-bounded-buffer-independent-snapshot.md](01-bounded-buffer-independent-snapshot.md) | Next: [03-subscriber-registry-delete-zeroing.md](03-subscriber-registry-delete-zeroing.md)
