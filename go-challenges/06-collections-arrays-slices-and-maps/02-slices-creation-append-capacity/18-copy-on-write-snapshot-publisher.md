# Exercise 18: A Copy-on-Write Config Publisher Over atomic.Pointer

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A config or rule set that many goroutines read concurrently and one control
loop occasionally updates — an access-control rule list, a feature-flag set,
a routing table — is a textbook case for the copy-on-write pattern:
`sync/atomic`'s `atomic.Pointer[T]` holds the current immutable snapshot,
readers `Load` it without ever taking a lock, and a writer builds an
entirely new snapshot before swapping the pointer in with a single atomic
store. The pattern only works if the writer genuinely builds a *new*
backing array every time. Reach for a plain `append` on the slice a reader
might already be holding, and you reintroduce exactly the shared-mutable-
state problem `atomic.Pointer` was supposed to eliminate — silently, and
in a way that only a concurrent test with the race detector will catch.

This module builds the publisher as a package you can drop into a service:
`NewPublisher` validates every rule it is seeded with and returns an error,
`Publish` clones before it appends and reports a rejected rule instead of
silently accepting one with no name, and `Snapshot` hands back an immutable
view a reader can hold forever. The naive in-place append that corrupts a
sibling snapshot is not part of that API. It lives in the test file, where
it belongs, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
rulepublisher/                module example.com/rulepublisher
  go.mod                       go 1.24
  rulepublisher.go               Rule, ErrEmptyRuleName, Publisher (atomic.Pointer[[]Rule]);
                                 NewPublisher, Snapshot, Publish
  rulepublisher_test.go          copy-on-publish, validation, torn-slice contrast, concurrent
                                 readers vs writer under -race, ExamplePublisher_Publish
```

- Files: `rulepublisher.go`, `rulepublisher_test.go`.
- Implement: `Publisher` holding an `atomic.Pointer[[]Rule]`; `NewPublisher(initial []Rule) (*Publisher, error)` storing a defensive copy of `initial` and rejecting any rule with an empty `Name` via `ErrEmptyRuleName`; `Snapshot() []Rule` doing a plain `Load`; `Publish(rule Rule) error` rejecting an empty `Name`, then cloning the current snapshot into a freshly sized array, appending `rule` to the clone, and committing with a `CompareAndSwap` retry loop.
- Test: a snapshot held before a `Publish` call is unchanged after it; `NewPublisher` copies rather than aliases its input; both constructor and `Publish` reject an empty rule name; a deterministic, non-concurrent test using an unexported naive in-place `append` helper that proves two "publishes" sharing one stale backing array corrupt each other's result (the torn-slice bug this design avoids); a `-race`-clean concurrency test with many reader goroutines looping on `Snapshot` while one writer goroutine calls `Publish` repeatedly; and `ExamplePublisher_Publish` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/18-copy-on-write-snapshot-publisher
cd go-solutions/06-collections-arrays-slices-and-maps/02-slices-creation-append-capacity/18-copy-on-write-snapshot-publisher
go mod edit -go=1.24
```

### Why append-in-place lets a reader observe a torn slice

`atomic.Pointer[T].Load` and `.Store`/`.CompareAndSwap` are atomic at the
level of the *pointer* — swapping which `*[]Rule` the field points at is a
single indivisible operation, and any reader that `Load`s gets either the
old pointer or the new one, never something in between. That guarantee says
nothing about the slice the pointer points *at*. If `Publish` took the
current snapshot and appended to it in place — `next := append(current,
rule)` — the append can find spare capacity in the current snapshot's
backing array (slices often have spare capacity after growth) and write
`rule` directly into it, *before* any pointer swap happens. Now imagine two
`Publish` calls both start from the same `current` snapshot, both find that
same spare slot, and both write their own rule into it: whichever write
lands second silently overwrites the first. A reader that had already
`Load`ed the *old* pointer and is mid-iteration over its rules is now
looking at a slice whose backing array is being mutated underneath it by a
goroutine it has no relationship to — a data race, and a "torn" read where
the values it sees depend on exactly how the writer's timing interleaved
with its own.

The fix is that `Publish` must build the *entire* new slice — clone plus
append — before it ever calls `CompareAndSwap`. `make([]Rule, len(old),
len(old)+1)` followed by `copy` and `append` guarantees the clone has
exactly the capacity it needs, so the `append` inside `Publish` can never
alias the old snapshot's backing array; it always allocates its own. Only
after that fresh array is fully built does the atomic swap make it visible.
A reader's `Snapshot()` therefore always returns a slice that is either
entirely the old state or entirely the new state, and once returned, that
slice is never mutated again by anyone — which is exactly the property that
lets readers use it without a lock.

Create `rulepublisher.go`:

```go
// Package rulepublisher implements the copy-on-write pattern over
// sync/atomic.Pointer: many readers Load an immutable snapshot without ever
// taking a lock, and a single writer publishes a new snapshot by cloning the
// current one, appending to the clone, and swapping the pointer in with a
// single atomic store.
package rulepublisher

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrEmptyRuleName means a Rule with an empty Name was passed to NewPublisher
// or Publish. A rule with no name cannot be looked up or logged usefully, so
// the publisher refuses to store one.
var ErrEmptyRuleName = errors.New("rulepublisher: rule name must not be empty")

// Rule is one routing or access-control rule held by the publisher.
type Rule struct {
	Name     string
	Priority int
}

// Publisher holds an immutable, atomically swapped snapshot of the current
// rule set behind an atomic.Pointer. Readers call Snapshot to get an
// immutable []Rule view; writers call Publish, which clones the current
// snapshot, appends to the clone, and swaps the pointer -- so no reader ever
// observes a slice that is still being mutated.
//
// Publisher is safe for concurrent use by multiple goroutines: any number of
// readers may call Snapshot concurrently with any number of writers calling
// Publish.
type Publisher struct {
	snap atomic.Pointer[[]Rule]
}

// NewPublisher returns a Publisher initialized with a private copy of
// initial, so later mutation of the caller's slice cannot affect the
// publisher's state. It returns ErrEmptyRuleName if any rule in initial has
// an empty Name.
func NewPublisher(initial []Rule) (*Publisher, error) {
	for i, r := range initial {
		if r.Name == "" {
			return nil, fmt.Errorf("%w: rule at index %d", ErrEmptyRuleName, i)
		}
	}
	p := &Publisher{}
	cp := append([]Rule(nil), initial...)
	p.snap.Store(&cp)
	return p, nil
}

// Snapshot returns the current rule set. The Publisher never mutates a
// slice once it has been published -- every Publish builds an entirely new
// backing array -- so callers may read the result, range over it, or hold
// onto it indefinitely without any synchronization of their own.
func (p *Publisher) Snapshot() []Rule {
	sp := p.snap.Load()
	if sp == nil {
		return nil
	}
	return *sp
}

// Publish adds rule to the rule set. It clones the current snapshot into a
// fresh, exactly-sized backing array, appends rule to the clone, and swaps
// the pointer with a compare-and-swap retry loop so concurrent Publish
// calls never lose an update. Because the clone is built entirely before
// the swap, no reader can ever observe a partially constructed slice: a
// reader's Snapshot call returns either the complete state from before this
// Publish or the complete state from after it, never something in between.
// Publish returns ErrEmptyRuleName without publishing anything if rule.Name
// is empty.
func (p *Publisher) Publish(rule Rule) error {
	if rule.Name == "" {
		return ErrEmptyRuleName
	}
	for {
		old := p.snap.Load()
		var oldRules []Rule
		if old != nil {
			oldRules = *old
		}
		next := make([]Rule, len(oldRules), len(oldRules)+1)
		copy(next, oldRules)
		next = append(next, rule)
		if p.snap.CompareAndSwap(old, &next) {
			return nil
		}
		// Another Publish committed first; retry against its result.
	}
}
```

### Using it

Construct a `Publisher` once at startup, optionally seeded with an initial
rule set, and share the pointer across every goroutine that needs to read or
update rules — that is what "safe for concurrent use" on the type's doc
comment promises, and what `TestConcurrentReadersWhileWriterPublishes` holds
it to. `NewPublisher` and `Publish` both reject a rule with an empty `Name`
via `ErrEmptyRuleName`, checkable with `errors.Is`, so a caller cannot
silently populate the rule set with unaddressable entries.

The aliasing contract on `Snapshot` is what makes the whole pattern useful:
a caller may hold the returned slice indefinitely, range over it, or pass it
to another goroutine, and it is guaranteed never to change underneath
them — `TestPublishDoesNotMutateHeldSnapshot` pins that directly. The module
has no `main.go`, because a config publisher is a library, not a tool. Its
executable demonstration is `ExamplePublisher_Publish`: `go test` runs it
and compares its standard output against the `// Output:` comment, so the
usage shown below cannot drift away from the code.

```go
func ExamplePublisher_Publish() {
	p, err := NewPublisher([]Rule{{Name: "deny-all", Priority: 0}})
	if err != nil {
		panic(err)
	}

	held := p.Snapshot()
	fmt.Printf("reader holds snapshot: %v\n", held)

	if err := p.Publish(Rule{Name: "allow-admin", Priority: 10}); err != nil {
		panic(err)
	}
	if err := p.Publish(Rule{Name: "allow-read", Priority: 5}); err != nil {
		panic(err)
	}

	fmt.Printf("held snapshot is still: %v\n", held)
	fmt.Printf("current snapshot is now: %v\n", p.Snapshot())

	// Output:
	// reader holds snapshot: [{deny-all 0}]
	// held snapshot is still: [{deny-all 0}]
	// current snapshot is now: [{deny-all 0} {allow-admin 10} {allow-read 5}]
}
```

The snapshot taken before either `Publish` call is byte-for-byte identical
after both of them run, because neither `Publish` ever touches the backing
array that snapshot points at — each one builds its own.

### Tests

`TestNewPublisherCopiesInitial` mutates the caller's slice after
construction and asserts the publisher is unaffected.
`TestNewPublisherRejectsEmptyRuleName` and `TestPublishRejectsEmptyRuleName`
cover the validation on both entry points.
`TestPublishGrowsAndIsolatesOldSnapshots` is a table over publish sequences,
asserting both the final state and that a snapshot taken before any publish
stays empty. `TestPublishDoesNotMutateHeldSnapshot` is the direct analogue
of the demo.

`TestNaiveAppendCorruptsSiblingSnapshot` is the deterministic proof of the
bug itself: it calls the unexported `naiveAppend` twice against the same
stale base slice — simulating two publishes racing on one shared backing
array — and shows the second call's write silently overwrites the first's,
with no goroutines needed to demonstrate it.
`TestConcurrentReadersWhileWriterPublishes` is the real concurrency proof:
20 reader goroutines loop on `Snapshot` and range over the result while one
writer goroutine calls `Publish` 200 times, and the whole thing must be
`-race`-clean.

Create `rulepublisher_test.go`:

```go
package rulepublisher

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestNewPublisherCopiesInitial(t *testing.T) {
	t.Parallel()

	initial := []Rule{{Name: "r0", Priority: 1}}
	p, err := NewPublisher(initial)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	initial[0].Name = "mutated-by-caller"

	got := p.Snapshot()
	if got[0].Name != "r0" {
		t.Fatalf("Snapshot()[0].Name = %q, want %q (NewPublisher must copy, not alias, initial)", got[0].Name, "r0")
	}
}

func TestNewPublisherRejectsEmptyRuleName(t *testing.T) {
	t.Parallel()

	_, err := NewPublisher([]Rule{{Name: "ok"}, {Name: ""}})
	if !errors.Is(err, ErrEmptyRuleName) {
		t.Fatalf("NewPublisher error = %v, want ErrEmptyRuleName", err)
	}
}

func TestPublishRejectsEmptyRuleName(t *testing.T) {
	t.Parallel()

	p, err := NewPublisher(nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := p.Publish(Rule{Name: ""}); !errors.Is(err, ErrEmptyRuleName) {
		t.Fatalf("Publish error = %v, want ErrEmptyRuleName", err)
	}
	if got := len(p.Snapshot()); got != 0 {
		t.Fatalf("Snapshot() length = %d after a rejected Publish, want 0", got)
	}
}

func TestPublishGrowsAndIsolatesOldSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		publish []Rule
		want    []Rule
	}{
		{
			name:    "single publish onto empty set",
			publish: []Rule{{Name: "a", Priority: 1}},
			want:    []Rule{{Name: "a", Priority: 1}},
		},
		{
			name:    "three sequential publishes preserve order",
			publish: []Rule{{Name: "a", Priority: 1}, {Name: "b", Priority: 2}, {Name: "c", Priority: 3}},
			want:    []Rule{{Name: "a", Priority: 1}, {Name: "b", Priority: 2}, {Name: "c", Priority: 3}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, err := NewPublisher(nil)
			if err != nil {
				t.Fatalf("NewPublisher: %v", err)
			}
			old := p.Snapshot()

			for _, r := range tc.publish {
				if err := p.Publish(r); err != nil {
					t.Fatalf("Publish(%+v): %v", r, err)
				}
			}

			if len(old) != 0 {
				t.Fatalf("snapshot held before any publish changed length to %d, want 0", len(old))
			}
			if got := p.Snapshot(); !slices.Equal(got, tc.want) {
				t.Errorf("final Snapshot() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPublishDoesNotMutateHeldSnapshot(t *testing.T) {
	t.Parallel()

	p, err := NewPublisher([]Rule{{Name: "r0", Priority: 0}})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	held := p.Snapshot()

	_ = p.Publish(Rule{Name: "r1", Priority: 1})
	_ = p.Publish(Rule{Name: "r2", Priority: 2})

	if !slices.Equal(held, []Rule{{Name: "r0", Priority: 0}}) {
		t.Fatalf("held snapshot changed to %v after later Publish calls, want it frozen at r0 only", held)
	}
	if want := []Rule{{Name: "r0", Priority: 0}, {Name: "r1", Priority: 1}, {Name: "r2", Priority: 2}}; !slices.Equal(p.Snapshot(), want) {
		t.Errorf("current Snapshot() = %v, want %v", p.Snapshot(), want)
	}
}

// naiveAppend is the bug this module guards against: it appends rule onto
// base in place, reusing base's backing array whenever base has spare
// capacity, instead of cloning first. It is unexported and lives only here,
// never in rulepublisher.go, so production code has no way to reach it --
// only the tests use it, to prove why Publisher.Publish must clone before it
// appends.
func naiveAppend(base []Rule, rule Rule) []Rule {
	return append(base, rule)
}

// TestNaiveAppendCorruptsSiblingSnapshot shows -- deterministically, with no
// goroutines -- exactly why Publish must clone before it appends. Two
// "publishes" both start from the same stale base slice, which has spare
// capacity, and append in place via naiveAppend instead of cloning first.
// Both writes land in the same backing-array slot, so the first append's
// result is silently overwritten by the second: a reader holding what
// looked like a complete, independent snapshot observes a torn slice.
func TestNaiveAppendCorruptsSiblingSnapshot(t *testing.T) {
	t.Parallel()

	base := make([]Rule, 2, 4) // len 2, spare capacity for 2 more
	base[0] = Rule{Name: "r0"}
	base[1] = Rule{Name: "r1"}

	snapA := naiveAppend(base, Rule{Name: "added-by-A"})
	snapB := naiveAppend(base, Rule{Name: "added-by-B"})

	if snapA[2] != (Rule{Name: "added-by-B"}) {
		t.Fatalf("snapA[2] = %+v, want %+v (snapB's in-place append should have overwritten it)", snapA[2], Rule{Name: "added-by-B"})
	}
	if snapB[2] != (Rule{Name: "added-by-B"}) {
		t.Fatalf("snapB[2] = %+v, want %+v", snapB[2], Rule{Name: "added-by-B"})
	}
}

// TestConcurrentReadersWhileWriterPublishes runs many reader goroutines
// that repeatedly call Snapshot and range over the result while a single
// writer goroutine publishes new rules, and must be race-free: readers only
// ever see a complete snapshot because Publish never mutates a slice that
// has already been stored.
func TestConcurrentReadersWhileWriterPublishes(t *testing.T) {
	t.Parallel()

	p, err := NewPublisher([]Rule{{Name: "seed", Priority: 0}})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	const readers = 20
	const publishes = 200

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, r := range p.Snapshot() {
						_ = r.Name
					}
				}
			}
		}()
	}

	for i := 0; i < publishes; i++ {
		if err := p.Publish(Rule{Name: "generated", Priority: i}); err != nil {
			t.Errorf("Publish: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if got := len(p.Snapshot()); got != 1+publishes {
		t.Fatalf("final snapshot length = %d, want %d", got, 1+publishes)
	}
}

// ExamplePublisher_Publish is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below.
func ExamplePublisher_Publish() {
	p, err := NewPublisher([]Rule{{Name: "deny-all", Priority: 0}})
	if err != nil {
		panic(err)
	}

	held := p.Snapshot()
	fmt.Printf("reader holds snapshot: %v\n", held)

	if err := p.Publish(Rule{Name: "allow-admin", Priority: 10}); err != nil {
		panic(err)
	}
	if err := p.Publish(Rule{Name: "allow-read", Priority: 5}); err != nil {
		panic(err)
	}

	fmt.Printf("held snapshot is still: %v\n", held)
	fmt.Printf("current snapshot is now: %v\n", p.Snapshot())

	// Output:
	// reader holds snapshot: [{deny-all 0}]
	// held snapshot is still: [{deny-all 0}]
	// current snapshot is now: [{deny-all 0} {allow-admin 10} {allow-read 5}]
}
```

## Review

`Publish` is correct when a snapshot handed to a reader is provably frozen
for the rest of that reader's use of it, no matter what the writer does
afterward — `TestPublishDoesNotMutateHeldSnapshot` and the sequential table
test both pin that directly. `NewPublisher` and `Publish` both reject a rule
with an empty `Name` via `ErrEmptyRuleName`, checkable with `errors.Is`, so a
misconfigured caller fails fast instead of publishing an unaddressable rule.
`TestNaiveAppendCorruptsSiblingSnapshot` earns its place by making the bug
concrete without needing a flaky, timing-dependent goroutine race to
demonstrate it: two sequential calls to a naive, non-cloning append are
enough to show one overwriting the other, because that is the exact
mechanism a real concurrent race would trigger. The `atomic.Pointer` swap
itself was never the risky part of this design — it is correct by
construction — the risk was always whether `Publish` builds a genuinely
independent array before that swap runs, and
`TestConcurrentReadersWhileWriterPublishes` is what proves it does, under
`-race`, with real goroutines. Run `go test -count=1 -race ./...` to
confirm; this module's whole point evaporates without the race detector
enabled.

## Resources

- [`sync/atomic` package — `atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — the atomically swapped pointer this publisher is built on.
- [Go memory model](https://go.dev/ref/mem) — what atomic Store/Load/CompareAndSwap actually guarantee about visibility.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — why append can alias an existing backing array.
- [Race Detector](https://go.dev/doc/articles/race_detector) — the tool that catches the concurrent version of this bug.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-flat-arena-backed-matrix-rows.md](17-flat-arena-backed-matrix-rows.md) | Next: [19-adaptive-flush-byte-budget-batcher.md](19-adaptive-flush-byte-budget-batcher.md)
