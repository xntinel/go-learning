# Exercise 12: Three severities of bad data, three different exits

**Nivel: Intermedio** — validacion rapida (un test corto).

A payment settlement job sums line items across transactions. Not all bad data
deserves the same response: a single malformed amount should not sink its whole
transaction, a bad transaction header should not sink the whole run, and a
stream-corruption sentinel should stop everything immediately, because nothing
after it can be trusted.

## What you'll build

```text
settlement/                  independent module: example.com/settlement
  go.mod                     go 1.24
  settlement.go               Item, Transaction, ProcessSettlement
  settlement_test.go          table test: normal, bad header, bad item, poison
```

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/12-settlement-batch-poison-abort
cd go-solutions/03-control-flow/06-labels-break-continue-goto/12-settlement-batch-poison-abort
go mod edit -go=1.24
```

Create `settlement.go`:

```go
package settlement

// Item is one line inside a transaction. IsHeader marks the transaction's
// checksum record; PoisonBatch marks a stream-corruption sentinel that
// invalidates everything after it, not just the current transaction.
type Item struct {
	IsHeader    bool
	ChecksumOK  bool
	PoisonBatch bool
	Amount      int
}

// Transaction groups the items that must be committed or skipped together.
type Transaction struct {
	ID    string
	Items []Item
}

// ProcessSettlement sums Amount across every valid item in every valid
// transaction. Three different severities of bad data are handled at three
// different granularities:
//   - a single item with a negative Amount is skipped on its own (plain
//     continue); the rest of its transaction still commits.
//   - a transaction whose header checksum is wrong is skipped WHOLESALE
//     (labeled continue): none of its items count, valid or not.
//   - a PoisonBatch item means the entire stream is untrustworthy from that
//     point on: processing stops immediately (labeled break), and no later
//     transaction is even inspected.
func ProcessSettlement(txns []Transaction) (total, processedCount, skippedTxns int, aborted bool) {
run:
	for _, txn := range txns {
		for _, item := range txn.Items {
			if item.PoisonBatch {
				aborted = true
				break run
			}
			if item.IsHeader {
				if !item.ChecksumOK {
					skippedTxns++
					continue run
				}
				continue
			}
			if item.Amount < 0 {
				continue
			}
			total += item.Amount
			processedCount++
		}
	}
	return total, processedCount, skippedTxns, aborted
}
```

### Reading the three exits in order of reach

All three decisions live inside the same items loop, but they reach different
distances. Plain `continue` (on a negative amount) only skips one item — the
transaction keeps committing everything else. `continue run` (on a bad header)
skips the rest of the CURRENT transaction's items and resumes at the next
transaction. `break run` (on a poison item) leaves both loops outright — no
later transaction, however clean, is ever looked at. The header check has to
sit inside the items loop (the header is itself an item), which is exactly why
a label — not a bare `continue` — is required to reach the transactions loop.

Create `settlement_test.go`:

```go
package settlement

import "testing"

func header(ok bool) Item { return Item{IsHeader: true, ChecksumOK: ok} }

func TestProcessSettlement(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		txns            []Transaction
		wantTotal       int
		wantProcessed   int
		wantSkippedTxns int
		wantAborted     bool
	}{
		"all valid transactions sum normally": {
			txns: []Transaction{
				{ID: "t1", Items: []Item{header(true), {Amount: 10}, {Amount: 5}}},
				{ID: "t2", Items: []Item{header(true), {Amount: 3}}},
			},
			wantTotal: 18, wantProcessed: 3,
		},
		"bad header skips the whole transaction, valid ones still count": {
			txns: []Transaction{
				{ID: "t1", Items: []Item{header(false), {Amount: 100}, {Amount: 200}}},
				{ID: "t2", Items: []Item{header(true), {Amount: 7}}},
			},
			wantTotal: 7, wantProcessed: 1, wantSkippedTxns: 1,
		},
		"a single negative item is skipped, siblings still count": {
			txns: []Transaction{
				{ID: "t1", Items: []Item{header(true), {Amount: 10}, {Amount: -5}, {Amount: 2}}},
			},
			wantTotal: 12, wantProcessed: 2,
		},
		"poison item aborts the whole run, later transactions untouched": {
			txns: []Transaction{
				{ID: "t1", Items: []Item{header(true), {Amount: 10}}},
				{ID: "t2", Items: []Item{header(true), {Amount: 999}, {PoisonBatch: true}, {Amount: 1}}},
				{ID: "t3", Items: []Item{header(true), {Amount: 500}}},
			},
			wantTotal: 1009, wantProcessed: 2, wantAborted: true,
		},
		"poison as the very first item aborts before anything counts": {
			txns: []Transaction{
				{ID: "t1", Items: []Item{{PoisonBatch: true}}},
				{ID: "t2", Items: []Item{header(true), {Amount: 500}}},
			},
			wantTotal: 0, wantProcessed: 0, wantAborted: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			total, processed, skipped, aborted := ProcessSettlement(tc.txns)
			if total != tc.wantTotal || processed != tc.wantProcessed || skipped != tc.wantSkippedTxns || aborted != tc.wantAborted {
				t.Fatalf("ProcessSettlement() = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
					total, processed, skipped, aborted,
					tc.wantTotal, tc.wantProcessed, tc.wantSkippedTxns, tc.wantAborted)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

Correctness here means each severity of bad data stops at exactly the
boundary it should: a bad amount at the item boundary, a bad header at the
transaction boundary, and a poison marker at the whole-run boundary. The
"poison mid-run" test is the sharpest check — it plants a valid, countable
amount right before the poison item in the SAME transaction, and that amount
must still count, because the abort only applies from the poison item
onward, not retroactively.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` on a `for` leaves that loop.
- [Effective Go: For](https://go.dev/doc/effective_go#for) — loop shapes and idiomatic early exits.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-region-failover-labeled-continue-break.md](11-region-failover-labeled-continue-break.md) | Next: [13-log-severity-scan-labeled-break.md](13-log-severity-scan-labeled-break.md)
