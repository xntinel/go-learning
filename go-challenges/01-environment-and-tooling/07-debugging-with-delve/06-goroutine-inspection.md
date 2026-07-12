# Exercise 6: Inspect Goroutines and a Blocked Channel

When a concurrent program hangs, the question is "which goroutine is stuck, and on
what". Delve's `goroutines` lists every goroutine with its wait reason, and
`goroutine <id>` switches context so you can read its stack. This module builds a
worker pool that deadlocks because its result channel is never drained, diagnoses
it, and ships the corrected, race-clean version.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
poolinspect/               independent module: example.com/poolinspect
  go.mod                   go 1.24
  pool/
    pool.go                Process(inputs, workers) []int  (drains results; terminates)
  cmd/
    demo/
      main.go              runs the pool, prints sorted squares
  pool/pool_test.go        table-driven test, -race clean, sorted-output assertion
```

- Files: `pool/pool.go`, `cmd/demo/main.go`, `pool/pool_test.go`.
- Implement: `Process(inputs []int, workers int) []int` — a fan-out/fan-in pool that squares each input and returns all results, draining the result channel so it always terminates.
- Test: assert the multiset of results (sorted) for several inputs; run under `-race`.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv` session on the deadlocked variant showing a `chan send` wait reason.

Set up the module:

```bash
go mod edit -go=1.24
```

### The deadlock, and reading it with goroutines

Here is the version that deadlocks. Main sends every job, then calls `wg.Wait()`
before reading any result — so the workers block forever on `results <- ...` with
no receiver, they stop reading `jobs`, and `wg.Wait()` never returns. It is
illustrative; do not save it:

```go
package pool

import "sync"

// DEADLOCK: main waits on the WaitGroup before draining results, so every worker
// blocks on the unbuffered results send and nothing ever completes.
func Process(inputs []int, workers int) []int {
	jobs := make(chan int)
	results := make(chan int)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				results <- n * n // blocks: no receiver
			}
		}()
	}

	for _, n := range inputs {
		jobs <- n
	}
	close(jobs)
	wg.Wait() // never returns

	out := make([]int, 0, len(inputs))
	for r := range results {
		out = append(out, r)
	}
	return out
}
```

Run a program built on this version under Delve. When every goroutine is blocked,
the Go runtime's deadlock detector aborts and Delve stops on the throw; list the
goroutines to see who is stuck and why:

```text
(dlv) continue
...
fatal error: all goroutines are asleep - deadlock!
(dlv) goroutines
  Goroutine 1 - User: ./pool/pool.go:23 example.com/poolinspect/pool.Process (0x...) [chan send]
* Goroutine 18 - User: ./pool/pool.go:17 example.com/poolinspect/pool.Process.func1 (0x...) [chan send]
  Goroutine 19 - User: ./pool/pool.go:17 example.com/poolinspect/pool.Process.func1 (0x...) [chan send]
...
(dlv) goroutine 18
Switched from 1 to 18 (thread ...)
(dlv) stack
0  runtime.gopark ...
...
N  example.com/poolinspect/pool.Process.func1() ./pool/pool.go:17
(dlv) frame N
(dlv) print n
3
```

The bracketed `[chan send]` is the wait reason: worker goroutine 18 is parked on
line 17, the `results <- n * n` send, and goroutine 1 (main) is on `[chan send]`
at line 23, blocked feeding `jobs`. That pair is the whole diagnosis — the results
channel has no reader, so the send never completes. `goroutine <id>` switches the
inspection context and `stack` shows that goroutine's frames; `goroutines -t`
prints every goroutine's stack in one shot.

### The fix: drain results concurrently

The correction reads results while the workers produce them: a separate goroutine
closes `results` once all workers finish, and main ranges over `results` until it
closes. No goroutine is ever blocked with no counterpart.

Create `pool/pool.go`:

```go
package pool

import "sync"

// Process squares every input using a pool of workers and returns all results.
// A closer goroutine closes the results channel once the workers finish, so the
// range over results terminates and no goroutine blocks with no counterpart.
func Process(inputs []int, workers int) []int {
	jobs := make(chan int)
	results := make(chan int)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				results <- n * n
			}
		}()
	}

	go func() {
		for _, n := range inputs {
			jobs <- n
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]int, 0, len(inputs))
	for r := range results {
		out = append(out, r)
	}
	return out
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/poolinspect/pool"
)

func main() {
	out := pool.Process([]int{1, 2, 3, 4, 5}, 3)
	sort.Ints(out)
	fmt.Println(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
[1 4 9 16 25]
```

### The test asserts the multiset and runs under -race

Because workers run concurrently, the result order is non-deterministic; the test
sorts before comparing so it asserts the multiset, not an accidental order. The
`-race` flag proves the channels, not shared memory, carry the data.

Create `pool/pool_test.go`:

```go
package pool

import (
	"fmt"
	"sort"
	"testing"
)

func TestProcess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []int
		workers int
		want    []int
	}{
		{name: "empty", in: nil, workers: 3, want: []int{}},
		{name: "single", in: []int{7}, workers: 1, want: []int{49}},
		{name: "five", in: []int{1, 2, 3, 4, 5}, workers: 3, want: []int{1, 4, 9, 16, 25}},
		{name: "more_workers_than_jobs", in: []int{2, 3}, workers: 8, want: []int{4, 9}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Process(tc.in, tc.workers)
			sort.Ints(got)
			if len(got) != len(tc.want) {
				t.Fatalf("Process(%v) len = %d; want %d", tc.in, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Process(%v) sorted = %v; want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func ExampleProcess() {
	out := Process([]int{1, 2, 3}, 2)
	sort.Ints(out)
	fmt.Println(out)
	// Output: [1 4 9]
}
```

Run the gate; the pool must be race-clean and always terminate:

```bash
go vet ./...
go test -count=1 -race ./...
```

### Scripted: capture the wait reason on the deadlocked variant

Build a demo on the deadlocking version, run it under Delve headless with a script
that lists goroutines after the deadlock throw, and grep for the wait reason:

```bash
go build -gcflags='all=-N -l' -o /tmp/pooldead ./cmd/demo   # built from the deadlock variant

cat > /tmp/pool.dlv <<'EOF'
continue
goroutines
quit
EOF

dlv exec /tmp/pooldead --init /tmp/pool.dlv 2>&1 | tee /tmp/pool.out
grep -q 'chan send' /tmp/pool.out && echo "confirmed: workers blocked on chan send"
```

The captured `goroutines` output lists several `[chan send]` goroutines: the
workers parked on the undrained results channel. The fixed version, by contrast,
runs to completion under `go test -race` with nothing to report.

## Review

The pool is correct when every result is delivered and the program always
terminates: the closer goroutine that runs `wg.Wait()` then `close(results)` is
what lets the `range results` loop end, and dropping it reintroduces the deadlock.
The diagnosis proof is `goroutines` showing `[chan send]` on the worker frames —
the wait reason names the exact operation that has no counterpart. The mistakes to
avoid are draining results only after `wg.Wait()` (the deadlock above) and
asserting a specific result order (workers finish in scheduler order, so sort
before comparing). Under `-race`, a passing run confirms the channels carry the
data with no unsynchronized sharing.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `goroutines`, `goroutine <id>`, and the `-t` stack flag.
- [Go blog: pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in with channels and how to drain them without leaking.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — coordinating worker completion before closing the results channel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-attach-running-server.md](07-attach-running-server.md)
