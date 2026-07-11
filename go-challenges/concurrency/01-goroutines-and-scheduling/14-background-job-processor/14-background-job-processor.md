# Exercise 14: Background Job Processor

Web applications constantly accept work that will not fit inside a single
request -- a user uploads a CSV with ten thousand rows, and you cannot make
them wait while every row is processed. The standard answer is to accept the
upload, hand back a job ID, return immediately, and grind through the work in a
background goroutine that the caller polls for status. Two things make or break
that goroutine: how it reports progress back to whoever is interested, and what
happens when it panics on a bad row. Both have to be designed in from the start.

## What you'll build

```text
14-background-job-processor/
  main.go        single job with a progress bar, three concurrent jobs, a
                 mutex-guarded JobTracker, and panic recovery per goroutine
```

- Build: a CSV upload processor that runs jobs in background goroutines and streams progress.
- Implement: `CSVProcessor.Process` sending `ProgressUpdate`s, a mutex-guarded `JobTracker` (Register/Update/PrintDashboard/PrintSummary), and a `defer`/`recover` block that turns a panic into a FAILED status.
- Verify: `go run main.go`.

### Why background workers live or die by communication and recovery

This pattern appears everywhere: image processing pipelines, report generation, data import/export, email campaigns, and batch API calls. The goroutine does the heavy lifting in the background while the main program remains responsive. The tricky part is communication: how does the background goroutine report progress, completion, or failure back to the interested parties? Channels are the answer.

A subtlety that catches many developers: if a background goroutine panics, it takes down the entire program unless you recover it. In production, this means one bad CSV row can crash your whole server. `defer`/`recover` inside the goroutine is not optional -- it is a safety net you must always install.


## Step 1 -- Single Background Job with Progress

A `CSVProcessor` accepts row data and processes it in a background goroutine. It sends progress updates through a channel. The caller reads progress and prints a live status bar.

```go
package main

import (
	"fmt"
	"time"
)

const (
	processingDelay = 30 * time.Millisecond
	statusPending   = "PENDING"
	statusRunning   = "RUNNING"
	statusDone      = "DONE"
)

type Job struct {
	ID     string
	Status string
	Total  int
	Done   int
}

type ProgressUpdate struct {
	JobID     string
	Processed int
	Total     int
	Status    string
}

type CSVProcessor struct {
	JobID string
	Rows  []string
}

func NewCSVProcessor(jobID string, rows []string) *CSVProcessor {
	return &CSVProcessor{
		JobID: jobID,
		Rows:  rows,
	}
}

func (cp *CSVProcessor) Process(progress chan<- ProgressUpdate) {
	defer close(progress)
	total := len(cp.Rows)
	progress <- ProgressUpdate{
		JobID: cp.JobID, Processed: 0, Total: total, Status: statusRunning,
	}

	for i, row := range cp.Rows {
		time.Sleep(processingDelay)
		_ = row // simulate processing
		progress <- ProgressUpdate{
			JobID:     cp.JobID,
			Processed: i + 1,
			Total:     total,
			Status:    statusRunning,
		}
	}

	progress <- ProgressUpdate{
		JobID: cp.JobID, Processed: total, Total: total, Status: statusDone,
	}
}

func renderProgressBar(processed, total int) string {
	const barWidth = 30
	filled := barWidth * processed / total
	bar := ""
	for i := 0; i < barWidth; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	pct := 100 * processed / total
	return fmt.Sprintf("[%s] %d%% (%d/%d)", bar, pct, processed, total)
}

func main() {
	rows := []string{
		"Alice,alice@example.com,Engineer",
		"Bob,bob@example.com,Designer",
		"Carol,carol@example.com,Manager",
		"Dave,dave@example.com,Analyst",
		"Eve,eve@example.com,DevOps",
		"Frank,frank@example.com,QA",
		"Grace,grace@example.com,PM",
		"Hank,hank@example.com,SRE",
	}

	progress := make(chan ProgressUpdate, len(rows)+2)
	processor := NewCSVProcessor("job-001", rows)

	fmt.Println("=== Submitting CSV Upload ===")
	fmt.Printf("  Job ID: %s | Rows: %d\n\n", processor.JobID, len(rows))

	go processor.Process(progress)

	for update := range progress {
		if update.Status == statusDone {
			fmt.Printf("\r  %s  %s\n", update.JobID, renderProgressBar(update.Processed, update.Total))
			fmt.Printf("\n  Job %s completed successfully.\n", update.JobID)
			break
		}
		fmt.Printf("\r  %s  %s", update.JobID, renderProgressBar(update.Processed, update.Total))
	}
}
```

**What's happening here:** The main goroutine submits a job and then enters a receive loop, printing a progress bar each time a new update arrives. The `CSVProcessor.Process` method runs in a background goroutine, sending one update per processed row. When it sends the final `DONE` update, the main goroutine prints the completion message and exits.

**Key insight:** The buffered channel with capacity `len(rows)+2` ensures the background goroutine never blocks. The `+2` accounts for the initial RUNNING update and the final DONE update. The sender (the `Process` goroutine) closes the channel with `defer close(progress)` -- this follows Go's convention that the producer closes the channel, not the consumer. In a real server, the progress channel would feed into a WebSocket or polling endpoint instead of a terminal progress bar.

### Verification
```bash
go run main.go
```
Expected output (progress bar fills incrementally):
```
=== Submitting CSV Upload ===
  Job ID: job-001 | Rows: 8

  job-001  [██████████████████████████████] 100% (8/8)

  Job job-001 completed successfully.
```


## Step 2 -- Three Concurrent Jobs

Multiple CSV uploads arrive in quick succession. Each gets its own background goroutine and sends progress to a shared channel. The main goroutine tracks all three.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	processingDelay = 25 * time.Millisecond
	statusRunning   = "RUNNING"
	statusDone      = "DONE"
)

type ProgressUpdate struct {
	JobID     string
	Processed int
	Total     int
	Status    string
}

type CSVProcessor struct {
	JobID string
	Rows  []string
}

func NewCSVProcessor(jobID string, rows []string) *CSVProcessor {
	return &CSVProcessor{JobID: jobID, Rows: rows}
}

func (cp *CSVProcessor) Process(progress chan<- ProgressUpdate, wg *sync.WaitGroup) {
	defer wg.Done()
	total := len(cp.Rows)

	progress <- ProgressUpdate{
		JobID: cp.JobID, Processed: 0, Total: total, Status: statusRunning,
	}

	for i, row := range cp.Rows {
		time.Sleep(processingDelay)
		_ = row
		progress <- ProgressUpdate{
			JobID: cp.JobID, Processed: i + 1, Total: total, Status: statusRunning,
		}
	}

	progress <- ProgressUpdate{
		JobID: cp.JobID, Processed: total, Total: total, Status: statusDone,
	}
}

func renderProgressBar(processed, total int) string {
	const barWidth = 25
	filled := barWidth * processed / total
	bar := ""
	for i := 0; i < barWidth; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	pct := 100 * processed / total
	return fmt.Sprintf("[%s] %3d%% (%d/%d)", bar, pct, processed, total)
}

func main() {
	jobs := []struct {
		id   string
		rows []string
	}{
		{"upload-A", generateRows(6)},
		{"upload-B", generateRows(10)},
		{"upload-C", generateRows(4)},
	}

	progress := make(chan ProgressUpdate, 100)
	var wg sync.WaitGroup

	fmt.Println("=== Launching 3 Concurrent CSV Jobs ===")
	fmt.Println()

	for _, j := range jobs {
		wg.Add(1)
		processor := NewCSVProcessor(j.id, j.rows)
		fmt.Printf("  Submitted: %-12s (%d rows)\n", j.id, len(j.rows))
		go processor.Process(progress, &wg)
	}

	go func() {
		wg.Wait()
		close(progress)
	}()

	fmt.Println()

	tracker := make(map[string]ProgressUpdate)
	completed := 0

	for update := range progress {
		tracker[update.JobID] = update
		if update.Status == statusDone {
			completed++
		}
		printDashboard(tracker)
	}

	fmt.Printf("\n  All %d jobs completed.\n", completed)
}

func printDashboard(tracker map[string]ProgressUpdate) {
	fmt.Print("\033[2K") // clear line
	for id, u := range tracker {
		status := "RUNNING"
		if u.Status == statusDone {
			status = "DONE   "
		}
		fmt.Printf("  %-12s %s %s\n", id, status, renderProgressBar(u.Processed, u.Total))
	}
	// move cursor up for overwrite on next update
	for range tracker {
		fmt.Print("\033[A")
	}
}

func generateRows(n int) []string {
	rows := make([]string, n)
	for i := 0; i < n; i++ {
		rows[i] = fmt.Sprintf("row-%d,data-%d,value-%d", i, i, i)
	}
	return rows
}
```

**What's happening here:** Three processors run concurrently, all sending updates to the same shared channel. A separate goroutine watches the `WaitGroup` and closes the channel when all processors finish. The main loop builds a tracker map and reprints the dashboard on each update.

**Key insight:** The goroutine that calls `wg.Wait()` and then `close(progress)` is the bridge between "all producers are done" and "the consumer should stop reading." Without this pattern, the `range progress` loop would block forever. This is idiomatic Go: the producer side closes the channel, the consumer side ranges over it.

### Verification
```bash
go run main.go
```
Expected output (dashboard updates in place; final state shown):
```
=== Launching 3 Concurrent CSV Jobs ===

  Submitted: upload-A      (6 rows)
  Submitted: upload-B      (10 rows)
  Submitted: upload-C      (4 rows)

  upload-A     DONE    [█████████████████████████] 100% (6/6)
  upload-B     DONE    [█████████████████████████] 100% (10/10)
  upload-C     DONE    [█████████████████████████] 100% (4/4)

  All 3 jobs completed.
```


## Step 3 -- Job Tracker Dashboard

Formalize the tracking into a `JobTracker` struct that manages job state, prints a formatted dashboard, and produces a final summary report.

```go
package main

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	processingDelay = 20 * time.Millisecond
	statusPending   = "PENDING"
	statusRunning   = "RUNNING"
	statusDone      = "DONE"
)

type ProgressUpdate struct {
	JobID     string
	Processed int
	Total     int
	Status    string
}

type JobState struct {
	ID        string
	Status    string
	Processed int
	Total     int
	StartedAt time.Time
	DoneAt    time.Time
}

type JobTracker struct {
	jobs map[string]*JobState
	mu   sync.Mutex
}

func NewJobTracker() *JobTracker {
	return &JobTracker{jobs: make(map[string]*JobState)}
}

func (jt *JobTracker) Register(jobID string, totalRows int) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.jobs[jobID] = &JobState{
		ID:     jobID,
		Status: statusPending,
		Total:  totalRows,
	}
}

func (jt *JobTracker) Update(update ProgressUpdate) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	job, exists := jt.jobs[update.JobID]
	if !exists {
		return
	}
	job.Processed = update.Processed
	job.Status = update.Status
	if update.Status == statusRunning && job.StartedAt.IsZero() {
		job.StartedAt = time.Now()
	}
	if update.Status == statusDone {
		job.DoneAt = time.Now()
	}
}

func (jt *JobTracker) PrintDashboard() {
	jt.mu.Lock()
	defer jt.mu.Unlock()

	ids := sortedKeys(jt.jobs)

	fmt.Println("  ┌──────────────┬──────────┬────────────────────────────────┐")
	fmt.Println("  │ Job          │ Status   │ Progress                       │")
	fmt.Println("  ├──────────────┼──────────┼────────────────────────────────┤")
	for _, id := range ids {
		job := jt.jobs[id]
		bar := renderBar(job.Processed, job.Total, 20)
		fmt.Printf("  │ %-12s │ %-8s │ %s %3d%%                  │\n",
			job.ID, job.Status, bar, percentage(job.Processed, job.Total))
	}
	fmt.Println("  └──────────────┴──────────┴────────────────────────────────┘")
}

func (jt *JobTracker) PrintSummary() {
	jt.mu.Lock()
	defer jt.mu.Unlock()

	fmt.Println()
	fmt.Println("  === Job Summary ===")
	totalRows := 0
	for _, job := range jt.jobs {
		elapsed := job.DoneAt.Sub(job.StartedAt)
		fmt.Printf("  %-12s  %d rows  %v\n", job.ID, job.Total, elapsed.Round(time.Millisecond))
		totalRows += job.Total
	}
	fmt.Printf("  Total rows processed: %d\n", totalRows)
}

func renderBar(done, total, width int) string {
	if total == 0 {
		return ""
	}
	filled := width * done / total
	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	return bar
}

func percentage(done, total int) int {
	if total == 0 {
		return 0
	}
	return 100 * done / total
}

func sortedKeys(m map[string]*JobState) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type CSVProcessor struct {
	JobID string
	Rows  []string
}

func NewCSVProcessor(jobID string, rows []string) *CSVProcessor {
	return &CSVProcessor{JobID: jobID, Rows: rows}
}

func (cp *CSVProcessor) Process(progress chan<- ProgressUpdate, wg *sync.WaitGroup) {
	defer wg.Done()
	total := len(cp.Rows)

	progress <- ProgressUpdate{JobID: cp.JobID, Processed: 0, Total: total, Status: statusRunning}

	for i, row := range cp.Rows {
		time.Sleep(processingDelay)
		_ = row
		progress <- ProgressUpdate{JobID: cp.JobID, Processed: i + 1, Total: total, Status: statusRunning}
	}

	progress <- ProgressUpdate{JobID: cp.JobID, Processed: total, Total: total, Status: statusDone}
}

func generateRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("user-%d,user%d@corp.com,dept-%d", i, i, i%5)
	}
	return rows
}

func main() {
	tracker := NewJobTracker()
	progress := make(chan ProgressUpdate, 100)
	var wg sync.WaitGroup

	uploads := []struct {
		id    string
		count int
	}{
		{"import-users", 8},
		{"import-orders", 12},
		{"import-skus", 5},
	}

	fmt.Println("=== Job Tracker Dashboard ===")
	fmt.Println()

	for _, u := range uploads {
		rows := generateRows(u.count)
		tracker.Register(u.id, u.count)
		processor := NewCSVProcessor(u.id, rows)
		wg.Add(1)
		go processor.Process(progress, &wg)
	}

	go func() {
		wg.Wait()
		close(progress)
	}()

	completed := 0
	for update := range progress {
		tracker.Update(update)
		if update.Status == statusDone {
			completed++
		}
	}

	tracker.PrintDashboard()
	tracker.PrintSummary()

	fmt.Printf("\n  All %d jobs completed successfully.\n", completed)
}
```

**What's happening here:** The `JobTracker` is a centralized state manager protected by a mutex. Each progress update modifies the tracker, and after all jobs complete, the dashboard and summary are printed. The tracker records start and end times so it can report per-job duration.

**Key insight:** The mutex on `JobTracker` is necessary because multiple goroutines send progress updates that the main goroutine applies to shared state. Even though the channel serializes message delivery to the main goroutine, the tracker itself must be safe because `PrintDashboard` could be called from different contexts in a real application. Designing for safety from the start prevents subtle bugs when requirements change.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Job Tracker Dashboard ===

  ┌──────────────┬──────────┬────────────────────────────────┐
  │ Job          │ Status   │ Progress                       │
  ├──────────────┼──────────┼────────────────────────────────┤
  │ import-orders│ DONE     │ ████████████████████ 100%      │
  │ import-skus  │ DONE     │ ████████████████████ 100%      │
  │ import-users │ DONE     │ ████████████████████ 100%      │
  └──────────────┴──────────┴────────────────────────────────┘

  === Job Summary ===
  import-orders   12 rows  240ms
  import-skus      5 rows  100ms
  import-users     8 rows  160ms
  Total rows processed: 25

  All 3 jobs completed successfully.
```


## Step 4 -- Panic Recovery in Background Jobs

One job encounters a corrupted row and panics. Without `defer`/`recover`, the entire program crashes. With it, the failed job is marked as FAILED while the others continue.

```go
package main

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	processingDelay = 20 * time.Millisecond
	statusRunning   = "RUNNING"
	statusDone      = "DONE"
	statusFailed    = "FAILED"
)

type ProgressUpdate struct {
	JobID     string
	Processed int
	Total     int
	Status    string
	Error     string
}

type JobState struct {
	ID        string
	Status    string
	Processed int
	Total     int
	Error     string
}

type JobTracker struct {
	jobs map[string]*JobState
	mu   sync.Mutex
}

func NewJobTracker() *JobTracker {
	return &JobTracker{jobs: make(map[string]*JobState)}
}

func (jt *JobTracker) Register(jobID string, totalRows int) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.jobs[jobID] = &JobState{ID: jobID, Status: "PENDING", Total: totalRows}
}

func (jt *JobTracker) Update(update ProgressUpdate) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	job, exists := jt.jobs[update.JobID]
	if !exists {
		return
	}
	job.Processed = update.Processed
	job.Status = update.Status
	job.Error = update.Error
}

func (jt *JobTracker) PrintReport() {
	jt.mu.Lock()
	defer jt.mu.Unlock()

	ids := make([]string, 0, len(jt.jobs))
	for k := range jt.jobs {
		ids = append(ids, k)
	}
	slices.Sort(ids)

	fmt.Println()
	fmt.Println("  === Final Job Report ===")
	fmt.Println("  Job              Status    Progress     Error")
	fmt.Println("  ───              ──────    ────────     ─────")
	for _, id := range ids {
		job := jt.jobs[id]
		errMsg := ""
		if job.Error != "" {
			errMsg = job.Error
		}
		fmt.Printf("  %-16s %-9s %3d/%-5d    %s\n",
			job.ID, job.Status, job.Processed, job.Total, errMsg)
	}
}

type CSVProcessor struct {
	JobID    string
	Rows     []string
	PanicRow int // row index that causes panic; -1 means no panic
}

func NewCSVProcessor(jobID string, rows []string, panicRow int) *CSVProcessor {
	return &CSVProcessor{JobID: jobID, Rows: rows, PanicRow: panicRow}
}

func (cp *CSVProcessor) Process(progress chan<- ProgressUpdate, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			progress <- ProgressUpdate{
				JobID:  cp.JobID,
				Status: statusFailed,
				Error:  fmt.Sprintf("panic recovered: %v", r),
			}
		}
	}()

	total := len(cp.Rows)
	progress <- ProgressUpdate{JobID: cp.JobID, Processed: 0, Total: total, Status: statusRunning}

	for i, row := range cp.Rows {
		if i == cp.PanicRow {
			panic(fmt.Sprintf("corrupted data in row %d: invalid UTF-8 sequence", i))
		}
		time.Sleep(processingDelay)
		_ = row
		progress <- ProgressUpdate{
			JobID: cp.JobID, Processed: i + 1, Total: total, Status: statusRunning,
		}
	}

	progress <- ProgressUpdate{JobID: cp.JobID, Processed: total, Total: total, Status: statusDone}
}

func generateRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("row-%d,data-%d", i, i)
	}
	return rows
}

func main() {
	tracker := NewJobTracker()
	progress := make(chan ProgressUpdate, 100)
	var wg sync.WaitGroup

	jobs := []struct {
		id       string
		rows     int
		panicRow int
	}{
		{"import-users", 8, -1},
		{"import-orders", 10, 4},
		{"import-products", 6, -1},
	}

	fmt.Println("=== Background Jobs with Panic Recovery ===")
	fmt.Println()

	for _, j := range jobs {
		rows := generateRows(j.rows)
		tracker.Register(j.id, j.rows)
		processor := NewCSVProcessor(j.id, rows, j.panicRow)
		panicInfo := ""
		if j.panicRow >= 0 {
			panicInfo = fmt.Sprintf(" (will panic at row %d)", j.panicRow)
		}
		fmt.Printf("  Submitted: %-18s %d rows%s\n", j.id, j.rows, panicInfo)
		wg.Add(1)
		go processor.Process(progress, &wg)
	}

	go func() {
		wg.Wait()
		close(progress)
	}()

	completed := 0
	failed := 0
	for update := range progress {
		tracker.Update(update)
		switch update.Status {
		case statusDone:
			completed++
			fmt.Printf("  [DONE]   %s\n", update.JobID)
		case statusFailed:
			failed++
			fmt.Printf("  [FAILED] %s -- %s\n", update.JobID, update.Error)
		}
	}

	tracker.PrintReport()

	fmt.Printf("\n  Completed: %d | Failed: %d | Total: %d\n", completed, failed, completed+failed)
}
```

**What's happening here:** The `import-orders` job panics at row 4 due to simulated corrupted data. The `defer`/`recover` block inside `Process` catches the panic, sends a FAILED update through the channel, and lets the goroutine exit cleanly. The other two jobs complete normally. The final report shows exactly which job failed and why.

**Key insight:** `defer`/`recover` must be inside the goroutine that might panic. A `recover` in main cannot catch a panic in a child goroutine -- Go panics propagate up the goroutine's own call stack, not across goroutines. If you forget this, one bad row in one upload crashes your entire server. The `defer wg.Done()` is placed before the `defer recover` so that `Done()` is called regardless of whether the goroutine panics or completes normally.

**Why `defer wg.Done()` comes first:** Deferred calls execute in LIFO (last-in, first-out) order. By deferring `wg.Done()` first and the recover second, the recover runs first (catching the panic), then `Done()` runs (signaling completion). If you reverse them, `Done()` would run before `recover()`, which could cause a race if the WaitGroup counter reaches zero while the panic is still unhandled.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Background Jobs with Panic Recovery ===

  Submitted: import-users       8 rows
  Submitted: import-orders      10 rows (will panic at row 4)
  Submitted: import-products    6 rows
  [FAILED] import-orders -- panic recovered: corrupted data in row 4: invalid UTF-8 sequence
  [DONE]   import-products
  [DONE]   import-users

  === Final Job Report ===
  Job              Status    Progress     Error
  ───              ──────    ────────     ─────
  import-orders    FAILED      4/10      panic recovered: corrupted data in row 4: invalid UTF-8 sequence
  import-products  DONE        6/6
  import-users     DONE        8/8

  Completed: 2 | Failed: 1 | Total: 3
```


## Common Mistakes

### Not Recovering Panics in Background Goroutines

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func processJob(id int, wg *sync.WaitGroup) {
	defer wg.Done()
	if id == 2 {
		panic("corrupted data")
	}
	fmt.Printf("Job %d done\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go processJob(i, &wg)
	}
	wg.Wait()
	fmt.Println("All jobs done") // never reached -- program crashes
}
```
**What happens:** Job 2 panics and crashes the entire program. Jobs 0 and 1 may or may not have finished. The crash is unrecoverable at the main goroutine level.

**Correct -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func processJob(id int, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Job %d failed: %v\n", id, r)
		}
	}()
	if id == 2 {
		panic("corrupted data")
	}
	fmt.Printf("Job %d done\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go processJob(i, &wg)
	}
	wg.Wait()
	fmt.Println("All jobs done")
}
```

### Closing a Channel from the Wrong Side

**Wrong -- complete program:**
```go
package main

import "fmt"

func producer(ch chan<- int) {
	for i := 0; i < 5; i++ {
		ch <- i
	}
	// producer forgets to close
}

func main() {
	ch := make(chan int, 5)
	go producer(ch)
	for v := range ch {
		fmt.Println(v)
	}
	// deadlock: range waits for close that never comes
}
```
**What happens:** `range ch` blocks forever after receiving all 5 values because the channel is never closed. The program deadlocks.

**Correct -- complete program:**
```go
package main

import "fmt"

func producer(ch chan<- int) {
	for i := 0; i < 5; i++ {
		ch <- i
	}
	close(ch)
}

func main() {
	ch := make(chan int, 5)
	go producer(ch)
	for v := range ch {
		fmt.Println(v)
	}
	fmt.Println("Done")
}
```

### Forgetting wg.Done() in the Panic Path

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func riskyWork(id int, wg *sync.WaitGroup) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("recovered: %v\n", r)
			// forgot wg.Done() -- WaitGroup counter never reaches zero
		}
	}()
	defer wg.Done() // this defer runs BEFORE recover, so Done runs AFTER recover
	// but if panic happens, the order matters
	if id == 1 {
		panic("boom")
	}
	fmt.Printf("Job %d ok\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go riskyWork(i, &wg)
	}
	wg.Wait() // may deadlock if Done is missed in any path
	fmt.Println("All done")
}
```
**What happens:** The defer order is subtle. Always place `defer wg.Done()` as the very first defer so it runs last (LIFO), guaranteeing it executes in every code path.

**Correct -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

func riskyWork(id int, wg *sync.WaitGroup) {
	defer wg.Done() // FIRST defer = runs LAST = always executes
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("recovered: %v\n", r)
		}
	}()
	if id == 1 {
		panic("boom")
	}
	fmt.Printf("Job %d ok\n", id)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go riskyWork(i, &wg)
	}
	wg.Wait()
	fmt.Println("All done")
}
```


## Review

A background job is just a goroutine that outlives the request that started it,
and everything hard about it is communication. Progress, completion, and
failure all travel back over a channel as `ProgressUpdate` values, because a
goroutine has no caller to return to; the producer owns the channel and closes
it, and when several producers share one channel the idiomatic bridge is a
watcher goroutine that calls `wg.Wait()` and then `close`, so the consumer's
`range` terminates exactly once every worker is done. Buffering that channel
with enough slack keeps a worker from stalling while the consumer is busy. The
shared state -- the `JobTracker` map -- is guarded by a mutex even though the
channel already serializes delivery, because the moment a second caller (a
dashboard, an API handler) touches it, the guarantee you skipped becomes the
bug you ship.

The non-negotiable is panic recovery: a panic propagates up its own goroutine's
stack and never crosses into `main`, so a `recover` in `main` cannot save a
child -- the `defer`/`recover` must sit inside the goroutine that might panic,
and `defer wg.Done()` must be the first defer so it runs last and fires on every
path. With that in hand you should be able to build a document converter that
runs one goroutine per document, streams per-page progress into a
`ConversionTracker`, lets one document panic on page three and recovers it as a
FAILED row showing partial progress, and prints a final table of succeeded
versus failed -- without looking back at Step 4.

## Resources
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) -- how the three interact and why recover only works inside a deferred call.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- the counter that lets the watcher goroutine know when to close the channel.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) -- guarding the shared JobTracker map against concurrent writes.
- [Go Spec: Handling Panics](https://go.dev/ref/spec#Handling_panics) -- the precise rule that a panic unwinds only its own goroutine.

---

Back to [Concurrency](../../concurrency.md) | Next: [15-concurrent-file-processing](../15-concurrent-file-processing/15-concurrent-file-processing.md)
