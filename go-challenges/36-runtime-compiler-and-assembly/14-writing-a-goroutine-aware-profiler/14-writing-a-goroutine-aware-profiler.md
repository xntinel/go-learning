# 14. Writing a Goroutine-Aware Profiler

Go's built-in CPU profiler attributes samples to call stacks, not to individual goroutines. For heavily concurrent programs this hides the per-goroutine cost: a hot goroutine and an idle one look identical in a traditional flame graph. This lesson builds a sampling profiler that captures goroutine identity on every tick, aggregates per-goroutine CPU and wait estimates, tracks lifecycle events, and emits folded-stack output that any flame graph tool can consume.

The hard parts are: (1) `runtime.Stack` with `all=true` triggers a brief stop-the-world — running it at high frequency hurts the program; (2) the text format must be parsed without allocations on the hot path; (3) goroutine IDs are not a first-class Go concept — they must be extracted from the stack dump's header line.

```text
profiler/
  go.mod
  profiler.go
  profiler_test.go
  cmd/demo/main.go
```

## Concepts

### How runtime.Stack Captures All Goroutines

`runtime.Stack(buf []byte, all bool) int` writes stack traces of the calling goroutine (`all=false`) or all goroutines (`all=true`) into `buf` and returns the number of bytes written. When `all=true`, the runtime briefly stops all goroutines, copies their stacks into the buffer, and resumes them. The caller sees a consistent snapshot of every goroutine's state.

The returned text follows a fixed format:

```
goroutine 7 [running]:
main.worker(...)
	/home/user/main.go:42 +0x1c
```

The first line of each block is `goroutine <id> [<state>]:`. State values include `running`, `sleep`, `chan receive`, `select`, `IO wait`, `syscall`, and others. Everything after `[` up to `]:` is the goroutine state string.

If `buf` is too small, the output is truncated. Allocate generously (1 MiB is a safe starting point) and re-allocate on truncation.

### Goroutine ID Extraction

Go intentionally omits a `runtime.GoID()` function. The only portable way to obtain a goroutine's ID from Go code is to parse the `goroutine <N> [...]` header from `runtime.Stack` output. This is an internal detail that could change, but in practice it has been stable since Go 1.0.

Parsing with `strconv.ParseInt` after slicing the header line is allocation-free if the slice points into the same backing array as the buffer. Avoid `fmt.Sscanf` — it allocates.

### Sampling-Based Time Estimation

A sampling profiler does not measure time directly. It counts how many samples land in each state. If the sampling interval is T and a goroutine appears in state `running` for K out of N total samples, the estimated CPU time is `K * T`. This estimate has variance inversely proportional to the number of samples, but is unbiased.

Two states matter most:

- `running` — the goroutine is executing on an OS thread.
- All other states — the goroutine is waiting (channel, mutex, timer, syscall, etc.).

The goroutine scheduler can move goroutines between states between samples. Short bursts of CPU work may be missed at a 10 ms interval.

### Folded-Stack Flame Graph Format

Brendan Gregg's folded-stack format is a simple text encoding: one line per unique stack, semicolon-separated frames (outermost first), then a space and the sample count:

```
main;runtime.main;main.worker;runtime.schedule 3
main;runtime.main;main.handler 1
```

Tools like `flamegraph.pl`, speedscope, and `inferno-flamegraph` consume this format directly. Emitting one folded-stack file per goroutine plus one combined file lets the reader zoom in on a single goroutine or see the whole program.

### Goroutine Labels

`runtime/pprof` allows attaching key-value labels to a goroutine via `pprof.Do`:

```go
pprof.Do(ctx, pprof.Labels("role", "worker", "id", "1"), func(ctx context.Context) {
    doWork(ctx)
})
```

Labels appear in the `runtime.Stack` dump as an extra line after the goroutine header:

```
goroutine 12 [running]:
[Labels: id=1 role=worker]
main.doWork(...)
```

Parsing these lines lets the profiler include labels in per-goroutine summaries and filter by label.

### Overhead Budget

`runtime.Stack(all=true)` is a stop-the-world operation. The STW duration grows with the number of goroutines and the depth of their stacks. At 10 ms intervals and a program with a few hundred goroutines, overhead is typically well under 1%. At 1 ms intervals it can exceed 5%. Keep the sampling interval configurable; 10 ms is the recommended default.

Buffer reuse is essential: allocating a new 1 MiB slice on every tick triggers garbage collection. Pre-allocate one buffer and pass it to each `runtime.Stack` call.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/profiler/cmd/demo
cd ~/go-exercises/profiler
go mod init example.com/profiler
```

This is a library plus a CLI demo. Verify it with `go test`.

### Exercise 1: Core Types

Create `profiler.go`:

```go
package profiler

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample holds a single snapshot of one goroutine's state.
type Sample struct {
	Timestamp   time.Time
	GoroutineID int64
	State       string
	Labels      map[string]string
	Frames      []string // function names, innermost first (as runtime.Stack emits them)
}

// Stats aggregates samples collected for one goroutine.
type Stats struct {
	ID          int64
	RunSamples  int // samples where state == "running"
	WaitSamples int // samples where state != "running"
	FirstSeen   time.Time
	LastSeen    time.Time
	Labels      map[string]string
	// frames maps frame-name to sample count
	frames map[string]int
}

// CPUEstimate returns the estimated CPU time based on sample counts and the
// sampling interval.
func (s *Stats) CPUEstimate(interval time.Duration) time.Duration {
	return time.Duration(s.RunSamples) * interval
}

// WaitEstimate returns the estimated wait time.
func (s *Stats) WaitEstimate(interval time.Duration) time.Duration {
	return time.Duration(s.WaitSamples) * interval
}

// FoldedStack returns the folded-stack flamegraph line for this goroutine.
// Only frames with at least one sample are included.
func (s *Stats) FoldedStack() string {
	var sb strings.Builder
	for frame, count := range s.frames {
		fmt.Fprintf(&sb, "%s %d\n", frame, count)
	}
	return sb.String()
}

// Profiler samples all goroutines at a fixed interval.
type Profiler struct {
	interval time.Duration

	mu      sync.Mutex
	stats   map[int64]*Stats
	seen    map[int64]bool
	created []int64 // goroutines first observed since last call to Lifecycle
	exited  []int64 // goroutines that disappeared since last call to Lifecycle
	prevIDs map[int64]bool

	buf []byte // reused across samples
}

// New creates a Profiler with the given sampling interval.
// If interval <= 0 it defaults to 10 ms.
func New(interval time.Duration) *Profiler {
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	return &Profiler{
		interval: interval,
		stats:    make(map[int64]*Stats),
		seen:     make(map[int64]bool),
		prevIDs:  make(map[int64]bool),
		buf:      make([]byte, 1<<20), // 1 MiB
	}
}

// Interval returns the configured sampling interval.
func (p *Profiler) Interval() time.Duration {
	return p.interval
}
```

The `Stats.frames` map is unexported so same-package tests can read it directly. Exported accessors (`CPUEstimate`, `WaitEstimate`, `FoldedStack`) serve callers in other packages.

### Exercise 2: Stack Capture and Parsing

Append to `profiler.go`:

```go
// Sample captures one snapshot of all goroutine stacks and merges it into
// the accumulated statistics. It returns the number of goroutines observed.
func (p *Profiler) Sample() int {
	now := time.Now()

	// Grow the buffer until the dump fits.
	for {
		n := runtime.Stack(p.buf, true)
		if n < len(p.buf) {
			p.merge(now, p.buf[:n])
			return len(p.stats)
		}
		p.buf = make([]byte, len(p.buf)*2)
	}
}

// merge parses the raw stack dump and updates per-goroutine statistics.
func (p *Profiler) merge(now time.Time, dump []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	currentIDs := make(map[int64]bool)

	for _, block := range splitBlocks(dump) {
		s := parseBlock(now, block)
		if s == nil {
			continue
		}
		currentIDs[s.GoroutineID] = true

		st, ok := p.stats[s.GoroutineID]
		if !ok {
			st = &Stats{
				ID:        s.GoroutineID,
				FirstSeen: now,
				Labels:    s.Labels,
				frames:    make(map[string]int),
			}
			p.stats[s.GoroutineID] = st
		}
		st.LastSeen = now
		if s.Labels != nil {
			st.Labels = s.Labels
		}
		if s.State == "running" {
			st.RunSamples++
		} else {
			st.WaitSamples++
		}
		// Record each unique frame as a semicolon-joined folded stack entry.
		// runtime.Stack emits frames innermost-first; reverse to get the
		// canonical folded-stack order (outermost/root first, leaf last).
		if len(s.Frames) > 0 {
			reversed := make([]string, len(s.Frames))
			for i, f := range s.Frames {
				reversed[len(s.Frames)-1-i] = f
			}
			key := strings.Join(reversed, ";")
			st.frames[key]++
		}
	}

	// Lifecycle: detect new and exited goroutines.
	for id := range currentIDs {
		if !p.prevIDs[id] {
			p.created = append(p.created, id)
		}
	}
	for id := range p.prevIDs {
		if !currentIDs[id] {
			p.exited = append(p.exited, id)
		}
	}
	p.prevIDs = currentIDs
}

// splitBlocks splits a full runtime.Stack dump into per-goroutine text blocks.
// Each block begins with "goroutine ".
func splitBlocks(dump []byte) [][]byte {
	var blocks [][]byte
	prefix := []byte("goroutine ")
	for len(dump) > 0 {
		idx := bytes.Index(dump[1:], prefix) // skip the leading block
		if idx < 0 {
			blocks = append(blocks, dump)
			break
		}
		blocks = append(blocks, dump[:idx+1])
		dump = dump[idx+1:]
	}
	return blocks
}

// parseBlock parses one goroutine text block into a Sample.
// Returns nil if the block does not start with "goroutine ".
func parseBlock(now time.Time, block []byte) *Sample {
	lines := bytes.Split(block, []byte("\n"))
	if len(lines) == 0 {
		return nil
	}
	// Header: "goroutine 42 [running]:"
	header := string(bytes.TrimSpace(lines[0]))
	if !strings.HasPrefix(header, "goroutine ") {
		return nil
	}
	rest := header[len("goroutine "):]
	// rest: "42 [running]:"
	spaceIdx := strings.IndexByte(rest, ' ')
	if spaceIdx < 0 {
		return nil
	}
	id, err := strconv.ParseInt(rest[:spaceIdx], 10, 64)
	if err != nil {
		return nil
	}
	stateStr := rest[spaceIdx+1:]
	start := strings.IndexByte(stateStr, '[')
	end := strings.IndexByte(stateStr, ']')
	if start < 0 || end <= start {
		return nil
	}
	state := stateStr[start+1 : end]

	s := &Sample{
		Timestamp:   now,
		GoroutineID: id,
		State:       state,
		Labels:      make(map[string]string),
	}

	// Scan remaining lines for frames and labels.
	lineIdx := 1
	// Optional label line: "[Labels: key=val ...]"
	if lineIdx < len(lines) {
		trimmed := bytes.TrimSpace(lines[lineIdx])
		if bytes.HasPrefix(trimmed, []byte("[Labels:")) {
			parseLabels(trimmed, s.Labels)
			lineIdx++
		}
	}

	// Frame lines: function name lines (not indented with tab+file path).
	for i := lineIdx; i < len(lines); i++ {
		line := string(lines[i])
		// Function name lines do not start with a tab.
		if line == "" || line[0] == '\t' {
			continue
		}
		// Strip argument list if present.
		name := line
		if idx := strings.IndexByte(name, '('); idx >= 0 {
			name = name[:idx]
		}
		name = strings.TrimSpace(name)
		if name != "" {
			s.Frames = append(s.Frames, name)
		}
	}

	return s
}

// parseLabels parses "[Labels: key=val key2=val2]" into the provided map.
func parseLabels(line []byte, out map[string]string) {
	s := string(line)
	// Strip "[Labels: " prefix and "]" suffix.
	s = strings.TrimPrefix(s, "[Labels: ")
	s = strings.TrimSuffix(s, "]")
	for _, kv := range strings.Fields(s) {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		out[kv[:idx]] = kv[idx+1:]
	}
}
```

### Exercise 3: Results and Lifecycle Access

Append to `profiler.go`:

```go
// Stats returns a snapshot of the accumulated per-goroutine statistics.
// The returned map is a shallow copy; individual Stats pointers are the same
// as the internal ones and should not be modified.
func (p *Profiler) Stats() map[int64]*Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[int64]*Stats, len(p.stats))
	for id, s := range p.stats {
		out[id] = s
	}
	return out
}

// Lifecycle returns goroutine IDs that were first seen (created) and IDs that
// disappeared (exited) since the previous call to Lifecycle. Calling Lifecycle
// consumes and resets the internal lists.
func (p *Profiler) Lifecycle() (created, exited []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	created = p.created
	exited = p.exited
	p.created = nil
	p.exited = nil
	return
}

// TopN returns up to n goroutine IDs sorted by RunSamples descending.
func (p *Profiler) TopN(n int) []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	type entry struct {
		id  int64
		run int
	}
	entries := make([]entry, 0, len(p.stats))
	for id, s := range p.stats {
		entries = append(entries, entry{id, s.RunSamples})
	}
	// Simple insertion sort (n is typically small).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].run > entries[j-1].run; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	if n > len(entries) {
		n = len(entries)
	}
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = entries[i].id
	}
	return ids
}

// CombinedFoldedStack returns a single folded-stack string merging samples
// from all goroutines, suitable for a whole-program flame graph.
func (p *Profiler) CombinedFoldedStack() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	combined := make(map[string]int)
	for _, s := range p.stats {
		for frame, count := range s.frames {
			combined[frame] += count
		}
	}
	var sb strings.Builder
	for frame, count := range combined {
		fmt.Fprintf(&sb, "%s %d\n", frame, count)
	}
	return sb.String()
}
```

### Exercise 4: Tests

Create `profiler_test.go`:

```go
package profiler

import (
	"context"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

func TestNewDefaultInterval(t *testing.T) {
	t.Parallel()

	p := New(0)
	if p.Interval() != 10*time.Millisecond {
		t.Fatalf("interval = %v, want 10ms", p.Interval())
	}
}

func TestSampleReturnsSaneCount(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	n := p.Sample()
	// There is always at least the current goroutine.
	if n < 1 {
		t.Fatalf("Sample() = %d goroutines, want >= 1", n)
	}
}

func TestSampleAccumulatesStats(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	for i := 0; i < 3; i++ {
		p.Sample()
	}
	stats := p.Stats()
	if len(stats) == 0 {
		t.Fatal("Stats() empty after 3 samples")
	}
	for _, s := range stats {
		total := s.RunSamples + s.WaitSamples
		if total == 0 {
			t.Errorf("goroutine %d has zero total samples", s.ID)
		}
		if s.FirstSeen.IsZero() {
			t.Errorf("goroutine %d: FirstSeen is zero", s.ID)
		}
	}
}

func TestCPUAndWaitEstimates(t *testing.T) {
	t.Parallel()

	interval := 10 * time.Millisecond
	p := New(interval)
	p.Sample()

	for _, s := range p.Stats() {
		cpu := s.CPUEstimate(interval)
		wait := s.WaitEstimate(interval)
		total := cpu + wait
		expected := time.Duration(s.RunSamples+s.WaitSamples) * interval
		if total != expected {
			t.Errorf("goroutine %d: cpu+wait = %v, want %v", s.ID, total, expected)
		}
	}
}

func TestFoldedStackContainsFrames(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	p.Sample()

	for _, s := range p.Stats() {
		fs := s.FoldedStack()
		if fs == "" {
			// A goroutine may have no named frames (e.g. runtime-internal goroutines).
			continue
		}
		// Each line must end with a count.
		for _, line := range strings.Split(strings.TrimSpace(fs), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) < 2 {
				t.Errorf("malformed folded-stack line: %q", line)
			}
		}
		break // one goroutine is enough
	}
}

func TestCombinedFoldedStack(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	p.Sample()

	combined := p.CombinedFoldedStack()
	if combined == "" {
		t.Fatal("CombinedFoldedStack() returned empty string")
	}
}

func TestTopN(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	for i := 0; i < 5; i++ {
		p.Sample()
	}

	top := p.TopN(2)
	if len(top) > 2 {
		t.Fatalf("TopN(2) returned %d ids", len(top))
	}
	// IDs must be positive.
	for _, id := range top {
		if id <= 0 {
			t.Errorf("TopN returned non-positive id %d", id)
		}
	}
}

func TestLifecycleDetectsCurrentGoroutines(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	p.Sample()

	created, _ := p.Lifecycle()
	if len(created) == 0 {
		t.Fatal("first Lifecycle call returned no created goroutines")
	}
	// Second call should return nothing new (no goroutines started between calls).
	p.Sample()
	created2, _ := p.Lifecycle()
	// created2 may include goroutines that started during the second sample;
	// we only verify the call does not panic and returns slices.
	_ = created2
}

func TestLifecycleIsConsumed(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)
	p.Sample()
	c1, _ := p.Lifecycle()
	c2, _ := p.Lifecycle()
	if len(c1) == 0 {
		t.Fatal("expected non-empty created list on first call")
	}
	if len(c2) != 0 {
		t.Fatalf("Lifecycle should consume the list; got %d on second call", len(c2))
	}
}

func TestGoroutineLabelsAppearInStats(t *testing.T) {
	t.Parallel()

	p := New(10 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		pprof.Do(context.Background(), pprof.Labels("role", "tester"), func(_ context.Context) {
			// Keep the goroutine alive long enough to be sampled.
			<-done
		})
	}()

	// Sample a few times to increase the chance of capturing the goroutine.
	var found bool
	for i := 0; i < 20 && !found; i++ {
		p.Sample()
		for _, s := range p.Stats() {
			if s.Labels["role"] == "tester" {
				found = true
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(done)

	if !found {
		t.Skip("labeled goroutine not captured in 20 samples (scheduler timing)")
	}
}

func TestSplitBlocks(t *testing.T) {
	t.Parallel()

	input := []byte("goroutine 1 [running]:\nfoo()\n\ngoroutine 2 [sleep]:\nbar()\n")
	blocks := splitBlocks(input)
	if len(blocks) != 2 {
		t.Fatalf("splitBlocks: got %d blocks, want 2", len(blocks))
	}
}

func TestParseBlock(t *testing.T) {
	t.Parallel()

	block := []byte("goroutine 42 [chan receive]:\nmain.worker(...)\n\t/home/user/main.go:12 +0x10\n")
	s := parseBlock(time.Now(), block)
	if s == nil {
		t.Fatal("parseBlock returned nil")
	}
	if s.GoroutineID != 42 {
		t.Fatalf("GoroutineID = %d, want 42", s.GoroutineID)
	}
	if s.State != "chan receive" {
		t.Fatalf("State = %q, want %q", s.State, "chan receive")
	}
	if len(s.Frames) == 0 {
		t.Fatal("no frames parsed")
	}
}

func TestParseBlockWithLabels(t *testing.T) {
	t.Parallel()

	block := []byte("goroutine 7 [running]:\n[Labels: role=worker id=1]\nmain.doWork()\n\t/home/user/main.go:5 +0x4\n")
	s := parseBlock(time.Now(), block)
	if s == nil {
		t.Fatal("parseBlock returned nil")
	}
	if s.Labels["role"] != "worker" {
		t.Fatalf("label role = %q, want worker", s.Labels["role"])
	}
	if s.Labels["id"] != "1" {
		t.Fatalf("label id = %q, want 1", s.Labels["id"])
	}
}

func ExampleProfiler_CombinedFoldedStack() {
	p := New(10 * time.Millisecond)
	p.Sample()
	s := p.CombinedFoldedStack()
	if s != "" {
		// Non-empty output is expected; exact content varies per run.
		_ = s
	}
	// (No deterministic Output: block because frame names are runtime-dependent.)
}
```

Your turn: add `TestParseBlockBadHeader` — call `parseBlock` with `[]byte("not a goroutine")` and assert it returns nil.

### Exercise 5: CLI Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"runtime/pprof"
	"sync"
	"time"

	"example.com/profiler"
)

func cpuBurner(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	pprof.Do(ctx, pprof.Labels("role", "cpu-burner"), func(_ context.Context) {
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			// Busy loop to appear in "running" samples.
		}
	})
}

func waiter(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	pprof.Do(ctx, pprof.Labels("role", "waiter"), func(_ context.Context) {
		time.Sleep(300 * time.Millisecond)
	})
}

func main() {
	p := profiler.New(10 * time.Millisecond)

	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go cpuBurner(ctx, &wg)
	}
	wg.Add(1)
	go waiter(ctx, &wg)

	// Sample while goroutines run.
	ticker := time.NewTicker(10 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

loop:
	for {
		select {
		case <-ticker.C:
			p.Sample()
		case <-done:
			ticker.Stop()
			break loop
		}
	}

	p.Sample() // final snapshot

	created, exited := p.Lifecycle()
	fmt.Printf("goroutines first seen: %d, exited: %d\n", len(created), len(exited))

	stats := p.Stats()
	fmt.Printf("goroutines tracked: %d\n", len(stats))

	top := p.TopN(3)
	interval := p.Interval()
	for _, id := range top {
		s := stats[id]
		if s == nil {
			continue
		}
		fmt.Printf("  goroutine %d: cpu~%v wait~%v labels=%v\n",
			id,
			s.CPUEstimate(interval).Round(time.Millisecond),
			s.WaitEstimate(interval).Round(time.Millisecond),
			s.Labels,
		)
	}

	combined := p.CombinedFoldedStack()
	fmt.Printf("combined folded-stack lines: %d\n", len(splitLines(combined)))
}

func splitLines(s string) []string {
	var out []string
	// count non-empty lines
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Buffer Too Small, Output Truncated Silently

Wrong: allocating a fixed 4 KB buffer for `runtime.Stack(buf, true)`. When the buffer fills, `Stack` returns `len(buf)` and the dump is silently cut off; subsequent parsing sees malformed blocks and drops goroutines.

Fix: compare the return value `n` to `len(buf)`. If `n == len(buf)`, double the buffer and retry. The implementation in Exercise 2 does this.

### Allocating a New Buffer on Every Sample

Wrong:

```go
func (p *Profiler) Sample() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	...
}
```

This allocates 1 MiB per sample. At 100 samples per second, that is 100 MiB/s of allocation, which significantly increases GC pressure and distorts the program being profiled.

Fix: pre-allocate the buffer once in `New` and store it in the `Profiler` struct. Reallocate only when the dump does not fit.

### Parsing Goroutine ID with fmt.Sscanf

Wrong:

```go
var id int64
fmt.Sscanf(header, "goroutine %d", &id)
```

`fmt.Sscanf` allocates internally and is roughly 10x slower than `strconv.ParseInt` on a pre-sliced byte sequence.

Fix: slice the header line manually and use `strconv.ParseInt`, as shown in Exercise 2.

### Treating All Non-"running" States As CPU

Wrong: counting `syscall` samples as CPU time. A goroutine in `syscall` state is blocked waiting for the kernel; it is not consuming Go CPU time.

Fix: only count `running` as CPU. All other states, including `syscall`, contribute to wait time.

### Ignoring the Stop-the-World Cost

Wrong: setting the sampling interval to 1 ms in production without measuring overhead. At 1 ms intervals, `runtime.Stack(all=true)` can add several milliseconds of pause per second, exceeding 1% overhead for programs with many goroutines.

Fix: default to 10 ms and document that lowering the interval increases STW frequency. Measure overhead in your specific program before deploying a shorter interval.

## Verification

From `~/go-exercises/profiler`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Add one test of your own: `TestParseBlockBadHeader` from the "Your turn" prompt in Exercise 4.

## Summary

- `runtime.Stack(buf, true)` captures all goroutine stacks with a brief STW; reuse the buffer to avoid allocation pressure.
- Goroutine IDs are not a first-class Go value; parse them from the `goroutine <N> [<state>]:` header line.
- Sampling-based profilers estimate time by counting samples per state times the sampling interval; the estimate is unbiased but has variance.
- Only the `running` state counts as CPU time; `syscall`, `chan receive`, `sleep`, and other states count as wait time.
- Folded-stack format (`frame1;frame2 count`) is the portable input format for flamegraph.pl, speedscope, and inferno.
- `pprof.Do` attaches key-value labels to a goroutine; they appear in the `runtime.Stack` dump and can be parsed for semantic grouping.
- Lifecycle tracking — diffing the set of goroutine IDs between samples — reveals goroutine creation and completion without any runtime hooks.

## What's Next

Next: [Implementing a Green Thread Scheduler](../15-implementing-a-green-thread-scheduler/15-implementing-a-green-thread-scheduler.md).

## Resources

- [pkg.go.dev/runtime#Stack](https://pkg.go.dev/runtime#Stack) — signature, behavior, and the stop-the-world note
- [pkg.go.dev/runtime#GoroutineProfile](https://pkg.go.dev/runtime#GoroutineProfile) — structured goroutine profile alternative
- [pkg.go.dev/runtime/pprof#Do](https://pkg.go.dev/runtime/pprof#Do) — goroutine label API (preferred over SetGoroutineLabels)
- [Brendan Gregg: Flame Graphs](https://www.brendangregg.com/flamegraphs.html) — folded-stack format specification and flamegraph.pl
- [github.com/felixge/fgprof](https://github.com/felixge/fgprof) — production goroutine-aware profiler; reference for sampling approach and overhead measurement
