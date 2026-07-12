# Exercise 1: A Metrics Collector Whose Zero Value Is Ready To Use

A senior instinct when instrumenting a service without a metrics library is to
reach for a constructor. This exercise builds the opposite: a concurrency-safe
request collector that works as `var c Collector`, with no `New`, because its
internal state is lazily allocated behind the methods that own it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
collector/                 independent module: example.com/collector
  go.mod
  collector.go             type Collector; Record, Count, Snapshot, Failures, Log
  cmd/
    demo/
      main.go              records a few requests, prints counts/failures/log
  collector_test.go        zero-value contract tests + -race concurrency test
```

Files: `collector.go`, `cmd/demo/main.go`, `collector_test.go`.
Implement: a `Collector` usable as its zero value — lazy nil-map allocation on first write, nil-slice failure accumulation, a zero-value `bytes.Buffer` log, and defensive `Snapshot`/`Failures` copies.
Test: zero value records without construction; missing keys read as `0`; `Snapshot`/`Failures` mutation cannot reach internal state; a `-race` test spawns goroutines calling `Record`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/01-zero-value-metrics-collector/cmd/demo
cd go-solutions/02-variables-types-and-constants/02-zero-values-and-default-initialization/01-zero-value-metrics-collector
```

## Why there is no constructor

The type composes four fields, each chosen because its zero value is already
correct for the first operation: a `sync.Mutex` (usable unlocked at its zero
value), a `map[string]int` of per-service counts (nil until the first write), a
`[]string` of failing services (nil, and `append` to a nil slice just works),
and a `bytes.Buffer` log (writable at its zero value). The only field that
cannot be used at its zero value for its first *write* is the map — writing to a
nil map panics — so `Record` owns that invariant: it allocates the map on first
use, under the lock, and nowhere else. Every other field needs no setup.

That is what lets a caller write `var c Collector` and immediately call
`c.Record(...)`. There is no "forgot to call `New()`" failure mode because there
is no `New()`. This is the same contract `sync.Mutex` and `bytes.Buffer` publish,
applied to your own type.

The read methods make a second design commitment: they never leak internal
mutable state. `Snapshot` returns a fresh map copied under the lock, and
`Failures` returns a `copy` of the failures slice. A caller can mutate what it
gets back without racing or corrupting the collector — the alternative (handing
out the internal map) is a data race and an aliasing bug waiting to happen. Note
also that `Count` on a missing service returns `0` because reading a nil (or
simply key-less) map yields the element zero value; that is correct "no requests
recorded yet", distinct from any recorded count.

Create `collector.go`:

```go
package collector

import (
	"bytes"
	"fmt"
	"sync"
)

// Collector aggregates per-service request counts, the services that returned
// 5xx, and a human-readable log. Its zero value is ready to use: var c Collector
// then c.Record(...). No constructor is required.
type Collector struct {
	mu       sync.Mutex
	counts   map[string]int
	failures []string
	log      bytes.Buffer
}

// Record counts one request for service with the given status. A status >= 500
// also appends service to the failures list. The counts map is allocated lazily
// on first write, under the lock that owns it.
func (c *Collector) Record(service string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[service]++

	if status >= 500 {
		c.failures = append(c.failures, service)
	}

	fmt.Fprintf(&c.log, "%s %d\n", service, status)
}

// Count returns how many requests were recorded for service. An unseen service
// reads as 0 from the (possibly nil) map.
func (c *Collector) Count(service string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.counts[service]
}

// Snapshot returns an independent copy of the per-service counts. Mutating the
// result does not affect the collector.
func (c *Collector) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]int, len(c.counts))
	for service, count := range c.counts {
		out[service] = count
	}
	return out
}

// Failures returns an independent copy of the failing-service list.
func (c *Collector) Failures() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, len(c.failures))
	copy(out, c.failures)
	return out
}

// Log returns the accumulated request log.
func (c *Collector) Log() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.log.String()
}
```

## The runnable demo

The demo declares the collector as a zero value, records a mix of successes and
a 5xx, and prints the aggregates. Because `cmd/demo` is a separate `package
main`, it can only reach the exported methods — which is the whole point: the
zero-value contract is exercised through the public API.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/collector"
)

func main() {
	var c collector.Collector
	c.Record("api", 200)
	c.Record("api", 503)
	c.Record("worker", 204)

	fmt.Printf("api=%d worker=%d missing=%d\n",
		c.Count("api"), c.Count("worker"), c.Count("missing"))
	fmt.Printf("failures=%v\n", c.Failures())
	fmt.Print(c.Log())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
api=2 worker=1 missing=0
failures=[api]
api 200
api 503
worker 204
```

## Tests

The tests are the type's contract, not illustrations. `TestZeroValueRecords`
proves a bare `var c Collector` records without construction and a missing key
reads as `0`. `TestReadBeforeWrite` proves every read method is safe before any
write. `TestSnapshotDoesNotExposeInternalState` mutates the returned snapshot and
asserts the collector is unchanged — the defensive-copy contract. `TestRace`
hammers `Record` from many goroutines so `go test -race` can prove the mutex
actually guards the map and slice.

Create `collector_test.go`:

```go
package collector

import (
	"fmt"
	"sync"
	"testing"
)

func TestZeroValueRecords(t *testing.T) {
	t.Parallel()

	var c Collector
	c.Record("api", 200)
	c.Record("api", 503)
	c.Record("worker", 204)

	if got := c.Count("api"); got != 2 {
		t.Fatalf("Count(api) = %d, want 2", got)
	}
	if got := c.Count("missing"); got != 0 {
		t.Fatalf("Count(missing) = %d, want 0", got)
	}

	failures := c.Failures()
	if len(failures) != 1 || failures[0] != "api" {
		t.Fatalf("Failures() = %#v, want [api]", failures)
	}
	if got := c.Log(); got == "" {
		t.Fatal("Log() should include recorded requests")
	}
}

func TestReadBeforeWrite(t *testing.T) {
	t.Parallel()

	var c Collector

	if got := c.Count("api"); got != 0 {
		t.Fatalf("Count(api) = %d, want 0", got)
	}
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() = %#v, want empty", got)
	}
	if got := c.Failures(); len(got) != 0 {
		t.Fatalf("Failures() = %#v, want empty", got)
	}
	if got := c.Log(); got != "" {
		t.Fatalf("Log() = %q, want empty", got)
	}
}

func TestSnapshotDoesNotExposeInternalState(t *testing.T) {
	t.Parallel()

	var c Collector
	c.Record("api", 200)

	snapshot := c.Snapshot()
	snapshot["api"] = 99
	failures := c.Failures()
	failures = append(failures, "poison")
	_ = failures

	if got := c.Count("api"); got != 1 {
		t.Fatalf("Count(api) = %d, want 1 after mutating snapshot", got)
	}
}

func TestRace(t *testing.T) {
	t.Parallel()

	var c Collector
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := 200
			if i%10 == 0 {
				status = 500
			}
			c.Record("api", status)
		}()
	}
	wg.Wait()

	if got := c.Count("api"); got != 100 {
		t.Fatalf("Count(api) = %d, want 100", got)
	}
}

func ExampleCollector() {
	var c Collector
	c.Record("api", 200)
	c.Record("api", 503)
	fmt.Println(c.Count("api"), len(c.Failures()))
	// Output: 2 1
}
```

## Review

The collector is correct when the zero value is a complete, usable object: a
bare `var c Collector` records, missing keys read as `0`, and no read method
hands out state a caller can mutate. The lazy `make` lives in exactly one place —
`Record`, under the lock — because that is the only method that writes the map;
putting the nil check anywhere else, or forgetting it, reintroduces the
nil-map-write panic. Keep the read methods returning copies: the moment
`Snapshot` returns `c.counts` directly, you have both a data race under `-race`
and an aliasing bug where a caller silently rewrites your counts. And keep the
receivers as pointers so the mutex is never copied after use — `go vet`'s
`copylocks` will flag a value receiver here. Run `go test -race` and confirm
`TestRace` is clean.

## Resources

- [Go Specification: The zero value](https://go.dev/ref/spec#The_zero_value) — the initialization guarantee.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — usable at its zero value; do not copy after use.
- [`bytes.Buffer`](https://pkg.go.dev/bytes#Buffer) — the zero value is an empty, ready-to-write buffer.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-absent-vs-zero-patch-handler.md](02-absent-vs-zero-patch-handler.md)
