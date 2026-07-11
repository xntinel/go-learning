# Exercise 24: WAL Replay with Transaction Deduplication and Ordered Reconstruction

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A write-ahead log guarantees durability by letting a crashed process replay
every logged mutation to reconstruct its last known state, but a real WAL —
or the replication stream shipping it to a follower — can redeliver the same
transaction and can deliver entries out of the order they were assigned. This
module exposes the log as `iter.Seq2[TxnID, Op]`, ranges it once to
deduplicate by transaction ID for idempotent replay, sorts the survivors by
LSN, and applies them in that corrected order to rebuild the final key/value
state — a small state machine over `Set` and `Delete` operations. The module
is fully self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
wal/                        independent module: example.com/write-ahead-log-replayer
  go.mod                    go 1.24
  wal.go                    type Op, Record; Source(records) iter.Seq2[TxnID, Op]; Replay(seq) map[string]string
  cmd/
    demo/
      main.go               runnable demo: out-of-order LSNs, a retried txn, a delete
  wal_test.go               table test: dedup + reordering + delete-of-missing-key edge case
```

- Files: `wal.go`, `cmd/demo/main.go`, `wal_test.go`.
- Implement: `Source(records []Record) iter.Seq2[TxnID, Op]` and
  `Replay(seq iter.Seq2[TxnID, Op]) map[string]string`.
- Test: one table covering out-of-order LSNs with a duplicated transaction,
  plus the edge case of deleting a key that was never set.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/write-ahead-log-replayer/cmd/demo
cd ~/go-exercises/write-ahead-log-replayer
go mod init example.com/write-ahead-log-replayer
go mod edit -go=1.24
```

### Three passes over the same range, each with a single responsibility

`Replay` ranges `seq` exactly once to do the deduplication — the `seen`
map's only job is "have I already applied this TxnID," and the first
occurrence of each transaction is kept in an `ops` slice in whatever order
the source delivered it. That order is explicitly not trusted: a second
step, `sort.Slice` on `LSN`, imposes the log's actual logical order, because
the WAL's arrival order (from disk, from a retry, from a replication
stream) and its assigned sequence number are two different things once
retries and network reordering are possible — exactly the same distinction
the resequencer exercise in this lesson draws between identity and order. A
third, separate loop then applies the sorted operations to build `state`,
so the "what order did the WAL entries arrive in" question is fully
resolved before any mutation touches the reconstructed state at all.

The state machine itself is intentionally small (`OpSet`/`OpDelete`) but has
a real edge case worth exercising directly: replaying an `OpDelete` for a key
that was never `OpSet` in this session. `delete(state, key)` on a missing
key is a documented no-op in Go — it neither panics nor errors — so `Replay`
does not need special-case code for it, but a test still needs to prove it,
because a slightly different implementation (one that tracked delete as an
explicit tombstone value, for instance) could get this wrong.

Create `wal.go`:

```go
package wal

import (
	"iter"
	"sort"
)

// TxnID identifies a transaction; the WAL may contain the same TxnID more
// than once if a producer retried a write, and replay must apply it only
// once.
type TxnID string

// OpType is the kind of mutation a WAL entry represents.
type OpType int

const (
	OpSet OpType = iota
	OpDelete
)

// Op is one WAL-logged mutation, ordered by LSN (log sequence number), not
// by arrival order.
type Op struct {
	LSN   uint64
	Type  OpType
	Key   string
	Value string
}

// Record pairs a transaction ID with the operation it logged.
type Record struct {
	Txn TxnID
	Op  Op
}

// Source adapts a plain slice of Record into a lazy iter.Seq2[TxnID, Op],
// standing in for a real WAL file or replication stream in this exercise.
func Source(records []Record) iter.Seq2[TxnID, Op] {
	return func(yield func(TxnID, Op) bool) {
		for _, r := range records {
			if !yield(r.Txn, r.Op) {
				return
			}
		}
	}
}

// Replay ranges seq, deduplicating by TxnID so a transaction replayed twice
// (a retried write, a re-sent replication batch) is applied exactly once,
// then sorts the surviving entries by LSN before applying them — the WAL's
// arrival order is not the same as its logical order once retries and
// network reordering are in play. It returns the reconstructed key/value
// state.
func Replay(seq iter.Seq2[TxnID, Op]) map[string]string {
	type applied struct {
		txn TxnID
		op  Op
	}
	seen := make(map[TxnID]bool)
	var ops []applied

	for txn, op := range seq {
		if seen[txn] {
			continue // idempotent replay: apply each txn at most once
		}
		seen[txn] = true
		ops = append(ops, applied{txn: txn, op: op})
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].op.LSN < ops[j].op.LSN })

	state := make(map[string]string)
	for _, a := range ops {
		switch a.op.Type {
		case OpSet:
			state[a.op.Key] = a.op.Value
		case OpDelete:
			delete(state, a.op.Key) // no-op cleanup if the key was never set
		}
	}
	return state
}
```

### The runnable demo

The demo feeds four records out of LSN order, with transaction `t1` logged
twice (a retried delivery) and a delete for key `a` logged at the highest
LSN so it must apply last.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/write-ahead-log-replayer"
)

func main() {
	records := []wal.Record{
		{Txn: "t3", Op: wal.Op{LSN: 3, Type: wal.OpDelete, Key: "a"}},
		{Txn: "t1", Op: wal.Op{LSN: 1, Type: wal.OpSet, Key: "a", Value: "1"}},
		{Txn: "t1", Op: wal.Op{LSN: 1, Type: wal.OpSet, Key: "a", Value: "1"}}, // retried
		{Txn: "t2", Op: wal.Op{LSN: 2, Type: wal.OpSet, Key: "b", Value: "2"}},
	}

	state := wal.Replay(wal.Source(records))

	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, state[k])
	}
	fmt.Printf("keys=%d\n", len(state))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
b=2
keys=1
```

Key `a` was set at LSN 1 and deleted at LSN 3 — after LSN sorting corrects
the arrival order, the delete is the last operation applied to `a`, so it
does not appear in the final state at all.

### Tests

The table covers out-of-order LSNs with a duplicated transaction (proving
both dedup and reordering), plus the edge case of deleting a key that was
never set, which must be a silent no-op rather than a panic or spurious
entry.

Create `wal_test.go`:

```go
package wal

import (
	"reflect"
	"testing"
)

func TestReplay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		records []Record
		want    map[string]string
	}{
		{
			name:    "empty log",
			records: nil,
			want:    map[string]string{},
		},
		{
			name: "out of order LSNs, duplicate txn applied once, delete after set",
			records: []Record{
				{Txn: "t3", Op: Op{LSN: 3, Type: OpDelete, Key: "a"}},
				{Txn: "t1", Op: Op{LSN: 1, Type: OpSet, Key: "a", Value: "1"}},
				{Txn: "t1", Op: Op{LSN: 1, Type: OpSet, Key: "a", Value: "1"}}, // retried delivery
				{Txn: "t2", Op: Op{LSN: 2, Type: OpSet, Key: "b", Value: "2"}},
			},
			want: map[string]string{"b": "2"},
		},
		{
			name: "delete of a key that was never set is a no-op",
			records: []Record{
				{Txn: "t1", Op: Op{LSN: 1, Type: OpDelete, Key: "ghost"}},
				{Txn: "t2", Op: Op{LSN: 2, Type: OpSet, Key: "x", Value: "9"}},
			},
			want: map[string]string{"x": "9"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Replay(Source(tc.records))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Replay() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

`Replay` is correct when every distinct transaction is applied exactly once,
in LSN order rather than delivery order, and the resulting state reflects
whichever operation on a given key had the highest LSN. The bug this design
specifically avoids is trusting delivery order as if it were LSN order — a
replayer that applied operations as they arrived, without the intermediate
sort, would get this exact demo's answer wrong: it would apply `t3`'s delete
of `a` first (since it arrived first), then `t1`'s set of `a`, leaving `a`
present in the final state when the true logical order says it should be
gone.

## Resources

- [package iter](https://pkg.go.dev/iter) — `Seq2` and the range-over-func iterator shape.
- [package sort (Slice)](https://pkg.go.dev/sort#Slice)
- [PostgreSQL WAL internals](https://www.postgresql.org/docs/current/wal-internals.html) — background on LSNs and replay-based recovery.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-multi-layer-cache-invalidation.md](23-multi-layer-cache-invalidation.md) | Next: [25-s3-multipart-upload-bufferer.md](25-s3-multipart-upload-bufferer.md)
