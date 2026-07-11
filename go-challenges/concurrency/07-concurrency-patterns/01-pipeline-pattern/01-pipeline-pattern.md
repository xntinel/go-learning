# Exercise 1: Pipeline Pattern

A pipeline is a chain of stages connected by channels: each stage is a
goroutine that receives values from upstream, transforms them, and sends the
results downstream. It is one of Go's foundational concurrency shapes because
it breaks a big job into small, testable, composable stages. The motivating
case here is log processing -- gigabytes of files that will not fit in memory,
streamed one line at a time so the parser works on line N while the reader is
already fetching N+1. The discipline that holds it together is simple and
unforgiving: when a stage finishes, it closes its output channel, or everything
downstream blocks forever.

## What you'll build

```text
01-pipeline-pattern/
  main.go        five-stage log pipeline -- read, parse, filter, count/enrich,
                 report (each step is its own runnable program)
```

- Build: a multi-stage log-processing pipeline -- read, parse, filter, count, and enrich.
- Implement: a `ReadLines` generator returning `<-chan string`, plus `ParseEntries`, `FilterByLevel`, `CountByService`, and `EnrichWithSeverity`, each `<-chan` in and `<-chan` out.
- Verify: `go run main.go` (each step is its own runnable program).

### Why closing the output channel is the contract

The pipeline pattern relies on a critical contract: when a stage is done producing values, it closes its output channel. This propagates a "done" signal downstream. Without this discipline, downstream stages would block forever waiting for values that will never arrive.

```
              Log Processing Pipeline

  +----------+     +---------+     +--------+     +---------+     +--------+
  | readLogs | --> | parse   | --> | filter | --> | count   | --> | report |
  +----------+     +---------+     +--------+     +---------+     +--------+
    (source)       (transform)     (filter)       (aggregate)      (drain)
       |               |               |               |              |
    <-chan string   <-chan LogEntry  <-chan LogEntry  <-chan Summary   range
```

## Step 1 -- Build the Log Reader Stage

The first stage of any pipeline is a generator: a function that produces values and sends them into a channel. In our log processor, this stage simulates reading raw log lines from a file. The generator takes the raw input, converts it into a stream, and returns a receive-only channel.

```go
package main

import "fmt"

// LogPipeline orchestrates a multi-stage log processing flow.
type LogPipeline struct {
	rawLines []string
}

func NewLogPipeline(lines []string) *LogPipeline {
	return &LogPipeline{rawLines: lines}
}

func (lp *LogPipeline) ReadLines() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for _, line := range lp.rawLines {
			out <- line
		}
	}()
	return out
}

func (lp *LogPipeline) PrintAllLines() {
	fmt.Println("=== Log Reader Stage ===")
	for line := range lp.ReadLines() {
		fmt.Printf("  %s\n", line)
	}
}

func main() {
	logs := []string{
		"2024-03-15T10:00:01Z ERROR [auth] failed login attempt from 192.168.1.50",
		"2024-03-15T10:00:02Z INFO  [api] GET /users returned 200",
		"2024-03-15T10:00:03Z ERROR [db] connection pool exhausted",
		"2024-03-15T10:00:04Z WARN  [api] slow query detected: 2300ms",
		"2024-03-15T10:00:05Z ERROR [auth] token expired for user=admin",
	}

	pipeline := NewLogPipeline(logs)
	pipeline.PrintAllLines()
}
```

The function launches a goroutine that sends each line into the channel and closes it when done. The caller receives a `<-chan string` -- it can only read from it. In a real system, this stage would read from a file using `bufio.Scanner`, but the pipeline structure is identical.

### Verification
```bash
go run main.go
```
You should see all log lines printed:
```
=== Log Reader Stage ===
  2024-03-15T10:00:01Z ERROR [auth] failed login attempt from 192.168.1.50
  2024-03-15T10:00:02Z INFO  [api] GET /users returned 200
  2024-03-15T10:00:03Z ERROR [db] connection pool exhausted
  2024-03-15T10:00:04Z WARN  [api] slow query detected: 2300ms
  2024-03-15T10:00:05Z ERROR [auth] token expired for user=admin
```

## Step 2 -- Build the Parse Stage

Now build a stage that transforms raw log strings into structured data. It reads strings from an input channel, parses them into `LogEntry` structs, and sends the results to an output channel. This is the transform stage of the pipeline.

```go
package main

import (
	"fmt"
	"strings"
)

const minLogFieldCount = 4

// LogEntry represents a structured log line.
type LogEntry struct {
	Timestamp string
	Level     string
	Service   string
	Message   string
}

// LogPipeline orchestrates a multi-stage log processing flow.
type LogPipeline struct {
	rawLines []string
}

func NewLogPipeline(lines []string) *LogPipeline {
	return &LogPipeline{rawLines: lines}
}

func (lp *LogPipeline) ReadLines() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for _, line := range lp.rawLines {
			out <- line
		}
	}()
	return out
}

func parseLine(line string) (LogEntry, bool) {
	parts := strings.SplitN(line, " ", minLogFieldCount)
	if len(parts) < minLogFieldCount {
		return LogEntry{}, false
	}
	return LogEntry{
		Timestamp: parts[0],
		Level:     parts[1],
		Service:   strings.Trim(parts[2], "[]"),
		Message:   parts[3],
	}, true
}

func (lp *LogPipeline) ParseEntries(in <-chan string) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for line := range in {
			if entry, ok := parseLine(line); ok {
				out <- entry
			}
		}
	}()
	return out
}

func (lp *LogPipeline) PrintParsedEntries() {
	fmt.Println("=== Parsed Log Entries ===")
	raw := lp.ReadLines()
	for entry := range lp.ParseEntries(raw) {
		fmt.Printf("  [%s] %-5s %s: %s\n", entry.Timestamp, entry.Level, entry.Service, entry.Message)
	}
}

func main() {
	logs := []string{
		"2024-03-15T10:00:01Z ERROR [auth] failed login attempt from 192.168.1.50",
		"2024-03-15T10:00:02Z INFO  [api] GET /users returned 200",
		"2024-03-15T10:00:03Z ERROR [db] connection pool exhausted",
	}

	pipeline := NewLogPipeline(logs)
	pipeline.PrintParsedEntries()
}
```

Notice the symmetry: every stage follows the same pattern -- accept a channel, return a channel, do work in a goroutine, close when done. This uniformity makes stages composable.

### Verification
```bash
go run main.go
```
You should see structured entries:
```
=== Parsed Log Entries ===
  [2024-03-15T10:00:01Z] ERROR auth: failed login attempt from 192.168.1.50
  [2024-03-15T10:00:02Z] INFO  api: GET /users returned 200
  [2024-03-15T10:00:03Z] ERROR db: connection pool exhausted
```

## Step 3 -- Add Filter and Count Stages

Now chain the complete pipeline: read -> parse -> filter errors -> count by service -> output summary. This is where the composition happens. Each new stage plugs in without modifying existing ones.

```go
package main

import (
	"fmt"
	"strings"
)

const (
	minLogFieldCount = 4
	levelError       = "ERROR"
)

// LogEntry represents a structured log line.
type LogEntry struct {
	Timestamp string
	Level     string
	Service   string
	Message   string
}

// ServiceSummary holds the error count for a service.
type ServiceSummary struct {
	Service string
	Count   int
}

// LogPipeline orchestrates a multi-stage log processing flow.
type LogPipeline struct {
	rawLines []string
}

func NewLogPipeline(lines []string) *LogPipeline {
	return &LogPipeline{rawLines: lines}
}

func (lp *LogPipeline) ReadLines() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for _, line := range lp.rawLines {
			out <- line
		}
	}()
	return out
}

func parseLine(line string) (LogEntry, bool) {
	parts := strings.SplitN(line, " ", minLogFieldCount)
	if len(parts) < minLogFieldCount {
		return LogEntry{}, false
	}
	return LogEntry{
		Timestamp: parts[0],
		Level:     parts[1],
		Service:   strings.Trim(parts[2], "[]"),
		Message:   parts[3],
	}, true
}

func (lp *LogPipeline) ParseEntries(in <-chan string) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for line := range in {
			if entry, ok := parseLine(line); ok {
				out <- entry
			}
		}
	}()
	return out
}

func (lp *LogPipeline) FilterByLevel(in <-chan LogEntry, level string) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for entry := range in {
			if entry.Level == level {
				out <- entry
			}
		}
	}()
	return out
}

func (lp *LogPipeline) CountByService(in <-chan LogEntry) <-chan ServiceSummary {
	out := make(chan ServiceSummary)
	go func() {
		defer close(out)
		counts := make(map[string]int)
		for entry := range in {
			counts[entry.Service]++
		}
		for service, count := range counts {
			out <- ServiceSummary{Service: service, Count: count}
		}
	}()
	return out
}

func (lp *LogPipeline) PrintErrorSummary() {
	raw := lp.ReadLines()
	parsed := lp.ParseEntries(raw)
	errors := lp.FilterByLevel(parsed, levelError)
	summary := lp.CountByService(errors)

	fmt.Println("=== Error Summary by Service ===")
	for s := range summary {
		fmt.Printf("  %s: %d errors\n", s.Service, s.Count)
	}
}

func main() {
	logs := []string{
		"2024-03-15T10:00:01Z ERROR [auth] failed login attempt from 192.168.1.50",
		"2024-03-15T10:00:02Z INFO  [api] GET /users returned 200",
		"2024-03-15T10:00:03Z ERROR [db] connection pool exhausted",
		"2024-03-15T10:00:04Z WARN  [api] slow query detected: 2300ms",
		"2024-03-15T10:00:05Z ERROR [auth] token expired for user=admin",
		"2024-03-15T10:00:06Z ERROR [db] deadlock detected on table=orders",
		"2024-03-15T10:00:07Z INFO  [auth] user=admin logged in successfully",
		"2024-03-15T10:00:08Z ERROR [api] upstream timeout after 30s",
		"2024-03-15T10:00:09Z ERROR [auth] invalid certificate presented",
		"2024-03-15T10:00:10Z ERROR [db] replication lag exceeded 5s",
	}

	pipeline := NewLogPipeline(logs)
	pipeline.PrintErrorSummary()
}
```

The pipeline reads naturally left-to-right: read -> parse -> filter -> count -> print. Each arrow is a channel. Each stage runs in its own goroutine. The consumer (the `range` loop) drives the pipeline by pulling values through. At no point is the entire log file buffered in memory -- each line flows through the stages one at a time.

### Verification
```bash
go run main.go
```
Expected output (map iteration order may vary):
```
=== Error Summary by Service ===
  auth: 3 errors
  db: 3 errors
  api: 1 errors
```

## Step 4 -- Full Pipeline with All Stages

Combine everything into a complete program that demonstrates the full flow with detailed output at each stage, showing how data transforms as it moves through the pipeline.

```go
package main

import (
	"fmt"
	"strings"
)

const (
	minLogFieldCount = 4
	levelError       = "ERROR"
	severityNormal   = "NORMAL"
	severityCritical = "CRITICAL"
)

var criticalKeywords = []string{"deadlock", "exhausted", "replication"}

// LogEntry represents a structured log line.
type LogEntry struct {
	Timestamp string
	Level     string
	Service   string
	Message   string
}

// EnrichedReport is an error entry annotated with severity.
type EnrichedReport struct {
	Severity string
	Service  string
	Level    string
	Message  string
}

// LogPipeline orchestrates a multi-stage log processing flow.
type LogPipeline struct {
	rawLines []string
}

func NewLogPipeline(lines []string) *LogPipeline {
	return &LogPipeline{rawLines: lines}
}

func (lp *LogPipeline) ReadLines() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for _, line := range lp.rawLines {
			out <- line
		}
	}()
	return out
}

func parseLine(line string) (LogEntry, bool) {
	parts := strings.SplitN(line, " ", minLogFieldCount)
	if len(parts) < minLogFieldCount {
		return LogEntry{}, false
	}
	return LogEntry{
		Timestamp: parts[0],
		Level:     parts[1],
		Service:   strings.Trim(parts[2], "[]"),
		Message:   parts[3],
	}, true
}

func (lp *LogPipeline) ParseEntries(in <-chan string) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for line := range in {
			if entry, ok := parseLine(line); ok {
				out <- entry
			}
		}
	}()
	return out
}

func (lp *LogPipeline) FilterByLevel(in <-chan LogEntry, level string) <-chan LogEntry {
	out := make(chan LogEntry)
	go func() {
		defer close(out)
		for entry := range in {
			if entry.Level == level {
				out <- entry
			}
		}
	}()
	return out
}

func classifySeverity(message string) string {
	lower := strings.ToLower(message)
	for _, kw := range criticalKeywords {
		if strings.Contains(lower, kw) {
			return severityCritical
		}
	}
	return severityNormal
}

func (lp *LogPipeline) EnrichWithSeverity(in <-chan LogEntry) <-chan EnrichedReport {
	out := make(chan EnrichedReport)
	go func() {
		defer close(out)
		for entry := range in {
			out <- EnrichedReport{
				Severity: classifySeverity(entry.Message),
				Service:  entry.Service,
				Level:    entry.Level,
				Message:  entry.Message,
			}
		}
	}()
	return out
}

func (lp *LogPipeline) PrintAllParsedEntries() {
	fmt.Println("=== All Parsed Entries ===")
	raw := lp.ReadLines()
	for entry := range lp.ParseEntries(raw) {
		fmt.Printf("  %-5s [%s] %s\n", entry.Level, entry.Service, entry.Message)
	}
}

func (lp *LogPipeline) PrintErrorReportWithSeverity() {
	fmt.Println("=== Error Report with Severity ===")
	raw := lp.ReadLines()
	parsed := lp.ParseEntries(raw)
	errorsOnly := lp.FilterByLevel(parsed, levelError)
	enriched := lp.EnrichWithSeverity(errorsOnly)
	for report := range enriched {
		fmt.Printf("  [%s] %s/%s: %s\n", report.Severity, report.Service, report.Level, report.Message)
	}
}

func main() {
	logs := []string{
		"2024-03-15T10:00:01Z ERROR [auth] failed login attempt from 192.168.1.50",
		"2024-03-15T10:00:02Z INFO  [api] GET /users returned 200",
		"2024-03-15T10:00:03Z ERROR [db] connection pool exhausted",
		"2024-03-15T10:00:04Z WARN  [api] slow query detected: 2300ms",
		"2024-03-15T10:00:05Z ERROR [auth] token expired for user=admin",
		"2024-03-15T10:00:06Z ERROR [db] deadlock detected on table=orders",
		"2024-03-15T10:00:07Z INFO  [auth] user=admin logged in successfully",
		"2024-03-15T10:00:08Z ERROR [api] upstream timeout after 30s",
		"2024-03-15T10:00:09Z ERROR [auth] invalid certificate presented",
		"2024-03-15T10:00:10Z ERROR [db] replication lag exceeded 5s",
	}

	pipeline := NewLogPipeline(logs)

	fmt.Println("Exercise: Log Processing Pipeline")
	fmt.Println()

	pipeline.PrintAllParsedEntries()

	fmt.Println()
	pipeline.PrintErrorReportWithSeverity()
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
Exercise: Log Processing Pipeline

=== All Parsed Entries ===
  ERROR [auth] failed login attempt from 192.168.1.50
  INFO  [api] GET /users returned 200
  ERROR [db] connection pool exhausted
  WARN  [api] slow query detected: 2300ms
  ERROR [auth] token expired for user=admin
  ERROR [db] deadlock detected on table=orders
  INFO  [auth] user=admin logged in successfully
  ERROR [api] upstream timeout after 30s
  ERROR [auth] invalid certificate presented
  ERROR [db] replication lag exceeded 5s

=== Error Report with Severity ===
  [NORMAL] auth/ERROR: failed login attempt from 192.168.1.50
  [CRITICAL] db/ERROR: connection pool exhausted
  [NORMAL] auth/ERROR: token expired for user=admin
  [CRITICAL] db/ERROR: deadlock detected on table=orders
  [NORMAL] api/ERROR: upstream timeout after 30s
  [NORMAL] auth/ERROR: invalid certificate presented
  [CRITICAL] db/ERROR: replication lag exceeded 5s
```

## Common Mistakes

### Forgetting to Close the Output Channel

**Wrong:**
```go
package main

import "fmt"

func parseLogEntries(in <-chan string) <-chan string {
	out := make(chan string)
	go func() {
		for line := range in {
			out <- "[PARSED] " + line
		}
		// forgot close(out) -- downstream blocks forever
	}()
	return out
}

func main() {
	in := make(chan string)
	go func() {
		in <- "2024-03-15T10:00:01Z ERROR [auth] login failed"
		close(in)
	}()
	for v := range parseLogEntries(in) {
		fmt.Println(v)
	}
	// deadlock: range never ends because out is never closed
}
```
**What happens:** The downstream `range` loop blocks forever waiting for more values. The program deadlocks.

**Fix:** Always close the output channel when the goroutine finishes producing values.

### Returning a Bidirectional Channel

**Wrong:**
```go
func readLogLines(lines []string) chan string { // bidirectional!
	out := make(chan string)
	go func() {
		for _, line := range lines {
			out <- line
		}
		close(out)
	}()
	return out
}
```
**What happens:** Callers could accidentally send values back into the channel, breaking the pipeline contract.

**Fix:** Return `<-chan string` (receive-only). Let the compiler enforce the data flow direction.

### Blocking the Generator on a Full Channel

If you use an unbuffered channel and the consumer is slow, the generator blocks on every send. This is actually correct behavior (backpressure), but if you need buffering, you can use `make(chan string, bufferSize)`. Be intentional about the choice.

## Review

Every stage in this pipeline has the same shape -- accept an input channel,
return an output channel, do its work in a goroutine, and close the output when
the input is exhausted. That uniformity is what makes the stages composable:
because a filter and an enricher present the identical interface as the reader
and the parser, you can splice a new stage into the chain without touching the
ones around it. The first stage is the generator, a function that owns no input
and simply returns `<-chan T`; every stage after it consumes upstream and
produces downstream. The stages run concurrently, so production and consumption
overlap -- the reader fetches the next line while the parser handles the
current one -- and nothing ever holds the whole log file in memory at once. The
single rule that keeps the chain from wedging is closing each output channel;
skip one close and the downstream range waits forever.

You should be able to run the full program and read the result as a check on
your understanding: all ten raw lines parse into structured entries, the error
report keeps only the six ERROR-level ones, and the severity stage tags an
entry CRITICAL exactly when its message contains deadlock, exhausted, or
replication. The property worth internalizing is the one you cannot see in the
output -- at no point is the entire log buffered; each line flows through read,
parse, filter, and enrich one at a time, which is why the same structure scales
from ten lines to gigabytes.

## Resources
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- the canonical treatment of staged channels, closing discipline, and adding cancellation.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the channel and goroutine mechanics each stage is built from.
- [Go Concurrency Patterns (Rob Pike)](https://www.youtube.com/watch?v=f6kdp27TYZs) -- the talk that frames pipelines and generators as reusable building blocks.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-fan-out-distribute-work](../02-fan-out-distribute-work/02-fan-out-distribute-work.md)
