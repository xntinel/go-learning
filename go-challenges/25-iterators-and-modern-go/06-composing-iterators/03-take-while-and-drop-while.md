# Exercise 3: TakeWhile And DropWhile

`Take(n)` slices a stream by position. `TakeWhile(pred)` slices it by *condition*: it yields values for as long as a predicate holds and stops at the first value that fails it. Its mirror, `DropWhile(pred)`, discards the leading run that satisfies the predicate and yields everything from the first failure onward. The headline property of `TakeWhile` is that it short-circuits — given an infinite source, it pulls only up to and including the first failing value and not one more — which makes it the combinator that lets a pipeline consume a bounded prefix of an unbounded stream. The test proves that exactly, by counting pulls against a source of a billion integers.

This module is fully self-contained. It begins with its own `go mod init`, defines both combinators, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
whileseq.go          TakeWhile (short-circuits on first failure) and DropWhile
cmd/
  demo/
    main.go          take the leading even run, drop the leading even run
whileseq_test.go     TakeWhile/DropWhile output, exact-pull short-circuit, early break
```

- Files: `whileseq.go`, `cmd/demo/main.go`, `whileseq_test.go`.
- Implement: `TakeWhile[V](seq iter.Seq[V], pred func(V) bool) iter.Seq[V]` and `DropWhile[V](seq iter.Seq[V], pred func(V) bool) iter.Seq[V]`.
- Test: `TakeWhile` stops at the first failing value; over an effectively infinite source it pulls exactly one past the last yielded value and no further; `DropWhile` skips only the *leading* run.
- Verify: `go test -race ./...`

### Short-circuiting on a condition

`TakeWhile` reads like one sentence of Go: for each upstream value, if the predicate fails, return; otherwise yield it, and if the consumer stops, return. The first `return` is the short-circuit — the moment a value fails the predicate, the combinator is done and stops pulling from upstream. That is the whole feature, and it is what makes `TakeWhile` safe to point at an infinite generator: it will consume the leading run plus exactly one more value (the one that fails the test, which it must pull to discover the run is over) and then leave upstream alone forever.

That "one more value" is not a bug, it is unavoidable and worth understanding. A push source delivers the failing value before `TakeWhile` can judge it — the value has to be pulled to be tested. So over the naturals `1, 2, 3, ...` with predicate `n < 4`, `TakeWhile` yields `1, 2, 3` and pulls `4` (to discover `4` fails) and stops. Four pulls, three yields. The laziness test asserts exactly that count, which is the strongest possible evidence that nothing past the boundary is touched: if the source were a billion integers, only four are ever produced.

`DropWhile` is the complement and has one piece of state. It carries a `dropping` flag that starts `true` and flips to `false` the first time the predicate fails. While `dropping` and the predicate holds, it `continue`s without yielding; the moment the predicate fails, it clears the flag and yields that value and every value after it unconditionally. The flag matters because the predicate may become true again later — `DropWhile(isEven)` over `2, 4, 6, 7, 8, 10` must drop only the leading `2, 4, 6` and keep `7, 8, 10` including the later evens. A version that simply skipped every value satisfying the predicate would wrongly drop the trailing `8` and `10`.

Create `whileseq.go`:

```go
package whileseq

import "iter"

// TakeWhile yields values of seq while pred holds and stops at the first value
// for which pred is false. Over an infinite source it pulls exactly the leading
// run plus the one failing value, then stops.
func TakeWhile[V any](seq iter.Seq[V], pred func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		for v := range seq {
			if !pred(v) {
				return // first failure: short-circuit, pull no more
			}
			if !yield(v) {
				return // the consumer stopped
			}
		}
	}
}

// DropWhile discards the leading run of values for which pred holds and yields
// every value from the first failure onward, including later values that would
// satisfy pred.
func DropWhile[V any](seq iter.Seq[V], pred func(V) bool) iter.Seq[V] {
	return func(yield func(V) bool) {
		dropping := true
		for v := range seq {
			if dropping && pred(v) {
				continue
			}
			dropping = false
			if !yield(v) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo runs both combinators over the same slice with the same predicate so the split is obvious: `TakeWhile(isEven)` keeps the leading even run, `DropWhile(isEven)` keeps everything after it. The slice `2, 4, 6, 7, 8, 10` is chosen so that evens appear again *after* the first odd, which is what distinguishes `DropWhile` from a plain `Filter`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"

	"example.com/whileseq"
)

func main() {
	isEven := func(n int) bool { return n%2 == 0 }
	nums := []int{2, 4, 6, 7, 8, 10}

	taken := whileseq.TakeWhile(slices.Values(nums), isEven)
	fmt.Println("take while even:", slices.Collect(taken))

	dropped := whileseq.DropWhile(slices.Values(nums), isEven)
	fmt.Println("drop while even:", slices.Collect(dropped))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
take while even: [2 4 6]
drop while even: [7 8 10]
```

### Tests

`TestTakeWhile` and `TestDropWhile` pin the output split, with `DropWhile` keeping the later evens to prove it drops only the *leading* run. `TestTakeWhileShortCircuits` is the laziness proof: it counts pulls from a source of a billion integers and asserts `TakeWhile(n < 4)` pulled exactly four — the three yielded plus the one failing value — so nothing past the boundary is ever produced. `TestTakeWhileEarlyBreak` confirms a consumer that breaks mid-run stops the upstream too.

Create `whileseq_test.go`:

```go
package whileseq

import (
	"slices"
	"testing"
)

func TestTakeWhile(t *testing.T) {
	t.Parallel()

	isEven := func(n int) bool { return n%2 == 0 }
	got := slices.Collect(TakeWhile(slices.Values([]int{2, 4, 6, 7, 8, 10}), isEven))
	if want := []int{2, 4, 6}; !slices.Equal(got, want) {
		t.Fatalf("TakeWhile = %v, want %v", got, want)
	}
}

func TestDropWhile(t *testing.T) {
	t.Parallel()

	isEven := func(n int) bool { return n%2 == 0 }
	got := slices.Collect(DropWhile(slices.Values([]int{2, 4, 6, 7, 8, 10}), isEven))
	if want := []int{7, 8, 10}; !slices.Equal(got, want) {
		t.Fatalf("DropWhile = %v, want %v", got, want)
	}
}

func TestTakeWhileShortCircuits(t *testing.T) {
	t.Parallel()

	pulled := 0
	naturals := func(yield func(int) bool) {
		for i := 1; i <= 1_000_000_000; i++ {
			pulled++
			if !yield(i) {
				return
			}
		}
	}

	got := slices.Collect(TakeWhile(naturals, func(n int) bool { return n < 4 }))
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("TakeWhile = %v, want %v", got, want)
	}
	// Three values yielded plus the one failing value (4) that had to be pulled
	// to discover the run was over. Nothing past 4 is ever produced.
	if pulled != 4 {
		t.Fatalf("source pulled %d values, want exactly 4", pulled)
	}
}

func TestTakeWhileEarlyBreak(t *testing.T) {
	t.Parallel()

	pulled := 0
	naturals := func(yield func(int) bool) {
		for i := 1; i <= 1_000_000; i++ {
			pulled++
			if !yield(i) {
				return
			}
		}
	}

	var got []int
	for v := range TakeWhile(naturals, func(n int) bool { return n < 100 }) {
		got = append(got, v)
		if v == 2 {
			break
		}
	}
	if want := []int{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	// The break after 2 propagates up through TakeWhile to the source, which
	// pulls 1 and 2 and then stops.
	if pulled != 2 {
		t.Fatalf("source pulled %d values, want exactly 2", pulled)
	}
}
```

## Review

`TakeWhile` is correct when it stops at the first failing value and propagates a downstream break. The decisive evidence is `TestTakeWhileShortCircuits`: a billion-element source yields only `1, 2, 3` and is pulled exactly four times, which can only happen if the `if !pred(v) { return }` short-circuit fires on the value 4 and nothing further is pulled. `TestTakeWhileEarlyBreak` shows the other exit path — `if !yield(v) { return }` carries a consumer's `break` back to the source. `DropWhile` is correct when its `dropping` flag drops only the leading run, which `TestDropWhile` confirms by keeping the trailing `8` and `10`.

Common mistakes for this feature. The first is implementing `DropWhile` as "skip every value that satisfies `pred`," which is just an inverted `Filter` and wrongly drops later values that satisfy the predicate; the `dropping` flag, cleared on the first failure, is what limits the skipping to the *leading* run. The second is expecting `TakeWhile` to pull exactly as many values as it yields — it must pull one extra, the failing value, because a push source delivers a value before the predicate can judge it. The third is the familiar bare `yield(v)` with no stop check, which would keep pulling from an infinite source after the consumer left and never return.

## Resources

- [`iter` package](https://pkg.go.dev/iter) — the `Seq` type both combinators take and return.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how a `break` in the consumer makes `yield` return `false` and how `return` short-circuits the source.
- [Rust `Iterator::take_while`](https://doc.rust-lang.org/std/iter/trait.Iterator.html#method.take_while) — the same combinator in another lazy-iterator language, useful for contrasting the "one extra pull" behavior.
- [`slices.Values`](https://pkg.go.dev/slices#Values) and [`slices.Collect`](https://pkg.go.dev/slices#Collect) — the slice source and slice sink used by the demo and tests.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-zip-and-flatten.md](02-zip-and-flatten.md) | Next: [04-lazy-etl-pipeline.md](04-lazy-etl-pipeline.md)
