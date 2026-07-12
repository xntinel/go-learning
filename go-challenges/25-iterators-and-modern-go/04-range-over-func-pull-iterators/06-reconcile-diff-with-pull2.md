# Exercise 6: A Reconciliation Diff over Two Sorted Sources with iter.Pull2

A sync job that mirrors a remote source into a local store has to answer one question efficiently: what changed? Given the local state and the desired remote state, both keyed and sorted by ID, it must emit the minimal set of operations — add the keys that are new, remove the keys that are gone, and change the keys whose value differs — while skipping everything that already matches. This is a reconciliation diff, and it is a single lockstep walk of two sorted cursors with `iter.Pull2`, the same shape as a merge-join but classifying every key instead of only the matches.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
reconcile.go         Op, Diff, Source, Reconcile (iter.Pull2 over two Seq2 cursors)
cmd/
  demo/
    main.go          diff a local store against a desired remote state
reconcile_test.go    add-only, remove-only, change-only, mixed, both stops fire
```

- Files: `reconcile.go`, `cmd/demo/main.go`, `reconcile_test.go`.
- Implement: the `Op` enum with `String`, the `Diff` struct, `Source(keys []string, vals []int) iter.Seq2[string, int]`, and `Reconcile(local, remote iter.Seq2[string, int]) iter.Seq[Diff]`.
- Test: `reconcile_test.go` covers each diff case in isolation (add, remove, change), the mixed case with unchanged keys skipped, and that both producers are stopped on an early break.
- Verify: `go test -race ./...`

### Why reconciliation is a lockstep walk, and the three cases

Computing the difference between two keyed sets is the relational `full outer join` followed by a classification of each row. Done on sorted inputs it costs O(n + m) with no hashing and no buffering of either side — exactly the property that matters when local and remote are large and already arrive in key order (a database index scan, a sorted object listing, a Merkle-ordered range). The walk advances the cursor with the smaller key and classifies as it goes, which is why it needs two pull cursors that can be stepped independently: `iter.Pull2` gives each side a `next` returning `(key, value, ok)`, so the loop always holds the live front `(key, value)` of both local and remote.

The body is a three-way comparison on the two current keys, and each branch maps to one diff case:

- `lk < rk`: the local key sits before anything left on the remote side, so it exists locally but not remotely — a `Remove`. Emit it carrying the local value, advance local.
- `lk > rk`: symmetric — the remote key is new, an `Add`. Emit it carrying the remote value, advance remote.
- equal keys: the same ID is on both sides. Compare values; if they differ emit a `Change` carrying both old and new, otherwise emit nothing (unchanged keys are not reported). Either way advance *both* sides.

When one side runs out, the loop exits and the leftover side is drained: remaining local keys are all `Remove`s (gone from remote), remaining remote keys are all `Add`s (new since local). This drain is the part a plain inner-join merge skips; reconciliation must report the tails, because a key present only at the very end of one source is still a real add or remove. Both `iter.Pull2` cursors get a `defer stop()`, so a consumer that stops reading after the first diff unwinds both producers.

Create `reconcile.go`:

```go
package reconcile

import "iter"

// Op classifies one reconciliation difference.
type Op int

const (
	Add Op = iota
	Remove
	Change
)

// String renders an Op as a lowercase verb for diagnostics.
func (o Op) String() string {
	switch o {
	case Add:
		return "add"
	case Remove:
		return "remove"
	case Change:
		return "change"
	}
	return "unknown"
}

// Diff is one operation needed to make local match remote. Old is the local
// value (set for Remove and Change); New is the remote value (set for Add and
// Change). The unused side is the zero value.
type Diff struct {
	Op  Op
	Key string
	Old int
	New int
}

// Source returns a key/value push iterator over parallel keys and vals slices.
// The keys are expected to be sorted ascending for use with Reconcile.
func Source(keys []string, vals []int) iter.Seq2[string, int] {
	return func(yield func(string, int) bool) {
		for i := range keys {
			if !yield(keys[i], vals[i]) {
				return
			}
		}
	}
}

// Reconcile walks local and remote — both key/value sequences sorted by key
// ascending — in lockstep and yields the minimal set of Diffs that would turn
// local into remote: Remove for keys only in local, Add for keys only in remote,
// Change for shared keys whose value differs. Shared keys with equal values are
// skipped. Both pull cursors are released with a deferred stop.
func Reconcile(local, remote iter.Seq2[string, int]) iter.Seq[Diff] {
	return func(yield func(Diff) bool) {
		nextL, stopL := iter.Pull2(local)
		defer stopL()
		nextR, stopR := iter.Pull2(remote)
		defer stopR()

		lk, lv, okL := nextL()
		rk, rv, okR := nextR()
		for okL && okR {
			switch {
			case lk < rk:
				if !yield(Diff{Op: Remove, Key: lk, Old: lv}) {
					return
				}
				lk, lv, okL = nextL()
			case lk > rk:
				if !yield(Diff{Op: Add, Key: rk, New: rv}) {
					return
				}
				rk, rv, okR = nextR()
			default:
				if lv != rv {
					if !yield(Diff{Op: Change, Key: lk, Old: lv, New: rv}) {
						return
					}
				}
				lk, lv, okL = nextL()
				rk, rv, okR = nextR()
			}
		}
		for okL {
			if !yield(Diff{Op: Remove, Key: lk, Old: lv}) {
				return
			}
			lk, lv, okL = nextL()
		}
		for okR {
			if !yield(Diff{Op: Add, Key: rk, New: rv}) {
				return
			}
			rk, rv, okR = nextR()
		}
	}
}
```

The result is an ordinary push iterator (`iter.Seq[Diff]`), so a sync driver consumes it with `for d := range Reconcile(local, remote)` and applies each operation. The two pull cursors and the three-way classification stay internal. The precondition is the load-bearing one for any merge diff: both sources must be sorted by key ascending. Out-of-order input does not panic — it produces a wrong diff (spurious adds and removes), the same way feeding an unsorted side to a merge-join silently under-joins.

### The runnable demo

The demo diffs a local store against a desired remote state. Key `a` is unchanged and skipped; `b` has a new value (`change`); `c` exists only locally (`remove`); `d` exists only remotely (`add`); `e` is unchanged and skipped. The output is the three operations a sync job would apply, in key order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reconcile"
)

func main() {
	local := reconcile.Source([]string{"a", "b", "c", "e"}, []int{1, 2, 3, 5})
	remote := reconcile.Source([]string{"a", "b", "d", "e"}, []int{1, 20, 4, 5})

	for d := range reconcile.Reconcile(local, remote) {
		switch d.Op {
		case reconcile.Change:
			fmt.Printf("%s %s: %d -> %d\n", d.Op, d.Key, d.Old, d.New)
		case reconcile.Remove:
			fmt.Printf("%s %s: %d\n", d.Op, d.Key, d.Old)
		case reconcile.Add:
			fmt.Printf("%s %s: %d\n", d.Op, d.Key, d.New)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
change b: 2 -> 20
remove c: 3
add d: 4
```

### Tests

`TestReconcileAdd`, `TestReconcileRemove`, and `TestReconcileChange` each isolate one case so a branch cannot be confused with another: an add-only diff where remote has an extra trailing key, a remove-only diff where local does, and a change-only diff where the keys match but a value differs. `TestReconcileMixed` runs all three together and asserts unchanged keys produce nothing. `TestReconcileStopsEarly` breaks after the first diff and checks both `iter.Pull2` cursors are released by their deferred `stop`s using tracked producers.

Create `reconcile_test.go`:

```go
package reconcile

import (
	"iter"
	"reflect"
	"testing"
)

func collect(seq iter.Seq[Diff]) []Diff {
	out := []Diff{}
	for d := range seq {
		out = append(out, d)
	}
	return out
}

// trackedSource is Source that flips done in a deferred cleanup, so a test can
// see whether the producer was unwound by stop on an early break.
func trackedSource(keys []string, vals []int, done *bool) iter.Seq2[string, int] {
	return func(yield func(string, int) bool) {
		defer func() { *done = true }()
		for i := range keys {
			if !yield(keys[i], vals[i]) {
				return
			}
		}
	}
}

func TestReconcileAdd(t *testing.T) {
	t.Parallel()

	local := Source([]string{"a", "b"}, []int{1, 2})
	remote := Source([]string{"a", "b", "c"}, []int{1, 2, 3})
	got := collect(Reconcile(local, remote))
	want := []Diff{{Op: Add, Key: "c", New: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("add diff = %v, want %v", got, want)
	}
}

func TestReconcileRemove(t *testing.T) {
	t.Parallel()

	local := Source([]string{"a", "b", "c"}, []int{1, 2, 3})
	remote := Source([]string{"a", "b"}, []int{1, 2})
	got := collect(Reconcile(local, remote))
	want := []Diff{{Op: Remove, Key: "c", Old: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remove diff = %v, want %v", got, want)
	}
}

func TestReconcileChange(t *testing.T) {
	t.Parallel()

	local := Source([]string{"a", "b"}, []int{1, 2})
	remote := Source([]string{"a", "b"}, []int{1, 99})
	got := collect(Reconcile(local, remote))
	want := []Diff{{Op: Change, Key: "b", Old: 2, New: 99}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("change diff = %v, want %v", got, want)
	}
}

func TestReconcileMixed(t *testing.T) {
	t.Parallel()

	local := Source([]string{"a", "b", "c", "e"}, []int{1, 2, 3, 5})
	remote := Source([]string{"a", "b", "d", "e"}, []int{1, 20, 4, 5})
	got := collect(Reconcile(local, remote))
	want := []Diff{
		{Op: Change, Key: "b", Old: 2, New: 20},
		{Op: Remove, Key: "c", Old: 3},
		{Op: Add, Key: "d", New: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed diff = %v, want %v", got, want)
	}
}

func TestReconcileStopsEarly(t *testing.T) {
	t.Parallel()

	localDone, remoteDone := false, false
	local := trackedSource([]string{"a", "b", "c"}, []int{1, 2, 3}, &localDone)
	remote := trackedSource([]string{"x", "y", "z"}, []int{1, 2, 3}, &remoteDone)

	for range Reconcile(local, remote) {
		break
	}
	if !localDone || !remoteDone {
		t.Fatalf("early break: localDone=%v remoteDone=%v, want both true", localDone, remoteDone)
	}
}
```

## Review

The diff is correct when each key is classified exactly once and unchanged keys produce nothing. The three-way `switch` is the classifier: a key behind on the local side is a `Remove`, a key behind on the remote side is an `Add`, and an equal key is a `Change` only when the values differ. The two drain loops after the main loop are not optional — they report the tail of whichever source outlived the other, which is where a trailing add or remove hides. The three single-case tests pin each branch in isolation so they cannot be transposed, and the mixed test confirms equal-value keys are skipped.

The traps are the merge-walk traps plus the classification. Advancing both cursors on a `<` or `>` branch instead of only the smaller skips keys and drops diffs. Forgetting the drain loops silently omits every operation past the point where one side ends — a sync that stops halfway. Emitting a `Change` for equal values floods the consumer with no-op writes, which the mixed test catches by requiring `a` and `e` to vanish. And both `iter.Pull2` cursors must be released by deferred `stop`; the early-break test fails if either producer is left running after the consumer stops reading.

## Resources

- [`iter.Pull2`](https://pkg.go.dev/iter#Pull2) — the two-value pull conversion whose `next` returns `(K, V, bool)` for each side.
- [`iter` package overview](https://pkg.go.dev/iter) — `Seq2`, push vs pull, and how `Pull2` relates to `Pull`.
- [Go spec: the range clause](https://go.dev/ref/spec#For_statements) — how range-over-func and `Seq2` are defined in the language.

---

Back to [05-k-way-merge-with-heap.md](05-k-way-merge-with-heap.md) | Next: [../05-designing-iterator-apis/00-concepts.md](../05-designing-iterator-apis/00-concepts.md)
