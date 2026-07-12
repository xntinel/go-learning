# Exercise 22: Raft Log Entry Matching with Prefix Memoization

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Raft's `AppendEntries` consistency check answers one question: how far does
a follower's log agree with the leader's? A leader that gets rejected
searches backward from the end of the follower's log — "does index 9 agree?
No. Index 8? No. Index 6? Yes" — recursively, one index at a time, until it
finds the highest index where the two logs match. Under a flaky network
partition, a leader may run this exact search against the same follower
several times as connectivity drops and returns; the prefix that already
agreed does not change between those retries, so re-deriving it every time
is wasted work a memo table can eliminate.

This module is fully self-contained: its own `go mod init`, the matcher
inline, its own demo and tests.

## What you'll build

```text
raftmatch/                    independent module: example.com/raftmatch
  go.mod                        go 1.24
  raftmatch.go                   type Entry; type Matcher; func NewMatcher; method FindMatchIndex
  raftmatch_test.go               identical logs, mid-log divergence, no agreement, follower longer than leader, memo reuse across retries, empty follower
  cmd/
    demo/
      main.go                     first-contact match, then a retry after a simulated partition heals
```

- Files: `raftmatch.go`, `cmd/demo/main.go`, `raftmatch_test.go`.
- Implement: `type Entry struct { Term int; Command string }`,
  `type Matcher struct { ... }`, `func NewMatcher(leader []Entry) *Matcher`,
  and `func (m *Matcher) FindMatchIndex(follower []Entry) int`, recursing
  backward from the end of `follower` with a per-index memo.
- Test: identical logs match at the last index; a log that diverges partway
  through is matched at the divergence point; logs with no agreement at all
  return `-1`; a follower log longer than the leader's is bounded by the
  leader's own length; a second call after a simulated partition reuses the
  memoized comparisons instead of recomputing them; an empty follower log
  returns `-1`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the same index gets asked about more than once

`FindMatchIndex` recurses from `len(follower)-1` down toward `0`, comparing
one index at a time until it finds agreement or runs out of indices. Inside
a single call that recursion never revisits an index — it only goes one
direction. The repetition this exercise targets happens *across* calls: a
leader that gets disconnected mid-negotiation and reconnects calls
`FindMatchIndex` again on the same (or nearly the same) follower log, and a
plain recursive implementation with no memory of the previous call starts
the search over from the end, re-comparing every index it already resolved
the first time — including the long prefix that matched and was never in
question.

The `Matcher` fixes this by keeping the memo (`map[int]bool`, keyed by
index) on the struct rather than local to one call. The first time index
`i` is compared, the result is recorded; any later call — whether the same
retry logic calls `FindMatchIndex` again, or a subsequent search happens to
walk through the same index from a different starting point — reads the
cached answer instead of touching `m.leader[i]` and `follower[i]` again.
This is valid only because of a real invariant, not just a convenient
assumption: a leader's own log entries do not change once written at a
given index (Raft guarantees this), so "does the leader's entry at index i
agree with what the follower had there" is a question whose answer, once
resolved for a given follower log, stays correct for anything that follower
appends *after* that index — which is exactly the shape of a retry
scenario, where a follower may accumulate new local entries during a
partition without altering what it already had.

Create `raftmatch.go`:

```go
// Package raftmatch finds how far a follower's replicated log agrees with
// the leader's, the core comparison behind Raft's AppendEntries consistency
// check. A leader recovering from a network partition may re-run this
// comparison against the same follower several times as connectivity flaps;
// a Matcher memoizes per-index comparison results across those calls so a
// leader that keeps retrying does not keep re-deriving the same answers for
// the prefix of the log that was already confirmed to agree.
package raftmatch

// Entry is one replicated log entry. Two entries agree only if both their
// term and command match, mirroring Raft's log matching property: if two
// logs have an entry with the same index and term, every earlier entry in
// both logs is identical.
type Entry struct {
	Term    int
	Command string
}

// Matcher finds the match index between a fixed leader log and one or more
// follower logs, memoizing comparisons at each index by position so that
// repeated calls (simulating a leader retrying after a network partition)
// reuse work instead of redoing it.
type Matcher struct {
	leader   []Entry
	memo     map[int]bool
	Attempts int // number of comparisons actually computed (not served from memo); exported for tests and the demo to observe caching.
}

// NewMatcher returns a Matcher for the given leader log.
func NewMatcher(leader []Entry) *Matcher {
	return &Matcher{leader: leader, memo: make(map[int]bool)}
}

// FindMatchIndex returns the highest index i such that follower[i] agrees
// with the leader's entry at i (same term and command), or -1 if no such
// index exists. It recurses from the end of follower backward — the same
// direction Raft's leader searches in when a follower rejects an
// AppendEntries RPC — memoizing each index's comparison result so a
// follower log re-checked in a later call (after the leader regains contact
// following a partition) does not re-compare indices already resolved.
func (m *Matcher) FindMatchIndex(follower []Entry) int {
	return m.matchFrom(follower, len(follower)-1)
}

func (m *Matcher) matchFrom(follower []Entry, i int) int {
	if i < 0 || i >= len(m.leader) {
		if i < 0 {
			return -1
		}
		// Index only exists in the follower's log, past the leader's
		// current length: no agreement is possible here, so step back
		// without spending a memoized slot (the answer depends only on the
		// leader's log, and the leader has nothing at this index).
		return m.matchFrom(follower, i-1)
	}

	if agree, ok := m.memo[i]; ok {
		if agree {
			return i
		}
		return m.matchFrom(follower, i-1)
	}

	m.Attempts++
	agree := m.leader[i] == follower[i]
	m.memo[i] = agree
	if agree {
		return i
	}
	return m.matchFrom(follower, i-1)
}
```

### The runnable demo

The demo matches a follower log that agrees through index 6 and diverges
after it, then simulates a partition healing and the leader retrying
against an updated (but still-diverged-at-the-same-point) follower log,
showing the attempt count does not grow on the retry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/raftmatch"
)

func makeLeaderLog() []raftmatch.Entry {
	log := make([]raftmatch.Entry, 10)
	for i := range log {
		log[i] = raftmatch.Entry{Term: 1, Command: fmt.Sprintf("cmd%d", i)}
	}
	return log
}

func main() {
	leader := makeLeaderLog()
	matcher := raftmatch.NewMatcher(leader)

	// First contact: the follower's log agrees with the leader through
	// index 6, then diverges (a stale leader wrote different commands at
	// indices 7-9 before the current leader was elected).
	follower1 := append([]raftmatch.Entry(nil), leader[:7]...)
	for i := 7; i < 10; i++ {
		follower1 = append(follower1, raftmatch.Entry{Term: 0, Command: "stale"})
	}

	matchIndex := matcher.FindMatchIndex(follower1)
	fmt.Printf("first contact: matchIndex=%d attempts=%d\n", matchIndex, matcher.Attempts)

	// Network partition heals; the leader retries against the follower,
	// whose log now also has two new (uncommitted, unrelated) local
	// entries appended past its own end. The 0-9 prefix is unchanged, so
	// every comparison the leader needs is already memoized.
	follower2 := append([]raftmatch.Entry(nil), follower1...)
	follower2 = append(follower2, raftmatch.Entry{Term: 0, Command: "local-only-a"})
	follower2 = append(follower2, raftmatch.Entry{Term: 0, Command: "local-only-b"})

	matchIndex2 := matcher.FindMatchIndex(follower2)
	fmt.Printf("retry after partition: matchIndex=%d attempts=%d\n", matchIndex2, matcher.Attempts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first contact: matchIndex=6 attempts=4
retry after partition: matchIndex=6 attempts=4
```

### Tests

`TestFindMatchIndexIdenticalLogsMatchAtEnd` and `TestFindMatchIndexDivergesAtMiddle`
check the core search. `TestFindMatchIndexNoAgreementReturnsMinusOne` and
`TestFindMatchIndexFollowerLongerThanLeader` cover the two boundary shapes.
`TestFindMatchIndexReusesMemoAcrossRetries` is the point of the exercise: it
calls `FindMatchIndex` twice on the same `Matcher` with a follower log that
grew but kept its existing (still-diverged) prefix, and asserts
`Attempts` did not increase on the second call. `TestFindMatchIndexEmptyFollower`
covers the trivial input.

Create `raftmatch_test.go`:

```go
package raftmatch

import (
	"fmt"
	"testing"
)

func sampleLeaderLog(n int) []Entry {
	log := make([]Entry, n)
	for i := range log {
		log[i] = Entry{Term: 1, Command: fmt.Sprintf("cmd%d", i)}
	}
	return log
}

func TestFindMatchIndexIdenticalLogsMatchAtEnd(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(5)
	follower := append([]Entry(nil), leader...)

	m := NewMatcher(leader)
	got := m.FindMatchIndex(follower)
	if got != 4 {
		t.Fatalf("FindMatchIndex() = %d, want 4", got)
	}
}

func TestFindMatchIndexDivergesAtMiddle(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(10)
	follower := append([]Entry(nil), leader[:7]...)
	for i := 7; i < 10; i++ {
		follower = append(follower, Entry{Term: 0, Command: "stale"})
	}

	m := NewMatcher(leader)
	got := m.FindMatchIndex(follower)
	if got != 6 {
		t.Fatalf("FindMatchIndex() = %d, want 6", got)
	}
}

func TestFindMatchIndexNoAgreementReturnsMinusOne(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(3)
	follower := []Entry{
		{Term: 0, Command: "a"},
		{Term: 0, Command: "b"},
		{Term: 0, Command: "c"},
	}

	m := NewMatcher(leader)
	if got := m.FindMatchIndex(follower); got != -1 {
		t.Fatalf("FindMatchIndex() = %d, want -1", got)
	}
}

func TestFindMatchIndexFollowerLongerThanLeader(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(3)
	follower := append([]Entry(nil), leader...)
	follower = append(follower, Entry{Term: 2, Command: "extra"})

	m := NewMatcher(leader)
	got := m.FindMatchIndex(follower)
	if got != 2 {
		t.Fatalf("FindMatchIndex() = %d, want 2 (leader's last index)", got)
	}
}

func TestFindMatchIndexReusesMemoAcrossRetries(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(10)
	follower1 := append([]Entry(nil), leader[:7]...)
	for i := 7; i < 10; i++ {
		follower1 = append(follower1, Entry{Term: 0, Command: "stale"})
	}

	m := NewMatcher(leader)
	first := m.FindMatchIndex(follower1)
	attemptsAfterFirst := m.Attempts

	// Same conflicting prefix, plus two entries the follower appended
	// locally past its own end during the partition. The leader retries
	// after reconnecting.
	follower2 := append([]Entry(nil), follower1...)
	follower2 = append(follower2, Entry{Term: 0, Command: "local-a"})
	follower2 = append(follower2, Entry{Term: 0, Command: "local-b"})

	second := m.FindMatchIndex(follower2)

	if first != second {
		t.Fatalf("matchIndex changed between retries: %d then %d", first, second)
	}
	if m.Attempts != attemptsAfterFirst {
		t.Fatalf("Attempts grew from %d to %d on a retry over an unchanged prefix; memo not reused",
			attemptsAfterFirst, m.Attempts)
	}
	if attemptsAfterFirst == 0 || attemptsAfterFirst >= len(leader) {
		t.Fatalf("attempts = %d, want a small nonzero number bounded by log length", attemptsAfterFirst)
	}
}

func TestFindMatchIndexEmptyFollower(t *testing.T) {
	t.Parallel()

	leader := sampleLeaderLog(5)
	m := NewMatcher(leader)
	if got := m.FindMatchIndex(nil); got != -1 {
		t.Fatalf("FindMatchIndex(nil) = %d, want -1", got)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`FindMatchIndex` is correct when it returns the true highest agreeing
index in every shape of input — identical logs, mid-log divergence, no
agreement, and a follower log longer than the leader's — and `Attempts`
proves the memo is doing its job: `TestFindMatchIndexReusesMemoAcrossRetries`
would fail if a retry silently recomputed the whole prefix instead of
reading it from `m.memo`. The mistake this exercise targets is scoping the
memo to a single call (a local map inside `FindMatchIndex` instead of a
field on `Matcher`) — that still speeds up one search, since a single
backward walk never revisits an index anyway, but it throws away every
result the moment the call returns, so a leader's second retry against the
same follower gets no benefit at all. Keeping the memo on the `Matcher`
instead is what actually captures the cross-call reuse the exercise is
about.

## Resources

- [Raft consensus paper, Section 5.3 (Log Matching Property)](https://raft.github.io/raft.pdf)
- [Go Specification: Struct types (comparability)](https://go.dev/ref/spec#Comparison_operators)
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-audit-log-pii-redaction-recursive.md](21-audit-log-pii-redaction-recursive.md) | Next: [23-sql-query-ast-optimization-rewrite.md](23-sql-query-ast-optimization-rewrite.md)
