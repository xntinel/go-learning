# Exercise 8: Triage Leaks In A Running Process With pprof

When a process is already climbing toward OOM you cannot re-run it under a test —
you have to interrogate it live. `runtime/pprof` and `runtime.NumGoroutine` are the
production-time signals: they tell you how many goroutines exist right now and,
by counting stacks that contain a target frame, *where* they are stuck. This
module builds a triage helper plus a `/debug/leaks` endpoint that reports the
goroutine count next to the detector's `ActiveContexts`, so the two signals can be
cross-checked during an incident.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leaktriage/                        module example.com/leaktriage
  go.mod
  triage.go                        CountGoroutinesOnFrame, GoroutineCount, LeaksHandler, minimal Detector
  cmd/
    demo/
      main.go                      park N workers, count them by frame, unblock
  triage_test.go                   frame count matches N; NumGoroutine ~ profile Count; endpoint JSON
```

Files: `triage.go`, `cmd/demo/main.go`, `triage_test.go`.
Implement: `CountGoroutinesOnFrame(target)` (parse the goroutine profile, count stacks containing `target`), `GoroutineCount()`, and `LeaksHandler(d)` reporting `NumGoroutine`, profile `Count`, and `ActiveContexts` as JSON.
Test: N goroutines parked on a named function are counted exactly; `NumGoroutine` and the profile `Count` agree within tolerance; the endpoint's `ActiveContexts` matches the detector.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leaktriage/cmd/demo
cd ~/go-exercises/leaktriage
go mod init example.com/leaktriage
```

### The goroutine profile is a text document you can grep

`pprof.Lookup("goroutine")` returns a `*pprof.Profile`. Its `Count()` is the
current number of goroutines — the same number as `runtime.NumGoroutine()`, give
or take the goroutine doing the profiling. Its `WriteTo(w, 2)` writes every
goroutine's full stack in the same format the runtime prints on an unrecovered
panic: one block per goroutine, `goroutine N [state]:` followed by its call stack,
blocks separated by a blank line. Triage is then a text problem — split on the
blank line, and count the blocks whose stack contains the frame you are hunting,
e.g. a worker parked on `ctx.Done()` inside one package. That count localizes a
leak to a call site in a process you cannot stop.

Cross-checking is the discipline that makes this reliable. `NumGoroutine` growing
tells you *something* is leaking; the frame count tells you *which* call site; and
comparing `NumGoroutine` to the detector's `ActiveContexts` tells you whether the
climb is context-driven (each leaked goroutine paired with a leaked context) or
something else entirely (a goroutine leak with no context behind it). A
`/debug/leaks` endpoint that returns all three numbers as JSON turns that
cross-check into a single `curl` during an incident.

Create `triage.go`:

```go
package leaktriage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
)

// CountGoroutinesOnFrame returns how many goroutines currently have target
// somewhere in their stack. target is a substring of a frame, e.g. a function
// name like ".parkedWorker(".
func CountGoroutinesOnFrame(target string) int {
	var buf bytes.Buffer
	// debug=2 prints one full stack per goroutine, blocks separated by "\n\n".
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		return 0
	}
	count := 0
	for _, block := range strings.Split(buf.String(), "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(block), "goroutine ") && strings.Contains(block, target) {
			count++
		}
	}
	return count
}

// GoroutineCount returns the goroutine profile's count, which tracks
// runtime.NumGoroutine.
func GoroutineCount() int {
	return pprof.Lookup("goroutine").Count()
}

// Detector is a minimal context tracker, enough for the endpoint to report
// ActiveContexts alongside the goroutine signals.
type Detector struct {
	mu     sync.Mutex
	active map[context.Context]struct{}
}

// NewDetector returns an empty Detector.
func NewDetector() *Detector {
	return &Detector{active: make(map[context.Context]struct{})}
}

// WithCancel wraps context.WithCancel and tracks the context until it is Done.
func (d *Detector) WithCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	d.mu.Lock()
	d.active[ctx] = struct{}{}
	d.mu.Unlock()
	context.AfterFunc(ctx, func() {
		d.mu.Lock()
		delete(d.active, ctx)
		d.mu.Unlock()
	})
	return ctx, func() {
		cancel()
		d.mu.Lock()
		delete(d.active, ctx)
		d.mu.Unlock()
	}
}

// ActiveContexts returns the number of outstanding contexts.
func (d *Detector) ActiveContexts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.active)
}

// LeakStats is the JSON payload of the /debug/leaks endpoint.
type LeakStats struct {
	NumGoroutine   int `json:"num_goroutine"`
	ProfileCount   int `json:"profile_count"`
	ActiveContexts int `json:"active_contexts"`
}

// LeaksHandler serves the current leak signals as JSON.
func LeaksHandler(d *Detector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := LeakStats{
			NumGoroutine:   runtime.NumGoroutine(),
			ProfileCount:   GoroutineCount(),
			ActiveContexts: d.ActiveContexts(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	}
}
```

### The runnable demo

`parkedWorker` is the frame we hunt. The demo parks four of them, counts by frame,
then unblocks them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"

	"example.com/leaktriage"
)

func parkedWorker(ctx context.Context, wg *sync.WaitGroup, ready chan<- struct{}) {
	defer wg.Done()
	ready <- struct{}{}
	<-ctx.Done() // blocked here; this frame is what triage counts
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ready := make(chan struct{})

	const n = 4
	for range n {
		wg.Add(1)
		go parkedWorker(ctx, &wg, ready)
	}
	for range n {
		<-ready // ensure all workers have reached the block
	}

	fmt.Println("goroutines on parkedWorker:", leaktriage.CountGoroutinesOnFrame(".parkedWorker("))

	cancel()
	wg.Wait()
	fmt.Println("goroutines on parkedWorker after cancel:", leaktriage.CountGoroutinesOnFrame(".parkedWorker("))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
goroutines on parkedWorker: 4
goroutines on parkedWorker after cancel: 0
```

### Tests

Create `triage_test.go`:

```go
package leaktriage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
)

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func parkedWorker(ctx context.Context, wg *sync.WaitGroup, ready chan<- struct{}) {
	defer wg.Done()
	ready <- struct{}{}
	<-ctx.Done()
}

func TestCountGoroutinesOnFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	ready := make(chan struct{})

	const n = 6
	for range n {
		wg.Add(1)
		go parkedWorker(ctx, &wg, ready)
	}
	for range n {
		<-ready
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	if got := CountGoroutinesOnFrame(".parkedWorker("); got != n {
		t.Fatalf("CountGoroutinesOnFrame = %d, want %d", got, n)
	}
}

func TestProfileCountTracksNumGoroutine(t *testing.T) {
	// The two counts are taken microseconds apart and may differ by the
	// profiling goroutine, so allow a small tolerance.
	pc := GoroutineCount()
	ng := runtime.NumGoroutine()
	if diff := abs(pc - ng); diff > 2 {
		t.Fatalf("profile Count=%d and NumGoroutine=%d differ by %d, want <= 2", pc, ng, diff)
	}
}

func TestLeaksEndpointReportsActiveContexts(t *testing.T) {
	d := NewDetector()
	_, c1 := d.WithCancel(context.Background())
	_, c2 := d.WithCancel(context.Background())
	defer c1()
	defer c2()

	srv := httptest.NewServer(LeaksHandler(d))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var stats LeakStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.ActiveContexts != 2 {
		t.Errorf("ActiveContexts = %d, want 2", stats.ActiveContexts)
	}
	if stats.NumGoroutine <= 0 || stats.ProfileCount <= 0 {
		t.Errorf("goroutine signals not populated: %+v", stats)
	}
}
```

## Review

The triage helper is correct when N goroutines parked on `parkedWorker` are counted
as exactly N, and the count drops to zero once they are unblocked — that is the
production skill of "which call site is stuck". `GoroutineCount()` tracking
`NumGoroutine` within a small tolerance is expected: the two are sampled a moment
apart and the profiling machinery itself can add a goroutine, so an exact-equality
assertion would flake. The endpoint's value is the cross-check: `ActiveContexts`
beside `NumGoroutine` tells an operator whether a goroutine climb is context-backed.
Parsing the `debug=2` profile by splitting on the blank line is robust because that
format is stable, but note it is a debugging convenience, not an API — the numbers
(`Count`, `NumGoroutine`) are the load-bearing signals.

## Resources

- [runtime/pprof.Lookup and Profile](https://pkg.go.dev/runtime/pprof#Lookup) — the goroutine profile, `Count`, and `WriteTo` debug levels.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the live goroutine count cross-checked against the profile.
- [net/http/pprof](https://pkg.go.dev/net/http/pprof) — the standard `/debug/pprof/goroutine` endpoints this triage helper complements.

---

Back to [07-cancelcause-diagnostics.md](07-cancelcause-diagnostics.md) | Next: [09-build-tag-shim.md](09-build-tag-shim.md)
