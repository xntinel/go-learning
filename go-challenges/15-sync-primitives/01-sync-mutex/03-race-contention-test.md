# Exercise 3: Writing a race-gated contention test harness

Guarding shared state is half the job; proving it is guarded is the other half.
A senior engineer does not merge "I added a mutex" — they merge a test that fans
out over many goroutines, asserts an exact result, and passes under
`go test -race`. This module builds that reusable harness and, critically, ships
a deliberately-unsynchronized variant behind a build tag so you can run it and
watch the race detector fire, then see the guarded version stay clean.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
racetest/                    independent module: example.com/racetest
  go.mod                     go 1.26
  counter.go                 type Counter (mutex-guarded, correct)
  badcounter.go              //go:build racebug — unsynchronized BadCounter
  cmd/
    demo/
      main.go                runnable demo: fan out over the correct Counter
  counter_test.go            fanOut harness + exact-count contention test, Example
  badcounter_test.go         //go:build racebug — the test that -race flags
```

- Files: `counter.go`, `badcounter.go`, `cmd/demo/main.go`, `counter_test.go`, `badcounter_test.go`.
- Implement: a mutex-guarded `Counter`, a reusable `fanOut(goroutines, perG, work)` harness in the test file, and a tag-guarded unsynchronized `BadCounter`.
- Test: the correct counter passes `go test -race` with an exact total of 1000; the tagged variant reports `WARNING: DATA RACE` under `go test -tags racebug -race`.
- Verify: `go test -count=1 -race ./...` (default, clean), then `go test -tags racebug -race ./...` (fires the detector).

```bash
mkdir -p go-solutions/15-sync-primitives/01-sync-mutex/03-race-contention-test/cmd/demo
cd go-solutions/15-sync-primitives/01-sync-mutex/03-race-contention-test
```

### Why the bug lives behind a build tag

The whole point of this module is to *see* the race detector work, which means
running code that races. But a racing file cannot live in the default build:
`go test -race` would report it and the module's own gate would fail. The clean
way to ship a reproducible bug is a build constraint. `badcounter.go` and its
test start with `//go:build racebug`, so the default `go build`, `go vet`, and
`go test` ignore them entirely — the module compiles and passes clean — while
`go test -tags racebug -race` pulls them in and fires the detector. This is a
real technique: keep a known-bad reproduction in the tree, tagged out, so the
next engineer can rebuild the failure on demand without breaking CI.

The harness itself is the reusable skill. `fanOut(goroutines, perG, work)` spins
up `goroutines` goroutines, each calling `work` exactly `perG` times, and waits
for all of them with a `sync.WaitGroup`. Every concurrency test in this chapter
is a variation of it. The correct test calls `fanOut(100, 10, c.Inc)` and
asserts the total is exactly `100*10 = 1000`. Exactness is the assertion that
matters: a lost update from an unsynchronized increment produces a total *less*
than 1000, so the count itself is a race probe even before `-race` speaks. And
`-race` is the only actual proof — the guarded counter can hit 1000 by luck once,
but a clean `-race` run over a hard fan-out is the evidence you merge on.

Create `counter.go`:

```go
package racetest

import "sync"

// Counter is a race-free integer counter guarded by a Mutex.
type Counter struct {
	mu sync.Mutex
	n  int
}

// Inc adds one under the lock.
func (c *Counter) Inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

// Value returns the current count under the lock.
func (c *Counter) Value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
```

Now the deliberately-broken variant. It is byte-for-byte a `Counter` without the
mutex — the minimal change that turns a correct counter into a data race.

Create `badcounter.go`:

```go
//go:build racebug

package racetest

// BadCounter is deliberately unsynchronized. It exists only to demonstrate the
// race detector; it is excluded from the default build by the racebug tag.
type BadCounter struct {
	n int
}

// Inc is an unsynchronized read-modify-write: two goroutines can lose updates.
func (c *BadCounter) Inc() { c.n++ }

// Value reads without synchronization.
func (c *BadCounter) Value() int { return c.n }
```

### The runnable demo

The demo runs the same fan-out the test does — 100 goroutines, 10 increments
each — against the correct counter and prints the total, which is always exactly
1000 because the mutex serializes every increment.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/racetest"
)

func main() {
	var c racetest.Counter
	const goroutines, perG = 100, 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	fmt.Printf("count=%d\n", c.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=1000
```

### Tests

`counter_test.go` holds the `fanOut` harness and the exact-count test. It is the
default, always-compiled test path and it passes clean under `-race`.

Create `counter_test.go`:

```go
package racetest

import (
	"sync"
	"testing"
)

// fanOut runs work concurrently: goroutines goroutines, each calling work perG
// times, waiting for all to finish. It is the reusable contention harness.
func fanOut(goroutines, perG int, work func()) {
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				work()
			}
		}()
	}
	wg.Wait()
}

func TestCounterExactUnderContention(t *testing.T) {
	t.Parallel()

	var c Counter
	const goroutines, perG = 100, 10
	fanOut(goroutines, perG, c.Inc)

	if got, want := c.Value(), goroutines*perG; got != want {
		t.Fatalf("Value() = %d, want %d (a lost update means the counter raced)", got, want)
	}
}

func ExampleCounter() {
	var c Counter
	fanOut(10, 100, c.Inc)
	println(c.Value() == 1000)
	// Output:
}
```

The tag-guarded test is the demonstration. Under the default build it does not
exist; run it explicitly to see the detector fire.

Create `badcounter_test.go`:

```go
//go:build racebug

package racetest

import "testing"

// TestBadCounterRace fails under `go test -tags racebug -race`: the detector
// prints WARNING: DATA RACE on BadCounter.Inc and the test exits non-zero. The
// logged Value is almost always less than 1000 because increments are lost.
func TestBadCounterRace(t *testing.T) {
	var c BadCounter
	fanOut(100, 10, c.Inc)
	t.Logf("BadCounter.Value() = %d (want 1000; -race reports the data race)", c.Value())
}
```

Run the correct path (clean), then the bug path (fires):

```bash
go test -count=1 -race ./...
go test -tags racebug -race ./...
```

The second command prints a `WARNING: DATA RACE` stanza naming `BadCounter.Inc`
and exits non-zero. That is the lesson: the detector, not inspection, is what
tells you the guard is missing.

## Review

The harness is correct when `fanOut` waits for every goroutine (a missing
`wg.Wait()` would read the counter before the writers finish) and the assertion
demands the *exact* total, not "roughly 1000". Exactness turns the count into a
first-line race probe; `-race` is the authoritative one. The build-tag split is
what lets a known-bad reproduction live in the repo without breaking the default
gate.

The mistake to internalize is trusting a green run. A clean `go test -race`
proves only that no race appeared on the interleavings this run exercised —
which is why the fan-out is deliberately hard (many goroutines, real
contention) and why `-race` belongs in CI rather than being run once by hand.
The `Example` here asserts nothing to stdout on purpose; its `// Output:` is
empty because its job is to exercise `fanOut`, and correctness is asserted in
the real test.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how to run `-race` and read its report.
- [Introducing the Go Race Detector](https://go.dev/blog/race-detector) — the blog introduction with example output.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the fan-out/join primitive in the harness.
- [Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — how `//go:build racebug` excludes the bug from the default build.

---

Back to [02-labeled-metrics-registry.md](02-labeled-metrics-registry.md) | Next: [04-ttl-cache.md](04-ttl-cache.md)
