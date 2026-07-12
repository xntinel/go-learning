# Exercise 1: Per-URL visit counter with lock-free lazy counters

Every request-facing service eventually needs per-route hit counts: how many times
`/home` was served, how many `/checkout`, feeding a dashboard or a rate-limit
decision. The key set is stable (a handful of routes, written once and hammered
forever) and the update is a pure increment, which is exactly the shape
`sync.Map` plus the typed-pointer idiom was built for. This module builds that
counter so it stays exactly accurate under heavy concurrency with no lock on the
increment path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
visitcounter/                 independent module: example.com/visitcounter
  go.mod                      go 1.26
  counter.go                  type VisitCounter; Visit, Count, URLs
  cmd/
    demo/
      main.go                 runnable demo: visit a few routes, print counts
  counter_test.go             accurate-under-concurrency race test, comma-ok miss, Example
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: `VisitCounter` with `Visit(url) int64`, `Count(url) int64` (0 for unseen), `URLs() []string`.
- Test: N goroutines x M urls x P increments assert every count is exact; unknown URL returns 0; an `Example` with an `// Output:` block.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir go-solutions/15-sync-primitives/04-sync-map/01-visit-counter && cd go-solutions/15-sync-primitives/04-sync-map/01-visit-counter
```

### Why a shared atomic pointer, not a stored int

The naive concurrent counter stores an `int64` in the map and, on each visit,
loads it, adds one, and stores it back. That read-modify-write is not atomic
across the two map operations: two goroutines can both load 7, both compute 8, and
both store 8, losing an increment. Guarding it with a mutex works but serializes
every visit through one lock.

The idiom instead stores a **pointer to an `atomic.Int64`** and mutates through
the atomic. `Visit` does `LoadOrStore(url, &atomic.Int64{})`: the first caller for
a URL inserts a fresh atomic, every later caller gets that same pointer back, and
all of them call `Add(1)` on the one shared atomic. The map is touched once per
new key and never again for increments; the increment itself is a single lock-free
atomic instruction. That is why the count stays exactly correct no matter how many
goroutines pound the same route — the correctness lives in `atomic.Int64.Add`, and
`sync.Map` only ever hands out the right pointer.

`Count` uses the comma-ok form of `Load` and returns 0 for a URL never visited —
an unseen route is not an error, it is a zero. `URLs` enumerates keys with
`Range`; order is unspecified, which is fine for a scrape.

Note the one subtlety in `Visit`: `LoadOrStore(url, &atomic.Int64{})` allocates a
fresh `atomic.Int64` on every call, even when the key already exists, because Go
evaluates the argument before the call. That allocation is cheap (a pointer-sized
atomic) and thrown away on the hit path; it is the acceptable cost of the idiom's
simplicity. The expensive-construction version of this problem — where you must
*not* build the object on every call — is the connection registry in Exercise 4,
which uses `sync.Once` instead.

Create `counter.go`:

```go
package visitcounter

import (
	"sync"
	"sync/atomic"
)

// VisitCounter tracks a lock-free hit count per URL. It fits the sync.Map
// "stable keys" pattern: a small set of routes written once and incremented
// under heavy concurrency. The map stores *atomic.Int64, so every caller for a
// given URL shares one atomic and increments through it without a lock.
type VisitCounter struct {
	counts sync.Map // map[string]*atomic.Int64
}

// NewVisitCounter returns an empty counter ready for concurrent use.
func NewVisitCounter() *VisitCounter {
	return &VisitCounter{}
}

// Visit increments the count for url and returns the new value. The first call
// for a url installs a fresh atomic; later calls share it.
func (vc *VisitCounter) Visit(url string) int64 {
	actual, _ := vc.counts.LoadOrStore(url, &atomic.Int64{})
	return actual.(*atomic.Int64).Add(1)
}

// Count returns the current count for url, or 0 if it was never visited.
func (vc *VisitCounter) Count(url string) int64 {
	actual, ok := vc.counts.Load(url)
	if !ok {
		return 0
	}
	return actual.(*atomic.Int64).Load()
}

// URLs returns every url seen so far. Order is unspecified.
func (vc *VisitCounter) URLs() []string {
	var urls []string
	vc.counts.Range(func(key, _ any) bool {
		urls = append(urls, key.(string))
		return true
	})
	return urls
}
```

### The runnable demo

The demo serves a fixed sequence of routes and prints the running count returned
by each `Visit`, then the count of a route that was never served (0).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/visitcounter"
)

func main() {
	vc := visitcounter.NewVisitCounter()
	for _, u := range []string{"/home", "/about", "/home", "/home", "/about"} {
		fmt.Printf("%s -> %d\n", u, vc.Visit(u))
	}
	fmt.Println("count /home:", vc.Count("/home"))
	fmt.Println("count /about:", vc.Count("/about"))
	fmt.Println("count /missing:", vc.Count("/missing"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/home -> 1
/about -> 1
/home -> 2
/home -> 3
/about -> 2
count /home: 3
count /about: 2
count /missing: 0
```

### Tests

`TestAccurateUnderConcurrency` is the hard contract: `goroutines` goroutines each
run `perURL` visits against each of three URLs, and after `wg.Wait` every count
must equal `goroutines*perURL` exactly and the grand total must be exact. Any lost
increment — the bug the atomic prevents — shows up here immediately, and `-race`
proves the map access itself is clean. `TestUnknownURL` pins that an unseen route
is 0, not a panic. The `Example` documents the running-count return.

Create `counter_test.go`:

```go
package visitcounter

import (
	"fmt"
	"sync"
	"testing"
)

func TestAccurateUnderConcurrency(t *testing.T) {
	t.Parallel()

	vc := NewVisitCounter()
	urls := []string{"/a", "/b", "/c"}
	const goroutines = 8
	const perURL = 500

	var wg sync.WaitGroup
	for _, u := range urls {
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perURL {
					vc.Visit(u)
				}
			}()
		}
	}
	wg.Wait()

	wantPer := int64(goroutines * perURL)
	var total int64
	for _, u := range urls {
		if got := vc.Count(u); got != wantPer {
			t.Errorf("Count(%q) = %d, want %d", u, got, wantPer)
		}
		total += vc.Count(u)
	}
	if want := int64(len(urls)) * wantPer; total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
}

func TestUnknownURL(t *testing.T) {
	t.Parallel()

	vc := NewVisitCounter()
	if got := vc.Count("/missing"); got != 0 {
		t.Fatalf("Count(/missing) = %d, want 0", got)
	}
}

func TestURLsEnumeratesSeenKeys(t *testing.T) {
	t.Parallel()

	vc := NewVisitCounter()
	for _, u := range []string{"/x", "/y", "/x"} {
		vc.Visit(u)
	}
	got := map[string]bool{}
	for _, u := range vc.URLs() {
		got[u] = true
	}
	if len(got) != 2 || !got["/x"] || !got["/y"] {
		t.Fatalf("URLs() = %v, want the set {/x,/y}", vc.URLs())
	}
}

func ExampleVisitCounter() {
	vc := NewVisitCounter()
	fmt.Println(vc.Visit("/x"))
	fmt.Println(vc.Visit("/x"))
	fmt.Println(vc.Count("/missing"))
	// Output:
	// 1
	// 2
	// 0
}
```

## Review

The counter is correct when every increment is a single atomic `Add` on a pointer
shared through the map, never a read-modify-write of a stored `int64`. The proof
is `TestAccurateUnderConcurrency`: if any count comes back below
`goroutines*perURL`, an increment was lost, which means the count was being mutated
outside an atomic. The two traps to avoid are storing an `int64` value instead of a
`*atomic.Int64` pointer (lost updates), and type-asserting `Load`'s result without
comma-ok in `Count` (a panic on an unseen URL instead of a clean 0). Run
`go test -race` to confirm the `sync.Map` and `atomic.Int64` interplay is
race-free under load; the accuracy assertion and the race detector together are
the whole contract.

## Resources

- [sync.Map](https://pkg.go.dev/sync#Map) — `Load`, `LoadOrStore`, `Range` and the forbidden-copy note.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` `Add`/`Load`, the lock-free increment.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before edges `sync.Map` operations establish.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-typed-store.md](02-typed-store.md)
