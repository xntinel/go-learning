# Exercise 35: Request Deduplicator with Goroutine Partition Writes (Race Education)

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Resolving five duplicate requests for the same key five separate times
wastes four calls to whatever backs `resolve` — a database, an upstream
API. Deduplicating them naively by writing each goroutine's result straight
into a shared `map[string]string` looks reasonable and is a genuine data
race: concurrent writes to a Go map, even to different keys from different
goroutines, are not merely undefined behavior, they are a race the runtime
itself can detect and crash the process over. This module builds `Do`,
which resolves each unique key exactly once, concurrently, without ever
writing to a shared map at all.

This module is fully self-contained. Nothing here imports another
exercise.

## What you'll build

```text
dedup/                         module example.com/dedup
  go.mod
  dedup.go                      Request, Do (goroutine literals partitioned by slice index)
  dedup_test.go                  resolve-once-per-key, error propagation, fan-out to duplicates
  cmd/demo/main.go               five requests, three unique keys
```

- Files: `dedup.go`, `dedup_test.go`, `cmd/demo/main.go`.
- Implement: `Do(requests, resolve)` launching one goroutine literal per *unique* key, each writing only its own slot in two plain slices (`resolved`, `errs`), then fanning the resolved values out to every duplicate's slot single-threaded.
- Test: each unique key's `resolve` is called exactly once, verified via an index-partitioned atomic-counter slice (never a shared map); a failing key's error propagates; a single repeated key fans out to every duplicate.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/35-dedup-partition-racing/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/35-dedup-partition-racing
go mod edit -go=1.24
```

### Slices, never a shared map, are what make the concurrent writes safe

`Do` first collapses `requests` down to `order`, the list of unique keys in
first-seen order, and `firstIdx`, mapping each key to the index of its
first occurrence in the original `requests` slice — both built
single-threaded, before any goroutine starts, so there is nothing
concurrent about constructing them. Then it launches one goroutine literal
per entry in `order`. Each literal writes to exactly two places: `errs[j]`,
its own slot indexed by its position in `order`, and
`resolved[firstIdx[key]]`, its own slot indexed by that key's first
occurrence in `requests` — never a map, and never any other goroutine's
slot. That is the entire safety argument: two goroutines resolving
different keys write to different indices of the same two slices
concurrently, which Go's memory model permits without a lock, whereas two
goroutines writing different keys into the same `map[string]string`
concurrently would not be permitted at all, regardless of how carefully the
keys are kept distinct. Only after `wg.Wait()` — single-threaded again —
does `Do` fan `resolved` out to every duplicate's own slot in `out`,
which is safe precisely because nothing is running concurrently by then.

Create `dedup.go`:

```go
package dedup

import "sync"

// Request is one inbound call that may share its Key with other requests
// in the same batch -- duplicate lookups for the same cache key, retries
// of the same idempotent write, and so on.
type Request struct {
	Key     string
	Payload string
}

// Do resolves each unique key in requests at most once, concurrently, and
// returns one result per request (duplicates get a copy of their key's
// single resolution). It is tempting to have every goroutine literal write
// its result straight into a shared map -- but concurrent writes to a Go
// map, even to different keys from different goroutines, are a data race
// the runtime can detect and crash on ("fatal error: concurrent map
// writes"), not something -race merely warns about. Do avoids that
// entirely: every goroutine literal writes only to its own index in two
// plain slices, resolved and errs, partitioned by the unique key's
// position -- never to a map -- so there is nothing shared to race on.
func Do(requests []Request, resolve func(key string) (string, error)) ([]string, error) {
	firstIdx := make(map[string]int, len(requests))
	order := make([]string, 0, len(requests))
	for i, r := range requests {
		if _, ok := firstIdx[r.Key]; !ok {
			firstIdx[r.Key] = i
			order = append(order, r.Key)
		}
	}

	resolved := make([]string, len(requests)) // slot per request index
	errs := make([]error, len(order))          // slot per unique key

	var wg sync.WaitGroup
	for j, key := range order {
		wg.Add(1)
		go func(j int, key string) {
			defer wg.Done()
			v, err := resolve(key)
			errs[j] = err                // this goroutine's own slot
			resolved[firstIdx[key]] = v  // this goroutine's own slot too
		}(j, key)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Fan the resolved unique-key value out to every duplicate's slot.
	// This runs single-threaded, after wg.Wait, so it is race-free even
	// though it reads resolved at indices other goroutines wrote to.
	out := make([]string, len(requests))
	for i, r := range requests {
		out[i] = resolved[firstIdx[r.Key]]
	}
	return out, nil
}
```

### The runnable demo

The demo resolves five requests over three unique keys.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/dedup"
)

func main() {
	requests := []dedup.Request{
		{Key: "user:1"},
		{Key: "user:2"},
		{Key: "user:1"}, // duplicate of the first
		{Key: "user:3"},
		{Key: "user:2"}, // duplicate of the second
	}

	resolve := func(key string) (string, error) {
		return strings.ToUpper(key), nil
	}

	out, err := dedup.Do(requests, resolve)
	fmt.Println(out, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[USER:1 USER:2 USER:1 USER:3 USER:2] <nil>
```

### Tests

`TestDoResolvesEachUniqueKeyExactlyOnce` counts calls per key using an
atomic-counter slice indexed the same way `Do` itself partitions its
writes — never a shared map — and checks every unique key was resolved
exactly once despite six requests over three keys.
`TestDoPropagatesResolverError` checks a failing key's error comes back
from `Do`. `TestDoSingleKeyFansOutToAllDuplicates` checks three requests
for one key all get the same resolved value.

Create `dedup_test.go`:

```go
package dedup

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDoResolvesEachUniqueKeyExactlyOnce(t *testing.T) {
	t.Parallel()
	requests := []Request{
		{Key: "a"}, {Key: "b"}, {Key: "a"}, {Key: "c"}, {Key: "a"}, {Key: "b"},
	}

	// One atomic counter per request, indexed by each key's first
	// occurrence -- itself an index-partitioned write (never a shared
	// map, which would race the same way the production code warns
	// against), matching the pattern Do itself uses.
	calls := make([]atomic.Int32, len(requests))
	seenIdx := map[string]int{}
	for i, r := range requests {
		if _, ok := seenIdx[r.Key]; !ok {
			seenIdx[r.Key] = i
		}
	}

	resolve := func(key string) (string, error) {
		calls[seenIdx[key]].Add(1)
		return strings.ToUpper(key), nil
	}

	out, err := Do(requests, resolve)
	if err != nil {
		t.Fatalf("Do() err = %v, want nil", err)
	}

	want := []string{"A", "B", "A", "C", "A", "B"}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out = %v, want %v", out, want)
		}
	}

	for key, idx := range seenIdx {
		if n := calls[idx].Load(); n != 1 {
			t.Errorf("resolve(%q) called %d times, want exactly 1", key, n)
		}
	}
}

func TestDoPropagatesResolverError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("upstream down")
	requests := []Request{{Key: "ok"}, {Key: "bad"}, {Key: "ok"}}

	resolve := func(key string) (string, error) {
		if key == "bad" {
			return "", sentinel
		}
		return key, nil
	}

	_, err := Do(requests, resolve)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Do() err = %v, want %v", err, sentinel)
	}
}

func TestDoSingleKeyFansOutToAllDuplicates(t *testing.T) {
	t.Parallel()
	requests := []Request{{Key: "x"}, {Key: "x"}, {Key: "x"}}
	resolve := func(key string) (string, error) { return "resolved-" + key, nil }

	out, err := Do(requests, resolve)
	if err != nil {
		t.Fatalf("Do() err = %v, want nil", err)
	}
	for i, v := range out {
		if v != "resolved-x" {
			t.Fatalf("out[%d] = %q, want %q", i, v, "resolved-x")
		}
	}
}
```

## Review

`Do` is correct when every unique key is resolved exactly once regardless
of how many requests share it, verified under `-race` with 50 or 5 requests
alike, and a failing key's error is never swallowed. The education this
exercise is built around is the map instinct: `map[string]string` feels
like the obvious shared result store for a dedup routine, and it is exactly
wrong the moment more than one goroutine can write to it, even to
different keys — Go maps carry no per-key locking, only a whole-map
race detector that panics the process rather than silently corrupting
data. Two plain slices, each partitioned so every goroutine owns exactly
one index, sidestep the question entirely: there is no shared mutable
structure for the runtime to catch a race on, because there is no point
during the concurrent phase where two goroutines can address the same
memory.

## Resources

- [Go Language Specification: Map types](https://go.dev/ref/spec#Map_types)
- [The Go Memory Model](https://go.dev/ref/mem)
- [Go blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-dns-cache-afterfunc.md](34-dns-cache-afterfunc.md) | Next: [../06-function-types-and-callbacks/00-concepts.md](../06-function-types-and-callbacks/00-concepts.md)
