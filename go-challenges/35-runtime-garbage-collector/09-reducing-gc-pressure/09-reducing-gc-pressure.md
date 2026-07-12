# 9. Reducing GC Pressure

The most effective way to reduce GC overhead is to allocate less. Every heap allocation eventually becomes marking work, write-barrier overhead, and sweep work. This lesson works through six concrete techniques — escape analysis, `sync.Pool`, slice reuse, value types, string building, and zero-allocation encoding — applied to a deliberately allocation-heavy pipeline, with before/after benchmarks to confirm each improvement.

```text
gcpressure/
  go.mod
  pipeline.go
  pipeline_test.go
  cmd/demo/main.go
```

## Concepts

### Escape Analysis: Stack vs Heap

The Go compiler performs escape analysis to decide whether an allocation can live on the stack (cheap, no GC involvement) or must live on the heap (GC-visible). Values that outlive their declaring function, values stored in interfaces, values passed to goroutines, and values returned as pointers all typically escape to the heap.

You can inspect escape decisions with `go build -gcflags='-m'`. Look for "escapes to heap" messages. Common escape triggers to eliminate:

- Storing a local in an `interface{}` (boxing): use concrete types.
- Returning a pointer to a local: return a value instead.
- Closures capturing a pointer to a local: capture by value.
- Appending to a nil slice repeatedly: pre-allocate with `make([]T, 0, n)`.

### sync.Pool: Reuse Short-Lived Objects

`sync.Pool` holds a pool of objects that can be reused across allocations. The pool is safe for concurrent use. Objects in the pool may be freed at any GC cycle (the pool is cleared during GC), so it is for transient buffers, not long-lived state.

Pattern:

```go
var bufPool = sync.Pool{
    New: func() any { return make([]byte, 0, 4096) },
}

func process() {
    buf := bufPool.Get().([]byte)
    buf = buf[:0] // reset without reallocating
    defer bufPool.Put(buf)
    // use buf
}
```

Measuring impact: `go test -bench=. -benchmem` shows `allocs/op` and `B/op`. A well-used pool reduces both to near zero for the pooled type.

### Slice Reuse

Returning a slice from scratch on every call allocates a new backing array. Reusing slices with `s = s[:0]` resets the length to zero while keeping the backing array. The GC still sees the backing array (it contains pointers if `T` is a pointer type), but the array is not reallocated.

For pure-value slices (`[]byte`, `[]int`, `[]*T` where T is already heap-allocated), reuse is a pure allocation win.

### Value Types Over Pointer Types

A struct field of type `T` (not `*T`) is stored inline. The GC does not need to follow a pointer to find it. A `[]T` is one GC root; a `[]*T` is n roots (one per element pointer). Prefer value semantics when:

- The object does not need to be shared across goroutines by reference.
- The object's lifetime is bounded by its owner.
- The object is not large enough that copying is prohibitive.

### strings.Builder and bytes.Buffer

String concatenation with `+` allocates a new string on each operation. `strings.Builder` amortises this by growing a `[]byte` backing store and converting to a string only at the end. `bytes.Buffer` is equivalent and poolable.

```go
var sb strings.Builder
for _, s := range parts {
    sb.WriteString(s)
}
return sb.String() // one allocation
```

Pool a `strings.Builder` by resetting it with `sb.Reset()` before returning to the pool.

### Zero-Allocation Encoding

`fmt.Sprintf`, `strconv.Itoa`, and `encoding/json` all allocate. For hot paths, write directly to a `[]byte` buffer using `strconv.AppendInt`, `strconv.AppendFloat`, and manual quoting. The `append`-based functions grow the buffer in place and return it without extra allocation.

## Exercises

### Exercise 1: Pipeline with Baseline and Optimised Versions

Create `pipeline.go`:

```go
package gcpressure

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Record is a data record processed by the pipeline.
type Record struct {
	ID    int
	Name  string
	Value float64
	Tags  []string
}

// ProcessBaseline is the allocation-heavy baseline: it creates new strings,
// slices, and maps on every call.
func ProcessBaseline(records []Record) []string {
	results := []string{}
	for _, r := range records {
		if r.Value < 0 {
			continue
		}
		// String concatenation allocates on every iteration.
		line := "id=" + strconv.Itoa(r.ID) +
			" name=" + r.Name +
			" value=" + strconv.FormatFloat(r.Value, 'f', 2, 64)
		for _, tag := range r.Tags {
			line += " tag=" + tag
		}
		results = append(results, line)
	}
	return results
}

// ProcessOptimised uses Builder and pre-allocation to reduce allocations.
func ProcessOptimised(records []Record) []string {
	results := make([]string, 0, len(records))
	var sb strings.Builder
	for _, r := range records {
		if r.Value < 0 {
			continue
		}
		sb.Reset()
		sb.WriteString("id=")
		sb.WriteString(strconv.Itoa(r.ID))
		sb.WriteString(" name=")
		sb.WriteString(r.Name)
		sb.WriteString(" value=")
		sb.WriteString(strconv.FormatFloat(r.Value, 'f', 2, 64))
		for _, tag := range r.Tags {
			sb.WriteString(" tag=")
			sb.WriteString(tag)
		}
		results = append(results, sb.String())
	}
	return results
}

// global pool for the pooled variant.
var sbPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// ProcessPooled reuses strings.Builder instances from a pool.
func ProcessPooled(records []Record) []string {
	results := make([]string, 0, len(records))
	for _, r := range records {
		if r.Value < 0 {
			continue
		}
		sb := sbPool.Get().(*strings.Builder)
		sb.Reset()
		sb.WriteString("id=")
		sb.WriteString(strconv.Itoa(r.ID))
		sb.WriteString(" name=")
		sb.WriteString(r.Name)
		sb.WriteString(" value=")
		sb.WriteString(strconv.FormatFloat(r.Value, 'f', 2, 64))
		for _, tag := range r.Tags {
			sb.WriteString(" tag=")
			sb.WriteString(tag)
		}
		results = append(results, sb.String())
		sbPool.Put(sb)
	}
	return results
}

// AppendRecord encodes a Record into a []byte buffer without any allocation
// (when the caller provides a buffer with sufficient capacity).
// It returns the extended buffer.
func AppendRecord(dst []byte, r Record) []byte {
	dst = append(dst, "id="...)
	dst = strconv.AppendInt(dst, int64(r.ID), 10)
	dst = append(dst, " name="...)
	dst = append(dst, r.Name...)
	dst = append(dst, " value="...)
	dst = strconv.AppendFloat(dst, r.Value, 'f', 2, 64)
	for _, tag := range r.Tags {
		dst = append(dst, " tag="...)
		dst = append(dst, tag...)
	}
	return dst
}

// ProcessZeroAlloc encodes all records into a single pre-allocated buffer.
// It returns a slice of string views backed by the buffer; callers must not
// modify the buffer while the strings are in use.
func ProcessZeroAlloc(records []Record, buf []byte) ([]string, []byte) {
	results := make([]string, 0, len(records))
	for _, r := range records {
		if r.Value < 0 {
			continue
		}
		start := len(buf)
		buf = AppendRecord(buf, r)
		// Convert the appended region to a string without copying.
		// This is safe because buf is not modified after this point in the loop
		// until the caller resets it.
		results = append(results, string(buf[start:]))
	}
	return results, buf
}

// MakeTestRecords builds a slice of n records with deterministic values
// for use in benchmarks and tests.
func MakeTestRecords(n int) []Record {
	records := make([]Record, n)
	for i := range records {
		records[i] = Record{
			ID:    i,
			Name:  "item" + strconv.Itoa(i),
			Value: float64(i) * 1.5,
			Tags:  []string{"alpha", "beta"},
		}
	}
	return records
}
```

### Exercise 2: Example Function

Append to `pipeline.go`:

```go
// ExampleAppendRecord shows zero-allocation encoding.
func ExampleAppendRecord() {
	r := Record{ID: 1, Name: "alice", Value: 3.14, Tags: []string{"vip"}}
	buf := AppendRecord(make([]byte, 0, 64), r)
	fmt.Println(string(buf))
	// Output:
	// id=1 name=alice value=3.14 tag=vip
}
```

### Exercise 3: Tests and Benchmarks

Create `pipeline_test.go`:

```go
package gcpressure

import (
	"strings"
	"testing"
)

func TestProcessBaselineFiltersNegative(t *testing.T) {
	t.Parallel()

	records := []Record{
		{ID: 1, Name: "a", Value: 1.0},
		{ID: 2, Name: "b", Value: -1.0},
		{ID: 3, Name: "c", Value: 2.5},
	}
	got := ProcessBaseline(records)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (negative filtered)", len(got))
	}
}

func TestAllVariantsProduceSameOutput(t *testing.T) {
	t.Parallel()

	records := MakeTestRecords(20)
	baseline := ProcessBaseline(records)
	optimised := ProcessOptimised(records)
	pooled := ProcessPooled(records)

	if len(baseline) != len(optimised) {
		t.Errorf("len baseline=%d optimised=%d", len(baseline), len(optimised))
	}
	if len(baseline) != len(pooled) {
		t.Errorf("len baseline=%d pooled=%d", len(baseline), len(pooled))
	}
	for i := range baseline {
		if baseline[i] != optimised[i] {
			t.Errorf("record %d: baseline=%q optimised=%q", i, baseline[i], optimised[i])
		}
		if baseline[i] != pooled[i] {
			t.Errorf("record %d: baseline=%q pooled=%q", i, baseline[i], pooled[i])
		}
	}
}

func TestProcessZeroAllocMatchesBaseline(t *testing.T) {
	t.Parallel()

	records := MakeTestRecords(10)
	baseline := ProcessBaseline(records)
	got, _ := ProcessZeroAlloc(records, make([]byte, 0, 4096))

	if len(baseline) != len(got) {
		t.Fatalf("len baseline=%d zero-alloc=%d", len(baseline), len(got))
	}
	for i := range baseline {
		if baseline[i] != got[i] {
			t.Errorf("record %d: baseline=%q zero-alloc=%q", i, baseline[i], got[i])
		}
	}
}

func TestAppendRecordOutput(t *testing.T) {
	t.Parallel()

	r := Record{ID: 42, Name: "test", Value: 1.23, Tags: []string{"x", "y"}}
	buf := AppendRecord(make([]byte, 0, 64), r)
	got := string(buf)
	if !strings.Contains(got, "id=42") {
		t.Errorf("expected id=42 in %q", got)
	}
	if !strings.Contains(got, "name=test") {
		t.Errorf("expected name=test in %q", got)
	}
	if !strings.Contains(got, "tag=x") || !strings.Contains(got, "tag=y") {
		t.Errorf("expected tags in %q", got)
	}
}

// Benchmarks: run with go test -bench=. -benchmem to see allocs/op.

func BenchmarkBaseline(b *testing.B) {
	records := MakeTestRecords(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ProcessBaseline(records)
	}
}

func BenchmarkOptimised(b *testing.B) {
	records := MakeTestRecords(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ProcessOptimised(records)
	}
}

func BenchmarkPooled(b *testing.B) {
	records := MakeTestRecords(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ProcessPooled(records)
	}
}

func BenchmarkZeroAlloc(b *testing.B) {
	records := MakeTestRecords(100)
	buf := make([]byte, 0, 16384)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		_, buf = ProcessZeroAlloc(records, buf)
	}
}

// Your turn: write BenchmarkAppendRecord that benchmarks AppendRecord for
// a single Record with two tags. It should report 0 allocs/op when the
// destination buffer has sufficient pre-allocated capacity.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gcpressure"
)

func main() {
	records := gcpressure.MakeTestRecords(5)

	fmt.Println("Sample outputs (first 3 records):")
	baseline := gcpressure.ProcessBaseline(records)
	for i := 0; i < 3 && i < len(baseline); i++ {
		fmt.Printf("  %s\n", baseline[i])
	}
	fmt.Println()

	// Demonstrate zero-allocation path.
	buf := make([]byte, 0, 4096)
	_, _ = gcpressure.ProcessZeroAlloc(records, buf)

	fmt.Println("Benchmark guide:")
	fmt.Println("  go test -bench=. -benchmem")
	fmt.Println()
	fmt.Println("Expected ordering (B/op, lowest to highest):")
	fmt.Println("  ZeroAlloc < Optimised ~ Pooled < Baseline")
	fmt.Println()
	fmt.Println("Escape analysis:")
	fmt.Println("  go build -gcflags='-m' ./...")
}
```

## Common Mistakes

### Pooling Large Objects That Are Rarely Reused

Wrong: creating a `sync.Pool` for large structs that are only occasionally reused before the next GC cycle clears the pool.

What happens: the pool holds one or more large allocations indefinitely (until the next GC), increasing baseline memory usage without a proportional reduction in allocation rate.

Fix: pool objects that are frequently reused within a single GC cycle (request-scoped buffers, per-goroutine scratch buffers). Do not pool large objects used infrequently.

### Forgetting to Reset Before Returning to the Pool

Wrong: `pool.Put(sb)` after using a `strings.Builder` without calling `sb.Reset()`.

What happens: the next `pool.Get()` receives a builder with leftover content from the previous use. The output is corrupted silently.

Fix: always call `Reset()` (or `buf = buf[:0]` for byte slices) before `Put`.

### Using fmt.Sprintf in a Benchmark Without ReportAllocs

Wrong: measuring a function that uses `fmt.Sprintf` and reporting only ns/op.

What happens: `fmt.Sprintf` allocates on every call. The benchmark looks fast in ns/op but the allocation rate is hidden.

Fix: always use `b.ReportAllocs()` in benchmarks for allocation-heavy code. The `allocs/op` and `B/op` columns reveal the true cost.

### Assuming append Does Not Allocate

Wrong: using `append` to grow a slice on every iteration and expecting zero allocations.

What happens: `append` allocates a new backing array when the current capacity is exceeded. In a loop, this causes O(log n) allocations for n elements.

Fix: pre-allocate with `make([]T, 0, n)` when n is known in advance. Use `b.ReportAllocs()` to verify.

## Verification

From `~/go-exercises/gcpressure`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=. -benchmem
```

Inspect escape decisions:

```bash
go build -gcflags='-m' ./... 2>&1 | grep escapes
```

## Summary

- Allocating less is the most reliable way to reduce GC overhead.
- Escape analysis determines stack vs heap; use `-gcflags='-m'` to audit escape decisions.
- `sync.Pool` eliminates repeated allocation of short-lived objects; always reset before returning.
- Pre-allocated slices (`make([]T, 0, n)`) avoid O(log n) backing-array reallocations.
- `strings.Builder` and `append`-based encoding (`strconv.AppendInt` etc.) reduce string allocation to one per output.
- Value types (not pointer types) reduce GC scan work by keeping data inline.
- Always benchmark before and after with `go test -bench=. -benchmem`; intuition about allocation is often wrong.

## What's Next

Next: [Arena Allocation Patterns](../10-arena-allocation-patterns/10-arena-allocation-patterns.md).

## Resources

- [Go Compiler Optimizations: Escape Analysis](https://github.com/golang/go/wiki/CompilerOptimizations#escape-analysis) — how and when the compiler promotes to the heap
- [sync.Pool](https://pkg.go.dev/sync#Pool) — object pool documentation including the GC-clearing behaviour
- [strings.Builder](https://pkg.go.dev/strings#Builder) — efficient string construction
- [strconv package](https://pkg.go.dev/strconv) — AppendInt, AppendFloat, and other zero-allocation formatters
- [Dave Cheney: High Performance Go Workshop](https://dave.cheney.net/high-performance-go-workshop/gophercon-2019.html) — comprehensive allocation reduction techniques
