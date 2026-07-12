# Exercise 24: Replay Heterogeneous Log Entries During Crash Recovery

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A database's recovery process rebuilds its in-memory state after a crash by
replaying its write-ahead log from the beginning: a `BeginTxn` entry opens a
transaction, `Write` entries buffer key/value changes under it, and
`Commit` or `Abort` entries finalize it. Three failure modes make this
harder than a straight replay. A crash can truncate the log mid-write, so a
`Write` entry can appear for a transaction that was never begun or was
already finalized. A crash can happen after `BeginTxn` but before `Commit`
or `Abort`, leaving an orphaned transaction the recovery process must never
apply. And the log itself can contain a duplicated record — the same
`Commit` flushed twice around a crash — that recovery must apply exactly
once, not twice.

## What you'll build

```text
transaction-log-recovery/   independent module: example.com/transaction-log-recovery
  go.mod                     go 1.24
  txrecovery.go               Recover(entries []any) (map[string]string, error)
  cmd/
    demo/
      main.go                 recovers a committed transaction and an orphaned one
  txrecovery_test.go           table of cases plus a concurrent-replay determinism test
```

- Files: `txrecovery.go`, `cmd/demo/main.go`, `txrecovery_test.go`.
- Implement: `Recover(entries []any) (map[string]string, error)`,
  type-switching on `BeginTxnEntry`, `WriteEntry`, `CommitEntry`, and
  `AbortEntry` inside a private `apply` method that tracks per-transaction
  status.
- Test: a committed transaction becoming visible, an aborted one leaving no
  trace, an orphaned (never-finalized) transaction being invisible, a
  partial write for an unknown transaction being dropped rather than
  erroring, a duplicated commit and a duplicated abort each being an
  idempotent no-op, two interleaved transactions, a reused transaction ID
  while still active being flagged corrupt, a commit against an already
  aborted transaction being flagged corrupt, an unknown entry type, and
  many goroutines replaying the same log concurrently to prove `Recover` is
  a pure function.

Set up the module:

```bash
go mod edit -go=1.24
```

`Recover` builds a fresh, private `recovery` value on every call and never
exposes it — the only thing that ever leaves the function is the final
`kv` snapshot. That is what makes recovery idempotent in the sense the
exercise cares about: replaying the exact same log from scratch, however
many times, from however many goroutines, always produces the same result,
because there is no state left over between calls for a second call to
interact with. Inside one replay, the three failure modes each get their
own deliberate handling. A `WriteEntry` for a transaction that is not
currently `active` — because it was never begun, or because it already
committed or aborted — is silently dropped rather than treated as
`ErrCorruptLog`: a write record trailing off after a truncated append is
an ordinary consequence of a crash, not evidence of a corrupt log, and
erroring on it would make recovery itself unable to survive the exact kind
of crash it exists to recover from. An orphaned transaction needs no
special-cased cleanup at all — it is simply left in `"active"` status
forever, and since only a `Commit` copies `pending` writes into `kv`, its
writes are never surfaced by construction. A duplicated `Commit` or `Abort`
for a transaction already in that terminal status is treated as an
idempotent no-op — applying the same finalization twice must not
double-apply writes — while a `Commit` or `Abort` for a transaction in any
*other* non-active status (reusing an ID, or finalizing one way after
already finalizing it the other way) is `ErrCorruptLog`, because that
combination cannot arise from a truncated append and does indicate real log
corruption.

Create `txrecovery.go`:

```go
package txrecovery

import (
	"errors"
	"fmt"
)

// ErrCorruptLog is the sentinel for a log entry that cannot be reconciled
// with the recovery process's view of transaction state, such as an unknown
// entry type or a transaction ID reused while still active.
var ErrCorruptLog = errors.New("corrupt transaction log")

// BeginTxnEntry opens a new transaction identified by TxnID.
type BeginTxnEntry struct{ TxnID string }

// WriteEntry buffers a key/value write under an open transaction. Writes
// are not visible in the recovered state until their transaction commits.
type WriteEntry struct{ TxnID, Key, Value string }

// CommitEntry makes every buffered write of TxnID visible in the recovered
// state.
type CommitEntry struct{ TxnID string }

// AbortEntry discards every buffered write of TxnID.
type AbortEntry struct{ TxnID string }

const (
	txnActive    = "active"
	txnCommitted = "committed"
	txnAborted   = "aborted"
)

// recovery is the internal, mutable state the log is replayed into. It is
// never exposed directly; Recover always builds one fresh and returns only
// the resulting key/value snapshot, which is what keeps recovery idempotent
// — replaying the same log twice, from two fresh recovery values, always
// produces the same result.
type recovery struct {
	kv      map[string]string
	pending map[string]map[string]string
	status  map[string]string
}

// Recover replays a write-ahead log and returns the key/value state that
// survives a crash. Three failure modes drive the design. A partial write —
// a WriteEntry for a transaction that was never begun, or one truncated
// before its Commit — is buffered but never surfaces in the result, because
// only a committed transaction's writes are copied into kv. An orphaned
// transaction — begun but never committed or aborted before the log ends —
// is left in "active" status forever and its writes are simply never
// copied out, with no special-cased cleanup required. And Commit or Abort
// entries duplicated in the log (a WAL record that got flushed twice around
// a crash) are treated as idempotent no-ops once a transaction has already
// reached that terminal status, since applying the same finalization twice
// must not double-apply writes or error.
func Recover(entries []any) (map[string]string, error) {
	r := &recovery{
		kv:      make(map[string]string),
		pending: make(map[string]map[string]string),
		status:  make(map[string]string),
	}
	for i, entry := range entries {
		if err := r.apply(entry); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return r.kv, nil
}

func (r *recovery) apply(entry any) error {
	switch e := entry.(type) {
	case BeginTxnEntry:
		if status, exists := r.status[e.TxnID]; exists {
			return fmt.Errorf("%w: txn %s already begun (status %s)", ErrCorruptLog, e.TxnID, status)
		}
		r.status[e.TxnID] = txnActive
		r.pending[e.TxnID] = make(map[string]string)
		return nil

	case WriteEntry:
		if r.status[e.TxnID] != txnActive {
			// Not an error: a write for a transaction that was never begun,
			// or one already finalized, is treated as a partial write from
			// a truncated append and simply dropped.
			return nil
		}
		r.pending[e.TxnID][e.Key] = e.Value
		return nil

	case CommitEntry:
		switch r.status[e.TxnID] {
		case txnActive:
			for k, v := range r.pending[e.TxnID] {
				r.kv[k] = v
			}
			delete(r.pending, e.TxnID)
			r.status[e.TxnID] = txnCommitted
			return nil
		case txnCommitted:
			return nil // duplicate commit record; already applied
		default:
			return fmt.Errorf("%w: commit for unknown or non-active txn %s", ErrCorruptLog, e.TxnID)
		}

	case AbortEntry:
		switch r.status[e.TxnID] {
		case txnActive:
			delete(r.pending, e.TxnID)
			r.status[e.TxnID] = txnAborted
			return nil
		case txnAborted:
			return nil // duplicate abort record; already discarded
		default:
			return fmt.Errorf("%w: abort for unknown or non-active txn %s", ErrCorruptLog, e.TxnID)
		}

	default:
		return fmt.Errorf("%w: unknown entry type %T", ErrCorruptLog, entry)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"sort"

	"example.com/transaction-log-recovery"
)

func main() {
	walog := []any{
		txrecovery.BeginTxnEntry{TxnID: "t1"},
		txrecovery.WriteEntry{TxnID: "t1", Key: "balance:alice", Value: "80"},
		txrecovery.WriteEntry{TxnID: "t1", Key: "balance:bob", Value: "120"},
		txrecovery.CommitEntry{TxnID: "t1"},
		txrecovery.BeginTxnEntry{TxnID: "t2"},
		txrecovery.WriteEntry{TxnID: "t2", Key: "balance:alice", Value: "0"},
		// crash here: no Commit or Abort for t2
	}

	kv, err := txrecovery.Recover(walog)
	if err != nil {
		log.Fatal(err)
	}

	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, kv[k])
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
balance:alice=80
balance:bob=120
```

Transaction `t2`'s write never appears: it opened but the log ends before a
`Commit` or `Abort`, so recovery correctly treats the crash as having
happened before that transaction took effect.

### Tests

Create `txrecovery_test.go`:

```go
package txrecovery

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestRecover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []any
		want    map[string]string
		wantErr bool
	}{
		{
			name: "committed transaction is visible",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				CommitEntry{TxnID: "t1"},
			},
			want: map[string]string{"a": "1"},
		},
		{
			name: "aborted transaction leaves no trace",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				AbortEntry{TxnID: "t1"},
			},
			want: map[string]string{},
		},
		{
			name: "orphaned transaction never committed is invisible",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				// log ends here: crash before commit or abort
			},
			want: map[string]string{},
		},
		{
			name: "partial write for a txn never begun is dropped, not an error",
			entries: []any{
				WriteEntry{TxnID: "ghost", Key: "a", Value: "1"},
			},
			want: map[string]string{},
		},
		{
			name: "write after commit is a partial write, dropped",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				CommitEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "b", Value: "2"}, // truncated re-append
			},
			want: map[string]string{"a": "1"},
		},
		{
			name: "duplicate commit record is an idempotent no-op",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				CommitEntry{TxnID: "t1"},
				CommitEntry{TxnID: "t1"},
			},
			want: map[string]string{"a": "1"},
		},
		{
			name: "duplicate abort record is an idempotent no-op",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				AbortEntry{TxnID: "t1"},
				AbortEntry{TxnID: "t1"},
			},
			want: map[string]string{},
		},
		{
			name: "two independent transactions interleave safely",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				BeginTxnEntry{TxnID: "t2"},
				WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
				WriteEntry{TxnID: "t2", Key: "b", Value: "2"},
				CommitEntry{TxnID: "t1"},
				AbortEntry{TxnID: "t2"},
			},
			want: map[string]string{"a": "1"},
		},
		{
			name: "begin reusing an active txn id is corrupt",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				BeginTxnEntry{TxnID: "t1"},
			},
			wantErr: true,
		},
		{
			name: "commit for a txn that was already aborted is corrupt",
			entries: []any{
				BeginTxnEntry{TxnID: "t1"},
				AbortEntry{TxnID: "t1"},
				CommitEntry{TxnID: "t1"},
			},
			wantErr: true,
		},
		{
			name:    "unknown entry type is corrupt",
			entries: []any{"not-an-entry"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Recover(tt.entries)
			if tt.wantErr {
				if !errors.Is(err, ErrCorruptLog) {
					t.Fatalf("Recover err = %v, want ErrCorruptLog", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Recover unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Recover = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRecoverIsIdempotentUnderConcurrentReplay runs the same log through
// Recover from many goroutines at once. Each call builds its own fresh
// internal state, so recovery run concurrently — as it might be during a
// multi-shard restart racing to recover overlapping logs — must still yield
// byte-for-byte the same result every time.
func TestRecoverIsIdempotentUnderConcurrentReplay(t *testing.T) {
	log := []any{
		BeginTxnEntry{TxnID: "t1"},
		WriteEntry{TxnID: "t1", Key: "a", Value: "1"},
		BeginTxnEntry{TxnID: "t2"},
		WriteEntry{TxnID: "t2", Key: "b", Value: "2"},
		CommitEntry{TxnID: "t1"},
		AbortEntry{TxnID: "t2"},
	}
	want := map[string]string{"a": "1"}

	const runs = 50
	var wg sync.WaitGroup
	results := make([]map[string]string, runs)
	errs := make([]error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Recover(log)
		}(i)
	}
	wg.Wait()

	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: unexpected error %v", i, errs[i])
		}
		if !reflect.DeepEqual(results[i], want) {
			t.Fatalf("run %d: Recover = %v, want %v", i, results[i], want)
		}
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Recover` is correct because `kv` is only ever written to from inside the
`CommitEntry` case, which means every other entry kind — an orphaned
`BeginTxn`, a dropped partial `Write`, an `Abort` — is safe by construction:
none of them can leak into the visible state no matter how the log ends.
The nested `switch r.status[e.TxnID]` inside both `CommitEntry` and
`AbortEntry` is what separates "duplicate of the same finalization"
(idempotent no-op) from "any other status" (`ErrCorruptLog`) — collapsing
that into a single `if r.status[e.TxnID] != txnActive` check would silently
treat a corrupt commit-after-abort the same as a harmless duplicate commit,
which is exactly the kind of bug that would only surface once a real crash
produced a log with genuine corruption in it. The concurrent-replay test
is not exercising any lock inside `txrecovery.go` — there is none — it is
exercising the absence of one: `Recover` never shares state across calls,
so it needs no lock, and running it from many goroutines at once is the
proof that this holds under `-race` rather than merely by inspection.

## Resources

- [Write-ahead logging (PostgreSQL documentation)](https://www.postgresql.org/docs/current/wal-intro.html)
- [ARIES: A Transaction Recovery Method](https://cs.stanford.edu/people/chrismre/cs345/rl/aries.pdf)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-cache-invalidation-broadcaster.md](23-cache-invalidation-broadcaster.md) | Next: [25-sliding-window-request-classifier.md](25-sliding-window-request-classifier.md)
