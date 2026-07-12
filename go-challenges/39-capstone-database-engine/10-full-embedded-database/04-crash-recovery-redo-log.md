# Exercise 4: Crash Recovery from a Redo Log

The integration layer's recovery routine delegates the real redo and undo work to the log and buffer pool. This exercise builds the kernel of that work as a small, fully offline component so the central guarantee can be exercised directly: after a crash, only committed transactions survive, and uncommitted ones vanish. The "crash" is modeled literally — you append records to an in-memory log, throw away all page state, and reconstruct a fresh page purely by replaying the log, applying an insert only when a commit for the same transaction also appears. That is the ARIES redo pass followed by an undo of loser transactions, reduced to the one property that makes a database trustworthy.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including its own slotted page — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
recovery.go          RecKind, LogRec, RedoLog, Append, Recover, TxID, ErrRecover
page.go              SlottedPage (the page recovery rebuilds into)
cmd/
  demo/
    main.go          replay a log with one committed and one uncommitted txn
recovery_test.go     committed survives, uncommitted discarded, only-committed mix, overfull is an error
```

- Files: `recovery.go`, `page.go`, `cmd/demo/main.go`, `recovery_test.go`.
- Implement: `RedoLog` with `Append`, the `RecKind` constants `RecInsert` and `RecCommit`, `LogRec`, and `Recover(log *RedoLog) (*SlottedPage, error)` that replays into a fresh page, applying an insert only if a commit for its transaction exists.
- Test: `recovery_test.go` proves a committed insert is restored, an uncommitted one is discarded, a mix keeps only the committed transaction's rows, and a tuple too large to replay returns `ErrRecover`.
- Verify: `go test -run 'TestRecover|ExampleRecover' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/10-full-embedded-database/04-crash-recovery-redo-log/cmd/demo && cd go-solutions/39-capstone-database-engine/10-full-embedded-database/04-crash-recovery-redo-log
```

### Why the commit record is the single source of truth

The deep idea behind crash recovery is that durable state is whatever the log says it is, and nothing else. After a crash, you cannot trust the in-memory page cache, because some of its pages reached disk and some did not, and you have no way to tell which. So recovery throws all of it away and rebuilds from the one artifact that was written durably in order: the log. The model here makes that brutal honesty explicit — `Recover` ignores any prior page state entirely and constructs a brand-new `SlottedPage`, replaying the log into it from scratch.

The replay is two conceptual passes folded into one scan. First it walks the log to find which transactions committed, building a set of committed transaction identifiers from the `RecCommit` records. Then it walks the log again and applies each `RecInsert`, but only if its transaction is in the committed set. An insert from a transaction that never committed is a *loser* — its effects may or may not have reached disk before the crash, but because there is no commit record, it is treated as if it never happened and is simply skipped. This is exactly ARIES in miniature: redo brings back every logged change, and the absence of a commit record is what drives the undo of a loser. The commit record, written durably before the engine acknowledged the commit, is the sole arbiter of what survives.

The error path matters as much as the happy path. If a committed insert cannot be replayed — for instance a tuple larger than an entire page, which can never fit — recovery cannot reconstruct a consistent state and must say so by returning `ErrRecover` rather than silently dropping the row. A recovery that quietly discards data it cannot place is worse than one that fails loudly, because it produces a database that looks fine and is missing committed writes. The test pins this: an oversized committed tuple is an error, not a no-op.

Create `recovery.go`:

```go
package recovery

import (
	"errors"
	"fmt"
)

// TxID identifies the transaction that produced a log record.
type TxID uint64

// RecKind classifies a record in the redo log.
type RecKind uint8

const (
	// RecInsert appends a tuple on behalf of a transaction. It is durable only once
	// a matching RecCommit for the same TxID follows it in the log.
	RecInsert RecKind = iota
	// RecCommit marks a transaction as committed. Its earlier RecInsert records
	// become visible; without it those inserts are discarded during recovery.
	RecCommit
)

// LogRec is one entry in the redo log: a typed, transaction-tagged payload.
type LogRec struct {
	TxID    TxID
	Kind    RecKind
	Payload []byte
}

// RedoLog is an append-only in-memory log standing in for the durable WAL. It is
// the single source of truth for recovery: page state is reconstructed entirely by
// replaying it, so nothing outside the log is trusted after a crash.
type RedoLog struct {
	records []LogRec
}

// Append adds a record to the log and returns its sequence number (1-based).
func (l *RedoLog) Append(rec LogRec) uint64 {
	l.records = append(l.records, rec)
	return uint64(len(l.records))
}

// ErrRecover is returned when replay cannot reconstruct state, for example when a
// committed tuple no longer fits in a freshly initialized page.
var ErrRecover = errors.New("recovery failed")

// Recover rebuilds page state from the log into a fresh SlottedPage. A RecInsert is
// applied only if a RecCommit for the same TxID appears anywhere in the log; inserts
// from transactions that never committed are discarded. This mirrors the ARIES redo
// pass followed by an undo of loser transactions, using the commit record as the
// source of truth rather than any in-memory state that did not survive the crash.
func Recover(log *RedoLog) (*SlottedPage, error) {
	committed := make(map[TxID]bool)
	for _, r := range log.records {
		if r.Kind == RecCommit {
			committed[r.TxID] = true
		}
	}
	var p SlottedPage
	p.Init()
	for _, r := range log.records {
		if r.Kind != RecInsert || !committed[r.TxID] {
			continue
		}
		if _, err := p.Insert(r.Payload); err != nil {
			return nil, fmt.Errorf("%w: replay insert tx %d: %v", ErrRecover, r.TxID, err)
		}
	}
	return &p, nil
}
```

The page recovery rebuilds into is the same slotted layout as the storage exercise, carried here so the module stands alone. Recovery uses only `Init` and `Insert`; the tests also read rows back and skip tombstones.

Create `page.go`:

```go
package recovery

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Page layout constants.
const (
	// PageSize is the unit of I/O between the buffer pool and the disk manager.
	PageSize = 4096
	// pageHeaderSize covers the slot count, free-space pointer, and 8-byte page LSN.
	pageHeaderSize = 12
	// slotEntrySize is the byte cost of one slot directory entry: offset (2) + length (2).
	slotEntrySize = 4
)

// Slot is a slot directory entry. A slot whose Length is 0 is a tombstone.
type Slot struct {
	Offset uint16
	Length uint16
}

// SlottedPage implements the slotted-page heap layout recovery rebuilds into.
type SlottedPage struct {
	data [PageSize]byte
}

// Sentinel errors for SlottedPage operations.
var (
	ErrPageFull    = errors.New("page is full")
	ErrInvalidSlot = errors.New("slot index out of range")
	ErrTombstone   = errors.New("slot has been deleted")
)

// Init resets the page to an empty state.
func (p *SlottedPage) Init() {
	for i := range p.data {
		p.data[i] = 0
	}
	p.setFreeSpacePtr(PageSize)
}

// SlotCount returns the number of slot directory entries, including tombstones.
func (p *SlottedPage) SlotCount() int {
	return int(binary.LittleEndian.Uint16(p.data[0:2]))
}

// FreeSpace returns the bytes available for a new tuple plus its slot entry.
func (p *SlottedPage) FreeSpace() int {
	directoryEnd := pageHeaderSize + p.SlotCount()*slotEntrySize
	tupleStart := int(p.freeSpacePtr())
	available := tupleStart - directoryEnd
	if available < 0 {
		return 0
	}
	return available
}

func (p *SlottedPage) freeSpacePtr() uint16 {
	return binary.LittleEndian.Uint16(p.data[2:4])
}

func (p *SlottedPage) setSlotCount(n int) {
	binary.LittleEndian.PutUint16(p.data[0:2], uint16(n))
}

func (p *SlottedPage) setFreeSpacePtr(ptr int) {
	binary.LittleEndian.PutUint16(p.data[2:4], uint16(ptr))
}

func (p *SlottedPage) getSlot(idx int) Slot {
	base := pageHeaderSize + idx*slotEntrySize
	return Slot{
		Offset: binary.LittleEndian.Uint16(p.data[base : base+2]),
		Length: binary.LittleEndian.Uint16(p.data[base+2 : base+4]),
	}
}

func (p *SlottedPage) setSlot(idx int, s Slot) {
	base := pageHeaderSize + idx*slotEntrySize
	binary.LittleEndian.PutUint16(p.data[base:base+2], s.Offset)
	binary.LittleEndian.PutUint16(p.data[base+2:base+4], s.Length)
}

// Insert copies tuple into the page and returns its stable slot index.
func (p *SlottedPage) Insert(tuple []byte) (int, error) {
	needed := len(tuple) + slotEntrySize
	if p.FreeSpace() < needed {
		return 0, ErrPageFull
	}
	newPtr := int(p.freeSpacePtr()) - len(tuple)
	copy(p.data[newPtr:], tuple)
	p.setFreeSpacePtr(newPtr)
	idx := p.SlotCount()
	p.setSlot(idx, Slot{Offset: uint16(newPtr), Length: uint16(len(tuple))})
	p.setSlotCount(idx + 1)
	return idx, nil
}

// Read returns a copy of the tuple at slotIdx.
func (p *SlottedPage) Read(slotIdx int) ([]byte, error) {
	if slotIdx < 0 || slotIdx >= p.SlotCount() {
		return nil, fmt.Errorf("%w: index %d", ErrInvalidSlot, slotIdx)
	}
	s := p.getSlot(slotIdx)
	if s.Length == 0 {
		return nil, ErrTombstone
	}
	out := make([]byte, s.Length)
	copy(out, p.data[s.Offset:int(s.Offset)+int(s.Length)])
	return out, nil
}
```

### The runnable demo

The demo builds a log with one committed transaction and one that never committed, then "crashes" by recovering into a fresh page. The committed row comes back; the uncommitted one is gone — the exact guarantee a database makes about durability.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/crash-recovery"
)

func main() {
	var redo recovery.RedoLog
	redo.Append(recovery.LogRec{TxID: 1, Kind: recovery.RecInsert, Payload: []byte("durable")})
	redo.Append(recovery.LogRec{TxID: 1, Kind: recovery.RecCommit})
	redo.Append(recovery.LogRec{TxID: 2, Kind: recovery.RecInsert, Payload: []byte("lost")})

	// "Crash": rebuild page state purely from the log.
	p, err := recovery.Recover(&redo)
	if err != nil {
		log.Fatalf("recover: %v", err)
	}
	first, _ := p.Read(0)
	fmt.Printf("recovered rows=%d\n", p.SlotCount())
	fmt.Printf("row 0=%q (tx 2 never committed, discarded)\n", first)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered rows=1
row 0="durable" (tx 2 never committed, discarded)
```

### Tests

The table-driven test covers the three visibility outcomes: a committed insert is restored, an uncommitted insert is discarded, and in a mix of two transactions only the committed one's row survives. Each case builds a log, then recovers into a brand-new page so no pre-crash state is carried over, and compares the recovered rows against the expectation. A separate test proves the error contract: a committed tuple as large as a whole page can never be replaced and must return `ErrRecover`.

Create `recovery_test.go`:

```go
package recovery

import (
	"errors"
	"fmt"
	"testing"
)

func TestRecoverVisibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(l *RedoLog)
		want  []string
	}{
		{
			name: "committed insert is restored",
			build: func(l *RedoLog) {
				l.Append(LogRec{TxID: 1, Kind: RecInsert, Payload: []byte("alice")})
				l.Append(LogRec{TxID: 1, Kind: RecCommit})
			},
			want: []string{"alice"},
		},
		{
			name: "uncommitted insert is discarded",
			build: func(l *RedoLog) {
				l.Append(LogRec{TxID: 1, Kind: RecInsert, Payload: []byte("ghost")})
			},
			want: nil,
		},
		{
			name: "only committed transactions survive",
			build: func(l *RedoLog) {
				l.Append(LogRec{TxID: 1, Kind: RecInsert, Payload: []byte("keep")})
				l.Append(LogRec{TxID: 2, Kind: RecInsert, Payload: []byte("drop")})
				l.Append(LogRec{TxID: 1, Kind: RecCommit})
			},
			want: []string{"keep"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Build the log, then "crash" by recovering into a brand-new page:
			// no in-memory state from before the crash is carried over.
			var log RedoLog
			tc.build(&log)
			p, err := Recover(&log)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			var got []string
			for i := 0; i < p.SlotCount(); i++ {
				b, err := p.Read(i)
				if errors.Is(err, ErrTombstone) {
					continue
				}
				if err != nil {
					t.Fatalf("Read(%d): %v", i, err)
				}
				got = append(got, string(b))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("recovered = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecoverOverfullIsError(t *testing.T) {
	t.Parallel()

	var log RedoLog
	// A single tuple larger than a page can never be replayed into one page.
	log.Append(LogRec{TxID: 1, Kind: RecInsert, Payload: make([]byte, PageSize)})
	log.Append(LogRec{TxID: 1, Kind: RecCommit})
	if _, err := Recover(&log); !errors.Is(err, ErrRecover) {
		t.Fatalf("Recover oversized tuple: err = %v, want ErrRecover", err)
	}
}

func ExampleRecover() {
	var log RedoLog
	log.Append(LogRec{TxID: 1, Kind: RecInsert, Payload: []byte("durable")})
	log.Append(LogRec{TxID: 1, Kind: RecCommit})
	log.Append(LogRec{TxID: 2, Kind: RecInsert, Payload: []byte("lost")})
	p, _ := Recover(&log)
	first, _ := p.Read(0)
	fmt.Printf("rows=%d first=%q\n", p.SlotCount(), first)
	// Output:
	// rows=1 first="durable"
}
```

## Review

Recovery is correct when the commit record, and nothing else, decides what survives. Confirm that a committed insert is restored, an uncommitted insert leaves no row, and a mix keeps only the committed transaction's data — and that every case rebuilds into a fresh page so no pre-crash state can leak in. The error contract is the other half: a committed tuple that cannot be replayed must return `ErrRecover` rather than silently vanish, because a recovery that quietly drops committed data hides the very corruption it exists to prevent.

Common mistakes for this kernel. Applying an insert before scanning the whole log for its commit makes the result depend on record order and can resurrect a loser. Trusting any in-memory page state instead of rebuilding from scratch defeats the entire point of replaying the log. Swallowing the insert error during replay turns "cannot reconstruct" into "looks fine but lost a row," the most dangerous failure a database can have.

## Resources

- [ARIES: A Transaction Recovery Method (Mohan et al., 1992)](https://cs.stanford.edu/people/chrismre/cs345/rl/aries.pdf) — the analysis/redo/undo three-pass model this exercise distills, with the commit record as the source of truth.
- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — its recovery section frames why redo replays committed work and undo removes losers.
- [CMU 15-445: Database Systems](https://15445.courses.cs.cmu.edu/) — the Logging and Recovery lectures derive the redo/undo passes and the no-steal/no-force trade-offs behind them.

---

Back to [03-database-integration.md](03-database-integration.md) | Next: [05-heap-file-pages.md](05-heap-file-pages.md)
