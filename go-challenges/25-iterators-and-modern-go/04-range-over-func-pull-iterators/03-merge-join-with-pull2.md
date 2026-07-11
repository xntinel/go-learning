# Exercise 3: Merge-Join Two Sequences with iter.Pull2

Key/value sequences have a two-value pull form, `iter.Pull2`, whose `next` returns `(K, V, bool)`. This exercise uses it to build a sorted merge-join: given two sequences ordered by key, emit one result per key that appears in *both*. It is the relational inner join expressed as a single coordinated walk over two cursors — something no single push iterator can do.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
join.go              Pairs, Match, Join (iter.Pull2 over two Seq2 cursors)
cmd/
  demo/
    main.go          join two name/value tables on matching names
join_test.go         inner-match set, disjoint keys, empty input, early break
```

- Files: `join.go`, `cmd/demo/main.go`, `join_test.go`.
- Implement: `Pairs(keys []string, vals []int) iter.Seq2[string, int]`, the `Match` struct, and `Join(left, right iter.Seq2[string, int]) iter.Seq[Match]`.
- Test: `join_test.go` checks the matched-key set, disjoint inputs (empty result), an empty side, and that the producer can be stopped after one match.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p merge-join/cmd/demo && cd merge-join
go mod init example.com/merge-join
```

### Why `iter.Pull2`, and the three-way key comparison

A merge-join consumes two inputs sorted by key and advances whichever cursor has the smaller key, emitting a row only when the two keys are equal. That is a lockstep walk, so it needs pull cursors — and because the inputs are key/value sequences (`iter.Seq2[string, int]`), the right conversion is `iter.Pull2`, whose signature is `func Pull2[K, V](seq Seq2[K, V]) (next func() (K, V, bool), stop func())`. Each `next` hands back a key, a value, and the `ok` flag in one call, so the join holds the current `(key, value)` of each side directly.

The body of the loop is a three-way comparison on the two current keys, and each branch does exactly one thing:

- `lk < rk`: the left key is behind, so it can never match anything still to come on the right (the right is sorted ascending). Advance left.
- `lk > rk`: symmetric. Advance right.
- equal: a match. Emit `Match{Key, Left, Right}` and advance *both* sides.

Because both inputs are sorted, each cursor only ever moves forward, so the whole join is a single linear pass — O(n + m), no buffering of one side, no nested scan. The loop runs while both sides still have values (`okL && okR`); the moment either is exhausted, no further key can appear in both, so the join is done and the leftover side is simply dropped. Both `iter.Pull2` calls get their own `defer stop()`, so an early `break` by the consumer unwinds both producers.

This is the canonical case the concepts file describes as "push cannot express it": a single `yield`-driven loop walks one sequence, but the join's decision — which cursor to step — depends on comparing the live fronts of two sequences at once.

Create `join.go`:

```go
package mergejoin

import "iter"

// Pairs returns a key/value push iterator over parallel keys and vals slices.
// The keys are expected to be sorted ascending for use with Join.
func Pairs(keys []string, vals []int) iter.Seq2[string, int] {
	return func(yield func(string, int) bool) {
		for i := range keys {
			if !yield(keys[i], vals[i]) {
				return
			}
		}
	}
}

// Match is one joined row: a key present in both inputs, with each side's value.
type Match struct {
	Key   string
	Left  int
	Right int
}

// Join performs a sorted merge-join of two key/value sequences, both ordered by
// key ascending, and yields one Match per key present in BOTH inputs. It walks
// each input once with an iter.Pull2 cursor and releases both with deferred stop.
func Join(left, right iter.Seq2[string, int]) iter.Seq[Match] {
	return func(yield func(Match) bool) {
		nextL, stopL := iter.Pull2(left)
		defer stopL()
		nextR, stopR := iter.Pull2(right)
		defer stopR()

		lk, lv, okL := nextL()
		rk, rv, okR := nextR()
		for okL && okR {
			switch {
			case lk < rk:
				lk, lv, okL = nextL()
			case lk > rk:
				rk, rv, okR = nextR()
			default:
				if !yield(Match{Key: lk, Left: lv, Right: rv}) {
					return
				}
				lk, lv, okL = nextL()
				rk, rv, okR = nextR()
			}
		}
	}
}
```

The output is a push iterator (`iter.Seq[Match]`), so callers consume it with a plain `for m := range Join(...)`. The pull machinery is entirely internal. Note `Join` assumes its inputs are sorted; that is the precondition a merge-join is built on, and feeding it unsorted keys produces a partial join rather than a panic — the same contract a database planner relies on when it chooses a merge-join over a hash-join.

### The runnable demo

The demo joins two small name/value tables. `alice` is only on the left and `carol` only on the right, so neither appears; `bob` and `dave` are in both and produce one `Match` each, carrying both side values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/merge-join"
)

func main() {
	left := mergejoin.Pairs([]string{"alice", "bob", "dave"}, []int{1, 2, 4})
	right := mergejoin.Pairs([]string{"bob", "carol", "dave"}, []int{200, 300, 400})

	for m := range mergejoin.Join(left, right) {
		fmt.Printf("%s: left=%d right=%d\n", m.Key, m.Left, m.Right)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bob: left=2 right=200
dave: left=4 right=400
```

### Tests

`TestJoinInner` joins two tables sharing two of three keys and asserts the exact matched rows, pinning the equal-key branch and both advance-the-smaller branches. `TestJoinDisjoint` joins inputs with no shared keys and expects an empty result, exercising the advance branches without ever reaching the match case. `TestJoinEmpty` feeds an empty left side so the loop never starts. `TestJoinStopsEarly` breaks after the first match, which fires both deferred `stop`s.

Create `join_test.go`:

```go
package mergejoin

import (
	"reflect"
	"testing"
)

func collect(seq func(func(Match) bool)) []Match {
	out := []Match{}
	for m := range seq {
		out = append(out, m)
	}
	return out
}

func TestJoinInner(t *testing.T) {
	t.Parallel()

	left := Pairs([]string{"a", "b", "d"}, []int{1, 2, 4})
	right := Pairs([]string{"b", "c", "d"}, []int{20, 30, 40})
	got := collect(Join(left, right))
	want := []Match{
		{Key: "b", Left: 2, Right: 20},
		{Key: "d", Left: 4, Right: 40},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Join = %v, want %v", got, want)
	}
}

func TestJoinDisjoint(t *testing.T) {
	t.Parallel()

	left := Pairs([]string{"a", "c"}, []int{1, 3})
	right := Pairs([]string{"b", "d"}, []int{2, 4})
	if got := collect(Join(left, right)); len(got) != 0 {
		t.Fatalf("disjoint Join = %v, want empty", got)
	}
}

func TestJoinEmpty(t *testing.T) {
	t.Parallel()

	left := Pairs(nil, nil)
	right := Pairs([]string{"a"}, []int{1})
	if got := collect(Join(left, right)); len(got) != 0 {
		t.Fatalf("empty-left Join = %v, want empty", got)
	}
}

func TestJoinStopsEarly(t *testing.T) {
	t.Parallel()

	left := Pairs([]string{"a", "b", "c"}, []int{1, 2, 3})
	right := Pairs([]string{"a", "b", "c"}, []int{10, 20, 30})
	first := Match{}
	for m := range Join(left, right) {
		first = m
		break
	}
	if want := (Match{Key: "a", Left: 1, Right: 10}); first != want {
		t.Fatalf("first match = %v, want %v", first, want)
	}
}
```

## Review

The join is correct when each cursor advances only forward and a row is emitted only on key equality. The three-way `switch` is the whole algorithm: advance the side with the smaller key, and on a tie emit and advance both. `iter.Pull2` is what makes the `(key, value, ok)` of each side available in one call so the comparison reads naturally. The matched-set test fixes the equal branch, the disjoint test exercises the two advance branches with no match ever firing, and the early-break test confirms both `iter.Pull2` cursors are released by their deferred `stop`s.

The traps are specific to merge-join. Advancing both sides on a `<` or `>` branch, rather than only the smaller, skips keys and drops valid matches — the disjoint and matched tests together pin one-sided advancement. Forgetting to advance *both* on a match re-compares the same key forever, an infinite loop the matched test would hang on. And the precondition is load-bearing: the inputs must be sorted by key, because the whole O(n + m) argument rests on each cursor moving monotonically forward; an unsorted input silently under-joins rather than erroring.

## Resources

- [`iter.Pull2`](https://pkg.go.dev/iter#Pull2) — the two-value pull conversion, with `next` returning `(K, V, bool)`.
- [`iter` package overview](https://pkg.go.dev/iter) — `Seq2`, push vs pull, and the relationship between `Pull` and `Pull2`.
- [Go spec: the range clause](https://go.dev/ref/spec#For_statements) — how range-over-func and `Seq2` are defined in the language.

---

Back to [02-lockstep-zip-and-merge.md](02-lockstep-zip-and-merge.md) | Next: [04-peekable-lookahead.md](04-peekable-lookahead.md)
