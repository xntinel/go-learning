# Exercise 11: Splice Raft Log Conflicts With slices.Replace

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every Raft-style consensus implementation -- etcd's raft package, CockroachDB's
range replication, any hand-rolled leader-election log -- has the same moment of
truth: the leader's `AppendEntries` RPC tells a follower that its log conflicts
with the leader's starting at some index, and the follower must discard its own
tail from that point on and splice the leader's entries into place. The same
operation, generalized, is how a committed prefix later gets collapsed into a
single snapshot placeholder during log compaction. Both are the same primitive:
replace a range of a slice with a different-length replacement, in place, without
corrupting anything else that shares the log's backing array.

`slices.Replace(s, i, j, v...)` is that primitive, built into the standard
library and tested against exactly this class of bug. The trap it replaces is a
hand-assembled `append(s[:i], append(v, s[j:]...)...)` splice, the kind of
expression that looks like a clever one-liner and reads correctly when you trace
through the values it returns. What it does not protect against is what happens
to anything else in the program that still holds a slice over the same backing
array: the outer `append` writes through that shared memory in place whenever
`s[:i]` has spare capacity, and a Raft log that has been truncated and re-grown a
few times almost always does. The bug is silent -- no panic, no wrong return
value from the splice itself -- and it shows up as a correctness incident in
whatever unrelated code was still holding that alias.

This module builds `raftlog`, the follower-side log storage: conflict
truncate-and-append, range compaction into a snapshot marker, and a read-only
`Entries` accessor. Both mutations go through `slices.Replace` on the log's own
internal slice, and `Entries` never hands out a reference to that slice -- it
returns a copy. The hand-rolled splice trap is never callable from this API; it
exists only in the test file, where it corrupts a snapshot on purpose so the
difference is provable rather than asserted.

This module is fully self-contained: its own `go mod init`, a reusable package,
and its tests. Nothing here imports another exercise.

## What you'll build

```text
raftlog/                  module example.com/raftlog
  go.mod                  go 1.24
  raftlog.go              Entry, Log; NewLog, ConflictTruncateAppend, CompactRange, Entries, Len
  raftlog_test.go         truncate/append table, compact table, aliasing, the splice-corruption
                          contrast, ExampleLog_ConflictTruncateAppend
```

- Files: `raftlog.go`, `raftlog_test.go`.
- Implement: `NewLog() *Log`; `(*Log).ConflictTruncateAppend(from int, leaderEntries []Entry) error` discarding every entry at or after the 1-based index `from` and splicing `leaderEntries` into place with `slices.Replace`, rejecting `from` outside `[1, Len()+1]` with `ErrConflictIndexOutOfRange`; `(*Log).CompactRange(i, j int, marker Entry) error` collapsing the half-open 1-based range `[i, j)` into the single `marker` entry with `slices.Replace`, rejecting an empty or out-of-range request with `ErrCompactRangeInvalid`; `(*Log).Entries() []Entry` returning a clone; `(*Log).Len() int`.
- Test: the truncate/append table (plain append, whole-log replace, tail truncation, empty log, empty `leaderEntries`, both out-of-range `from` cases); the compact-range table (middle range, single entry, entire log, empty range, out-of-range `i`/`j`); `Entries` never aliasing the log in either direction; the naive double-append splice silently corrupting a previously captured alias, contrasted against `Entries` never exposing one to corrupt; `ExampleLog_ConflictTruncateAppend`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/raftlog
cd ~/go-exercises/raftlog
go mod init example.com/raftlog
go mod edit -go=1.24
```

### slices.Replace's general contract, and the two-append expression it replaces

`slices.Replace(s, i, j, v...)` means exactly one thing: `s[i:j]` becomes `v`,
and everything is renumbered accordingly. It handles every relative size of `v`
and `j-i` -- shrinking, growing, or exact replacement -- as one operation, and it
is the same function whether `j-i` is one element or the whole slice. That
generality is what makes it the right tool for two operations that look
different at the call site (splice a conflicting tail with the leader's entries,
collapse a committed range into a snapshot marker) but are the same slice
operation underneath.

The hand-rolled alternative most people reach for first is a Go slice trick that
circulates in blog posts and cheat sheets:

```go
result := append(s[:i], append(v, s[j:]...)...)
```

Traced through by hand, this looks correct, and for the value it returns, it
usually is: the inner `append` reads `s[j:]` before the outer `append` writes
anything, so the returned slice's contents are right. What this expression does
not account for is that the outer `append` writes into `s[:i]`'s *own* backing
array whenever that array has spare capacity past `i` -- and a log slice that has
grown by repeated `append` calls, or been truncated and re-grown, routinely does.
That write happens *in place*, through the same memory any other slice that still
aliases `s` is looking at. Nothing about the call signals this: no panic, no
changed return value, just a second variable somewhere else in the program that
silently shows different data than it did a moment ago. `slices.Replace` is
implemented once, tested against exactly this class of aliasing behavior, and
used the same way regardless of whether it happens to grow, shrink, or reuse the
backing array -- callers do not have to reason about capacity by eye each time.

Create `raftlog.go`:

```go
// Package raftlog implements the follower side of a Raft-style consensus
// log: appending entries, splicing away a conflicting tail when
// AppendEntries finds a term mismatch, and compacting a committed range
// into a single snapshot placeholder.
//
// Every mutation goes through slices.Replace on the log's own internal
// backing array. See the package tests for why the tempting hand-rolled
// alternative -- append(s[:i], append(v, s[j:]...)...) -- is not part of
// this API: whether it silently corrupts memory some other part of the
// program still holds a reference to depends on how much spare capacity
// s[:i] happens to retain, which is a fact about the log's growth history,
// not about the code that reads it.
package raftlog

import (
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by Log's mutating methods. Callers should test
// for them with errors.Is.
var (
	// ErrConflictIndexOutOfRange means from was below 1 or left a gap
	// after the current end of the log.
	ErrConflictIndexOutOfRange = errors.New("raftlog: conflict index out of range")
	// ErrCompactRangeInvalid means [i, j) was not a valid, non-empty
	// subrange of the current log.
	ErrCompactRangeInvalid = errors.New("raftlog: compact range invalid")
)

// Entry is one record in the replicated log: the Raft term that created it,
// its 1-based log index, and the opaque command payload.
type Entry struct {
	Term  int64
	Index int
	Data  string
}

// Log is a single follower's replicated log, indexed from 1 the way the
// Raft paper does; there is no entry at index 0.
//
// Log is not safe for concurrent use. A Raft node drives its log from a
// single goroutine (the state machine loop); a caller that needs concurrent
// access must synchronize externally.
type Log struct {
	entries []Entry // entries[k] holds the entry at Index k+1.
}

// NewLog returns an empty Log.
func NewLog() *Log {
	return &Log{}
}

// Len reports the number of entries currently in the log.
func (l *Log) Len() int {
	return len(l.entries)
}

// ConflictTruncateAppend implements the follower side of AppendEntries once
// a term mismatch has been found at index from: every entry at or after
// from is discarded and leaderEntries is spliced into place starting there.
// from must be between 1 and Len()+1 inclusive; from == Len()+1 means there
// is nothing to discard, a plain append.
//
// leaderEntries is copied into the log; ConflictTruncateAppend does not
// retain a reference to it, so the caller may reuse or mutate the slice
// after this call returns. The Index field of every entry from and after
// from is rewritten to its resulting position, so leaderEntries need not
// carry correct indices in.
func (l *Log) ConflictTruncateAppend(from int, leaderEntries []Entry) error {
	if from < 1 || from > len(l.entries)+1 {
		return fmt.Errorf("%w: from=%d, log has %d entries", ErrConflictIndexOutOfRange, from, len(l.entries))
	}
	l.entries = slices.Replace(l.entries, from-1, len(l.entries), leaderEntries...)
	l.reindexFrom(from - 1)
	return nil
}

// CompactRange collapses the committed entries in the 1-based, half-open
// range [i, j) into the single marker entry, discarding the entries it
// replaces. Both i and j must fall within the current log, with i < j;
// i == j is rejected because there would be nothing to compact.
//
// marker.Index is overwritten to the position the placeholder lands at;
// only its Term and Data are kept as given.
func (l *Log) CompactRange(i, j int, marker Entry) error {
	if i < 1 || j <= i || j > len(l.entries)+1 {
		return fmt.Errorf("%w: i=%d j=%d, log has %d entries", ErrCompactRangeInvalid, i, j, len(l.entries))
	}
	l.entries = slices.Replace(l.entries, i-1, j-1, marker)
	l.reindexFrom(i - 1)
	return nil
}

// reindexFrom rewrites the Index field of every entry from position start
// (0-based) to the end of the log, after a splice has shifted them.
func (l *Log) reindexFrom(start int) {
	for k := start; k < len(l.entries); k++ {
		l.entries[k].Index = k + 1
	}
}

// Entries returns every entry currently in the log, in order.
//
// The returned slice is a fresh copy: it never aliases the Log's internal
// storage, so the caller may retain, sort, or mutate it freely without
// affecting a later call to ConflictTruncateAppend or CompactRange, and
// without a later call to either of those affecting a slice already
// returned here.
func (l *Log) Entries() []Entry {
	return slices.Clone(l.entries)
}
```

### Using it

Construct one `Log` per follower with `NewLog()` and drive it from the Raft state
machine's single goroutine -- the type's doc comment is explicit that it is not
safe for concurrent use, matching how a real Raft implementation already
serializes log mutations through one loop. `ConflictTruncateAppend` is what an
`AppendEntries` RPC handler calls once it has found the first index where the
follower's term diverges from the leader's; `CompactRange` is what a snapshot
routine calls once a prefix of the log is known to be committed and durable
elsewhere. Both return a sentinel error, checkable with `errors.Is`, for a
request that does not describe a valid range of the current log -- deliberately,
since these are exactly the requests a buggy peer or a stale RPC could send.

`Entries` is the only way to read the log's contents, and it always returns an
independent copy: the doc comment states that a caller may mutate what `Entries`
returns without touching the log, and that a later `ConflictTruncateAppend` or
`CompactRange` cannot retroactively change a slice `Entries` already handed back.
That contract is what makes the log's own internal use of `slices.Replace` --
which does reuse its backing array in place -- invisible to every caller outside
the package.

`ExampleLog_ConflictTruncateAppend` in the `_test.go` file is the runnable
demonstration of this module: `go test` executes it and compares its stdout
against the `// Output:` comment, so it cannot drift from the code it documents.

```go
func ExampleLog_ConflictTruncateAppend() {
	l := NewLog()
	_ = l.ConflictTruncateAppend(1, []Entry{
		{Term: 1, Data: "set x=1"},
		{Term: 1, Data: "set y=2"},
		{Term: 1, Data: "set z=3"},
	})
	fmt.Println("after initial append:", l.Entries())

	_ = l.ConflictTruncateAppend(2, []Entry{
		{Term: 2, Data: "set y=99"},
	})
	fmt.Println("after conflict splice:", l.Entries())

	if err := l.CompactRange(1, 2, Entry{Term: 9, Data: "snapshot@1"}); err != nil {
		fmt.Println("compact error:", err)
	}
	fmt.Println("after compaction:", l.Entries())

	if err := l.ConflictTruncateAppend(0, nil); err != nil {
		fmt.Println("rejected:", err)
	}

	// Output:
	// after initial append: [{1 1 set x=1} {1 2 set y=2} {1 3 set z=3}]
	// after conflict splice: [{1 1 set x=1} {2 2 set y=99}]
	// after compaction: [{9 1 snapshot@1} {2 2 set y=99}]
	// rejected: raftlog: conflict index out of range: from=0, log has 2 entries
}
```

### Tests

`TestConflictTruncateAppend` and `TestCompactRange` are tables covering the
ordinary splices (a plain append with nothing to discard, a full-log replace, a
partial-tail truncation, a middle-range and a whole-log compaction) alongside the
edge cases the component bar requires: an empty log, an empty `leaderEntries`
that degenerates to a no-op, and every way a caller can name a range outside the
log. `TestEntriesDoesNotAliasLog` pins the aliasing contract in both directions
-- mutating a returned slice cannot reach the log, and mutating the log later
cannot reach a slice already returned.

`TestNaiveDoubleAppendCorruptsSharedAlias` is the module's core test. It builds
an `Entry` slice with visible spare capacity (six live elements in a ten-slot
backing array -- exactly what a log looks like after some growth), takes a
sub-slice of its tail the way an unrelated part of a program might hold one, and
runs the unexported `spliceNaive` helper -- the `append(s[:i], append(v,
s[j:]...)...)` expression from the prose above, never reachable through the
package API -- against the full slice. It asserts two things side by side: the
naive splice's own return value is exactly correct, and the previously captured
tail alias is now corrupted anyway, even though nothing ever reassigned it. It
then repeats the same splice through the real `ConflictTruncateAppend` and shows
a snapshot taken from `Entries` beforehand survives untouched, because `Entries`
never handed out an alias in the first place.

Create `raftlog_test.go`:

```go
package raftlog

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// entries builds n sequential entries at term 1, indices 1..n.
func entries(n int) []Entry {
	out := make([]Entry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Entry{Term: 1, Index: i, Data: fmt.Sprintf("cmd%d", i)})
	}
	return out
}

// spliceNaive is the hand-rolled two-append splice a follower's AppendEntries
// handler reaches for instead of slices.Replace. It computes s[i:j]'s
// replacement by v the way it looks correct: the inner append reads s[j:]
// before the outer one writes anything. But the outer append still writes
// into s[:i]'s own backing array whenever that array has spare capacity --
// and every entry a log has ever discarded via ConflictTruncateAppend
// leaves exactly that kind of spare capacity behind. It is not part of the
// package API; it exists only to demonstrate why ConflictTruncateAppend and
// CompactRange never touch a raw slice this way.
func spliceNaive(s []Entry, i, j int, v []Entry) []Entry {
	return append(s[:i], append(v, s[j:]...)...)
}

func TestConflictTruncateAppend(t *testing.T) {
	t.Parallel()

	xy := []Entry{{Term: 2, Data: "X"}, {Term: 2, Data: "Y"}}
	tests := []struct {
		name    string
		initial []Entry
		from    int
		leader  []Entry
		want    []Entry
		wantErr error
	}{
		{"plain append at the end", entries(3), 4, xy,
			[]Entry{{1, 1, "cmd1"}, {1, 2, "cmd2"}, {1, 3, "cmd3"}, {2, 4, "X"}, {2, 5, "Y"}}, nil},
		{"truncate the whole log and replace", entries(3), 1, xy,
			[]Entry{{2, 1, "X"}, {2, 2, "Y"}}, nil},
		{"truncate a conflicting tail, keep the prefix", entries(5), 3, xy,
			[]Entry{{1, 1, "cmd1"}, {1, 2, "cmd2"}, {2, 3, "X"}, {2, 4, "Y"}}, nil},
		{"append into an empty log", nil, 1, xy[:1],
			[]Entry{{2, 1, "X"}}, nil},
		{"empty leaderEntries at the end is a no-op", entries(2), 3, nil,
			entries(2), nil},
		{"from below 1 is rejected", entries(3), 0, xy,
			nil, ErrConflictIndexOutOfRange},
		{"from past the end leaves a gap and is rejected", entries(3), 5, xy,
			nil, ErrConflictIndexOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := NewLog()
			l.entries = slices.Clone(tc.initial)

			err := l.ConflictTruncateAppend(tc.from, tc.leader)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ConflictTruncateAppend error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConflictTruncateAppend: unexpected error: %v", err)
			}
			if !slices.Equal(l.Entries(), tc.want) {
				t.Fatalf("entries = %+v, want %+v", l.Entries(), tc.want)
			}
		})
	}
}

func TestCompactRange(t *testing.T) {
	t.Parallel()

	marker := Entry{Term: 9, Data: "snapshot"}

	tests := []struct {
		name    string
		initial []Entry
		i, j    int
		want    []Entry
		wantErr error
	}{
		{"collapse a middle range into the marker", entries(6), 2, 5,
			[]Entry{{1, 1, "cmd1"}, {9, 2, "snapshot"}, {1, 3, "cmd5"}, {1, 4, "cmd6"}}, nil},
		{"collapse a single entry", entries(3), 2, 3,
			[]Entry{{1, 1, "cmd1"}, {9, 2, "snapshot"}, {1, 3, "cmd3"}}, nil},
		{"collapse the entire log", entries(4), 1, 5,
			[]Entry{{9, 1, "snapshot"}}, nil},
		{"empty range is rejected", entries(3), 2, 2, nil, ErrCompactRangeInvalid},
		{"i below 1 is rejected", entries(3), 0, 2, nil, ErrCompactRangeInvalid},
		{"j past the end is rejected", entries(3), 1, 5, nil, ErrCompactRangeInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := NewLog()
			l.entries = slices.Clone(tc.initial)

			err := l.CompactRange(tc.i, tc.j, marker)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("CompactRange error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CompactRange: unexpected error: %v", err)
			}
			if !slices.Equal(l.Entries(), tc.want) {
				t.Fatalf("entries = %+v, want %+v", l.Entries(), tc.want)
			}
		})
	}
}

// TestEntriesDoesNotAliasLog proves the aliasing contract on Entries: a
// caller that mutates the returned slice cannot corrupt the log, and a
// later mutation to the log cannot corrupt a slice already handed out.
func TestEntriesDoesNotAliasLog(t *testing.T) {
	t.Parallel()

	l := NewLog()
	l.entries = entries(3)

	snapshot := l.Entries()
	snapshot[0].Data = "tampered"
	if l.entries[0].Data != "cmd1" {
		t.Fatalf("mutating the returned slice changed the log: %q", l.entries[0].Data)
	}

	snapshot2 := l.Entries()
	if err := l.CompactRange(1, 3, Entry{Term: 9, Data: "snapshot"}); err != nil {
		t.Fatalf("CompactRange: %v", err)
	}
	want := entries(3)
	if !slices.Equal(snapshot2, want) {
		t.Fatalf("a later CompactRange changed a slice returned earlier: got %+v, want %+v", snapshot2, want)
	}
}

// TestNaiveDoubleAppendCorruptsSharedAlias is the heart of the module. It
// shows the defect append(s[:i], append(v, s[j:]...)...) ships to
// production: the outer append writes into s[:i]'s own backing array
// whenever that array has spare capacity, silently mutating any other
// slice that still views the same memory -- even though that other slice's
// variable was never reassigned and the naive splice's own return value is
// perfectly correct.
func TestNaiveDoubleAppendCorruptsSharedAlias(t *testing.T) {
	t.Parallel()

	// Six live entries in a ten-slot backing array: exactly the kind of
	// spare capacity a log accumulates after growing and being truncated
	// a few times.
	base := make([]Entry, 0, 10)
	base = append(base, entries(6)...)

	// Some other part of the program -- a replication tracker, a metrics
	// sampler -- captured a view of the tail before the splice ran.
	tailSnapshot := base[3:6]
	wantBefore := []Entry{{1, 4, "cmd4"}, {1, 5, "cmd5"}, {1, 6, "cmd6"}}
	if !slices.Equal(tailSnapshot, wantBefore) {
		t.Fatalf("tailSnapshot before splice = %+v, want %+v", tailSnapshot, wantBefore)
	}

	v := []Entry{{Term: 2, Data: "A"}, {Term: 2, Data: "B"}, {Term: 2, Data: "C"}}
	result := spliceNaive(base, 2, 3, v)

	// The naive splice's own return value looks entirely reasonable.
	wantResult := []Entry{
		{1, 1, "cmd1"}, {1, 2, "cmd2"},
		{2, 0, "A"}, {2, 0, "B"}, {2, 0, "C"},
		{1, 4, "cmd4"}, {1, 5, "cmd5"}, {1, 6, "cmd6"},
	}
	if !slices.Equal(result, wantResult) {
		t.Fatalf("spliceNaive result = %+v, want %+v", result, wantResult)
	}

	// tailSnapshot's variable was never touched, yet its contents changed:
	// the outer append reused base's backing array and wrote straight
	// through the memory tailSnapshot still views.
	if slices.Equal(tailSnapshot, wantBefore) {
		t.Fatal("tailSnapshot unexpectedly unchanged; base must not have had spare capacity")
	}

	// Contrast: a snapshot taken from the real Entries() before a real
	// ConflictTruncateAppend is unaffected by it, because Entries() never
	// hands out an alias of the log's storage in the first place.
	l := NewLog()
	l.entries = entries(6)
	safeSnapshot := l.Entries()
	if err := l.ConflictTruncateAppend(3, v); err != nil {
		t.Fatalf("ConflictTruncateAppend: %v", err)
	}
	if !slices.Equal(safeSnapshot, entries(6)) {
		t.Fatalf("safeSnapshot corrupted by a later ConflictTruncateAppend: %+v", safeSnapshot)
	}
}

// ExampleLog_ConflictTruncateAppend is the runnable demonstration of this
// module: go test executes it and compares its stdout against the Output
// comment below.
func ExampleLog_ConflictTruncateAppend() {
	l := NewLog()
	_ = l.ConflictTruncateAppend(1, []Entry{
		{Term: 1, Data: "set x=1"},
		{Term: 1, Data: "set y=2"},
		{Term: 1, Data: "set z=3"},
	})
	fmt.Println("after initial append:", l.Entries())

	// The leader's term-2 entries conflict starting at index 2: the
	// follower must discard cmd2 and cmd3 and splice the leader's in.
	_ = l.ConflictTruncateAppend(2, []Entry{
		{Term: 2, Data: "set y=99"},
	})
	fmt.Println("after conflict splice:", l.Entries())

	if err := l.CompactRange(1, 2, Entry{Term: 9, Data: "snapshot@1"}); err != nil {
		fmt.Println("compact error:", err)
	}
	fmt.Println("after compaction:", l.Entries())

	if err := l.ConflictTruncateAppend(0, nil); err != nil {
		fmt.Println("rejected:", err)
	}

	// Output:
	// after initial append: [{1 1 set x=1} {1 2 set y=2} {1 3 set z=3}]
	// after conflict splice: [{1 1 set x=1} {2 2 set y=99}]
	// after compaction: [{9 1 snapshot@1} {2 2 set y=99}]
	// rejected: raftlog: conflict index out of range: from=0, log has 2 entries
}
```

## Review

`ConflictTruncateAppend` and `CompactRange` are correct when they produce exactly
the entries `slices.Replace`'s contract promises and reindex them to their new
positions -- the tables above pin both against the ordinary cases and every
out-of-range request, checkable with `errors.Is` against `ErrConflictIndexOutOfRange`
and `ErrCompactRangeInvalid`. The trap this module exists to name is the
hand-rolled `append(s[:i], append(v, s[j:]...)...)` splice: its own return value
is correct, which is exactly what makes it dangerous, because the outer `append`
still writes in place through `s`'s backing array whenever there is spare
capacity, silently corrupting any other slice that aliases it -- proven directly
in `TestNaiveDoubleAppendCorruptsSharedAlias` rather than merely asserted.
`Entries` closes that door for every caller of this package by always returning
a clone, so nothing outside `raftlog` can ever hold the kind of alias the naive
splice corrupts. `Log` is documented as unsafe for concurrent use, matching how a
real Raft node already serializes log mutation through one goroutine. Run
`go test -count=1 -race ./...` to confirm the tables, the aliasing contract, the
splice-corruption contrast, and the runnable example.

## Resources

- [`slices.Replace`](https://pkg.go.dev/slices#Replace) — the general `s[i:j] = v...` splice this module builds on.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the copy `Entries` returns so callers never alias the log's storage.
- [The Go Blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — how `append` decides to reuse a backing array versus allocate a new one.
- [Raft paper, section 5.3](https://raft.github.io/raft.pdf) — the AppendEntries consistency check this module's `ConflictTruncateAppend` implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-slo-extremes-max-min-func.md](10-slo-extremes-max-min-func.md) | Next: [12-sstable-order-guard-issortedfunc.md](12-sstable-order-guard-issortedfunc.md)
