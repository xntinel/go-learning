# Exercise 6: Value vs pointer parameters — measure the copy cost of a fat request struct

"Pass big structs by pointer" is folklore until you attach a number to it. This
module builds a realistically large request DTO, writes two functions that do
identical read-only work over it — one by value, one by pointer — and benchmarks
them with the Go 1.24 `for b.Loop()` loop, so the copy cost is a measured fact, not
a guess.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
reqcost/                   independent module: example.com/reqcost
  go.mod                   module example.com/reqcost
  request.go               Request (many fields + fixed array); process(Request); processPtr(*Request); SizeOfRequest
  cmd/
    demo/
      main.go              prints unsafe.Sizeof(Request{}) and both function results
  request_test.go          equality of both variants; no mutation; BenchmarkValue/BenchmarkPointer; size threshold
```

- Files: `request.go`, `cmd/demo/main.go`, `request_test.go`.
- Implement: a large `Request`, `process(r Request) int` and `processPtr(r *Request) int` doing identical read-only work, and a `SizeOfRequest()` probe via `unsafe.Sizeof`.
- Test: both variants return identical results; `processPtr` does not mutate the caller; `BenchmarkValue`/`BenchmarkPointer` with `for b.Loop()` and `b.ReportAllocs()`; assert `unsafe.Sizeof(Request{})` exceeds a threshold.
- Verify: `go test -count=1 -race ./...`

### The copy is real and its size is knowable

`Request` is a deliberately fat DTO: scalar fields, an embedded `Metadata` struct,
and a fixed `[256]byte` payload buffer. A fixed array is part of the struct's own
storage (unlike a slice, which is a 24-byte header pointing elsewhere), so
`unsafe.Sizeof(Request{})` includes all 256 bytes plus the other fields — well over
256 bytes total. `unsafe.Sizeof` returns the size in bytes of the struct's
representation at compile time (it does not follow pointers or slice backing
arrays); it is the exact number of bytes a value copy moves.

`process(r Request)` receives a full copy of those bytes on every call; `processPtr(r
*Request)` receives an 8-byte pointer and reads through it. Both do the same
read-only work — sum a few fields and part of the buffer — and return the same
`int`. Because the work is identical, the benchmark isolates the one difference:
the value variant copies `unsafe.Sizeof(Request)` bytes per call, the pointer
variant copies 8. For a struct this size the value copy is a measurable cost; for a
small struct (a couple of words) it would be cheap and often *faster* than the
pointer indirection, which is why the decision is "measure, do not reflexively
pointer-ize". `processPtr` is read-only on purpose — sharing by pointer here buys
cheap passing, not mutation, and the test asserts it does not change the caller.

Create `request.go`:

```go
package reqcost

import "unsafe"

// Metadata is embedded in Request to make it a realistically fat DTO.
type Metadata struct {
	TraceID  [16]byte
	SpanID   [8]byte
	Sampled  bool
	Priority int32
}

// Request is a large request DTO: scalars, embedded metadata, and a fixed
// payload buffer. The fixed array is stored inline, so a value copy moves the
// whole thing.
type Request struct {
	ID       uint64
	Method   uint8
	Flags    uint32
	Meta     Metadata
	Deadline int64
	Retries  int32
	Payload  [256]byte
	PayloadN int
}

// process reads Request by value: every call copies unsafe.Sizeof(Request)
// bytes. The work is a simple read-only checksum.
func process(r Request) int {
	sum := int(r.ID) + int(r.Method) + int(r.Flags) + int(r.Meta.Priority)
	for i := 0; i < r.PayloadN && i < len(r.Payload); i++ {
		sum += int(r.Payload[i])
	}
	return sum
}

// processPtr reads Request through a pointer: every call copies 8 bytes. The
// work is identical to process, and it does not mutate *r.
func processPtr(r *Request) int {
	sum := int(r.ID) + int(r.Method) + int(r.Flags) + int(r.Meta.Priority)
	for i := 0; i < r.PayloadN && i < len(r.Payload); i++ {
		sum += int(r.Payload[i])
	}
	return sum
}

// SizeOfRequest reports the byte width of a Request value copy.
func SizeOfRequest() uintptr {
	return unsafe.Sizeof(Request{})
}
```

### The runnable demo

The demo prints the struct's byte width — the exact size of a value copy — and
confirms both functions agree.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqcost"
)

func main() {
	r := reqcost.Request{ID: 7, Method: 2, Flags: 5, PayloadN: 3}
	r.Meta.Priority = 1
	r.Payload[0], r.Payload[1], r.Payload[2] = 10, 20, 30

	fmt.Printf("sizeof(Request) = %d bytes\n", reqcost.SizeOfRequest())
	fmt.Printf("process    = %d\n", reqcost.Process(r))
	fmt.Printf("processPtr = %d\n", reqcost.ProcessPtr(&r))
}
```

Expose the two functions to the demo package.

Append to `request.go`:

```go
// Process is the exported value-parameter variant.
func Process(r Request) int { return process(r) }

// ProcessPtr is the exported pointer-parameter variant.
func ProcessPtr(r *Request) int { return processPtr(r) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sizeof(Request) = 328 bytes
process    = 75
processPtr = 75
```

The exact size (328 here on a 64-bit target) is what alignment/padding produces for
these fields; run it on your machine to see the precise number. Sum: `7 + 2 + 5 + 1
+ (10 + 20 + 30) = 75`.

### Tests

`TestBothVariantsAgree` asserts `process` and `processPtr` return the same result
for the same input. `TestPointerVariantDoesNotMutate` fills a `Request`, calls
`processPtr`, and asserts the caller's struct is unchanged (read-only sharing).
`TestSizeJustifiesPointer` asserts `unsafe.Sizeof(Request{})` exceeds a threshold —
the numeric justification for preferring the pointer. `BenchmarkValue` and
`BenchmarkPointer` use `for b.Loop()` (Go 1.24) with `b.ReportAllocs()`; `b.Loop`
runs the body the right number of times and keeps the arguments alive so the
compiler cannot optimize the call away.

Create `request_test.go`:

```go
package reqcost

import (
	"fmt"
	"testing"
	"unsafe"
)

func sample() Request {
	r := Request{ID: 100, Method: 3, Flags: 9, PayloadN: 4}
	r.Meta.Priority = 2
	for i := range 4 {
		r.Payload[i] = byte(i + 1)
	}
	return r
}

func TestBothVariantsAgree(t *testing.T) {
	t.Parallel()

	r := sample()
	if got, want := process(r), processPtr(&r); got != want {
		t.Fatalf("process = %d, processPtr = %d; want equal", got, want)
	}
}

func TestPointerVariantDoesNotMutate(t *testing.T) {
	t.Parallel()

	r := sample()
	before := r
	_ = processPtr(&r)
	if r != before {
		t.Fatal("processPtr mutated the caller; it must be read-only")
	}
}

func TestSizeJustifiesPointer(t *testing.T) {
	t.Parallel()

	const threshold = 256
	if sz := unsafe.Sizeof(Request{}); sz <= threshold {
		t.Fatalf("sizeof(Request) = %d, want > %d to justify a pointer", sz, threshold)
	}
}

func BenchmarkValue(b *testing.B) {
	r := sample()
	b.ReportAllocs()
	var sink int
	for b.Loop() {
		sink = process(r) // copies unsafe.Sizeof(Request) bytes per call
	}
	_ = sink
}

func BenchmarkPointer(b *testing.B) {
	r := sample()
	b.ReportAllocs()
	var sink int
	for b.Loop() {
		sink = processPtr(&r) // copies 8 bytes per call
	}
	_ = sink
}

func Example() {
	r := Request{ID: 7, Method: 2, Flags: 5, PayloadN: 0}
	r.Meta.Priority = 1
	fmt.Println(process(r) == processPtr(&r))
	// Output: true
}
```

Run the benchmarks:

```bash
go test -bench=. -run=^$ .
```

The value benchmark reports a higher ns/op than the pointer benchmark on this fat
struct, and both report zero allocations (the copy lives on the stack). The gap is
the copy cost `unsafe.Sizeof(Request)` predicts. On a small struct the ordering can
reverse — that is exactly why you benchmark instead of assuming.

## Review

The measurement is honest when both variants return identical results (so you are
timing the same work) and the only difference is how the argument is passed. The
`unsafe.Sizeof` assertion turns "this struct is big" into a number, and the
benchmark turns "the copy costs something" into ns/op. Do not read this exercise as
"always use pointers": for a two-word struct the value copy is often faster and
avoids a heap escape that `&` could force; the crossover is real and this is how you
find it. Keep `processPtr` read-only — if a benchmark's function mutated shared
state, `-race` and repeated `b.Loop()` iterations would expose it. The `Example`
here is a compile/no-output placeholder because the interesting output is
benchmark timings, which are not stable enough to assert with `// Output:`.

## Resources

- [`testing.B.Loop`](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop and why it beats the old `for i := 0; i < b.N`.
- [`testing.B.ReportAllocs`](https://pkg.go.dev/testing#B.ReportAllocs) — per-op allocation reporting.
- [`unsafe.Sizeof`](https://pkg.go.dev/unsafe#Sizeof) — compile-time byte width of a value.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — pass-by-value vs pass-by-pointer guidance.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-patch-partial-update-semantics.md](05-patch-partial-update-semantics.md) | Next: [07-in-place-metrics-aggregation.md](07-in-place-metrics-aggregation.md)
