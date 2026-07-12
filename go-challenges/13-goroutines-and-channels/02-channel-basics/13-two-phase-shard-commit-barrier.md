# Exercise 13: All-or-Nothing Shard Commit over Request/Reply Channels

**Level: Advanced**

A single logical write often spans several in-memory shards, and the contract is
atomicity: either every shard applies the change or none does. The naive approach
lets each shard apply as soon as it is ready, and a later failure leaves the
system half-written with no clean way back. This exercise builds a two-phase
commit barrier out of nothing but channels: every shard prepares and votes, a
coordinator collects all votes as a fan-in of a known count, decides once, and
fans that single decision back to each shard on its own private reply channel.

This module is self-contained: its own module, a `commit` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
commit/                      independent module: example.com/commit
  go.mod                     go 1.26
  commit.go                  type Shard; Commit(shards []Shard) error
  cmd/demo/main.go           runnable demo: a committing batch and an aborting batch
  commit_test.go             all-yes commits, one-no aborts all, exactly-once delivery, memory publish
```

- Files: `commit.go`, `cmd/demo/main.go`, `commit_test.go`.
- Implement: `Commit(shards []Shard) error`, where each `Shard` carries `Prepare`, `OnCommit`, and `OnAbort` callbacks. Commit runs each shard in its own goroutine, collects exactly `len(shards)` votes, decides commit only if all vote yes, and delivers the decision to every shard.
- Test: the all-yes path fires `OnCommit` on every shard and `OnAbort` on none; a single failing `Prepare` aborts every shard and returns a joined error naming the failure; the decision reaches each shard exactly once; a failure at any index still aborts (table-driven); state written before a vote is visible after the commit reply.
- Verify: `go test -count=1 -race ./...`

### A two-phase barrier built from request/reply channels

Two-phase commit has a prepare phase and a decide phase. The invariant is
atomicity: a shard may apply its change only after it knows every shard voted
yes, and if any shard voted no, no shard applies anything. The subtlety is that
the shards are independent goroutines with no shared lock — the entire agreement
is carried by channel sends, and the Go memory model's happens-before edges are
what make it correct rather than merely likely.

The protocol has four steps:

1. Each shard runs in its own goroutine. It calls `Prepare`, which does the
   shard-local staging and votes: a nil error is yes, a non-nil error is no. The
   goroutine then sends a `vote{ok, name, err, reply}` onto a single shared
   `votes` channel. That channel is the fan-in point.
2. The coordinator (the body of `Commit`) receives from `votes` exactly
   `len(shards)` times. Because the count is known in advance, this fan-in needs
   no `close` and no `WaitGroup` around the receive — the loop `for range n`
   terminates on its own. As each vote arrives, the coordinator remembers that
   shard's private `reply` channel and folds the vote into the running decision.
3. After all `n` votes are in, the coordinator computes `commit = all-yes`. Every
   shard goroutine is now parked on `<-reply`, so the coordinator walks the saved
   reply channels and sends the one decision to each. Because each reply channel
   is private to one shard and unbuffered, each send rendezvous with exactly one
   shard exactly once: no double-commit, no missed shard.
4. Each shard receives its decision and calls `OnCommit` or `OnAbort`. `Commit`
   waits for all shard goroutines with a `WaitGroup` before returning, so the
   callbacks' effects are complete when the caller sees the result.

The memory-model chain is the load-bearing part. Anything `Prepare` writes to
shard-local state happens-before the `votes <- ...` send; that send happens-before
the coordinator's receive; the coordinator's `reply <- commit` send happens-before
the shard's `<-reply`; and that receive happens-before `OnCommit`. Transitively,
the prepared state is visible to `OnCommit` with no lock and no data race. That is
the whole point of "share memory by communicating": the channel handoffs publish
the data.

Create `commit.go`:

```go
package commit

import (
	"errors"
	"fmt"
	"sync"
)

// Shard is one participant in an all-or-nothing batch write. Prepare does the
// shard-local work and votes: a nil error is a yes, a non-nil error is a no.
// Exactly one of OnCommit or OnAbort runs, after the whole batch has decided.
type Shard struct {
	Name     string
	Prepare  func() error // shard-local prepare; a non-nil error is a vote of no
	OnCommit func()       // applied iff the whole batch commits
	OnAbort  func()       // applied iff the batch aborts
}

// vote is one shard's ballot, carrying its own private reply channel so the
// coordinator can fan the single decision back to exactly that shard.
type vote struct {
	ok    bool
	name  string
	err   error
	reply chan bool
}

// Commit runs a two-phase, all-or-nothing commit across shards using only
// request/reply channels. Each shard prepares in its own goroutine and votes on
// a shared fan-in channel; the coordinator receives exactly len(shards) votes
// (a fan-in of a known count, so no close is needed), decides commit only if
// every vote is yes, then sends that one decision to each shard's private reply
// channel. It returns nil on commit, or errors.Join of the failing shards'
// errors on abort. Commit does not return until every shard has run its
// OnCommit or OnAbort, so the callbacks' effects are complete on return.
func Commit(shards []Shard) error {
	n := len(shards)
	if n == 0 {
		return nil
	}

	votes := make(chan vote) // fan-in; coordinator receives exactly n times
	var wg sync.WaitGroup

	for i := range shards {
		s := shards[i]
		reply := make(chan bool) // private to this shard, unbuffered rendezvous
		wg.Go(func() {
			err := s.Prepare()
			// The send publishes any state Prepare wrote: send happens-before
			// the coordinator's receive, which happens-before the reply send,
			// which happens-before this receive, which happens-before OnCommit.
			votes <- vote{ok: err == nil, name: s.Name, err: err, reply: reply}
			if <-reply {
				if s.OnCommit != nil {
					s.OnCommit()
				}
			} else {
				if s.OnAbort != nil {
					s.OnAbort()
				}
			}
		})
	}

	replies := make([]chan bool, 0, n)
	commit := true
	var errs []error
	for range n {
		v := <-votes
		replies = append(replies, v.reply)
		if !v.ok {
			commit = false
			errs = append(errs, fmt.Errorf("shard %q prepare failed: %w", v.name, v.err))
		}
	}

	// Every shard is now blocked on <-reply, so each of these sends reaches
	// exactly one shard exactly once: no double-commit, no missed shard.
	for _, r := range replies {
		r <- commit
	}

	wg.Wait() // all OnCommit/OnAbort callbacks have run before we return
	if commit {
		return nil
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo defines a tiny `store` per shard. `Prepare` stages a value into
`prepared`; `OnCommit` copies it to `applied`; `OnAbort` sets a flag. It runs one
batch where every shard votes yes and one where the `ledger` shard fails its
prepare. Output order is made deterministic by iterating a fixed sorted name list,
never a map.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"example.com/commit"
)

// store is a tiny in-memory shard: prepared holds the value staged by Prepare,
// applied is set only if the batch commits. A mutex guards it because the
// callbacks run in shard goroutines.
type store struct {
	mu       sync.Mutex
	prepared int
	applied  int
	aborted  bool
}

var names = []string{"accounts", "audit", "ledger"} // sorted for deterministic output

func newStores() map[string]*store {
	m := make(map[string]*store, len(names))
	for _, n := range names {
		m[n] = &store{}
	}
	return m
}

// buildShards stages value 10, 20, 30 into the three shards; the shard named
// fail (if any) votes no by returning an error from Prepare.
func buildShards(stores map[string]*store, fail string) []commit.Shard {
	shards := make([]commit.Shard, 0, len(names))
	for i, name := range names {
		st := stores[name]
		staged := (i + 1) * 10
		shards = append(shards, commit.Shard{
			Name: name,
			Prepare: func() error {
				if name == fail {
					return errors.New("disk full")
				}
				st.mu.Lock()
				st.prepared = staged // written before the vote is sent
				st.mu.Unlock()
				return nil
			},
			OnCommit: func() {
				st.mu.Lock()
				st.applied = st.prepared // reads the published prepared value
				st.mu.Unlock()
			},
			OnAbort: func() {
				st.mu.Lock()
				st.aborted = true
				st.mu.Unlock()
			},
		})
	}
	return shards
}

func report(label string, stores map[string]*store, err error) {
	fmt.Printf("%s: err=%v\n", label, err)
	for _, name := range names {
		st := stores[name]
		st.mu.Lock()
		fmt.Printf("  %-8s applied=%d aborted=%v\n", name, st.applied, st.aborted)
		st.mu.Unlock()
	}
}

func main() {
	// Happy path: every shard votes yes, so every shard commits.
	good := newStores()
	errCommit := commit.Commit(buildShards(good, ""))
	report("commit", good, errCommit)

	// One shard fails Prepare: the whole batch aborts, nothing is applied.
	bad := newStores()
	errAbort := commit.Commit(buildShards(bad, "ledger"))
	report("abort ", bad, errAbort)

	fmt.Println("names failing shard:", strings.Contains(errAbort.Error(), "ledger"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
commit: err=<nil>
  accounts applied=10 aborted=false
  audit    applied=20 aborted=false
  ledger   applied=30 aborted=false
abort : err=shard "ledger" prepare failed: disk full
  accounts applied=0 aborted=true
  audit    applied=0 aborted=true
  ledger   applied=0 aborted=true
names failing shard: true
```

### Tests

`TestAllYesCommitsEveryShard` asserts the happy path: `OnCommit` fires exactly `n`
times, `OnAbort` zero, and the error is nil. `TestOneFailingPrepareAbortsAll`
plants a single no vote and checks that every shard aborts, none commits, and the
returned error names the failing shard. `TestDecisionReachesEachShardExactlyOnce`
gives every shard its own counter and asserts each is incremented exactly once —
the direct proof of no double-commit and no missed shard.
`TestFailingPositionAlwaysAborts` is table-driven over the failing index so a no
vote at any position aborts the whole batch, proving the outcome is
order-independent. `TestPreparedStateVisibleAfterCommit` writes shard-local state
in `Prepare` with no lock and reads it in `OnCommit`, relying solely on the
channel happens-before chain; under `-race` it also proves there is no data race.
`TestJoinedErrorCarriesEveryFailure` checks that `errors.Is` finds every failing
shard's sentinel in the joined result. `TestConcurrentBatches` runs many
independent commits at once. Counters use `atomic.Int64` because the callbacks run
concurrently across shard goroutines.

Create `commit_test.go`:

```go
package commit

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// counters tracks how many times each callback fired across all shards.
type counters struct {
	commits atomic.Int64
	aborts  atomic.Int64
}

// yesShard votes yes and bumps the shared counters on the outcome.
func yesShard(name string, c *counters) Shard {
	return Shard{
		Name:     name,
		Prepare:  func() error { return nil },
		OnCommit: func() { c.commits.Add(1) },
		OnAbort:  func() { c.aborts.Add(1) },
	}
}

// noShard votes no with a distinctive error.
func noShard(name string, c *counters) Shard {
	return Shard{
		Name:     name,
		Prepare:  func() error { return fmt.Errorf("prepare rejected") },
		OnCommit: func() { c.commits.Add(1) },
		OnAbort:  func() { c.aborts.Add(1) },
	}
}

func TestAllYesCommitsEveryShard(t *testing.T) {
	t.Parallel()

	var c counters
	const n = 5
	shards := make([]Shard, n)
	for i := range shards {
		shards[i] = yesShard(fmt.Sprintf("s%d", i), &c)
	}

	if err := Commit(shards); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
	if got := c.commits.Load(); got != n {
		t.Fatalf("OnCommit fired %d times, want %d", got, n)
	}
	if got := c.aborts.Load(); got != 0 {
		t.Fatalf("OnAbort fired %d times, want 0", got)
	}
}

func TestOneFailingPrepareAbortsAll(t *testing.T) {
	t.Parallel()

	var c counters
	shards := []Shard{
		yesShard("accounts", &c),
		noShard("ledger", &c),
		yesShard("audit", &c),
	}

	err := Commit(shards)
	if err == nil {
		t.Fatal("Commit = nil, want an error naming the failing shard")
	}
	if !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("error %q does not name the failing shard ledger", err.Error())
	}
	if got := c.aborts.Load(); got != int64(len(shards)) {
		t.Fatalf("OnAbort fired %d times, want %d", got, len(shards))
	}
	if got := c.commits.Load(); got != 0 {
		t.Fatalf("OnCommit fired %d times, want 0", got)
	}
}

func TestDecisionReachesEachShardExactlyOnce(t *testing.T) {
	t.Parallel()

	// Each shard records how many times it was told the outcome. Any value
	// other than 1 means a double-commit or a missed shard.
	const n = 8
	deliveries := make([]atomic.Int64, n)
	shards := make([]Shard, n)
	for i := range shards {
		i := i
		shards[i] = Shard{
			Name:     fmt.Sprintf("s%d", i),
			Prepare:  func() error { return nil },
			OnCommit: func() { deliveries[i].Add(1) },
			OnAbort:  func() { deliveries[i].Add(1) },
		}
	}

	if err := Commit(shards); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
	for i := range deliveries {
		if got := deliveries[i].Load(); got != 1 {
			t.Fatalf("shard %d received the decision %d times, want exactly 1", i, got)
		}
	}
}

func TestFailingPositionAlwaysAborts(t *testing.T) {
	t.Parallel()

	const n = 6
	for fail := range n { // table-driven over the failing index
		t.Run(fmt.Sprintf("fail-at-%d", fail), func(t *testing.T) {
			t.Parallel()
			var c counters
			shards := make([]Shard, n)
			for i := range shards {
				name := fmt.Sprintf("s%d", i)
				if i == fail {
					shards[i] = noShard(name, &c)
				} else {
					shards[i] = yesShard(name, &c)
				}
			}
			err := Commit(shards)
			if err == nil {
				t.Fatalf("fail at %d: Commit = nil, want error", fail)
			}
			if want := fmt.Sprintf("s%d", fail); !strings.Contains(err.Error(), want) {
				t.Fatalf("fail at %d: error %q does not name %q", fail, err.Error(), want)
			}
			if got := c.aborts.Load(); got != n {
				t.Fatalf("fail at %d: OnAbort fired %d times, want %d", fail, got, n)
			}
			if got := c.commits.Load(); got != 0 {
				t.Fatalf("fail at %d: OnCommit fired %d times, want 0", fail, got)
			}
		})
	}
}

func TestPreparedStateVisibleAfterCommit(t *testing.T) {
	t.Parallel()

	// Each shard writes a value in Prepare with no lock, then reads it in
	// OnCommit. Correctness relies solely on the channel happens-before edges:
	// Prepare write -> vote send -> reply receive -> OnCommit read. Under -race
	// this also proves there is no data race on the shared field.
	const n = 16
	prepared := make([]int, n)
	seen := make([]int, n)
	shards := make([]Shard, n)
	for i := range shards {
		i := i
		shards[i] = Shard{
			Name: fmt.Sprintf("s%d", i),
			Prepare: func() error {
				prepared[i] = (i + 1) * 7 // published by the vote send
				return nil
			},
			OnCommit: func() { seen[i] = prepared[i] },
			OnAbort:  func() {},
		}
	}

	if err := Commit(shards); err != nil {
		t.Fatalf("Commit = %v, want nil", err)
	}
	for i := range seen {
		if want := (i + 1) * 7; seen[i] != want {
			t.Fatalf("shard %d saw prepared=%d after commit, want %d", i, seen[i], want)
		}
	}
}

func TestJoinedErrorCarriesEveryFailure(t *testing.T) {
	t.Parallel()

	sentinelA := errors.New("shard A down")
	sentinelB := errors.New("shard B down")
	shards := []Shard{
		{Name: "a", Prepare: func() error { return sentinelA }, OnCommit: func() {}, OnAbort: func() {}},
		{Name: "ok", Prepare: func() error { return nil }, OnCommit: func() {}, OnAbort: func() {}},
		{Name: "b", Prepare: func() error { return sentinelB }, OnCommit: func() {}, OnAbort: func() {}},
	}

	err := Commit(shards)
	if !errors.Is(err, sentinelA) || !errors.Is(err, sentinelB) {
		t.Fatalf("joined error %v must wrap both failing shard errors", err)
	}
}

func TestEmptyBatchCommits(t *testing.T) {
	t.Parallel()

	if err := Commit(nil); err != nil {
		t.Fatalf("Commit(nil) = %v, want nil", err)
	}
}

// TestConcurrentBatches runs many independent Commit calls at once to shake out
// cross-batch interference under -race.
func TestConcurrentBatches(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			var c counters
			shards := []Shard{yesShard("x", &c), yesShard("y", &c), yesShard("z", &c)}
			if err := Commit(shards); err != nil {
				t.Errorf("Commit = %v, want nil", err)
			}
		})
	}
	wg.Wait()
}
```

## Review

`Commit` is correct when the outcome is atomic: on the all-yes path every shard's
`OnCommit` runs and no `OnAbort` does, and on any-no every shard aborts and none
commits. What guarantees it is the barrier structure — the coordinator receives
all `n` votes before it decides anything, so no shard can act on a partial view,
and the single decision is fanned back on per-shard reply channels so each shard
is told exactly once. The exactly-once test proves there is no double-delivery or
missed shard, the table-driven position test proves the abort is
order-independent, and the memory-publish test (clean under `-race`) proves the
prepared state is visible through the channel handoffs alone, with no lock. The
production bug this prevents is the half-applied write: without the barrier, a
shard that prepares successfully applies immediately, and a peer's later failure
leaves the system inconsistent with no atomic rollback. Collect every vote, decide
once, then broadcast the decision — that is what makes independent goroutines
agree.

## Resources

- [The Go Memory Model](https://go.dev/ref/mem) — the channel happens-before rules that make the prepare-then-vote publish and the reply-then-commit visibility correct without a lock.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — request/reply with a private reply channel per caller, the shape used to fan the decision back.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining every failing shard's error into the single value Commit returns on abort.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — waiting for all shard goroutines' callbacks to finish before Commit returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-hedged-replica-read-leak-free.md](12-hedged-replica-read-leak-free.md) | Next: [../03-buffered-vs-unbuffered-channels/00-concepts.md](../03-buffered-vs-unbuffered-channels/00-concepts.md)
