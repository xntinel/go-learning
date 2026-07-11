# Exercise 21: Goroutine Profiling with pprof

A Go service that degrades after hours in production gives you only vague
symptoms: latency creeps up, memory climbs, and eventually the pod gets
OOM-killed. A restart makes it go away, until it comes back. The real cause is
almost always one of two things -- a goroutine leak, or blocked goroutines
piling up. This exercise builds the tooling that turns "restart and hope" into
"found the root cause in five minutes": a count monitor, a stack-dump analyzer,
and pprof-backed HTTP endpoints that name exactly which function is leaking.

## What you'll build

```text
21-goroutine-profiling-pprof/
  main.go        count monitor, stack-dump analyzer, pprof HTTP endpoints, and
                 a snapshot-diffing leak detector (each step a runnable program)
```

- Build: goroutine diagnostics -- a count monitor, a stack-dump analyzer, and pprof HTTP profiling endpoints.
- Implement: `GoroutineMonitor`, `GoroutineProfiler.CaptureStacks`/`Analyze`, a `MetricsService` around `pprof.Lookup("goroutine")`, and a `LeakDetector` that diffs snapshots over time.
- Verify: `go run main.go` (each step is its own runnable program).

### Why the count is the alarm and the stack dump is the diagnosis

`runtime.NumGoroutine()` tells you how many goroutines exist right now. If that number grows monotonically over hours, you have a leak. But the count alone does not tell you *which* goroutines are leaking. That is where `runtime/pprof` and `runtime.Stack` come in -- they capture full stack traces of every goroutine, grouped by state (running, waiting on channel, waiting on mutex, sleeping). By analyzing the dump, you can see that 5,000 goroutines are stuck on the same channel receive in `processOrder`, which means the producer stopped sending but consumers were never cleaned up.

This is the difference between "restart the pod and hope it goes away" and "found the root cause in 5 minutes." Every production Go service should have profiling endpoints accessible to operators. The standard library provides everything you need -- no third-party tools required.


## Step 1 -- Goroutine Count Monitor

Build a service that tracks goroutine count over time, detecting upward trends that signal leaks.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	monitorInterval  = 200 * time.Millisecond
	monitorDuration  = 2 * time.Second
	leakBatchSize    = 5
	leakInterval     = 300 * time.Millisecond
)

type GoroutineMonitor struct {
	samples []int
	mu      sync.Mutex
	stop    chan struct{}
}

func NewGoroutineMonitor() *GoroutineMonitor {
	return &GoroutineMonitor{
		samples: make([]int, 0, 20),
		stop:    make(chan struct{}),
	}
}

func (m *GoroutineMonitor) Start() {
	go m.collectLoop()
}

func (m *GoroutineMonitor) Stop() {
	close(m.stop)
}

func (m *GoroutineMonitor) collectLoop() {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			count := runtime.NumGoroutine()
			m.mu.Lock()
			m.samples = append(m.samples, count)
			m.mu.Unlock()
			fmt.Printf("  [monitor] goroutines: %d\n", count)
		case <-m.stop:
			return
		}
	}
}

func (m *GoroutineMonitor) SampleCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.samples)
}

func (m *GoroutineMonitor) Report() (minVal, maxVal, latest int, trend string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.samples) == 0 {
		return 0, 0, 0, "no data"
	}

	minVal = m.samples[0]
	maxVal = m.samples[0]
	for _, s := range m.samples {
		if s < minVal {
			minVal = s
		}
		if s > maxVal {
			maxVal = s
		}
	}
	latest = m.samples[len(m.samples)-1]

	if len(m.samples) >= 3 {
		first := m.samples[0]
		last := m.samples[len(m.samples)-1]
		if last > first+5 {
			trend = "GROWING -- possible leak"
		} else if last < first-5 {
			trend = "SHRINKING"
		} else {
			trend = "STABLE"
		}
	} else {
		trend = "insufficient data"
	}
	return
}

func simulateLeakyWork() {
	for i := 0; i < 3; i++ {
		for j := 0; j < leakBatchSize; j++ {
			ch := make(chan struct{})
			go func(id int) {
				<-ch // blocks forever -- channel is never closed
			}(i*leakBatchSize + j)
		}
		time.Sleep(leakInterval)
	}
}

func main() {
	monitor := NewGoroutineMonitor()
	monitor.Start()

	fmt.Println("=== Goroutine Count Monitor ===")
	fmt.Printf("  Initial goroutines: %d\n\n", runtime.NumGoroutine())

	go simulateLeakyWork()

	time.Sleep(monitorDuration)
	monitor.Stop()

	minVal, maxVal, latest, trend := monitor.Report()
	fmt.Printf("\n=== Monitor Report ===\n")
	fmt.Printf("  Samples:  %d\n", monitor.SampleCount())
	fmt.Printf("  Min:      %d\n", minVal)
	fmt.Printf("  Max:      %d\n", maxVal)
	fmt.Printf("  Latest:   %d\n", latest)
	fmt.Printf("  Trend:    %s\n", trend)
}
```

**What's happening here:** The `GoroutineMonitor` periodically samples `runtime.NumGoroutine()` and stores the history. `simulateLeakyWork` intentionally creates goroutines that block forever on channels that are never closed -- a realistic leak pattern. The monitor detects the upward trend by comparing the first and last samples.

**Key insight:** `runtime.NumGoroutine()` is the first diagnostic you check in production. If it grows over time, you have a leak. But the count alone does not tell you *where* the leak is -- that requires stack analysis in the next step.

### Verification
```bash
go run main.go
```
Expected output (goroutine counts may vary slightly):
```
=== Goroutine Count Monitor ===
  Initial goroutines: 4

  [monitor] goroutines: 4
  [monitor] goroutines: 9
  [monitor] goroutines: 9
  [monitor] goroutines: 14
  [monitor] goroutines: 14
  [monitor] goroutines: 19
  [monitor] goroutines: 19
  [monitor] goroutines: 19
  [monitor] goroutines: 19

=== Monitor Report ===
  Samples:  9
  Min:      4
  Max:      19
  Latest:   19
  Trend:    GROWING -- possible leak
```


## Step 2 -- Stack Dump Analysis

Capture goroutine stack traces using `runtime.Stack` and parse them to identify which functions have the most blocked goroutines.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	stackBufferSize = 1 << 16
	leakyWorkers    = 8
	healthyWorkers  = 3
)

type StackAnalysis struct {
	TotalGoroutines int
	ByState         map[string]int
	ByFunction      map[string]int
}

type GoroutineProfiler struct {
	mu sync.Mutex
}

func NewGoroutineProfiler() *GoroutineProfiler {
	return &GoroutineProfiler{}
}

func (p *GoroutineProfiler) CaptureStacks() string {
	buf := make([]byte, stackBufferSize)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

func (p *GoroutineProfiler) Analyze(dump string) StackAnalysis {
	analysis := StackAnalysis{
		ByState:    make(map[string]int),
		ByFunction: make(map[string]int),
	}

	blocks := strings.Split(dump, "\n\n")
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		lines := strings.Split(block, "\n")
		if len(lines) < 2 {
			continue
		}

		header := lines[0]
		if !strings.HasPrefix(header, "goroutine ") {
			continue
		}

		analysis.TotalGoroutines++

		if start := strings.Index(header, "["); start != -1 {
			if end := strings.Index(header[start:], "]"); end != -1 {
				state := header[start+1 : start+end]
				if commaIdx := strings.Index(state, ","); commaIdx != -1 {
					state = state[:commaIdx]
				}
				analysis.ByState[state]++
			}
		}

		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "main.") && strings.Contains(line, "(") {
				funcName := line[:strings.Index(line, "(")]
				analysis.ByFunction[funcName]++
				break
			}
		}
	}

	return analysis
}

func leakyHandler(ch chan struct{}) {
	<-ch
}

func healthyHandler(wg *sync.WaitGroup) {
	defer wg.Done()
	time.Sleep(2 * time.Second)
}

func main() {
	profiler := NewGoroutineProfiler()

	fmt.Println("=== Goroutine Stack Dump Analysis ===")
	fmt.Printf("  Goroutines before: %d\n\n", runtime.NumGoroutine())

	leakChannels := make([]chan struct{}, leakyWorkers)
	for i := 0; i < leakyWorkers; i++ {
		leakChannels[i] = make(chan struct{})
		go leakyHandler(leakChannels[i])
	}

	var wg sync.WaitGroup
	for i := 0; i < healthyWorkers; i++ {
		wg.Add(1)
		go healthyHandler(&wg)
	}

	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  Goroutines after spawning: %d\n\n", runtime.NumGoroutine())

	dump := profiler.CaptureStacks()
	analysis := profiler.Analyze(dump)

	fmt.Printf("=== Stack Analysis ===\n")
	fmt.Printf("  Total goroutines: %d\n\n", analysis.TotalGoroutines)

	fmt.Println("  By state:")
	for state, count := range analysis.ByState {
		fmt.Printf("    %-20s %d\n", state, count)
	}

	fmt.Println()
	fmt.Println("  By function (main.* only):")
	for fn, count := range analysis.ByFunction {
		fmt.Printf("    %-30s %d\n", fn, count)
	}

	fmt.Println()
	fmt.Println("  Diagnosis:")
	if count, ok := analysis.ByFunction["main.leakyHandler"]; ok && count > 2 {
		fmt.Printf("    ALERT: %d goroutines blocked in main.leakyHandler\n", count)
		fmt.Println("    These goroutines are waiting on channels that may never close")
	}

	for i := range leakChannels {
		close(leakChannels[i])
	}
	wg.Wait()

	fmt.Printf("\n  Goroutines after cleanup: %d\n", runtime.NumGoroutine())
}
```

**What's happening here:** `runtime.Stack(buf, true)` captures stack traces for ALL goroutines (the `true` parameter means "all goroutines," not just the current one). The profiler parses the raw dump to extract goroutine states (running, chan receive, sleep) and the functions they are executing. This reveals that 8 goroutines are stuck in `leakyHandler` waiting on channel receives.

**Key insight:** The raw stack dump is the most powerful diagnostic tool for goroutine issues. Each goroutine entry shows its state (`chan receive`, `select`, `sleep`, `running`) and the full call stack. When you see hundreds of goroutines in the same state at the same call site, you have found your leak or bottleneck.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Goroutine Stack Dump Analysis ===
  Goroutines before: 1

  Goroutines after spawning: 12

=== Stack Analysis ===
  Total goroutines: 12

  By state:
    chan receive          8
    sleep                3
    running              1

  By function (main.* only):
    main.leakyHandler              8
    main.healthyHandler            3

  Diagnosis:
    ALERT: 8 goroutines blocked in main.leakyHandler
    These goroutines are waiting on channels that may never close

  Goroutines after cleanup: 1
```


## Step 3 -- Programmatic pprof Goroutine Profile

Use `runtime/pprof` to capture a structured goroutine profile and write it to a file, then build an HTTP endpoint that serves it on demand.

```go
package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"
)

const (
	profileWorkers  = 10
	serverAddr      = "127.0.0.1:0"
	profileFilename = "/tmp/goroutine_profile.txt"
)

type MetricsService struct {
	mu       sync.Mutex
	workers  []chan struct{}
	listener net.Listener
}

func NewMetricsService() *MetricsService {
	return &MetricsService{
		workers: make([]chan struct{}, 0),
	}
}

func (s *MetricsService) SpawnWorker(id int) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.workers = append(s.workers, ch)
	s.mu.Unlock()

	go func() {
		<-ch
		fmt.Printf("  worker %d: shutting down\n", id)
	}()
}

func (s *MetricsService) StopWorkers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.workers {
		close(ch)
	}
}

func (s *MetricsService) CaptureProfileToFile(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()

	profile := pprof.Lookup("goroutine")
	if profile == nil {
		return fmt.Errorf("goroutine profile not found")
	}

	return profile.WriteTo(f, 1)
}

func (s *MetricsService) CaptureProfileToBuffer() (string, error) {
	var buf bytes.Buffer
	profile := pprof.Lookup("goroutine")
	if profile == nil {
		return "", fmt.Errorf("goroutine profile not found")
	}

	if err := profile.WriteTo(&buf, 1); err != nil {
		return "", fmt.Errorf("write profile: %w", err)
	}

	return buf.String(), nil
}

func (s *MetricsService) StartHTTP() (string, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/debug/goroutines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		profile := pprof.Lookup("goroutine")
		if profile == nil {
			http.Error(w, "profile not found", http.StatusInternalServerError)
			return
		}
		profile.WriteTo(w, 1)
	})

	mux.HandleFunc("/debug/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "goroutines: %d\n", runtime.NumGoroutine())
		fmt.Fprintf(w, "GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
		fmt.Fprintf(w, "NumCPU: %d\n", runtime.NumCPU())

		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Fprintf(w, "Alloc: %d KB\n", m.Alloc/1024)
		fmt.Fprintf(w, "NumGC: %d\n", m.NumGC)
	})

	var err error
	s.listener, err = net.Listen("tcp", serverAddr)
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	go http.Serve(s.listener, mux)

	return s.listener.Addr().String(), nil
}

func (s *MetricsService) StopHTTP() {
	if s.listener != nil {
		s.listener.Close()
	}
}

func main() {
	svc := NewMetricsService()

	fmt.Println("=== Goroutine Profiling with pprof ===")
	fmt.Printf("  Initial goroutines: %d\n\n", runtime.NumGoroutine())

	for i := 1; i <= profileWorkers; i++ {
		svc.SpawnWorker(i)
	}
	time.Sleep(50 * time.Millisecond)
	fmt.Printf("  After spawning %d workers: %d goroutines\n\n", profileWorkers, runtime.NumGoroutine())

	addr, err := svc.StartHTTP()
	if err != nil {
		fmt.Printf("  Failed to start HTTP: %v\n", err)
		return
	}
	fmt.Printf("  HTTP profiling server: http://%s\n", addr)
	fmt.Printf("    GET /debug/goroutines  -- full goroutine dump\n")
	fmt.Printf("    GET /debug/summary     -- runtime summary\n\n")

	err = svc.CaptureProfileToFile(profileFilename)
	if err != nil {
		fmt.Printf("  Failed to capture profile: %v\n", err)
		return
	}

	fileInfo, _ := os.Stat(profileFilename)
	fmt.Printf("  Profile written to: %s (%d bytes)\n\n", profileFilename, fileInfo.Size())

	profileText, err := svc.CaptureProfileToBuffer()
	if err != nil {
		fmt.Printf("  Failed to capture profile: %v\n", err)
		return
	}

	fmt.Println("  === Profile Summary (first 15 lines) ===")
	lines := splitLines(profileText, 15)
	for _, line := range lines {
		fmt.Printf("    %s\n", line)
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/debug/summary", addr))
	if err != nil {
		fmt.Printf("  HTTP request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)

	fmt.Printf("\n  === HTTP /debug/summary ===\n")
	for _, line := range splitLines(buf.String(), 10) {
		fmt.Printf("    %s\n", line)
	}

	fmt.Println()
	svc.StopHTTP()
	svc.StopWorkers()
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("  After cleanup: %d goroutines\n", runtime.NumGoroutine())
	os.Remove(profileFilename)
}

func splitLines(s string, max int) []string {
	var result []string
	start := 0
	count := 0
	for i, c := range s {
		if c == '\n' {
			if count >= max {
				break
			}
			result = append(result, s[start:i])
			start = i + 1
			count++
		}
	}
	if start < len(s) && count < max {
		result = append(result, s[start:])
	}
	return result
}
```

**What's happening here:** `pprof.Lookup("goroutine")` accesses the built-in goroutine profile that Go maintains automatically. `WriteTo(w, 1)` outputs human-readable text (debug=1). The service exposes two HTTP endpoints: one for the full goroutine dump and one for a runtime summary. This is exactly the pattern production Go services use -- `net/http/pprof` does this automatically, but building it manually teaches you what it actually captures.

**Key insight:** The `runtime/pprof` package provides several profile types: `goroutine`, `heap`, `threadcreate`, `block`, and `mutex`. The goroutine profile is the most useful for diagnosing concurrency issues. Writing to a file lets you diff profiles taken at different times. The HTTP endpoint lets operators check production services without SSH access.

### Verification
```bash
go run main.go
```
Expected output (profile content and memory values will vary):
```
=== Goroutine Profiling with pprof ===
  Initial goroutines: 1

  After spawning 10 workers: 11 goroutines

  HTTP profiling server: http://127.0.0.1:52341
    GET /debug/goroutines  -- full goroutine dump
    GET /debug/summary     -- runtime summary

  Profile written to: /tmp/goroutine_profile.txt (4821 bytes)

  === Profile Summary (first 15 lines) ===
    goroutine profile: total 14

    10 @ 0x100d3df60 0x100d1c258 0x100d1c238 0x100d72e44 0x100d3e4e4
    #	0x100d72e43	main.NewMetricsService.SpawnWorker.func1+0x23	/path/main.go:40

    1 @ 0x100d3df60 0x100d44684 0x100d6e594 0x100d72da4 0x100d3e4e4
    #	0x100d6e593	net/http.(*Server).Serve+0x0	/path/server.go:3285

  === HTTP /debug/summary ===
    goroutines: 14
    GOMAXPROCS: 10
    NumCPU: 10
    Alloc: 312 KB
    NumGC: 0

  After cleanup: 2
```


## Step 4 -- Detecting Goroutine Accumulation Under Load

Build a complete scenario: a service that leaks goroutines under load, a profiler that captures snapshots at intervals, and a comparison report that pinpoints the leak.

```go
package main

import (
	"bytes"
	"fmt"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"
)

const (
	snapshotInterval  = 500 * time.Millisecond
	totalSnapshots    = 4
	requestsPerBatch  = 5
	requestInterval   = 200 * time.Millisecond
)

type ProfileSnapshot struct {
	Timestamp   time.Duration
	Goroutines  int
	TopStacks   map[string]int
}

type LeakDetector struct {
	mu        sync.Mutex
	snapshots []ProfileSnapshot
	start     time.Time
}

func NewLeakDetector() *LeakDetector {
	return &LeakDetector{
		snapshots: make([]ProfileSnapshot, 0),
		start:     time.Now(),
	}
}

func (d *LeakDetector) TakeSnapshot() ProfileSnapshot {
	var buf bytes.Buffer
	profile := pprof.Lookup("goroutine")
	profile.WriteTo(&buf, 1)

	topStacks := parseTopFunctions(buf.String())

	snap := ProfileSnapshot{
		Timestamp:  time.Since(d.start).Round(time.Millisecond),
		Goroutines: runtime.NumGoroutine(),
		TopStacks:  topStacks,
	}

	d.mu.Lock()
	d.snapshots = append(d.snapshots, snap)
	d.mu.Unlock()

	return snap
}

func (d *LeakDetector) Report() {
	d.mu.Lock()
	defer d.mu.Unlock()

	fmt.Println("=== Leak Detection Report ===")
	fmt.Printf("  Snapshots: %d\n\n", len(d.snapshots))

	for i, snap := range d.snapshots {
		fmt.Printf("  Snapshot %d (t=%v): %d goroutines\n", i+1, snap.Timestamp, snap.Goroutines)
		for fn, count := range snap.TopStacks {
			if count > 1 {
				fmt.Printf("    %-40s %d\n", fn, count)
			}
		}
		fmt.Println()
	}

	if len(d.snapshots) >= 2 {
		first := d.snapshots[0]
		last := d.snapshots[len(d.snapshots)-1]
		growth := last.Goroutines - first.Goroutines

		fmt.Printf("  Growth: %d -> %d (+%d goroutines)\n\n", first.Goroutines, last.Goroutines, growth)

		fmt.Println("  Functions with growing goroutine count:")
		for fn, lastCount := range last.TopStacks {
			firstCount := first.TopStacks[fn]
			if lastCount > firstCount+1 {
				fmt.Printf("    LEAK: %-35s %d -> %d (+%d)\n", fn, firstCount, lastCount, lastCount-firstCount)
			}
		}
	}
}

func parseTopFunctions(profile string) map[string]int {
	result := make(map[string]int)
	lines := strings.Split(profile, "\n")

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		for _, part := range parts {
			if strings.HasPrefix(part, "main.") {
				funcName := part
				if paren := strings.Index(funcName, "+"); paren != -1 {
					funcName = funcName[:paren]
				}
				result[funcName]++
				break
			}
		}
	}
	return result
}

type FaultyService struct {
	mu       sync.Mutex
	pending  []chan struct{}
}

func NewFaultyService() *FaultyService {
	return &FaultyService{
		pending: make([]chan struct{}, 0),
	}
}

func (s *FaultyService) HandleRequest(id int) {
	resultCh := make(chan struct{})

	go s.processAsync(id, resultCh)

	select {
	case <-resultCh:
		return
	case <-time.After(50 * time.Millisecond):
		// timeout -- but the goroutine running processAsync is now leaked
		return
	}
}

func (s *FaultyService) processAsync(id int, result chan struct{}) {
	// simulates slow processing that exceeds the caller's timeout
	time.Sleep(10 * time.Second)
	result <- struct{}{}
}

func (s *FaultyService) HandleRequestCorrectly(id int) {
	resultCh := make(chan struct{}, 1)

	go func() {
		time.Sleep(10 * time.Second)
		resultCh <- struct{}{}
	}()

	select {
	case <-resultCh:
		return
	case <-time.After(50 * time.Millisecond):
		return
	}
}

func main() {
	detector := NewLeakDetector()
	svc := NewFaultyService()

	fmt.Println("=== Goroutine Leak Detection Under Load ===")
	fmt.Printf("  Initial goroutines: %d\n\n", runtime.NumGoroutine())

	detector.TakeSnapshot()

	for batch := 0; batch < totalSnapshots-1; batch++ {
		for i := 0; i < requestsPerBatch; i++ {
			svc.HandleRequest(batch*requestsPerBatch + i)
		}
		time.Sleep(snapshotInterval)
		snap := detector.TakeSnapshot()
		fmt.Printf("  Batch %d complete: %d goroutines\n", batch+1, snap.Goroutines)
	}

	fmt.Println()
	detector.Report()

	fmt.Println()
	fmt.Println("  Root cause: processAsync goroutines outlive their callers")
	fmt.Println("  Fix: use buffered channel (cap=1) so send does not block if receiver timed out")
}
```

**What's happening here:** `FaultyService.HandleRequest` spawns an async goroutine and waits with a timeout. When the timeout fires, the caller returns -- but the goroutine continues running `processAsync`, blocked on sending to a channel nobody is reading anymore. Each timed-out request leaks one goroutine. The `LeakDetector` takes snapshots at intervals, then compares the first and last to identify which functions are accumulating goroutines.

**Key insight:** This is the most common goroutine leak pattern in production: a request handler spawns work, times out, and abandons the goroutine. The fix is simple -- use a buffered channel with capacity 1 so the orphaned goroutine's send does not block forever. The profiling technique (snapshot, compare, diff) is how you find this in a live service.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Goroutine Leak Detection Under Load ===
  Initial goroutines: 1

  Batch 1 complete: 7
  Batch 2 complete: 12
  Batch 3 complete: 17

=== Leak Detection Report ===
  Snapshots: 4

  Snapshot 1 (t=0s): 1 goroutines

  Snapshot 2 (t=761ms): 7 goroutines
    main.(*FaultyService).processAsync      5

  Snapshot 3 (t=1.523s): 12 goroutines
    main.(*FaultyService).processAsync      10

  Snapshot 4 (t=2.284s): 17 goroutines
    main.(*FaultyService).processAsync      15

  Growth: 1 -> 17 (+16 goroutines)

  Functions with growing goroutine count:
    LEAK: main.(*FaultyService).processAsync 0 -> 15 (+15)

  Root cause: processAsync goroutines outlive their callers
  Fix: use buffered channel (cap=1) so send does not block if receiver timed out
```


## Common Mistakes

### Ignoring the Goroutine Count in Production

```go
package main

import (
	"fmt"
	"time"
)

func handleRequest() {
	ch := make(chan int)
	go func() {
		time.Sleep(5 * time.Second)
		ch <- 42 // blocks forever if nobody reads
	}()
	// caller returns without reading from ch
}

func main() {
	for i := 0; i < 5; i++ {
		handleRequest()
	}
	fmt.Println("requests handled")
	// 5 goroutines are now leaked, each sleeping then blocked on ch send
}
```
**What happens:** Each call to `handleRequest` leaks a goroutine. In production, this grows to thousands over hours. Without `runtime.NumGoroutine()` monitoring, the only symptom is increasing memory and eventual OOM.

**Fix:** Monitor goroutine count in your metrics pipeline (Prometheus, etc.). Alert when the count grows beyond expected bounds. Use `runtime/pprof` to investigate.


### Using a Too-Small Buffer for runtime.Stack

```go
package main

import (
	"fmt"
	"runtime"
)

func main() {
	buf := make([]byte, 256) // too small for many goroutines
	n := runtime.Stack(buf, true)
	fmt.Println(string(buf[:n])) // truncated output, missing goroutines
}
```
**What happens:** `runtime.Stack` fills the buffer and stops. With a 256-byte buffer, you only see one or two goroutines. The rest are silently dropped.

**Fix:** Use a large buffer (64KB or more) and check if `n == len(buf)` -- if so, the output was truncated and you need a bigger buffer:
```go
package main

import (
	"fmt"
	"runtime"
)

func main() {
	size := 1 << 16 // 64KB
	buf := make([]byte, size)
	n := runtime.Stack(buf, true)
	if n == size {
		fmt.Println("WARNING: stack buffer may be truncated, increase size")
	}
	fmt.Printf("Captured %d bytes of stack traces\n", n)
}
```


### Using pprof.WriteTo with debug=0 When You Want Human-Readable Output

```go
package main

import (
	"os"
	"runtime/pprof"
)

func main() {
	profile := pprof.Lookup("goroutine")
	profile.WriteTo(os.Stdout, 0) // binary protobuf format, not readable
}
```
**What happens:** `debug=0` writes a protocol buffer binary format intended for `go tool pprof`. It is not human-readable and cannot be parsed with string operations.

**Fix:** Use `debug=1` for human-readable text output, or `debug=2` for output matching the format of `SIGQUIT` dumps:
```go
package main

import (
	"os"
	"runtime/pprof"
)

func main() {
	profile := pprof.Lookup("goroutine")
	profile.WriteTo(os.Stdout, 1) // human-readable text
}
```


## Review

These four tools form a diagnostic ladder. `runtime.NumGoroutine()` is the
first rung: a single integer you should export continuously, because a count
that climbs monotonically is the earliest sign of a leak. It tells you
something is wrong but not what, so the next rung is `runtime.Stack(buf, true)`,
which dumps full traces for every goroutine and lets you group them by state
and call site -- hundreds of goroutines parked at the same channel receive is a
leak with a return address. `pprof.Lookup("goroutine").WriteTo(w, 1)` produces
the same information in a structured, human-readable form you can serve over
HTTP, so an operator diagnoses a live pod without SSH or a redeploy. The top
rung is comparison: snapshot the profile at intervals and diff, and the
function whose count grows between snapshots is the leak. The pattern the
exercise pins down is timeout-and-abandon -- a caller times out and walks away
while its spawned goroutine blocks forever on an unbuffered send, which a
capacity-1 buffered channel fixes outright.

With those pieces you should be able to build the goroutine health checker the
exercise asks for from scratch: run a service with fast, slow, and deliberately
broken handlers, drive fifty randomly distributed requests at it, snapshot once
a second for five seconds, and produce a report that names the total growth,
the leaking handler, an estimated leak rate in goroutines per second, and which
handler to fix first. The judgment that separates a real checker from a naive
one is distinguishing a goroutine that is legitimately busy in a slow handler
from one that is leaked -- and the answer is that the busy ones drain out of the
post-load snapshots while the leaked ones do not.

## Resources
- [runtime/pprof](https://pkg.go.dev/runtime/pprof) -- the profiling package behind `Lookup("goroutine")` and the debug=1/2 text formats used here.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) -- the one-integer count that is your first-line leak alarm.
- [runtime.Stack](https://pkg.go.dev/runtime#Stack) -- how to capture full stack traces for every goroutine into a buffer.
- [Go Blog: Profiling Go Programs](https://go.dev/blog/pprof) -- the official walkthrough of collecting and reading profiles.
- [Diagnostics](https://go.dev/doc/diagnostics) -- the overview page tying together pprof, tracing, and the runtime's other debug knobs.

---

Back to [Concurrency](../../concurrency.md) | Next: [22-testing-concurrent-code](../22-testing-concurrent-code/22-testing-concurrent-code.md)
