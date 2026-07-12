# Exercise 12: Metrics Downsampler — Keep Every Nth Event with a `Sample` Combinator

**Nivel: Intermedio** — validacion rapida (un test corto).

A high-volume event stream feeding a low-fidelity dashboard does not need every
event — it needs a stable, deterministic 1-in-N sample. This exercise builds
`Every`, a stride-sampling `iter.Seq` combinator that keeps positions `0, n, 2n,
...` and forwards the cooperative stop, distinct from `Take` because it walks the
whole source rather than stopping after the first N kept values.

## What you'll build

```text
sample/                   independent module: example.com/sample
  go.mod                  module example.com/sample
  sample.go                Every[T]
  sample_test.go           stride correctness + upstream-stop proof
```

Implement: `Every[T any](n int, src iter.Seq[T]) iter.Seq[T]` keeping 0-based positions `0, n, 2n, ...`; `n < 1` is treated as `1`.
Test: `n=3` over ten events keeps four of them; `n=1` keeps everything; `n=0` behaves like `n=1`; a consumer `break` after keeping 3 stops the upstream source at exactly the right call count.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

Create `sample.go`:

```go
package sample

import "iter"

// Every returns an iter.Seq that keeps elements of src at 0-based positions
// 0, n, 2n, ... and drops the rest. It is a stride sampler for downsampling a
// high-volume event stream before it reaches a low-fidelity dashboard feed:
// n=1 keeps everything, n=10 keeps 1 event in 10. n below 1 is treated as 1.
func Every[T any](n int, src iter.Seq[T]) iter.Seq[T] {
	if n < 1 {
		n = 1
	}
	return func(yield func(T) bool) {
		i := 0
		for v := range src {
			keep := i%n == 0
			i++
			if keep {
				if !yield(v) {
					return
				}
			}
		}
	}
}
```

Create `sample_test.go`:

```go
package sample

import (
	"iter"
	"slices"
	"testing"
)

func TestEveryThird(t *testing.T) {
	t.Parallel()

	events := []int{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	var got []int
	for v := range Every(3, slices.Values(events)) {
		got = append(got, v)
	}
	want := []int{10, 13, 16, 19}
	if !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

// countingSeq yields 0..n-1 and records into calls how many values the
// producer actually emitted, proving Every's break propagates upstream.
func countingSeq(n int, calls *int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := range n {
			*calls++
			if !yield(i) {
				return
			}
		}
	}
}

func TestEveryStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	calls := 0
	kept := 0
	for range Every(2, countingSeq(1000, &calls)) {
		kept++
		if kept == 3 {
			break
		}
	}
	if kept != 3 {
		t.Fatalf("kept = %d, want 3", kept)
	}
	// Keeping indices 0, 2, 4 requires consuming positions 0..4, i.e. 5 calls.
	if calls != 5 {
		t.Fatalf("calls = %d, want 5 (source must stop, not run to completion)", calls)
	}
}
```

## Verify

```bash
go test -count=1 ./...
```

## Review

`Every` looks like `Filter` with a counter, and structurally it is — but the
production distinction matters: `Filter` drops values based on their content,
`Every` drops them based on their position in the stream, which is exactly what a
metrics downsampler needs when every event is "valid" but there are too many of
them. The upstream-stop test is the part worth internalizing: `Every` forwards
`yield`'s `false` immediately, so a consumer `break` after keeping 3 samples with
`n=2` stops the source after exactly 5 values, not 1000. A sampler that "keeps
going in the background to stay warm" is a resource leak wearing a feature.

## Resources

- [`iter.Seq` — cooperative termination contract](https://pkg.go.dev/iter#Seq)
- [`slices.Values`](https://pkg.go.dev/slices#Values)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-sharded-request-id-generator.md](11-sharded-request-id-generator.md) | Next: [13-sliding-window-moving-average.md](13-sliding-window-moving-average.md)
