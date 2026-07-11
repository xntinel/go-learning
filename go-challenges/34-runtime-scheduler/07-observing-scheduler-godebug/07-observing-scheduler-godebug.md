# 7. Observing the Scheduler with GODEBUG

Setting `GODEBUG=schedtrace=<interval_ms>` makes the Go runtime print one line of scheduler state per interval to stderr. This is the cheapest observability tool for scheduler behavior — zero instrumentation in your code, zero external dependencies. The hard part is that the output is not machine-readable by default and the format is undocumented: you need to know what each field means and how to parse it.

## Concepts

### The schedtrace Output Format

A typical schedtrace line looks like this:

```
SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 [34 28 31 29]
```

Fields:
- `100ms` — elapsed time since program start
- `gomaxprocs=4` — number of active logical processors (P's)
- `idleprocs=0` — P's with no goroutine to run
- `threads=5` — total OS threads (M's), including system threads
- `spinningthreads=0` — M's currently spinning looking for work
- `idlethreads=2` — M's parked (sleeping)
- `runqueue=12` — goroutines in the global run queue
- `[34 28 31 29]` — goroutines in each P's local run queue

When `idleprocs=0` and all local queues are non-zero, every P has work: the scheduler is fully loaded. When one P has an empty queue while others are full, work stealing should trigger momentarily.

### What schedtrace Cannot Tell You

schedtrace samples at a fixed interval and can miss transient spikes. It reports counts, not identities: you cannot tell which goroutine is in which queue. For goroutine-level visibility, use `GODEBUG=scheddetail=1` (very verbose) or `runtime/trace`.

### schedtrace Interval Trade-offs

Short intervals (e.g., 10ms) reveal transient behavior but add overhead — the runtime serializes all P's to collect the state. Use 100ms–1000ms in production diagnostics; 10ms–50ms in controlled benchmarks.

## Exercises

Module path: `example.com/schedparse`. Set up the module:

```go
// go.mod
module example.com/schedparse

go 1.26
```

### Exercise 1: Implement the schedparse package

Create `schedparse.go`:

```go
// schedparse.go
package schedparse

import (
	"errors"
	"strconv"
	"strings"
)

// ErrInvalidFormat is returned when a line cannot be parsed as a schedtrace record.
var ErrInvalidFormat = errors.New("invalid schedtrace format")

// TraceRecord holds the fields from one GODEBUG=schedtrace output line.
type TraceRecord struct {
	ElapsedMs       int
	Gomaxprocs      int
	Idleprocs       int
	Threads         int
	Spinningthreads int
	Idlethreads     int
	Runqueue        int
	LocalQueues     []int
}

// Parse parses a single GODEBUG=schedtrace line into a TraceRecord.
// Returns ErrInvalidFormat if the line is not a valid SCHED line.
func Parse(line string) (TraceRecord, error) {
	if !strings.HasPrefix(line, "SCHED ") {
		return TraceRecord{}, ErrInvalidFormat
	}

	// Parse local queues from the bracket section.
	lqStart := strings.Index(line, "[")
	lqEnd := strings.Index(line, "]")
	if lqStart == -1 || lqEnd == -1 || lqEnd <= lqStart {
		return TraceRecord{}, ErrInvalidFormat
	}
	lqStr := line[lqStart+1 : lqEnd]
	var localQueues []int
	if strings.TrimSpace(lqStr) != "" {
		for _, s := range strings.Fields(lqStr) {
			v, err := strconv.Atoi(s)
			if err != nil {
				return TraceRecord{}, ErrInvalidFormat
			}
			localQueues = append(localQueues, v)
		}
	}

	// Work on the part before the brackets.
	main := strings.TrimSpace(line[:lqStart])

	// Extract elapsed: second token, looks like "100ms:"
	parts := strings.Fields(main)
	if len(parts) < 2 {
		return TraceRecord{}, ErrInvalidFormat
	}
	elapsedToken := strings.TrimSuffix(parts[1], ":")
	elapsedToken = strings.TrimSuffix(elapsedToken, "ms")
	elapsedMs, err := strconv.Atoi(elapsedToken)
	if err != nil {
		return TraceRecord{}, ErrInvalidFormat
	}

	// Parse key=value pairs.
	kvMap := make(map[string]int)
	for _, part := range parts[2:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v, err := strconv.Atoi(kv[1])
		if err != nil {
			continue
		}
		kvMap[kv[0]] = v
	}

	required := []string{"gomaxprocs", "idleprocs", "threads", "spinningthreads", "idlethreads", "runqueue"}
	for _, k := range required {
		if _, ok := kvMap[k]; !ok {
			return TraceRecord{}, ErrInvalidFormat
		}
	}

	return TraceRecord{
		ElapsedMs:       elapsedMs,
		Gomaxprocs:      kvMap["gomaxprocs"],
		Idleprocs:       kvMap["idleprocs"],
		Threads:         kvMap["threads"],
		Spinningthreads: kvMap["spinningthreads"],
		Idlethreads:     kvMap["idlethreads"],
		Runqueue:        kvMap["runqueue"],
		LocalQueues:     localQueues,
	}, nil
}

// ParseAll parses multiple schedtrace lines. Non-SCHED lines are silently skipped.
// Returns an error if any SCHED line fails to parse.
func ParseAll(lines []string) ([]TraceRecord, error) {
	var records []TraceRecord
	for _, line := range lines {
		if !strings.HasPrefix(line, "SCHED ") {
			continue
		}
		r, err := Parse(line)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}
```

### Exercise 2: Write the test file

Create `schedparse_test.go`:

```go
// schedparse_test.go
package schedparse

import (
	"errors"
	"testing"
)

const sampleLine = "SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 [34 28 31 29]"

func TestParseValid(t *testing.T) {
	t.Parallel()
	r, err := Parse(sampleLine)
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"ElapsedMs", r.ElapsedMs, 100},
		{"Gomaxprocs", r.Gomaxprocs, 4},
		{"Idleprocs", r.Idleprocs, 0},
		{"Threads", r.Threads, 5},
		{"Spinningthreads", r.Spinningthreads, 0},
		{"Idlethreads", r.Idlethreads, 2},
		{"Runqueue", r.Runqueue, 12},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("got %d, want %d", tc.got, tc.want)
			}
		})
	}
	if len(r.LocalQueues) != 4 {
		t.Errorf("LocalQueues len = %d, want 4", len(r.LocalQueues))
	}
	want := []int{34, 28, 31, 29}
	for i, v := range r.LocalQueues {
		if v != want[i] {
			t.Errorf("LocalQueues[%d] = %d, want %d", i, v, want[i])
		}
	}
}

func TestParseInvalidFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
	}{
		{"empty", ""},
		{"not-sched", "INFO: nothing here"},
		{"no-brackets", "SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12"},
		{"bad-elapsed", "SCHED xyzms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 []"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.line)
			if !errors.Is(err, ErrInvalidFormat) {
				t.Errorf("Parse(%q) error = %v, want ErrInvalidFormat", tc.line, err)
			}
		})
	}
}

func TestParseAll(t *testing.T) {
	t.Parallel()
	lines := []string{
		"SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 [34 28 31 29]",
		"some random log line to ignore",
		"SCHED 200ms: gomaxprocs=4 idleprocs=1 threads=5 spinningthreads=0 idlethreads=2 runqueue=5 [10 8 9 7]",
		"another non-sched line",
	}
	records, err := ParseAll(lines)
	if err != nil {
		t.Fatalf("ParseAll returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("ParseAll returned %d records, want 2", len(records))
	}
	if records[0].ElapsedMs != 100 {
		t.Errorf("records[0].ElapsedMs = %d, want 100", records[0].ElapsedMs)
	}
	if records[1].ElapsedMs != 200 {
		t.Errorf("records[1].ElapsedMs = %d, want 200", records[1].ElapsedMs)
	}
}
```

### Exercise 3: Example and demo

Create `example_test.go`:

```go
// example_test.go
package schedparse_test

import (
	"example.com/schedparse"
	"fmt"
)

func ExampleParse() {
	r, err := schedparse.Parse("SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 [34 28 31 29]")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("elapsed=%dms procs=%d runqueue=%d local=%v\n", r.ElapsedMs, r.Gomaxprocs, r.Runqueue, r.LocalQueues)
	// Output:
	// elapsed=100ms procs=4 runqueue=12 local=[34 28 31 29]
}
```

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"example.com/schedparse"
	"fmt"
)

func main() {
	input := []string{
		"SCHED 100ms: gomaxprocs=4 idleprocs=0 threads=5 spinningthreads=0 idlethreads=2 runqueue=12 [34 28 31 29]",
		"runtime: some other runtime message",
		"SCHED 200ms: gomaxprocs=4 idleprocs=1 threads=5 spinningthreads=0 idlethreads=2 runqueue=5 [10 8 9 7]",
		"SCHED 300ms: gomaxprocs=4 idleprocs=0 threads=6 spinningthreads=1 idlethreads=1 runqueue=20 [50 45 48 42]",
	}

	records, err := schedparse.ParseAll(input)
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	fmt.Printf("parsed %d schedtrace records\n\n", len(records))
	for _, r := range records {
		total := r.Runqueue
		for _, q := range r.LocalQueues {
			total += q
		}
		fmt.Printf("t=%4dms  procs=%d  global_q=%3d  local_q=%v  total_runnable=%d\n",
			r.ElapsedMs, r.Gomaxprocs, r.Runqueue, r.LocalQueues, total)
	}
}
```

## Common Mistakes

**Wrong**: Assuming all lines in a process's stderr are schedtrace lines when `GODEBUG=schedtrace` is set.

What happens: Other runtime messages (GC, panics, fatal errors) are also written to stderr. Passing non-SCHED lines to `Parse` returns `ErrInvalidFormat`. `ParseAll` silently skips them, which is the correct behavior.

**Fix**: Use `ParseAll` when processing real output; check `errors.Is(err, schedparse.ErrInvalidFormat)` when handling individual lines.

---

**Wrong**: Parsing the elapsed field without handling the "ms" suffix.

What happens: `strconv.Atoi("100ms")` returns an error. The field in the line is literally `100ms:` — you must strip both the colon and the "ms" suffix before parsing.

**Fix**: Use `strings.TrimSuffix(token, ":")` then `strings.TrimSuffix(token, "ms")` before calling `strconv.Atoi`.

---

**Wrong**: Treating `idlethreads` as "threads not doing useful work".

What happens: `idlethreads` counts M's that are parked in the OS scheduler (sleeping). `spinningthreads` counts M's that are awake but actively searching for runnable goroutines. Both are "not running goroutines" but they have very different CPU costs: spinning threads burn a core, idle threads do not.

**Fix**: Monitor `spinningthreads` separately. High sustained values indicate the scheduler is having trouble finding work, which can waste CPU.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo to see parsed output:

```bash
go run ./cmd/demo
```

To observe real schedtrace output from a running program:

```bash
GODEBUG=schedtrace=100 go run ./cmd/demo 2>&1 | grep SCHED
```

## Summary

- `GODEBUG=schedtrace=<ms>` prints one scheduler state line per interval — zero code changes needed.
- Each line reports: elapsed time, GOMAXPROCS, idle P count, thread count, spinning thread count, idle thread count, global queue length, and per-P local queue lengths.
- `idleprocs=0` with full local queues means the scheduler is fully utilized.
- `spinningthreads > 0` indicates M's are actively searching for work — watch for sustained high values.
- Parsing the output requires stripping the "ms" suffix on the elapsed field and splitting the bracket-enclosed local queue values.
- `ParseAll` skips non-SCHED lines, which is the correct behavior for real program stderr.

## What's Next

Next: [Scheduler Latency Trace](../08-scheduler-latency-trace/08-scheduler-latency-trace.md).

## Resources

- [runtime package — GODEBUG environment variables](https://pkg.go.dev/runtime#hdr-Environment_Variables)
- [Go Scheduler Design Document](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw)
- [Scheduling in Go Part III: Concurrency (Ardan Labs)](https://www.ardanlabs.com/blog/2018/12/scheduling-in-go-part3.html)
- [strconv package](https://pkg.go.dev/strconv)
- [strings package](https://pkg.go.dev/strings)
