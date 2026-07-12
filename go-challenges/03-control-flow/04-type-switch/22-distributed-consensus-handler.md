# Exercise 22: Dispatch Raft Consensus RPCs with Term and Log Matching

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A Raft follower receives three kinds of RPCs from its cluster peers:
`AppendEntries` (log replication, and the heartbeat when it carries no
entries), `RequestVote` (a candidate asking to be elected leader), and
`InstallSnapshot` (a leader forcing a lagging follower to catch up
wholesale). All three share the same term rule from the Raft paper, but
diverge completely after that — `AppendEntries` needs a log-matching check,
`RequestVote` needs a log-freshness check plus a one-vote-per-term guard,
and `InstallSnapshot` replaces the log outright. A real follower serves
these RPCs concurrently from many peers, so the handler must also be safe
under concurrent calls.

## What you'll build

```text
distributed-consensus-handler/  independent module: example.com/distributed-consensus-handler
  go.mod                         go 1.24
  raftfollower.go                (*Node).Handle(rpc any) (any, error)
  cmd/
    demo/
      main.go                    drives a vote, a heartbeat, and a second vote request
  raftfollower_test.go            table of cases per RPC kind, plus a concurrency test
```

- Files: `raftfollower.go`, `cmd/demo/main.go`, `raftfollower_test.go`.
- Implement: `(*Node).Handle(rpc any) (any, error)`, type-switching on
  `AppendEntriesArgs`, `RequestVoteArgs`, and `InstallSnapshotArgs`, guarded
  by a mutex so `Handle` is safe to call from many goroutines at once.
- Test: stale-term rejection, term step-down, log-matching acceptance and
  rejection, vote granting and log-freshness rejection, an unsupported RPC
  type, and many goroutines racing to request a vote in the same term.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/22-distributed-consensus-handler/cmd/demo
cd go-solutions/03-control-flow/04-type-switch/22-distributed-consensus-handler
go mod edit -go=1.24
```

The term rule — reject anything from an older term; step down and adopt a
newer term *before* processing the rest of the RPC — is identical across
all three RPC kinds, so `Handle` applies it once per case rather than
duplicating it three times and risking one copy drifting from the others.
After that shared prefix, each RPC's logic is genuinely different.
`AppendEntries` walks the follower's own log at `PrevLogIndex` and compares
`PrevLogTerm`: if the follower is missing that entry, or holds a different
term there, replication fails and the leader must back up and retry with an
earlier index — this is the log-matching property that makes Raft's log
converge. `RequestVote` grants at most one vote per term (`VotedFor`) and
additionally refuses a candidate whose log is behind the follower's own —
Raft's leader-completeness property depends on never electing a leader
whose log is missing entries a majority already has. `InstallSnapshot`
skips both checks: a snapshot is the leader unilaterally overwriting stale
follower state, not something the follower gets to negotiate over.
Locking inside `Handle` — not at the call site — is what the concurrency
test actually exercises: many goroutines calling `Handle` concurrently for
distinct `RequestVote` candidates in the same term must still grant exactly
one vote, which only holds if the read-check-write around `VotedFor` is
atomic.

Create `raftfollower.go`:

```go
package raftfollower

import (
	"fmt"
	"sync"
)

// LogEntry is one committed-or-pending entry in a Raft replicated log.
type LogEntry struct {
	Term    uint64
	Command string
}

// Node is a single Raft follower's local state. All access goes through
// Handle, which takes the lock, so Node is safe to call concurrently from
// many RPC-serving goroutines, as a real follower must be.
type Node struct {
	mu          sync.Mutex
	CurrentTerm uint64
	VotedFor    string
	Log         []LogEntry
}

// AppendEntriesArgs replicates log entries from the leader, or serves as a
// heartbeat when Entries is empty.
type AppendEntriesArgs struct {
	Term         uint64
	LeaderID     string
	PrevLogIndex uint64 // 0 means "no previous entry"; entry N is Log[N-1]
	PrevLogTerm  uint64
	Entries      []string
}

// AppendEntriesReply tells the leader whether replication succeeded and the
// follower's current term, so a stale leader can step down.
type AppendEntriesReply struct {
	Term    uint64
	Success bool
}

// RequestVoteArgs asks this follower to vote for a candidate in a new
// election term.
type RequestVoteArgs struct {
	Term         uint64
	CandidateID  string
	LastLogIndex uint64
	LastLogTerm  uint64
}

// RequestVoteReply carries the vote decision and the follower's term.
type RequestVoteReply struct {
	Term        uint64
	VoteGranted bool
}

// InstallSnapshotArgs replaces this follower's entire log with a leader
// snapshot, used when the leader has already compacted entries the follower
// is missing.
type InstallSnapshotArgs struct {
	Term              uint64
	LeaderID          string
	LastIncludedIndex uint64
	LastIncludedTerm  uint64
	Data              []byte
}

// InstallSnapshotReply carries the follower's term back to the leader.
type InstallSnapshotReply struct {
	Term uint64
}

// Handle dispatches one incoming RPC to its follower-side logic by concrete
// type. All three RPCs share the same term rule from the Raft paper (reject
// anything from an older term; step down and adopt a newer term before
// processing), but differ completely after that: AppendEntries requires a
// log-matching check, RequestVote requires an up-to-date-log check plus a
// one-vote-per-term guard, and InstallSnapshot replaces the log outright.
// Centralizing the term rule here, ahead of the switch, keeps it from being
// duplicated — and from drifting — across three handlers.
func (n *Node) Handle(rpc any) (any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	switch args := rpc.(type) {
	case AppendEntriesArgs:
		if args.Term < n.CurrentTerm {
			return AppendEntriesReply{Term: n.CurrentTerm, Success: false}, nil
		}
		if args.Term > n.CurrentTerm {
			n.CurrentTerm = args.Term
			n.VotedFor = ""
		}
		if args.PrevLogIndex > uint64(len(n.Log)) {
			return AppendEntriesReply{Term: n.CurrentTerm, Success: false}, nil
		}
		if args.PrevLogIndex > 0 && n.Log[args.PrevLogIndex-1].Term != args.PrevLogTerm {
			return AppendEntriesReply{Term: n.CurrentTerm, Success: false}, nil
		}
		n.Log = n.Log[:args.PrevLogIndex]
		for _, cmd := range args.Entries {
			n.Log = append(n.Log, LogEntry{Term: args.Term, Command: cmd})
		}
		return AppendEntriesReply{Term: n.CurrentTerm, Success: true}, nil

	case RequestVoteArgs:
		if args.Term < n.CurrentTerm {
			return RequestVoteReply{Term: n.CurrentTerm, VoteGranted: false}, nil
		}
		if args.Term > n.CurrentTerm {
			n.CurrentTerm = args.Term
			n.VotedFor = ""
		}
		lastIndex, lastTerm := n.lastLogInfo()
		logUpToDate := args.LastLogTerm > lastTerm ||
			(args.LastLogTerm == lastTerm && args.LastLogIndex >= lastIndex)
		alreadyVotedElsewhere := n.VotedFor != "" && n.VotedFor != args.CandidateID
		if !logUpToDate || alreadyVotedElsewhere {
			return RequestVoteReply{Term: n.CurrentTerm, VoteGranted: false}, nil
		}
		n.VotedFor = args.CandidateID
		return RequestVoteReply{Term: n.CurrentTerm, VoteGranted: true}, nil

	case InstallSnapshotArgs:
		if args.Term < n.CurrentTerm {
			return InstallSnapshotReply{Term: n.CurrentTerm}, nil
		}
		if args.Term > n.CurrentTerm {
			n.CurrentTerm = args.Term
			n.VotedFor = ""
		}
		n.Log = []LogEntry{{Term: args.LastIncludedTerm, Command: "<snapshot>"}}
		return InstallSnapshotReply{Term: n.CurrentTerm}, nil

	default:
		return nil, fmt.Errorf("raftfollower: unsupported RPC type %T", rpc)
	}
}

// lastLogInfo returns the index and term of the last log entry, or (0, 0)
// for an empty log.
func (n *Node) lastLogInfo() (uint64, uint64) {
	if len(n.Log) == 0 {
		return 0, 0
	}
	last := n.Log[len(n.Log)-1]
	return uint64(len(n.Log)), last.Term
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/distributed-consensus-handler"
)

func main() {
	n := &raftfollower.Node{}

	rpcs := []any{
		raftfollower.RequestVoteArgs{Term: 1, CandidateID: "node-b"},
		raftfollower.AppendEntriesArgs{Term: 1, LeaderID: "node-b", Entries: []string{"set x=1"}},
		raftfollower.RequestVoteArgs{Term: 1, CandidateID: "node-c"},
	}
	for _, rpc := range rpcs {
		reply, err := n.Handle(rpc)
		if err != nil {
			fmt.Printf("%T rejected: %v\n", rpc, err)
			continue
		}
		fmt.Printf("%T -> %+v\n", rpc, reply)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
raftfollower.RequestVoteArgs -> {Term:1 VoteGranted:true}
raftfollower.AppendEntriesArgs -> {Term:1 Success:true}
raftfollower.RequestVoteArgs -> {Term:1 VoteGranted:false}
```

The third RPC is rejected not because of the one-vote-per-term rule — it is
a different candidate in the same term — but because after the
`AppendEntries` call the follower's log has one entry at term 1, and
`node-c`'s vote request claims an empty log (`LastLogIndex: 0, LastLogTerm:
0`), which is no longer at least as up to date as the follower's own log.

### Tests

Node values are never copied in this test file — each test constructs a
fresh `&Node{...}` — because `Node` embeds a `sync.Mutex`, and copying a
value that contains one is a correctness bug `go vet` catches on sight.

Create `raftfollower_test.go`:

```go
package raftfollower

import (
	"fmt"
	"sync"
	"testing"
)

func TestHandleAppendEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		currentTerm uint64
		log         []LogEntry
		args        AppendEntriesArgs
		want        AppendEntriesReply
		wantLogN    int
	}{
		{
			name:        "stale leader term is rejected",
			currentTerm: 5,
			args:        AppendEntriesArgs{Term: 3},
			want:        AppendEntriesReply{Term: 5, Success: false},
		},
		{
			name:        "newer term steps down and heartbeat matches empty log",
			currentTerm: 1,
			args:        AppendEntriesArgs{Term: 2, PrevLogIndex: 0, PrevLogTerm: 0},
			want:        AppendEntriesReply{Term: 2, Success: true},
		},
		{
			name:        "prev log index beyond local log is rejected",
			currentTerm: 2,
			log:         []LogEntry{{Term: 1, Command: "a"}},
			args:        AppendEntriesArgs{Term: 2, PrevLogIndex: 5, PrevLogTerm: 1},
			want:        AppendEntriesReply{Term: 2, Success: false},
		},
		{
			name:        "prev log term mismatch is rejected",
			currentTerm: 2,
			log:         []LogEntry{{Term: 1, Command: "a"}},
			args:        AppendEntriesArgs{Term: 2, PrevLogIndex: 1, PrevLogTerm: 9},
			want:        AppendEntriesReply{Term: 2, Success: false},
		},
		{
			name:        "matching prev log accepts and appends new entries",
			currentTerm: 2,
			log:         []LogEntry{{Term: 1, Command: "a"}},
			args:        AppendEntriesArgs{Term: 2, PrevLogIndex: 1, PrevLogTerm: 1, Entries: []string{"b", "c"}},
			want:        AppendEntriesReply{Term: 2, Success: true},
			wantLogN:    3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := &Node{CurrentTerm: tt.currentTerm, Log: tt.log}
			reply, err := n.Handle(tt.args)
			if err != nil {
				t.Fatalf("Handle: unexpected error %v", err)
			}
			got, ok := reply.(AppendEntriesReply)
			if !ok {
				t.Fatalf("reply type = %T, want AppendEntriesReply", reply)
			}
			if got != tt.want {
				t.Fatalf("Handle(%+v) = %+v, want %+v", tt.args, got, tt.want)
			}
			if tt.wantLogN != 0 && len(n.Log) != tt.wantLogN {
				t.Fatalf("log length = %d, want %d", len(n.Log), tt.wantLogN)
			}
		})
	}
}

func TestHandleRequestVote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		currentTerm uint64
		votedFor    string
		log         []LogEntry
		args        RequestVoteArgs
		want        RequestVoteReply
	}{
		{
			name:        "stale candidate term is rejected",
			currentTerm: 5,
			args:        RequestVoteArgs{Term: 4, CandidateID: "c1"},
			want:        RequestVoteReply{Term: 5, VoteGranted: false},
		},
		{
			name:        "grants vote when log at least as up to date",
			currentTerm: 1,
			args:        RequestVoteArgs{Term: 2, CandidateID: "c1", LastLogIndex: 0, LastLogTerm: 0},
			want:        RequestVoteReply{Term: 2, VoteGranted: true},
		},
		{
			name:        "rejects candidate with stale log",
			currentTerm: 2,
			log:         []LogEntry{{Term: 2, Command: "a"}},
			args:        RequestVoteArgs{Term: 3, CandidateID: "c1", LastLogIndex: 0, LastLogTerm: 0},
			want:        RequestVoteReply{Term: 3, VoteGranted: false},
		},
		{
			name:        "rejects second candidate in the same term",
			currentTerm: 3,
			votedFor:    "c1",
			args:        RequestVoteArgs{Term: 3, CandidateID: "c2", LastLogIndex: 0, LastLogTerm: 0},
			want:        RequestVoteReply{Term: 3, VoteGranted: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := &Node{CurrentTerm: tt.currentTerm, VotedFor: tt.votedFor, Log: tt.log}
			reply, err := n.Handle(tt.args)
			if err != nil {
				t.Fatalf("Handle: unexpected error %v", err)
			}
			got, ok := reply.(RequestVoteReply)
			if !ok {
				t.Fatalf("reply type = %T, want RequestVoteReply", reply)
			}
			if got != tt.want {
				t.Fatalf("Handle(%+v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestHandleUnsupportedRPC(t *testing.T) {
	t.Parallel()
	n := &Node{}
	if _, err := n.Handle("not-an-rpc"); err == nil {
		t.Fatal("expected error for unsupported RPC type")
	}
}

// TestConcurrentVotesGrantExactlyOnePerTerm fires many concurrent
// RequestVote RPCs for the same term from distinct candidates at one node.
// Raft's safety property requires at most one vote per term; the mutex
// inside Handle is what makes that true under real concurrent RPC delivery
// instead of just in a single-goroutine unit test.
func TestConcurrentVotesGrantExactlyOnePerTerm(t *testing.T) {
	n := &Node{CurrentTerm: 1}
	const candidates = 50

	var wg sync.WaitGroup
	granted := make([]bool, candidates)
	for i := 0; i < candidates; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reply, err := n.Handle(RequestVoteArgs{
				Term:        2,
				CandidateID: fmt.Sprintf("candidate-%d", i),
			})
			if err != nil {
				t.Errorf("Handle: unexpected error %v", err)
				return
			}
			granted[i] = reply.(RequestVoteReply).VoteGranted
		}(i)
	}
	wg.Wait()

	count := 0
	for _, g := range granted {
		if g {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("granted %d votes in term 2, want exactly 1", count)
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Handle` is correct because the term rule is checked and applied exactly
once, ahead of the three-way switch, so stepping down to a newer term can
never be forgotten in one branch while present in another — that
duplication is exactly the kind of drift that produces a follower which
handles `AppendEntries` correctly but has a stale `RequestVote` bug. The
log-matching and log-freshness checks are the properties that make Raft
safe at all: skip the `PrevLogTerm` comparison in `AppendEntries` and a
follower can accept a leader's entries onto a log that has diverged from
the majority; skip the log-freshness comparison in `RequestVote` and a
candidate missing committed entries can be elected leader and silently
lose them. The concurrency test is the part most likely to be skipped under
time pressure, and it is the part that would have caught a bug that only
unit tests calling `Handle` sequentially cannot: a read of `VotedFor`
outside the lock, or a lock acquired after the check instead of before it,
both of which let two candidates race to a granted vote in the same term.

## Resources

- [Raft: In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf)
- [The Raft Consensus Algorithm (raft.github.io)](https://raft.github.io/)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-state-machine-event-processor.md](21-state-machine-event-processor.md) | Next: [23-cache-invalidation-broadcaster.md](23-cache-invalidation-broadcaster.md)
