# 6. Goroutine Dump Analysis

When a Go service hangs, becomes unresponsive, or accumulates goroutines without bound, a goroutine dump is the primary diagnostic. It shows every live goroutine, its current state, and its full call stack, giving you the exact location of every blocked operation. This lesson builds an HTTP service with a `/debug/goroutines` endpoint, populates it with goroutines in known states (running, channel-waiting, mutex-waiting), and teaches you to read the dump output precisely enough to distinguish a goroutine leak from a goroutine correctly waiting on work.

```text
dumpdemo/
  go.mod
  server/
    server.go
    server_test.go
  cmd/demo/
    main.go
```

## Concepts

### Three Ways to Capture a Goroutine Dump

**SIGQUIT** (`kill -QUIT <pid>` on Unix): the Go runtime prints all goroutine stacks to stderr and exits. Useful for a quick snapshot in development; destructive in production because it terminates the process.

**`runtime.Stack(buf, true)`**: captures all goroutine stacks into a caller-supplied byte slice. Returns the number of bytes written. `true` means "all goroutines", not just the current one. The buffer must be large enough; a common starting size is 1 MB. Non-destructive and safe to call from any goroutine.

**`net/http/pprof` endpoint**: importing `_ "net/http/pprof"` registers `/debug/pprof/goroutine?debug=2` on the default HTTP mux. `debug=2` returns full stack traces in the same format as SIGQUIT. `debug=1` returns a count-by-state summary. Safe to call repeatedly; requires authentication in production.

### Reading a Goroutine Dump

A dump consists of blocks separated by blank lines. Each block begins with a goroutine header:

```
goroutine 18 [chan receive]:
example.com/dumpdemo/server.(*Server).worker(...)
    /path/server.go:42 +0x68
created by example.com/dumpdemo/server.(*Server).Start
    /path/server.go:31 +0x90
```

The header gives: goroutine number (stable within a process lifetime but not across restarts), state in brackets, the function frames (most recent first), and the creation site.

### Goroutine States

| State | Meaning |
| --- | --- |
| `running` | Executing on an OS thread right now |
| `runnable` | Ready to execute; waiting for an OS thread |
| `chan receive` | Blocked on `<-ch` |
| `chan send` | Blocked on `ch <-` |
| `semacquire` | Blocked waiting for a mutex or RWMutex |
| `select` | Blocked in a select with no ready case |
| `IO wait` | Blocked on network or file I/O (netpoller) |
| `sleep` | In `time.Sleep` |
| `syscall` | Executing a system call |
| `GC ...` | Participating in garbage collection |

A goroutine in `chan receive` at the same call site in dozens of entries is a worker pool or a channel-based fanout. One goroutine in `chan receive` for which no other goroutine is the sender is a leak.

### Production Considerations

Never expose `/debug/pprof` without authentication in production. Add an authentication middleware or bind the pprof server to a non-public interface (e.g., `127.0.0.1:6060`). Goroutine dumps expose function names, package paths, and goroutine counts; this is sensitive operational data.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/32-concurrency-debugging-and-testing/06-goroutine-dump-analysis/06-goroutine-dump-analysis/server
mkdir -p go-solutions/32-concurrency-debugging-and-testing/06-goroutine-dump-analysis/06-goroutine-dump-analysis/cmd/demo
cd go-solutions/32-concurrency-debugging-and-testing/06-goroutine-dump-analysis/06-goroutine-dump-analysis
```

### Exercise 1: Server With Goroutine Dump Endpoint

Create `server/server.go`:

```go
package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
)

// Server is a minimal HTTP server with a built-in goroutine dump endpoint.
type Server struct {
	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	workers int
	jobs    chan struct{}
	wg      sync.WaitGroup
}

// New creates a Server that starts workers background goroutines.
// The server binds to a random port (use Addr to discover it).
func New(workers int) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	s := &Server{
		workers: workers,
		jobs:    make(chan struct{}),
		ln:      ln,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/goroutines", s.goroutinesHandler)
	mux.HandleFunc("/debug/goroutines/summary", s.summaryHandler)
	s.srv = &http.Server{Handler: mux}
	return s, nil
}

// Start begins serving HTTP and launches the worker goroutines.
func (s *Server) Start() {
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	go s.srv.Serve(s.ln) //nolint:errcheck // Serve returns non-nil only after Close
}

// worker blocks on s.jobs. In a real server this would process work items.
func (s *Server) worker() {
	defer s.wg.Done()
	for range s.jobs {
		// process job
	}
}

// Addr returns the TCP address the server is listening on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Shutdown shuts down the HTTP server and all workers.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.jobs)
	s.wg.Wait()
	return s.srv.Shutdown(ctx)
}

// CaptureStacks returns the full goroutine dump for all goroutines.
func CaptureStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

// CountByState returns a map of goroutine state -> count by parsing
// pprof's debug=1 output (count + state label, one per line).
func CountByState() map[string]int {
	var buf bytes.Buffer
	p := pprof.Lookup("goroutine")
	if p == nil {
		return nil
	}
	p.WriteTo(&buf, 1) //nolint:errcheck
	counts := make(map[string]int)
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var n int
		var state string
		if _, err := fmt.Sscanf(line, "%d @ %s", &n, &state); err == nil {
			counts[state] += n
			continue
		}
		// pprof debug=1 format: "N goroutine(s) in state <state>:"
		// actual format varies; fall back to full text for summary
	}
	return counts
}

func (s *Server) goroutinesHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, CaptureStacks()) //nolint:errcheck
}

func (s *Server) summaryHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var buf bytes.Buffer
	p := pprof.Lookup("goroutine")
	if p != nil {
		p.WriteTo(&buf, 1) //nolint:errcheck
	}
	// Parse counts from full stack dump (portable across Go versions).
	full := CaptureStacks()
	counts := goroutineStateCounts(full)
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, st)
	}
	sort.Strings(states)
	fmt.Fprintf(w, "total goroutines: %d\n", runtime.NumGoroutine())
	for _, st := range states {
		fmt.Fprintf(w, "  %-20s %d\n", st, counts[st])
	}
}

// goroutineStateCounts parses the bracket state from each goroutine header.
func goroutineStateCounts(dump string) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(dump, "\n") {
		if !strings.HasPrefix(line, "goroutine ") {
			continue
		}
		// Format: "goroutine N [state]:" or "goroutine N [state, N minutes]:"
		start := strings.IndexByte(line, '[')
		end := strings.IndexByte(line, ']')
		if start < 0 || end < 0 || end <= start {
			continue
		}
		state := line[start+1 : end]
		// Strip duration suffix: "chan receive, 5 minutes" -> "chan receive"
		if idx := strings.Index(state, ","); idx >= 0 {
			state = strings.TrimSpace(state[:idx])
		}
		counts[state]++
	}
	return counts
}
```

### Exercise 2: Tests for the Debug Endpoints

Create `server/server_test.go`:

```go
package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, workers int) *Server {
	t.Helper()
	s, err := New(workers)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return s
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d for %s", resp.StatusCode, url)
	}
	return string(body)
}

// TestGoroutinesDumpContainsHeader verifies the dump endpoint returns goroutine stacks.
func TestGoroutinesDumpContainsHeader(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, 3)
	body := get(t, "http://"+s.Addr()+"/debug/goroutines")

	if !strings.Contains(body, "goroutine ") {
		t.Fatalf("expected 'goroutine ' in dump, got:\n%s", body[:min(len(body), 200)])
	}
}

// TestGoroutinesDumpShowsWorkers verifies worker goroutines appear in the dump.
func TestGoroutinesDumpShowsWorkers(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, 4)

	// Give workers time to park on the jobs channel.
	time.Sleep(50 * time.Millisecond)

	body := get(t, "http://"+s.Addr()+"/debug/goroutines")

	if !strings.Contains(body, "chan receive") && !strings.Contains(body, "select") {
		t.Logf("dump:\n%s", body)
		t.Fatal("expected 'chan receive' or 'select' state in dump (workers blocked on jobs channel)")
	}
}

// TestSummaryEndpointReturnsTotal verifies the summary reports at least one goroutine.
func TestSummaryEndpointReturnsTotal(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, 2)
	body := get(t, "http://"+s.Addr()+"/debug/goroutines/summary")

	if !strings.Contains(body, "total goroutines:") {
		t.Fatalf("expected 'total goroutines:' in summary, got:\n%s", body)
	}
}

// TestCaptureStacksContainsCurrentGoroutine verifies the capture includes the caller.
func TestCaptureStacksContainsCurrentGoroutine(t *testing.T) {
	t.Parallel()

	dump := CaptureStacks()
	if !strings.Contains(dump, "server.TestCaptureStacksContainsCurrentGoroutine") {
		t.Fatalf("expected current test function in dump, got:\n%s", dump[:min(len(dump), 400)])
	}
}

// TestGoroutineStateCounts parses a synthetic dump correctly.
func TestGoroutineStateCounts(t *testing.T) {
	t.Parallel()

	dump := `goroutine 1 [running]:
main.main()
    /path/main.go:10 +0x20

goroutine 5 [chan receive]:
example.com/worker()
    /path/worker.go:15 +0x30

goroutine 6 [chan receive, 3 minutes]:
example.com/worker()
    /path/worker.go:15 +0x30

goroutine 7 [semacquire]:
example.com/locker()
    /path/locker.go:20 +0x40
`
	counts := goroutineStateCounts(dump)

	if counts["running"] != 1 {
		t.Errorf("running = %d, want 1", counts["running"])
	}
	if counts["chan receive"] != 2 {
		t.Errorf("chan receive = %d, want 2", counts["chan receive"])
	}
	if counts["semacquire"] != 1 {
		t.Errorf("semacquire = %d, want 1", counts["semacquire"])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ExampleCaptureStacks shows the API surface of the capture function.
func ExampleCaptureStacks() {
	dump := CaptureStacks()
	if len(dump) > 0 {
		// dump contains at least the current goroutine
	}
	_ = dump
	// Output:
}
```

Your turn: add `TestCountByStateReturnsNonNilMap` that calls `CountByState()` and asserts the result is non-nil and has at least one entry. Use `t.Parallel()`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"example.com/dumpdemo/server"
)

func main() {
	s, err := server.New(5)
	if err != nil {
		fmt.Printf("new server: %v\n", err)
		return
	}
	s.Start()
	fmt.Printf("server listening on %s\n", s.Addr())

	// Give workers time to park.
	time.Sleep(50 * time.Millisecond)

	// Fetch and print the goroutine summary.
	resp, err := http.Get("http://" + s.Addr() + "/debug/goroutines/summary")
	if err != nil {
		fmt.Printf("GET summary: %v\n", err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("--- goroutine summary ---\n%s", body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		fmt.Printf("shutdown: %v\n", err)
	}
	fmt.Println("server stopped")
}
```

## Common Mistakes

### Exposing pprof in Production Without Authentication

Wrong: `import _ "net/http/pprof"` in a production binary with the default HTTP mux exposed on a public interface.

What happens: anyone who can reach the port can read goroutine stacks, memory profiles, CPU profiles, and heap dumps, revealing internal package paths, goroutine states, and live memory contents.

Fix: bind the pprof server to localhost only (`net.Listen("tcp", "127.0.0.1:6060")`), or add authentication middleware before registering the handlers.

### Buffer Too Small for `runtime.Stack`

Wrong:

```go
buf := make([]byte, 1024)
n := runtime.Stack(buf, true)
```

What happens: with many goroutines or deep stacks, the dump is silently truncated. `n == len(buf)` is the signal that truncation occurred.

Fix: use a large initial buffer (1 MB) or grow dynamically:

```go
for size := 1 << 20; ; size *= 2 {
	buf := make([]byte, size)
	n := runtime.Stack(buf, true)
	if n < size {
		return string(buf[:n])
	}
}
```

### Confusing `semacquire` With a Bug

Wrong: treating every `semacquire` goroutine as a deadlock or bug.

What happens: `semacquire` is the normal state for a goroutine waiting on a mutex. A channel-based worker pool shows workers in `chan receive`; a mutex-protected service shows goroutines in `semacquire` while the lock is held. These are expected states, not bugs.

Fix: look for goroutines that have been in `semacquire` or `chan receive` for an abnormally long time (the duration appears in the state field after a comma) and trace back to the goroutine holding the resource.

## Verification

From `~/go-exercises/dumpdemo`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

The tests must pass with no race warnings. The demo must print a goroutine summary showing at least `running` and `chan receive` states (for the worker goroutines blocked on the jobs channel).

## Summary

- `runtime.Stack(buf, true)` captures all goroutine stacks into a byte slice; use a 1 MB or larger buffer.
- `net/http/pprof` exposes goroutine dumps at `/debug/pprof/goroutine?debug=2`; never expose without authentication in production.
- The goroutine state in brackets (`chan receive`, `semacquire`, `IO wait`) tells you what each goroutine is waiting on.
- Goroutines accumulating in `chan receive` at the same location over time indicate a leak; workers legitimately blocking on work channels appear there transiently.
- Parse state counts from the dump to get a fast operational view of goroutine health.

## What's Next

Next: [Concurrent Test Isolation](../07-concurrent-test-isolation/07-concurrent-test-isolation.md).

## Resources

- [runtime.Stack](https://pkg.go.dev/runtime#Stack)
- [net/http/pprof](https://pkg.go.dev/net/http/pprof)
- [runtime/pprof](https://pkg.go.dev/runtime/pprof)
- [Go Diagnostics Guide](https://go.dev/doc/diagnostics)
