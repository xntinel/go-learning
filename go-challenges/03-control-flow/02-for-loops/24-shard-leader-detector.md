# Exercise 24: Detecting and Electing a Live Leader Shard via Heartbeat

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sharded system that routes writes to a single leader per shard group needs
to notice when that leader stops answering and promote a replacement — and
it needs to do that without hammering a fully down cluster in a tight loop.
This module builds a minimal leader-election state machine: an outer round
loop bounds the total election attempts (a resource budget against a
cluster that might be entirely unreachable), and an inner probe loop, run
once per round, searches the other shards in order for the first one that
answers alive.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
leaderelect/                   module example.com/leaderelect
  go.mod                       go 1.24
  leaderelect.go                Shard; ProbeFunc; Detect(shards, currentLeader, maxRounds, interval, sleep); ErrNoLeader
  leaderelect_test.go             table (current alive, current down, unknown leader, all down), recovers in a later round, budget respected
  cmd/demo/
    main.go                     leader down, one round with nothing alive, then the third shard recovers
```

- Files: `leaderelect.go`, `leaderelect_test.go`, `cmd/demo/main.go`.
- Implement: `Detect(shards []Shard, currentLeader, maxRounds int, interval time.Duration, sleep func(time.Duration)) (int, error)` — a counted outer `for round := 0; round < maxRounds; round++` loop; each round probes the current leader once, and if it is down, an inner `for _, s := range shards` searches the rest for the first live responder, `sleep`ing only when an entire round finds nothing alive.
- Test: the current leader answering alive needs no election; the leader down elects the first alive other shard; an unknown `currentLeader` ID is treated as down; every shard down for the whole budget returns `ErrNoLeader`; a shard that recovers in a later round is elected, and the sleep count matches exactly the number of fully-down rounds; the round budget is honored exactly.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the round budget and the search are two separate loops

The outer loop's job is entirely about *time*: it is the answer to "how many
times do we retry the whole election before giving up," and that is a
resource budget exactly like a retry loop's attempt count — bounded,
visible at the top of the function, paired with a `sleep` so a fully-down
cluster produces a handled `ErrNoLeader` instead of a hot loop. The inner
loop's job is entirely about *order*: given that a round has already
determined the current leader is unreachable, which of the remaining shards
should take over, and the answer is "the first one, in a fixed order, that
answers alive" — a linear search with an immediate `break` the instant one
is found. Collapsing these into one loop would conflate two different
bounds (how many rounds vs. how many shards) into a single counter that
means neither thing clearly. Keeping them nested and separate is also what
makes `sleep` correct: it must fire once per *round* that finds nothing
alive at all, not once per shard probed within a round — `TestDetectRecoversInALaterRound`
is the test that pins down the exact sleep count.

Create `leaderelect.go`:

```go
package leaderelect

import (
	"errors"
	"time"
)

// ErrNoLeader means no shard responded alive within maxRounds.
var ErrNoLeader = errors.New("leaderelect: no live shard found")

// ProbeFunc reports whether a shard is alive right now.
type ProbeFunc func() (alive bool, err error)

// Shard is one node that can act as leader.
type Shard struct {
	ID    int
	Probe ProbeFunc
}

// Detect finds a live leader among shards, starting from currentLeader. If
// the current leader's heartbeat fails (or currentLeader names no known
// shard), it probes every other shard in order and promotes the first one
// that responds alive. It repeats this for up to maxRounds rounds, sleeping
// interval between rounds when an entire round finds nothing alive, so it
// does not hot-loop against a fully down cluster.
//
// This is the nested, timeout-driven loop shape of a simple leader election
// state machine: the outer for bounds the number of election rounds (a
// resource budget against a cluster that might be completely unreachable);
// the inner for, run once per round, is the election search itself -- it
// probes candidates in a fixed order and stops the instant one answers.
func Detect(shards []Shard, currentLeader int, maxRounds int, interval time.Duration, sleep func(time.Duration)) (int, error) {
	leader := currentLeader

	for round := 0; round < maxRounds; round++ {
		if idx := indexOf(shards, leader); idx >= 0 {
			if alive, err := shards[idx].Probe(); err == nil && alive {
				return leader, nil
			}
		}

		elected := -1
		for _, s := range shards {
			if s.ID == leader {
				continue
			}
			if alive, err := s.Probe(); err == nil && alive {
				elected = s.ID
				break
			}
		}

		if elected == -1 {
			sleep(interval)
			continue
		}
		return elected, nil
	}

	return -1, ErrNoLeader
}

func indexOf(shards []Shard, id int) int {
	for i, s := range shards {
		if s.ID == id {
			return i
		}
	}
	return -1
}
```

### The runnable demo

The demo's shard 1 (the current leader) and shard 2 are both down; shard 3
is flaky, reporting down on the first probe and alive from the second probe
onward. The first round finds nothing alive and sleeps once; the second
round's probe of shard 3 succeeds and it is elected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/leaderelect"
)

func main() {
	round := 0
	shards := []leaderelect.Shard{
		{ID: 1, Probe: func() (bool, error) { return false, nil }}, // leader is down
		{ID: 2, Probe: func() (bool, error) { return false, nil }}, // down this round too
		{ID: 3, Probe: func() (bool, error) {
			round++
			return round >= 2, nil // only comes alive on the second round
		}},
	}

	sleeps := 0
	sleep := func(d time.Duration) {
		sleeps++
		fmt.Printf("round had no live shard, sleeping %v\n", d)
	}

	leader, err := leaderelect.Detect(shards, 1, 5, 50*time.Millisecond, sleep)
	if err != nil {
		fmt.Println("election failed:", err)
		return
	}
	fmt.Printf("elected shard %d after %d sleep(s)\n", leader, sleeps)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
round had no live shard, sleeping 50ms
elected shard 3 after 1 sleep(s)
```

### Tests

`TestDetect` is a table covering the leader-alive fast path, a leader-down
election, an unknown `currentLeader` ID (treated identically to "down"), and
every shard down for the whole budget. `TestDetectRecoversInALaterRound` is
the sharpest one — it asserts both the elected shard *and* the exact number
of sleeps, which is the only way to confirm the outer/inner loop boundary is
where `sleep` actually lives. `TestDetectRespectsMaxRoundsBudget` confirms
the round count is exact, not "at least" or "roughly."

Create `leaderelect_test.go`:

```go
package leaderelect

import (
	"errors"
	"testing"
	"time"
)

func alive() (bool, error) { return true, nil }
func dead() (bool, error)  { return false, nil }

func TestDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		shards        func() []Shard
		currentLeader int
		maxRounds     int
		wantLeader    int
		wantErr       error
	}{
		{
			name: "current leader alive: no election needed",
			shards: func() []Shard {
				return []Shard{
					{ID: 1, Probe: alive},
					{ID: 2, Probe: alive},
				}
			},
			currentLeader: 1,
			maxRounds:     3,
			wantLeader:    1,
		},
		{
			name: "current leader down: elects first alive other shard",
			shards: func() []Shard {
				return []Shard{
					{ID: 1, Probe: dead},
					{ID: 2, Probe: dead},
					{ID: 3, Probe: alive},
				}
			},
			currentLeader: 1,
			maxRounds:     3,
			wantLeader:    3,
		},
		{
			name: "unknown current leader treated as down",
			shards: func() []Shard {
				return []Shard{
					{ID: 2, Probe: alive},
				}
			},
			currentLeader: 99,
			maxRounds:     3,
			wantLeader:    2,
		},
		{
			name: "everything down for the whole budget",
			shards: func() []Shard {
				return []Shard{
					{ID: 1, Probe: dead},
					{ID: 2, Probe: dead},
				}
			},
			currentLeader: 1,
			maxRounds:     2,
			wantErr:       ErrNoLeader,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			leader, err := Detect(tc.shards(), tc.currentLeader, tc.maxRounds, time.Millisecond, func(time.Duration) {})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if leader != tc.wantLeader {
				t.Fatalf("leader = %d, want %d", leader, tc.wantLeader)
			}
		})
	}
}

func TestDetectRecoversInALaterRound(t *testing.T) {
	t.Parallel()

	round := 0
	// Shard 2 is down for the first round, then comes back alive.
	flaky := func() (bool, error) {
		round++
		return round >= 2, nil
	}

	shards := []Shard{
		{ID: 1, Probe: dead},
		{ID: 2, Probe: flaky},
	}

	var sleeps int
	sleep := func(time.Duration) { sleeps++ }

	leader, err := Detect(shards, 1, 5, time.Millisecond, sleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if leader != 2 {
		t.Fatalf("leader = %d, want 2", leader)
	}
	if sleeps != 1 {
		t.Fatalf("sleeps = %d, want 1 (one fully-down round before recovery)", sleeps)
	}
}

func TestDetectRespectsMaxRoundsBudget(t *testing.T) {
	t.Parallel()

	probes := 0
	shards := []Shard{
		{ID: 1, Probe: func() (bool, error) { probes++; return false, nil }},
	}

	_, err := Detect(shards, 1, 4, time.Millisecond, func(time.Duration) {})
	if !errors.Is(err, ErrNoLeader) {
		t.Fatalf("err = %v, want ErrNoLeader", err)
	}
	if probes != 4 {
		t.Fatalf("probes = %d, want 4 (exactly maxRounds)", probes)
	}
}
```

## Review

`Detect` is correct when it never elects a shard that has not itself
answered alive, and when it never runs more than `maxRounds` rounds
regardless of how persistently down the cluster is. The common mistake this
design avoids is a single flat loop that tries to probe "the next candidate"
on every iteration without distinguishing rounds — that shape makes it
unclear how many times the *whole cluster* has been checked versus how many
individual shards have been probed, and it is easy to end up sleeping after
every failed probe instead of after every failed round, which turns a
three-shard cluster's down period into three times as many sleeps as
intended. Run `go test -count=1 ./...`.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the nested counted and range forms used here.
- [Raft Consensus Algorithm](https://raft.github.io/) — the leader-heartbeat-and-election model this module is a simplified version of.
- [etcd: Understanding Failure Modes](https://etcd.io/docs/v3.5/learning/failure-modes/) — how a real distributed system reasons about a downed leader.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-task-queue-priority-dequeuer.md](23-task-queue-priority-dequeuer.md) | Next: [25-stream-n-way-merger.md](25-stream-n-way-merger.md)
