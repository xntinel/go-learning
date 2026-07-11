# Exercise 25: Ordered Fan-Out Results

You have a CSV file with 1000 rows, each needing validation, transformation, and
enrichment that takes a variable amount of time. The obvious speedup is fan-out
-- distribute rows across N worker goroutines -- but the output must preserve the
original order: row 5 has to appear before row 6 even when worker 3 finishes row
6 before worker 1 finishes row 5. Naive fan-out destroys that order, because
Go's scheduler guarantees nothing about execution order and faster items overtake
slower ones, and in a streaming pipeline you cannot just sort at the end. This
exercise builds the fix: a resequencer that puts the stream back in order as
results become ready.

## What you'll build

```text
25-ordered-fan-out-results/
  main.go        watch naive fan-out scramble order, tag items with sequence
                 numbers, then resequence -- including gaps from failed items
```

- Build: four programs that progress from unordered fan-out to a resequenced pipeline that survives failed items.
- Implement: sequence-numbered work items, a `resequencer` goroutine that buffers early arrivals in a map and emits strictly in order from `nextExpected`, and workers that always emit a result (marking failures `Skipped`) so the resequencer never stalls.
- Verify: `go run main.go` (Steps 3-4 are worth `go run -race main.go`)

### Why a resequencer instead of a final sort

The solution is a resequencer: each item carries a sequence number assigned
before fan-out. Workers process items in any order but tag results with the
original sequence. A resequencer goroutine collects results, buffers those that
arrive early, and emits items strictly in sequence. This is the same pattern
used by TCP to reassemble out-of-order packets and by database engines to order
concurrent transaction results.

## Step 1 -- Unordered Fan-Out: Observe the Scrambling

Start by fanning out work without any ordering. Observe how results arrive in unpredictable order.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	rowCount    = 12
	workerCount = 4
)

// Row represents a CSV row to process.
type Row struct {
	LineNum int
	Data    string
}

// ProcessedRow is the result of transforming a row.
type ProcessedRow struct {
	LineNum int
	Result  string
}

// processRow simulates variable-time processing.
func processRow(row Row) ProcessedRow {
	// Simulate variable work: higher line numbers take less time (intentional scrambling).
	delay := time.Duration(rowCount-row.LineNum) * 5 * time.Millisecond
	time.Sleep(delay)
	return ProcessedRow{
		LineNum: row.LineNum,
		Result:  fmt.Sprintf("validated(%s)", row.Data),
	}
}

func main() {
	rows := make(chan Row, rowCount)
	results := make(chan ProcessedRow, rowCount)

	// Start workers.
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for row := range rows {
				result := processRow(row)
				results <- result
			}
		}(i)
	}

	// Send rows in order.
	for i := 1; i <= rowCount; i++ {
		rows <- Row{LineNum: i, Data: fmt.Sprintf("row-%d", i)}
	}
	close(rows)

	// Close results after all workers finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect and print results.
	fmt.Println("OUTPUT ORDER (unordered fan-out):")
	outputOrder := make([]int, 0, rowCount)
	for r := range results {
		outputOrder = append(outputOrder, r.LineNum)
		fmt.Printf("  line %2d: %s\n", r.LineNum, r.Result)
	}

	// Check if order was preserved.
	ordered := true
	for i := 1; i < len(outputOrder); i++ {
		if outputOrder[i] < outputOrder[i-1] {
			ordered = false
			break
		}
	}
	fmt.Printf("\nOrder preserved: %v\n", ordered)
}
```

Key observation: results arrive in scrambled order. Lower-numbered rows (which take longer) arrive after higher-numbered rows. The output file would be corrupted.

### Verification
```bash
go run main.go
```
Expected output (order will vary, but NOT sequential):
```
OUTPUT ORDER (unordered fan-out):
  line 12: validated(row-12)
  line 11: validated(row-11)
  line  9: validated(row-9)
  ...
  line  1: validated(row-1)

Order preserved: false
```

## Step 2 -- Add Sequence Numbers

Attach explicit sequence numbers to each item before fan-out. Workers propagate the sequence number through processing. Results now carry positional information.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	seqRowCount    = 12
	seqWorkerCount = 4
)

// SequencedRow carries an explicit ordering index.
type SequencedRow struct {
	SeqNum int
	Data   string
}

// SequencedResult carries the sequence number through processing.
type SequencedResult struct {
	SeqNum int
	Result string
}

func processSequenced(row SequencedRow) SequencedResult {
	delay := time.Duration(seqRowCount-row.SeqNum) * 5 * time.Millisecond
	time.Sleep(delay)
	return SequencedResult{
		SeqNum: row.SeqNum,
		Result: fmt.Sprintf("validated(%s)", row.Data),
	}
}

func main() {
	rows := make(chan SequencedRow, seqRowCount)
	results := make(chan SequencedResult, seqRowCount)

	var wg sync.WaitGroup
	for i := 0; i < seqWorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range rows {
				results <- processSequenced(row)
			}
		}()
	}

	// Assign sequence numbers at ingestion.
	for i := 0; i < seqRowCount; i++ {
		rows <- SequencedRow{
			SeqNum: i,
			Data:   fmt.Sprintf("row-%d", i+1),
		}
	}
	close(rows)

	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect all results and show arrival vs expected order.
	fmt.Printf("%-8s %-8s %s\n", "ARRIVED", "SEQ", "RESULT")
	fmt.Println("------------------------------")
	arrival := 0
	for r := range results {
		marker := ""
		if r.SeqNum != arrival {
			marker = " <- out of order"
		}
		fmt.Printf("%-8d %-8d %s%s\n", arrival, r.SeqNum, r.Result, marker)
		arrival++
	}
}
```

Now every result carries a `SeqNum`. We can see exactly which items arrived out of order. The next step is to use this information to resequence.

### Verification
```bash
go run main.go
```
Expected output (sequence numbers visible, many marked out of order):
```
ARRIVED  SEQ      RESULT
------------------------------
0        11       validated(row-12) <- out of order
1        10       validated(row-11) <- out of order
2        9        validated(row-10) <- out of order
...
```

## Step 3 -- Build the Resequencer

Add a resequencer goroutine that buffers out-of-order results and emits them strictly in sequence.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	reseqRowCount    = 12
	reseqWorkerCount = 4
)

type SequencedRow struct {
	SeqNum int
	Data   string
}

type SequencedResult struct {
	SeqNum int
	Result string
}

func processRow(row SequencedRow) SequencedResult {
	delay := time.Duration(reseqRowCount-row.SeqNum) * 5 * time.Millisecond
	time.Sleep(delay)
	return SequencedResult{
		SeqNum: row.SeqNum,
		Result: fmt.Sprintf("validated(%s)", row.Data),
	}
}

// resequencer buffers out-of-order results and emits them in strict sequence.
func resequencer(totalItems int, input <-chan SequencedResult, output chan<- SequencedResult) {
	buffer := make(map[int]SequencedResult)
	nextExpected := 0
	buffered := 0

	for r := range input {
		buffer[r.SeqNum] = r
		buffered++

		// Emit all consecutive items starting from nextExpected.
		for {
			item, ok := buffer[nextExpected]
			if !ok {
				break
			}
			output <- item
			delete(buffer, nextExpected)
			buffered--
			nextExpected++
		}
	}
	close(output)

	if nextExpected != totalItems {
		fmt.Printf("WARNING: resequencer emitted %d of %d items\n", nextExpected, totalItems)
	}
}

func main() {
	rows := make(chan SequencedRow, reseqRowCount)
	unordered := make(chan SequencedResult, reseqRowCount)
	ordered := make(chan SequencedResult, reseqRowCount)

	// Start workers.
	var wg sync.WaitGroup
	for i := 0; i < reseqWorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range rows {
				unordered <- processRow(row)
			}
		}()
	}

	// Start resequencer.
	go resequencer(reseqRowCount, unordered, ordered)

	// Send rows.
	for i := 0; i < reseqRowCount; i++ {
		rows <- SequencedRow{SeqNum: i, Data: fmt.Sprintf("row-%d", i+1)}
	}
	close(rows)

	go func() {
		wg.Wait()
		close(unordered)
	}()

	// Read ordered output.
	fmt.Println("RESEQUENCED OUTPUT:")
	allOrdered := true
	prev := -1
	for r := range ordered {
		marker := ""
		if prev >= 0 && r.SeqNum != prev+1 {
			marker = " <- GAP"
			allOrdered = false
		}
		fmt.Printf("  seq %2d: %s%s\n", r.SeqNum, r.Result, marker)
		prev = r.SeqNum
	}
	fmt.Printf("\nStrictly ordered: %v\n", allOrdered)
}
```

The resequencer uses a map as a buffer. When result 5 arrives but we are still waiting for result 3, it goes into the buffer. When result 3 finally arrives, the resequencer emits 3, checks for 4 (buffered), emits 4, checks for 5 (buffered), emits 5 -- flushing the consecutive run.

### Verification
```bash
go run -race main.go
```
Expected output:
```
RESEQUENCED OUTPUT:
  seq  0: validated(row-1)
  seq  1: validated(row-2)
  seq  2: validated(row-3)
  seq  3: validated(row-4)
  seq  4: validated(row-5)
  seq  5: validated(row-6)
  seq  6: validated(row-7)
  seq  7: validated(row-8)
  seq  8: validated(row-9)
  seq  9: validated(row-10)
  seq 10: validated(row-11)
  seq 11: validated(row-12)

Strictly ordered: true
```

## Step 4 -- Handle Gaps from Failed Items

In production, some items fail processing. The resequencer must handle gaps: if item 5 fails, it should skip it and continue emitting from item 6 onward.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	gapRowCount    = 15
	gapWorkerCount = 4
)

type SequencedRow struct {
	SeqNum int
	Data   string
}

type SequencedResult struct {
	SeqNum  int
	Result  string
	Skipped bool
}

// processWithFailures fails items at sequence 3, 7, and 11.
func processWithFailures(row SequencedRow) SequencedResult {
	delay := time.Duration(gapRowCount-row.SeqNum) * 3 * time.Millisecond
	time.Sleep(delay)

	failSeqs := map[int]bool{3: true, 7: true, 11: true}
	if failSeqs[row.SeqNum] {
		return SequencedResult{
			SeqNum:  row.SeqNum,
			Result:  fmt.Sprintf("FAILED(%s)", row.Data),
			Skipped: true,
		}
	}
	return SequencedResult{
		SeqNum: row.SeqNum,
		Result: fmt.Sprintf("validated(%s)", row.Data),
	}
}

// resequencer handles gaps by emitting skipped markers in order.
func resequencer(totalItems int, input <-chan SequencedResult, output chan<- SequencedResult) {
	buffer := make(map[int]SequencedResult)
	nextExpected := 0

	for r := range input {
		buffer[r.SeqNum] = r

		for {
			item, ok := buffer[nextExpected]
			if !ok {
				break
			}
			output <- item
			delete(buffer, nextExpected)
			nextExpected++
		}
	}
	close(output)
}

func main() {
	rows := make(chan SequencedRow, gapRowCount)
	unordered := make(chan SequencedResult, gapRowCount)
	ordered := make(chan SequencedResult, gapRowCount)

	var wg sync.WaitGroup
	for i := 0; i < gapWorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range rows {
				unordered <- processWithFailures(row)
			}
		}()
	}

	go resequencer(gapRowCount, unordered, ordered)

	epoch := time.Now()
	for i := 0; i < gapRowCount; i++ {
		rows <- SequencedRow{SeqNum: i, Data: fmt.Sprintf("row-%d", i+1)}
	}
	close(rows)

	go func() {
		wg.Wait()
		close(unordered)
	}()

	// Read ordered output, separating successes from failures.
	successCount, failCount := 0, 0
	fmt.Printf("%-6s %-8s %s\n", "SEQ", "STATUS", "RESULT")
	fmt.Println("---------------------------------------")
	for r := range ordered {
		status := "OK"
		if r.Skipped {
			status = "SKIP"
			failCount++
		} else {
			successCount++
		}
		fmt.Printf("%-6d %-8s %s\n", r.SeqNum, status, r.Result)
	}

	elapsed := time.Since(epoch).Round(time.Millisecond)
	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("Total:     %d\n", gapRowCount)
	fmt.Printf("Succeeded: %d\n", successCount)
	fmt.Printf("Failed:    %d\n", failCount)
	fmt.Printf("Wall time: %v\n", elapsed)
}
```

Workers always send a result (even for failures) so the resequencer never stalls. Failed items are marked with `Skipped: true`. The consumer decides whether to include them in the output file, log them separately, or retry them.

### Verification
```bash
go run -race main.go
```
Expected output:
```
SEQ    STATUS   RESULT
---------------------------------------
0      OK       validated(row-1)
1      OK       validated(row-2)
2      OK       validated(row-3)
3      SKIP     FAILED(row-4)
4      OK       validated(row-5)
5      OK       validated(row-6)
6      OK       validated(row-7)
7      SKIP     FAILED(row-8)
8      OK       validated(row-9)
9      OK       validated(row-10)
10     OK       validated(row-11)
11     SKIP     FAILED(row-12)
12     OK       validated(row-13)
13     OK       validated(row-14)
14     OK       validated(row-15)

=== Summary ===
Total:     15
Succeeded: 12
Failed:    3
Wall time: Xms
```

## Common Mistakes

### Resequencer Deadlock on Missing Items
**What happens:** A worker drops a failed item without sending a result. The resequencer waits forever for sequence number N, blocking all subsequent items. The pipeline deadlocks.
**Fix:** Workers must always send a result, even for failures. Use a `Skipped` or `Error` field to mark failures. The resequencer should never assume all items succeed.

### Unbounded Buffer Growth
**What happens:** If one item is extremely slow, all subsequent items pile up in the resequencer buffer. With millions of items, this consumes unbounded memory.
**Fix:** Set a maximum buffer size. If the buffer exceeds the limit, apply backpressure by blocking on the input channel, which naturally slows down the workers. Alternatively, use a bounded buffer channel between workers and the resequencer.

### Sequence Numbers Starting at 1 Instead of 0
**What happens:** The resequencer starts waiting for sequence 0, but the first item has sequence 1. It buffers everything, never emits anything, and eventually the buffer holds all items while the output channel is empty.
**Fix:** Use consistent zero-based indexing, or initialize `nextExpected` to match the first sequence number in the dataset.

## Review

The design rests on one asymmetry: goroutine scheduling is non-deterministic, so
the arrival order of results carries no information, but a sequence number
stamped on each item before fan-out does. The resequencer turns those numbers
back into order -- it buffers early arrivals in a map keyed by sequence and, each
time a new result lands, drains every consecutive item starting at
`nextExpected`, so output is emitted in a strict run rather than one item at a
time. The pattern is tag, scatter, gather, reorder, the same discipline TCP uses
to reassemble out-of-order packets. Two failure modes bound it. Workers must
always emit a result, even for failures (a `Skipped` marker), or the resequencer
waits forever for a sequence number that never comes and the pipeline deadlocks.
And the buffer must be watched: one very slow item makes every later item pile up
behind it, so unbounded input plus one straggler is unbounded memory unless you
apply backpressure.

To be sure it stuck, add a fast-lane check that logs a warning with the current
`nextExpected` and buffer size whenever the buffer exceeds 100 items, then switch
the workers from variable to fixed processing time and confirm the buffer stays
small when everyone finishes at similar speeds. You should also be able to say,
without looking, why zero-based sequence numbers matter: if items start at 1 but
`nextExpected` starts at 0, nothing is ever emitted and the buffer swells to hold
the entire dataset.

## Resources

- [Go Concurrency Patterns: Pipelines](https://go.dev/blog/pipelines) -- the fan-out/fan-in and cancellation building blocks this exercise composes.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the channel-passing philosophy behind the scatter/gather stages.
- [TCP Reordering (RFC 793)](https://www.rfc-editor.org/rfc/rfc793) -- the transport-layer spec whose reassembly of sequence-numbered segments mirrors the resequencer.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- the language reference on goroutines and channels used throughout.

---

Back to [Concurrency](../../concurrency.md) | Next: [26-channel-saga-compensation](../26-channel-saga-compensation/26-channel-saga-compensation.md)
