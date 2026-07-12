# Exercise 14: Byte-Budget Capped Export — Size-Based `TakeUntilBytes` Combinator

**Nivel: Intermedio** — validacion rapida (un test corto).

An S3 multipart part or an HTTP response body has a hard size cap, not a count
cap — you cannot just `Take(1000, records)` and hope the serialized size fits.
This exercise builds `TakeUntilBytes`, an `iter.Seq[T]` combinator that stops the
instant the next value would exceed a byte budget, so the caller can flush the
current chunk and start a new one with that same value.

## What you'll build

```text
export/                   independent module: example.com/export
  go.mod                  module example.com/export
  export.go                TakeUntilBytes[T]
  export_test.go           budget correctness, oversized-first, early break
```

Implement: `TakeUntilBytes[T any](maxBytes int, sizeOf func(T) int, src iter.Seq[T]) iter.Seq[T]` stopping before the running total would exceed `maxBytes`.
Test: four 100-byte records under a 250-byte budget yield exactly two; a budget that exactly matches the total includes everything; a first record larger than the whole budget is still yielded alone so the sequence makes progress; a consumer break stops after the requested count.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

Create `export.go`:

```go
package export

import "iter"

// TakeUntilBytes returns an iter.Seq that yields values from src while their
// cumulative size stays within maxBytes. sizeOf reports the serialized size
// of one value (e.g. len(json.Marshal(v))). The first value that would push
// the running total over maxBytes is not yielded, and the sequence stops
// there so the caller can flush the current export chunk and start a fresh
// one with that same value. A single value whose own size exceeds maxBytes is
// still yielded alone when it is the first one, so the sequence always makes
// progress instead of stalling forever.
func TakeUntilBytes[T any](maxBytes int, sizeOf func(T) int, src iter.Seq[T]) iter.Seq[T] {
	return func(yield func(T) bool) {
		used := 0
		for v := range src {
			size := sizeOf(v)
			if used > 0 && used+size > maxBytes {
				return
			}
			used += size
			if !yield(v) {
				return
			}
		}
	}
}
```

Create `export_test.go`:

```go
package export

import (
	"slices"
	"testing"
)

func sizeOfInt(v int) int { return v }

func TestTakeUntilBytesStopsBeforeOverBudget(t *testing.T) {
	t.Parallel()

	records := []int{100, 100, 100, 100}
	var got []int
	for v := range TakeUntilBytes(250, sizeOfInt, slices.Values(records)) {
		got = append(got, v)
	}
	if want := []int{100, 100}; !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestTakeUntilBytesOversizedFirstStillYields(t *testing.T) {
	t.Parallel()

	records := []int{500, 10, 10}
	var got []int
	for v := range TakeUntilBytes(50, sizeOfInt, slices.Values(records)) {
		got = append(got, v)
	}
	if want := []int{500}; !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestTakeUntilBytesConsumerBreaksEarly(t *testing.T) {
	t.Parallel()

	records := []int{10, 10, 10, 10, 10}
	var got []int
	for v := range TakeUntilBytes(1000, sizeOfInt, slices.Values(records)) {
		got = append(got, v)
		if len(got) == 2 {
			break
		}
	}
	if want := []int{10, 10}; !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}
```

## Verify

```bash
go test -count=1 ./...
```

## Review

The `used > 0 &&` guard is the whole trick: it is what lets the first record
through even when it alone exceeds `maxBytes`, so the combinator never produces an
empty chunk and stalls a caller that loops "fill a chunk, flush, repeat" forever.
Everything after the first record is checked strictly against the remaining
budget before it is counted, never after — so `used` never actually exceeds
`maxBytes` except in that single-oversized-first case, which is documented and
intentional. This is the size-based sibling of the count-based `Take` combinator:
same "stop the producer early" contract, but the stopping condition is measured
in bytes a caller actually has to fit into a fixed-size response or object part.

## Resources

- [`iter.Seq` — cooperative termination contract](https://pkg.go.dev/iter#Seq)
- [`slices.Equal`](https://pkg.go.dev/slices#Equal)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-sliding-window-moving-average.md](13-sliding-window-moving-average.md) | Next: [15-offset-limit-fanout-splitter.md](15-offset-limit-fanout-splitter.md)
