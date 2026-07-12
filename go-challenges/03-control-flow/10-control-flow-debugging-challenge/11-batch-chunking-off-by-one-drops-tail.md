# Exercise 11: The Batch Sender That Dropped the Last Partial Batch

**Nivel: Intermedio** — validacion rapida (un test corto).

A batch sender splits a slice of items into fixed-size batches by computing
the number of batches up front with integer division. It shipped correctly
for input sizes that are an exact multiple of the batch size and silently
dropped the trailing partial batch for every other size. You will reproduce it
with a non-multiple input, diagnose the truncated division, and fix the
loop to walk the slice by offset instead.

## What you'll build

```text
chunk/                     module example.com/chunk
  go.mod
  chunk.go                 Chunk(items []string, size int) [][]string
  chunk_test.go             batch count, trailing batch, total-items assertion
```

- Files: `chunk.go`, `chunk_test.go`.
- Implement: `Chunk(items []string, size int) [][]string` that returns ordered batches of at most `size` elements, including a final partial batch.
- Test: 7 items with `size=3` must produce 3 batches, the last one holding exactly the 1 leftover item, and the total item count across all batches must equal `len(items)`.
- Verify: `go test -count=1 ./...`.

### The artifact and the planted bug

```go
func Chunk(items []string, size int) [][]string {
	if size <= 0 {
		return nil
	}
	numBatches := len(items) / size // BUG: truncates, dropping the remainder
	batches := make([][]string, 0, numBatches)
	for i := 0; i < numBatches; i++ {
		start := i * size
		end := start + size
		batches = append(batches, items[start:end])
	}
	return batches
}
```

`len(items) / size` is integer division: it floors, so `7 / 3` evaluates to
`2`, not `2.33`. The loop then only ever produces 2 full batches and stops —
the 7th item is never sliced into any batch and is silently dropped. This
passes every test written against a size that happens to divide evenly (6
items at size 3, say), which is exactly why it reached production: the demo
data was a round number and the bug only shows up against a real, uneven
feed.

The failing output reads:

```text
--- FAIL: TestChunkKeepsTrailingPartialBatch
    chunk_test.go:9: len(batches) = 2, want 3
```

The fix walks the slice by starting offset instead of pre-computing a batch
count, so the loop condition — not a division — decides when to stop:

```go
func Chunk(items []string, size int) [][]string {
	if size <= 0 {
		return nil
	}
	var batches [][]string
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}
	return batches
}
```

Create `chunk.go`:

```go
package chunk

// Chunk splits items into ordered batches of at most size elements each,
// including a final partial batch when len(items) is not a multiple of size.
func Chunk(items []string, size int) [][]string {
	if size <= 0 {
		return nil
	}
	var batches [][]string
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}
	return batches
}
```

### Tests

Create `chunk_test.go`:

```go
package chunk

import "testing"

func TestChunkKeepsTrailingPartialBatch(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := Chunk(items, 3)
	if len(got) != 3 {
		t.Fatalf("len(batches) = %d, want 3", len(got))
	}
	if len(got[2]) != 1 || got[2][0] != "g" {
		t.Fatalf("last batch = %v, want [g]", got[2])
	}
	total := 0
	for _, b := range got {
		total += len(b)
	}
	if total != len(items) {
		t.Fatalf("total items across batches = %d, want %d", total, len(items))
	}
}
```

Run: `go test -count=1 ./...`.

## Review

Computing a loop bound with `len(items) / size` and then iterating that many
times is the same defect as an off-by-one retry counter: the arithmetic
silently discards a remainder instead of surfacing it as one more iteration.
The fix removes the pre-computed count entirely and lets the slice bounds
drive the loop, so there is nothing to get wrong about rounding. The test
pins both the batch count *and* the total item count, because a version that
merges the leftover into the last full batch instead of dropping it would
still pass a test that only checks the total.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — three-clause `for` and how a condition on the loop variable avoids a separately computed bound.
- [Go Slices: usage and internals](https://go.dev/blog/slices-intro) — half-open slice bounds `[start:end]` and clamping `end` to `len(items)`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-continue-skips-cleanup-leaked-handle.md](10-continue-skips-cleanup-leaked-handle.md) | Next: [12-defer-registered-too-late-leak-on-error-path.md](12-defer-registered-too-late-leak-on-error-path.md)
