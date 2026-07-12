# Exercise 17: Exponential Seek Over a Paginated Log With sort.Find

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

`git bisect` does not know how many commits lie between the last known-good
one and HEAD, yet it finds the offending commit in a logarithmic number of
steps. The same shape of problem shows up whenever a backend needs to jump
into the middle of a cursor-paginated API -- an audit-log endpoint, a
commit-history API, an LSM-tree's on-disk run during a merge -- to find the
first record at or after some target sequence number. The data is sorted,
but there is no `len()` to hand to a binary search: the only operation
available is "fetch position i and tell me if it exists", and every call
costs a network round trip or a page read a caller wants to minimize.

Paging forward one record at a time from the start always works and is
always wrong for this problem: it costs `O(n)` calls where `n` is the target's
position, exactly the cost a binary search exists to avoid, except a binary
search cannot start until it knows the far end of its search space. The fix
is exponential (or "galloping") search: probe at position 1, 2, 4, 8, 16 --
doubling the stride -- until the probe overshoots the target or falls off the
known data, then binary-search the small window that doubling just bounded.
The total cost is `O(log n)`: the doubling phase and the binary-search phase
are each logarithmic, and neither one ever has to know `n` in advance.

This module builds `Seek`, a function over a `Source` interface -- the shape
any paginated store can implement -- using `sort.Find` for the binary-search
half, which is the one scenario in this lesson where `sort.Find` earns its
place over `slices.BinarySearch`: the data here is not a plain slice.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
logseek/                 module example.com/logseek
  go.mod                 go 1.24
  logseek.go             Source, Seek
  logseek_test.go        seek table, empty/single/nil sources, the
                         linear-walk contrast, probe-count property,
                         ExampleSeek
```

- Files: `logseek.go`, `logseek_test.go`.
- Implement: `type Source interface { At(i int) (seq int64, ok bool) }`, where `ok` is true for a known, strictly increasing prefix of positions and false forever after; `func Seek(src Source, target int64) (int, bool)`, returning the position of the first record at or after `target` and whether it is an exact match, using an exponentially growing probe to bound the search window and `sort.Find` to binary-search within it.
- Test: an exact match, a miss between two records, a target before the first record, a target exactly on a power-of-two-indexed record, a target straddling a power-of-two probe boundary, a target past the last known record, an empty source, a single-record source, a nil `Source`, the linear-walk contrast, the probe-count property over a hundred-thousand-record source, and `ExampleSeek` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/17-paginated-log-exponential-seek
cd go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/17-paginated-log-exponential-seek
go mod edit -go=1.24
```

### Doubling first, then sort.Find -- and why missing means "already past"

The doubling phase is the part that makes `sort.Find` legal to call at all:
`sort.Find(n, cmp)` needs a fixed `n` up front, and `Source` never hands you
one. So `Seek` probes `At(1)`, `At(2)`, `At(4)`, ... , doubling the stride
each time, until a probe's sequence number reaches `target` or the probe
falls off the known data (`ok=false`). Whichever stops the loop, the previous
successful probe (`lo`) is a confirmed lower bound still strictly before
`target`, and the stopping probe (`hi`) is a confirmed upper bound -- so the
answer lives in the half-open window `(lo, hi]`, a window of size at most
`lo`, which `sort.Find` can now binary-search in `O(log(hi-lo))` calls.

The one subtlety worth sitting with is what a missing position *means* inside
that window's comparator. `sort.Find`'s `cmp(i)` must be positive over a
leading prefix and `<= 0` from some point on -- the opposite sign convention
from `slices.BinarySearchFunc`, and worth double-checking every time you
reach for it. A record that exists with a sequence number below `target` is
part of that positive prefix. A *missing* position, though, is not "still
before the target" -- it is the end of the log, which is logically *at least
as far along* as any target could be, so it belongs on the `<= 0` side, not
the positive side:

```go
seq, ok := src.At(lo + 1 + i)
if !ok {
    return -1 // the log ends here: treat it as already past target
}
```

Getting that sign backwards (returning `+1` for a missing position, treating
it as "not there yet") breaks monotonicity in exactly the case that matters
most: a target beyond every record the source has. `sort.Find` would then
report the doubling phase's raw upper bound `hi` -- a power-of-two-ish
overshoot -- instead of the true one-past-the-end position, silently
returning a wrong "insertion point" for a target that does not exist.

Create `logseek.go`:

```go
// Package logseek finds the first record at or after a target sequence
// number in a cursor-paginated log without knowing the total record count
// and without walking every page from the start.
//
// This is the technique behind git bisect and behind how LSM-tree merging
// iterators skip forward within a run: probe with an exponentially growing
// stride until the probe overshoots the target or runs off the known data,
// then binary-search the small window the probe just bounded.
package logseek

import "sort"

// Source is a sequence of ascending, unique sequence numbers addressed by a
// 0-based position, typically backed by a cursor-paginated API: an audit
// log, a commit history, or one run of an LSM-tree's merge. At(i) returns
// the sequence number at position i and true if position i exists.
//
// Once At returns ok=false for some i, it must return ok=false for every
// j > i as well -- a Source has no holes, only a known prefix followed by an
// unknown tail. The known prefix must be strictly increasing; Seek's result
// is meaningless over a Source that violates either property.
type Source interface {
	At(i int) (seq int64, ok bool)
}

// Seek returns the position of the first record whose sequence number is
// greater than or equal to target, and whether that record's sequence
// number exactly equals target. If every known record precedes target, Seek
// returns the position one past the last known record and false -- the same
// "not found, here is where it would go" signal sort.Search gives over a
// plain slice.
//
// Seek probes src with an exponentially growing stride to establish an
// upper bound on the search window without knowing len(src) in advance,
// then binary-searches within that window with sort.Find. For a target at
// position n, this costs O(log n) calls to At, against the O(n) calls a
// naive page-by-page walk from the start would need.
//
// Seek holds no state and is safe to call concurrently, provided the Source
// it is given is either read-only or synchronizes its own access -- Seek
// does not synchronize on the caller's behalf.
func Seek(src Source, target int64) (int, bool) {
	if src == nil {
		return 0, false
	}
	seq0, ok0 := src.At(0)
	if !ok0 {
		return 0, false
	}
	if seq0 >= target {
		return 0, seq0 == target
	}

	// Exponential probe: double the stride until At(hi) either falls off the
	// known prefix or reaches target, keeping lo as the largest confirmed
	// position still strictly before target.
	lo, hi := 0, 1
	for {
		seq, ok := src.At(hi)
		if !ok || seq >= target {
			break
		}
		lo = hi
		hi *= 2
	}

	// Binary search the window (lo, hi] with sort.Find. A missing position
	// within the window means the log ends there: treat it as if its
	// sequence number were infinitely large, i.e. already past target, so
	// the search converges on the first known position >= target, or on the
	// first missing position if target lies beyond every known record.
	n := hi - lo
	pos, found := sort.Find(n, func(i int) int {
		seq, ok := src.At(lo + 1 + i)
		if !ok {
			return -1
		}
		switch {
		case seq < target:
			return 1
		case seq > target:
			return -1
		default:
			return 0
		}
	})
	return lo + 1 + pos, found
}
```

### Using it

`Seek` has nothing to construct: it takes a `Source` and a `target` per call
and keeps no state of its own between calls, so there is no `New` -- the
configuration a caller would validate lives entirely in whatever
`Source` implementation they hand it, not in this package. Adapting your own
store to `Source` means writing one method, `At(i int) (int64, bool)`, over
whatever pagination your API already exposes; `Seek` never needs to know
`len(src)`.

`Seek` returns two plain values, not a slice, so there is no aliasing
contract to document. The concurrency contract is about what it does *not*
do: `Seek` holds no state, so calling it from many goroutines at once is safe
exactly to the extent the `Source` implementation it is given is safe -- a
`Source` backed by an HTTP client with its own connection pool is typically
fine, one backed by an unsynchronized in-memory cache is not, and `Seek`
does not paper over that either way.

A realistic `Source` over, say, a paginated audit-log endpoint would fetch
one page per call to `At`, cache it, and translate the requested position
into a page number and an offset within that page -- exactly the kind of
adapter this package expects a caller to write, and deliberately not
something `Seek` provides itself. Keeping that adapter outside the package
is what lets `Seek` stay a pure function with no HTTP client, no retry
policy, and no page-size configuration to get wrong: those decisions belong
to whoever owns the paginated store, not to the search algorithm sitting on
top of it.

`ExampleSeek` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below.

```go
func ExampleSeek() {
	// A cursor-paginated audit log; sequence numbers are not contiguous
	// because some records were filtered out upstream.
	log := sliceSource{100, 104, 108, 115, 130, 131, 150}

	pos, found := Seek(log, 115)
	fmt.Printf("target 115: pos=%d found=%v\n", pos, found)

	pos, found = Seek(log, 110)
	fmt.Printf("target 110: pos=%d found=%v\n", pos, found)

	pos, found = Seek(log, 999)
	fmt.Printf("target 999: pos=%d found=%v\n", pos, found)

	// Output:
	// target 115: pos=3 found=true
	// target 110: pos=3 found=false
	// target 999: pos=7 found=false
}
```

### Tests

`TestSeek` is the table, run over a 200-record source: an exact match at the
first record, an exact match in the middle, a miss that falls between two
records, a target landing precisely on a power-of-two index (64, where a
probe boundary sits) and one just past it, the last record, and a target
beyond every known record. `TestSeekEmptyAndSingleRecordSource` and
`TestSeekNilSource` cover the sources with nothing to search at all.

`TestExponentialProbeCallsFewerTimesThanLinearWalk` is the heart of the
module. `seekLinear` is unexported and unreachable from the package API: it
is the page-by-page walk from the start, correct but linear. The test wraps
the same hundred-thousand-record source in a call-counting `Source` for each
strategy, confirms both agree on the answer, and then asserts a property --
`Seek` needs strictly fewer calls to `At` -- never an exact count, since the
precise number of doubling steps is an implementation detail of the probe,
not a documented contract.

Create `logseek_test.go`:

```go
package logseek

import (
	"fmt"
	"testing"
)

// sliceSource is a Source backed by a plain, strictly increasing slice. It
// is unexported: callers of the package are expected to adapt their own
// paginated store to Source, and this type exists only to drive the tests
// and the Example against something concrete.
type sliceSource []int64

func (s sliceSource) At(i int) (int64, bool) {
	if i < 0 || i >= len(s) {
		return 0, false
	}
	return s[i], true
}

// countingSource wraps a Source and counts every call to At, so a test can
// compare how many probes two strategies need without asserting an exact
// number tied to one input size.
type countingSource struct {
	src   Source
	calls int
}

func (c *countingSource) At(i int) (int64, bool) {
	c.calls++
	return c.src.At(i)
}

func TestSeek(t *testing.T) {
	t.Parallel()

	// 200 records, sequence numbers 0, 10, 20, ..., 1990.
	data := make(sliceSource, 200)
	for i := range data {
		data[i] = int64(i * 10)
	}

	tests := []struct {
		name      string
		target    int64
		wantPos   int
		wantFound bool
	}{
		{name: "target before first record", target: -5, wantPos: 0, wantFound: false},
		{name: "target equals first record", target: 0, wantPos: 0, wantFound: true},
		{name: "target equals a middle record", target: 200, wantPos: 20, wantFound: true},
		{name: "target between two records", target: 205, wantPos: 21, wantFound: false},
		{name: "target lands exactly on a power-of-two index", target: 640, wantPos: 64, wantFound: true},
		{name: "target straddles a power-of-two probe boundary", target: 645, wantPos: 65, wantFound: false},
		{name: "target equals the last record", target: 1990, wantPos: 199, wantFound: true},
		{name: "target beyond the last record", target: 100000, wantPos: 200, wantFound: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pos, found := Seek(data, tc.target)
			if pos != tc.wantPos || found != tc.wantFound {
				t.Fatalf("Seek(%d) = (%d, %v), want (%d, %v)", tc.target, pos, found, tc.wantPos, tc.wantFound)
			}
		})
	}
}

func TestSeekEmptyAndSingleRecordSource(t *testing.T) {
	t.Parallel()

	if pos, found := Seek(sliceSource{}, 5); pos != 0 || found {
		t.Fatalf("Seek(empty, 5) = (%d, %v), want (0, false)", pos, found)
	}

	single := sliceSource{42}
	if pos, found := Seek(single, 42); pos != 0 || !found {
		t.Fatalf("Seek(single, 42) = (%d, %v), want (0, true)", pos, found)
	}
	if pos, found := Seek(single, 100); pos != 1 || found {
		t.Fatalf("Seek(single, 100) = (%d, %v), want (1, false)", pos, found)
	}
}

func TestSeekNilSource(t *testing.T) {
	t.Parallel()

	if pos, found := Seek(nil, 5); pos != 0 || found {
		t.Fatalf("Seek(nil, 5) = (%d, %v), want (0, false)", pos, found)
	}
}

// seekLinear is the antipattern this module contrasts, kept unexported and
// unreachable from the package API: it pages forward one position at a time
// from the start until it finds the target or falls off the known data. It
// is correct -- every value it returns agrees with Seek -- but it costs
// O(n) calls to At instead of O(log n).
func seekLinear(src Source, target int64) (int, bool) {
	for i := 0; ; i++ {
		seq, ok := src.At(i)
		if !ok {
			return i, false
		}
		if seq >= target {
			return i, seq == target
		}
	}
}

// TestExponentialProbeCallsFewerTimesThanLinearWalk is the heart of the
// module. It runs the same seek, over the same large source, through Seek
// and through seekLinear, each behind its own call-counting wrapper, and
// asserts a property -- exponential probing needs strictly fewer calls to
// At -- rather than an exact count tied to this one input size.
func TestExponentialProbeCallsFewerTimesThanLinearWalk(t *testing.T) {
	t.Parallel()

	data := make(sliceSource, 100000)
	for i := range data {
		data[i] = int64(i)
	}
	const target = int64(99000)

	expCounter := &countingSource{src: data}
	expPos, expFound := Seek(expCounter, target)

	linCounter := &countingSource{src: data}
	linPos, linFound := seekLinear(linCounter, target)

	if expPos != linPos || expFound != linFound {
		t.Fatalf("Seek = (%d, %v), seekLinear = (%d, %v); results disagree",
			expPos, expFound, linPos, linFound)
	}
	if !(expCounter.calls < linCounter.calls) {
		t.Fatalf("At calls: Seek = %d, seekLinear = %d; want Seek strictly fewer",
			expCounter.calls, linCounter.calls)
	}
}

// ExampleSeek is the runnable demonstration of this module: go test executes
// it and compares its stdout against the Output comment below.
func ExampleSeek() {
	// A cursor-paginated audit log; sequence numbers are not contiguous
	// because some records were filtered out upstream.
	log := sliceSource{100, 104, 108, 115, 130, 131, 150}

	pos, found := Seek(log, 115)
	fmt.Printf("target 115: pos=%d found=%v\n", pos, found)

	pos, found = Seek(log, 110)
	fmt.Printf("target 110: pos=%d found=%v\n", pos, found)

	pos, found = Seek(log, 999)
	fmt.Printf("target 999: pos=%d found=%v\n", pos, found)

	// Output:
	// target 115: pos=3 found=true
	// target 110: pos=3 found=false
	// target 999: pos=7 found=false
}
```

## Review

`Seek` is correct when it returns the same `(pos, found)` a linear walk would
have found, for every target -- inside the data, between two records, at the
very first or last record, and past the end -- while making a logarithmic
number of calls to `At` to get there. The mechanism worth internalizing is
the two-phase shape: doubling to bound a window without knowing the source's
length, then `sort.Find` to binary-search within it, remembering that its
`cmp` convention is the mirror image of `slices.BinarySearchFunc`'s. The trap
this module isolates is what a missing position means inside that window's
comparator -- treating it as "already past the target" rather than "not
there yet" is what makes a target beyond every known record resolve to the
true end of the log instead of to the doubling phase's raw, oversized probe
bound. `Seek` holds no state of its own, so its concurrency safety is
entirely a function of the `Source` it is given. `ExampleSeek` is the
executable documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`sort.Find`](https://pkg.go.dev/sort#Find) — the closure-based binary search this module's window phase uses, and its cmp sign convention.
- [`sort.Search`](https://pkg.go.dev/sort#Search) — the underlying monotone-predicate primitive `Find` is built on.
- [Exponential search (galloping search)](https://en.wikipedia.org/wiki/Exponential_search) — the doubling-probe technique this module implements.
- [`git bisect`](https://git-scm.com/docs/git-bisect) — a widely used instance of the same "find the boundary without knowing the range" problem.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-wal-tail-reconciliation-compact.md](16-wal-tail-reconciliation-compact.md) | Next: [18-sorted-wordlist-prefix-lookup.md](18-sorted-wordlist-prefix-lookup.md)
