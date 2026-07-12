# Exercise 3: Make the Race Detector a Mandatory CI Gate

A race detector that only runs on a developer's laptop is a race detector that
ships races to production. This module builds a concurrent test that passes only
when synchronization is correct, wraps the CI contract (`go test -count=1 -race`,
`go vet`, `gofmt`) in a runnable check, and teaches you to read a real race report
using a racy counter as the illustrative example that must never be committed
enabled.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
racegate/                  independent module: example.com/racegate
  go.mod                   module example.com/racegate
  racegate.go              SafeCounter (atomic.Int64); RunCIChecks describing the CI contract as data
  cmd/
    demo/
      main.go              runnable demo: run the concurrent workload, print the CI command list
  racegate_test.go         concurrent SafeCounter test that gates under -race; contract assertions
```

- Files: `racegate.go`, `cmd/demo/main.go`, `racegate_test.go`.
- Implement: a `SafeCounter` backed by `atomic.Int64`, and a `CIChecks()` helper returning the exact command sequence the CI gate must run.
- Test: a committed concurrent test that passes only under correct synchronization when run with `-race`; a test asserting the CI contract lists `-race` and `-count=1`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/09-pointer-aliasing-and-data-races/03-race-detector-as-ci-gate/cmd/demo
cd go-solutions/09-pointers/09-pointer-aliasing-and-data-races/03-race-detector-as-ci-gate
```

### How to read a race report

When `go test -race` catches a race it prints a structured report. Learn to read
it; it points straight at the bug. A report for a racy `c.value++` looks like:

```
==================
WARNING: DATA RACE
Write at 0x00c0000b4010 by goroutine 8:
  example.com/racegate.(*Counter).Inc()
      /path/racegate.go:12 +0x1c

Previous write at 0x00c0000b4010 by goroutine 7:
  example.com/racegate.(*Counter).Inc()
      /path/racegate.go:12 +0x1c

Goroutine 8 (running) created at:
  example.com/racegate_test.TestRacy()
      /path/racegate_test.go:20 +0x9c
==================
```

Read it top to bottom: the *address* (`0x00c0000b4010`) is the memory location
being raced; the first stack is one access (a write, at `racegate.go:12`); the
"Previous" stack is the conflicting access (another write to the same address); and
the goroutine-creation stacks tell you where each racing goroutine was spawned.
Two writes to one address with no ordering edge is exactly the memory-model
definition of a race. The fix is not to move code around until the detector goes
quiet — it is to add the missing happens-before edge (here, an `atomic.Int64`).

The detector is dynamic: it only flags races on the interleavings that *actually
execute*. So the committed concurrent test must drive real concurrency (many
goroutines hitting the shared state), and CI must run `-race` on the full suite.
`-count=1` disables test caching so the concurrent test genuinely re-runs each
time rather than reporting a cached PASS. `go vet` adds the static copylocks check
that catches a struct with an embedded atomic being copied by value — a bug the
dynamic detector would only find if the copy happened on an executed path.

Create `racegate.go`:

```go
package racegate

import "sync/atomic"

// SafeCounter is the shipped, race-free counter. Its concurrent test passes
// under -race, which is what makes it a positive CI gate: if someone weakens the
// synchronization, the -race run fails.
type SafeCounter struct {
	value atomic.Int64
}

// Inc atomically increments and returns the new value.
func (c *SafeCounter) Inc() int64 {
	return c.value.Add(1)
}

// Get atomically loads the current value.
func (c *SafeCounter) Get() int64 {
	return c.value.Load()
}

// CIChecks returns the exact command sequence the CI pipeline must run to gate
// races. Encoding the contract as data lets a test assert it never drifts (for
// example, that -race is never dropped).
func CIChecks() []string {
	return []string{
		"gofmt -l .",
		"go vet ./...",
		"go test -count=1 -race ./...",
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/racegate"
)

func main() {
	c := &racegate.SafeCounter{}
	const n = 500

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	fmt.Printf("workload final count: %d\n", c.Get())

	fmt.Println("CI gate:")
	for _, cmd := range racegate.CIChecks() {
		fmt.Printf("  %s\n", cmd)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workload final count: 500
CI gate:
  gofmt -l .
  go vet ./...
  go test -count=1 -race ./...
```

### Tests

`TestSafeCounterIsRaceFreeUnderLoad` is the positive gate: it drives 1000
goroutines through `Inc` and asserts the exact final count, so it passes only when
the synchronization is intact — run under `-race` it fails the moment someone
weakens it. `TestCIContractIncludesRace` pins the CI contract as data, asserting
`-race` and `-count=1` are present so the gate cannot silently regress to a plain
`go test`.

Create `racegate_test.go`:

```go
package racegate

import (
	"strings"
	"sync"
	"testing"
)

func TestSafeCounterIsRaceFreeUnderLoad(t *testing.T) {
	t.Parallel()

	c := &SafeCounter{}
	const n = 1000

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if got := c.Get(); got != n {
		t.Fatalf("Get() = %d under concurrent load; want %d (synchronization is broken)", got, n)
	}
}

func TestCIContractIncludesRace(t *testing.T) {
	t.Parallel()

	checks := CIChecks()
	joined := strings.Join(checks, "\n")

	for _, must := range []string{"-race", "-count=1", "go vet", "gofmt"} {
		if !strings.Contains(joined, must) {
			t.Errorf("CI contract is missing %q; got:\n%s", must, joined)
		}
	}
}

func TestCICheckIsARealTestCommand(t *testing.T) {
	t.Parallel()

	var found bool
	for _, c := range CIChecks() {
		if strings.HasPrefix(c, "go test") && strings.Contains(c, "-race") {
			found = true
		}
	}
	if !found {
		t.Fatal("CI contract must contain a `go test ... -race` command")
	}
}
```

## Review

The gate is correct when the concurrent test passes under `-race` and the contract
test locks in `-race` and `-count=1`. The mistake to avoid is treating a `-race`
report as noise to silence: moving code until the detector goes quiet often just
hides the racing interleaving from the current run rather than removing the race.
Read the report — address, two stacks, the missing ordering edge — and add real
synchronization. And do not let CI drift to a plain `go test`: the detector only
finds races on executed paths, so `-race` on the full suite, driving realistic
concurrency, is the contract. `go vet` complements it with the static copylocks
check for copied atomics/mutexes.

## Resources

- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — how to read a report and where to run it.
- [Go command: testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-race`, `-count`, and what caching does.
- [`go vet`](https://pkg.go.dev/cmd/vet) — the copylocks analyzer that catches copied atomics and mutexes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-atomic-pointer-config-hotreload.md](04-atomic-pointer-config-hotreload.md)
