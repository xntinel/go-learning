# Exercise 9: Prove a Handler Leaves No Goroutines Running After It Returns

A handler that spins up helper goroutines per request and forgets to join one of
them leaks a goroutine on every request. Nothing crashes; the goroutine count and
the memory behind it just climb until the process degrades hours or days later.
This exercise builds a handler that joins every helper it starts, and a test that
proves it by sampling `runtime.NumGoroutine()` before and after — the cheapest
first-line leak check there is.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
leakguard/                   independent module: example.com/leakguard
  go.mod                     go 1.25 (WaitGroup.Go)
  leakguard.go               Handle(Request) Response; LeakyHandle (documents the bug)
  cmd/
    demo/
      main.go                handle a request, print the result
  leakguard_test.go          NumGoroutine returns to baseline; leaky variant does not
```

Files: `leakguard.go`, `cmd/demo/main.go`, `leakguard_test.go`.
Implement: `Handle(req Request) Response` that spawns one helper goroutine per key and joins all of them before returning; and a `LeakyHandle` that starts a goroutine with no exit path to document the failure.
Test: capture `runtime.NumGoroutine()` baseline, run `Handle`, then assert the count returns to baseline via a bounded poll (not a fixed sleep); the leaky variant shows a positive delta until it is stopped.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/leakguard/cmd/demo
cd ~/go-exercises/leakguard
go mod init example.com/leakguard
go mod edit -go=1.25
```

### Every spawned goroutine must be joined before return

The leak-safe contract for a request handler is simple to state and easy to
violate: every goroutine the handler starts must have terminated by the time the
handler returns. `Handle` starts one helper per key, each writing its result into
a shared map under a mutex, and calls `wg.Wait()` before returning. After `Wait`,
all helpers have finished; `NumGoroutine` is back where it started. There is no
per-request residue.

`LeakyHandle` is the anti-pattern, made explicit and controllable so a test can
observe it without leaking into the whole test binary. It starts a goroutine
blocked on a channel that is never signaled — a goroutine with no exit path, which
is the definition of a leak — and returns a `stop` function the test uses to
release it during cleanup. In real code the leak is rarely this obvious; it is a
helper that sends on a channel no one drains, or that waits on a context that is
never cancelled, or that ranges over a channel that is never closed. The symptom
is always the same: `NumGoroutine` climbs and never comes back.

The test technique is `runtime.NumGoroutine()` sampled before and after, with a
*bounded poll* rather than a fixed `time.Sleep` to allow the scheduler a moment to
retire finished goroutines. A poll that yields with `runtime.Gosched()` and
rechecks is deterministic-enough and fast; a `Sleep` is either too short (flaky)
or too long (slow) and never actually correct. This is the lightweight check; a
dedicated tool like `goleak` (a later lesson) formalizes it, but the before/after
delta catches the leaks that matter.

Create `leakguard.go`:

```go
package leakguard

import "sync"

// Request is a unit of work with independent keys to resolve.
type Request struct {
	ID   string
	Keys []string
}

// Response holds the resolved value per key.
type Response struct {
	ID      string
	Results map[string]int
}

// Handle resolves every key in parallel and joins all helper goroutines before
// returning, so it leaves no goroutine running after it returns.
func Handle(req Request) Response {
	results := make(map[string]int, len(req.Keys))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, k := range req.Keys {
		wg.Go(func() {
			v := len(k) // stand-in for a real per-key lookup
			mu.Lock()
			results[k] = v
			mu.Unlock()
		})
	}
	wg.Wait() // join every helper before returning: no leak
	return Response{ID: req.ID, Results: results}
}

// LeakyHandle documents the bug: it starts a goroutine with no exit path and
// returns a stop func so a test can release it during cleanup. Do not write
// code like this.
func LeakyHandle(req Request) (Response, func()) {
	stop := make(chan struct{})
	go func() {
		<-stop // never signaled by Handle: this goroutine leaks
	}()
	return Response{ID: req.ID}, func() { close(stop) }
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/leakguard"
)

func main() {
	resp := leakguard.Handle(leakguard.Request{
		ID:   "req-1",
		Keys: []string{"alpha", "beta", "gamma"},
	})

	keys := make([]string, 0, len(resp.Results))
	for k := range resp.Results {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Printf("request %s resolved %d keys\n", resp.ID, len(resp.Results))
	for _, k := range keys {
		fmt.Printf("  %s=%d\n", k, resp.Results[k])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request req-1 resolved 3 keys
  alpha=5
  beta=4
  gamma=5
```

### Tests

`TestHandleLeavesNoGoroutines` records the baseline, runs `Handle`, and asserts
the count returns to baseline via `waitGoroutines`, a bounded poll.
`TestLeakyHandleLeaksUntilStopped` shows the failure: after `LeakyHandle`, the
count is above baseline; after calling `stop`, it returns. Neither test is
parallel, so the goroutine counts are not perturbed by other tests in the package.

Create `leakguard_test.go`:

```go
package leakguard

import (
	"runtime"
	"testing"
)

// waitGoroutines polls (yielding, not sleeping) until NumGoroutine drops to at
// most target, or a bounded number of iterations elapse.
func waitGoroutines(target int) bool {
	for range 2000 {
		if runtime.NumGoroutine() <= target {
			return true
		}
		runtime.Gosched()
	}
	return runtime.NumGoroutine() <= target
}

func TestHandleLeavesNoGoroutines(t *testing.T) {
	req := Request{ID: "r", Keys: []string{"a", "bb", "ccc", "dddd"}}

	base := runtime.NumGoroutine()
	resp := Handle(req)
	if len(resp.Results) != len(req.Keys) {
		t.Fatalf("resolved %d keys, want %d", len(resp.Results), len(req.Keys))
	}
	if !waitGoroutines(base) {
		t.Fatalf("goroutines did not return to baseline: base=%d now=%d",
			base, runtime.NumGoroutine())
	}
}

func TestLeakyHandleLeaksUntilStopped(t *testing.T) {
	req := Request{ID: "r"}

	base := runtime.NumGoroutine()
	_, stop := LeakyHandle(req)

	if now := runtime.NumGoroutine(); now <= base {
		t.Fatalf("expected a leaked goroutine: base=%d now=%d", base, now)
	}

	stop() // release the leaked goroutine
	if !waitGoroutines(base) {
		t.Fatalf("goroutine did not terminate after stop: base=%d now=%d",
			base, runtime.NumGoroutine())
	}
}
```

## Review

The handler is leak-safe when `NumGoroutine` returns to its pre-call baseline
after `Handle` returns — proof that `wg.Wait()` joined every helper. The test's
discipline matters as much as the handler's: sample the baseline, act, then poll
by yielding rather than sleeping, and keep the count-sensitive tests non-parallel
so other goroutines do not pollute the reading. The leaky variant makes the
failure mode concrete — a goroutine with no exit path — and the contrast is the
lesson: a goroutine you start without a join or a termination signal is a leak,
and leaks are invisible until the process is already sick. Run `-race` so the
shared map's mutex is validated under the concurrent helper writes.

## Resources

- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine)
- [sync.WaitGroup.Go](https://pkg.go.dev/sync#WaitGroup.Go)
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — the dedicated leak detector introduced later.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-bounded-fanout-errgroup.md](08-bounded-fanout-errgroup.md) | Next: [10-context-scoped-background-task.md](10-context-scoped-background-task.md)
