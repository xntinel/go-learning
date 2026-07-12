# Exercise 30: Request coalescing (singleflight) pattern

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache-miss stampede happens when a hot key expires and dozens of
concurrent requests all fall through to the same expensive backend call at
once — a database query, an upstream RPC, a cryptographic operation — before
any of them has a chance to repopulate the cache. Request coalescing
(often called "singleflight") fixes this at the source: when several
requests for the same key are already waiting to be served, only the first
one actually triggers the backend call, and every other one is answered
with that same result once it completes. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
coalesce/                   independent module: example.com/coalesce
  go.mod                     go 1.24
  coalesce.go                  Request, CallResult, Backend, Run
  cmd/
    demo/
      main.go                runnable demo: 5 requests across 2 keys, coalesced into 2 backend calls
  coalesce_test.go             table-style tests: coalescing, a lone request, backend errors, an empty closed channel, cancellation draining a buffered burst
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `Run(ctx, requests <-chan Request, backend Backend)`, dequeuing requests in a `for`-`select` and coalescing every request for the same key present in the channel at that instant into one `Backend` call.
- Test: several requests for the same key producing exactly one backend call, a lone request never marked as shared, a backend error reaching every coalesced caller, an already-closed empty channel returning immediately, and a cancelled context still draining and answering a buffered burst.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/30-request-coalescing-singleflight/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/30-request-coalescing-singleflight
go mod edit -go=1.24
```

### Why the dequeue loop needs labeled breaks, not bare ones

`Run` is a `for`-`select` event loop — the canonical shape this entire
chapter is about, and the canonical place a bare `break` is a production
bug. A bare `break` in either `select` case would leave only the `select`;
the outer `for` would immediately loop again and re-enter the exact same
`select`, spinning forever instead of stopping. `break loop`, naming the
outer `for`, is what actually leaves the dequeue loop, whether the reason is
a cancelled context or a closed, drained channel.

The coalescing itself lives one level deeper, inside `drainAndAnswer`'s own
small `for`-`select`: after a request is dequeued (or a cancellation fires),
a *non-blocking* receive scans the channel for every other request
currently sitting there. The moment the channel would block (`default`) or
report closed, that inner scan has to stop — and it needs its own label,
`collect`, because it sits inside a function whose caller has its own `loop`
label for an entirely different `for`. Reusing an unlabeled `break` here
would still work by coincidence (this inner `for` is the innermost one), but
naming it removes any ambiguity about which loop stops, which matters once a
reader is staring at two labeled loops nested across two functions.

Once the burst is collected, requests are grouped by key, preserving the
order each key first appeared in, and the `Backend` is called exactly once
per distinct key — every other request in that key's group is answered with
the identical value or error, marked `Shared: true`.

Create `coalesce.go`:

```go
package coalesce

import "context"

// Request is one caller's request for a keyed backend value, answered on
// Result once the backend call for its key completes.
type Request struct {
	Key    string
	Result chan<- CallResult
}

// CallResult is what a Request is answered with. Shared reports whether
// this particular request was coalesced onto another request's backend
// call rather than triggering its own.
type CallResult struct {
	Value  string
	Err    error
	Shared bool
}

// Backend is the (potentially slow, potentially expensive) call a key maps
// to -- a downstream RPC, a database query, a cache-miss fetch.
type Backend func(key string) (string, error)

// Run dequeues Requests and answers them, coalescing by key: every request
// for the same key that arrives in the SAME dequeue burst -- the request
// that was just dequeued, plus every other request already buffered in the
// channel at that instant -- is answered from a single Backend call instead
// of one call per request. This is the classic fix for a cache-stampede or
// a burst of identical client retries hitting the same expensive call. On
// ctx.Done() it drains whatever is still buffered, coalescing it the same
// way, before stopping -- a cancellation must not abandon requests that
// were already accepted onto the channel.
func Run(ctx context.Context, requests <-chan Request, backend Backend) {
loop:
	for {
		select {
		case <-ctx.Done():
			drainAndAnswer(requests, backend, nil)
			break loop
		case req, ok := <-requests:
			if !ok {
				break loop
			}
			drainAndAnswer(requests, backend, &req)
		}
	}
}

// drainAndAnswer collects first (if non-nil) plus every request currently
// buffered in the channel via a non-blocking scan, groups them by key
// preserving first-seen order, and answers each group from exactly one
// Backend call.
func drainAndAnswer(requests <-chan Request, backend Backend, first *Request) {
	var burst []Request
	if first != nil {
		burst = append(burst, *first)
	}

collect:
	for {
		select {
		case req, ok := <-requests:
			if !ok {
				// Channel closed with nothing left buffered: leave the
				// SELECT here, not the caller's loop -- a bare break would
				// do exactly this already, since this select has no
				// enclosing for beyond the one it belongs to, but naming it
				// keeps the intent explicit next to the sibling break below
				// that means something different (see Run's break loop).
				break collect
			}
			burst = append(burst, req)
		default:
			// Nothing left buffered right now: stop collecting, the burst
			// is complete.
			break collect
		}
	}

	groups := make(map[string][]Request)
	order := make([]string, 0, len(burst))
	for _, req := range burst {
		if _, seen := groups[req.Key]; !seen {
			order = append(order, req.Key)
		}
		groups[req.Key] = append(groups[req.Key], req)
	}

	for _, key := range order {
		value, err := backend(key)
		for i, req := range groups[key] {
			req.Result <- CallResult{Value: value, Err: err, Shared: i > 0}
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/coalesce"
)

func main() {
	calls := 0
	backend := func(key string) (string, error) {
		calls++
		return fmt.Sprintf("value-for-%s-call-%d", key, calls), nil
	}

	keys := []string{"user:1", "user:1", "user:2", "user:1", "user:2"}
	requests := make(chan coalesce.Request, len(keys))
	results := make([]chan coalesce.CallResult, len(keys))
	for i, k := range keys {
		results[i] = make(chan coalesce.CallResult, 1)
		requests <- coalesce.Request{Key: k, Result: results[i]}
	}
	close(requests)

	coalesce.Run(context.Background(), requests, backend)

	for i, k := range keys {
		res := <-results[i]
		fmt.Printf("%s -> %s (shared=%v)\n", k, res.Value, res.Shared)
	}
	fmt.Println("backend calls made:", calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:1 -> value-for-user:1-call-1 (shared=false)
user:1 -> value-for-user:1-call-1 (shared=true)
user:2 -> value-for-user:2-call-2 (shared=false)
user:1 -> value-for-user:1-call-1 (shared=true)
user:2 -> value-for-user:2-call-2 (shared=true)
backend calls made: 2
```

Five requests across two keys produce exactly two backend calls. Every
`user:1` request receives the identical value from the single call made for
that key, and only the first one in dequeue order is `shared=false`.

### Tests

`TestRunCoalescesRepeatedKeysIntoOneBackendCall` is the core case: five
requests, two keys, exactly two backend calls, and every request for the
same key seeing the identical value. `TestRunSingleRequestIsNeverMarkedShared`,
`TestRunPropagatesBackendError`, and `TestRunOnClosedEmptyChannelReturnsImmediately`
round out the ordinary cases. `TestRunDrainsBufferedRequestsOnCancel`
confirms the shutdown path: a pre-cancelled context still drains and
answers every request that was already buffered on the channel, sharing one
backend call across all of them, exactly like the steady-state path.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"context"
	"testing"
)

// run buffers every request in keys before starting Run, so the whole burst
// is dequeued and coalesced deterministically in a single pass, then closes
// the channel so Run returns on its own once drained.
func run(t *testing.T, keys []string, backend Backend) []CallResult {
	t.Helper()

	requests := make(chan Request, len(keys))
	results := make([]chan CallResult, len(keys))
	for i, k := range keys {
		results[i] = make(chan CallResult, 1)
		requests <- Request{Key: k, Result: results[i]}
	}
	close(requests)

	Run(context.Background(), requests, backend)

	out := make([]CallResult, len(keys))
	for i := range keys {
		out[i] = <-results[i]
	}
	return out
}

func TestRunCoalescesRepeatedKeysIntoOneBackendCall(t *testing.T) {
	t.Parallel()

	calls := 0
	backend := func(key string) (string, error) {
		calls++
		return key + "-result", nil
	}

	results := run(t, []string{"a", "a", "b", "a", "b"}, backend)

	if calls != 2 {
		t.Fatalf("backend calls = %d, want 2 (one per distinct key)", calls)
	}

	wantShared := []bool{false, true, false, true, true}
	for i, r := range results {
		if r.Shared != wantShared[i] {
			t.Fatalf("results[%d].Shared = %v, want %v", i, r.Shared, wantShared[i])
		}
	}
	// Every request for the same key must see the identical value, proving
	// they really were answered from the same call, not independent ones.
	if results[0].Value != results[1].Value || results[1].Value != results[3].Value {
		t.Fatalf("requests for key %q got different values: %v", "a", results)
	}
	if results[2].Value != results[4].Value {
		t.Fatalf("requests for key %q got different values: %v", "b", results)
	}
}

func TestRunSingleRequestIsNeverMarkedShared(t *testing.T) {
	t.Parallel()

	backend := func(key string) (string, error) { return key + "-result", nil }

	results := run(t, []string{"solo"}, backend)
	if results[0].Shared {
		t.Fatal("a lone request should never be marked Shared")
	}
	if results[0].Value != "solo-result" {
		t.Fatalf("value = %q, want %q", results[0].Value, "solo-result")
	}
}

func TestRunPropagatesBackendError(t *testing.T) {
	t.Parallel()

	wantErr := errBoom
	backend := func(key string) (string, error) { return "", wantErr }

	results := run(t, []string{"x", "x"}, backend)
	for i, r := range results {
		if r.Err != wantErr {
			t.Fatalf("results[%d].Err = %v, want %v", i, r.Err, wantErr)
		}
	}
}

func TestRunOnClosedEmptyChannelReturnsImmediately(t *testing.T) {
	t.Parallel()

	requests := make(chan Request)
	close(requests)

	calls := 0
	backend := func(key string) (string, error) {
		calls++
		return "", nil
	}

	Run(context.Background(), requests, backend)

	if calls != 0 {
		t.Fatalf("backend calls = %d, want 0 for an empty channel", calls)
	}
}

func TestRunDrainsBufferedRequestsOnCancel(t *testing.T) {
	t.Parallel()

	const n = 6
	requests := make(chan Request, n)
	results := make([]chan CallResult, n)
	for i := range n {
		results[i] = make(chan CallResult, 1)
		requests <- Request{Key: "k", Result: results[i]}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel with all n requests already buffered

	calls := 0
	backend := func(key string) (string, error) {
		calls++
		return "v", nil
	}

	Run(ctx, requests, backend)

	for i := range n {
		res := <-results[i]
		if res.Value != "v" {
			t.Fatalf("results[%d].Value = %q, want %q", i, res.Value, "v")
		}
	}
	if calls != 1 {
		t.Fatalf("backend calls = %d, want 1 (all n requests share the same key)", calls)
	}
}

var errBoom = boomError{}

type boomError struct{}

func (boomError) Error() string { return "backend boom" }
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

Coalescing is correct when every request for the same key present in one
dequeue burst sees the identical result, exactly one of them is
`Shared: false`, and the `Backend` runs exactly once per distinct key in
that burst — `TestRunCoalescesRepeatedKeysIntoOneBackendCall` pins all
three at once. The bug this exercise guards against is a bare `break`
anywhere in either `for`-`select`: in `Run`, it would spin forever instead
of stopping; in `drainAndAnswer`'s collection scan, it happens to work only
because that `for` is the innermost one, which is exactly the kind of
coincidental correctness that breaks the moment the function is refactored.
`TestRunDrainsBufferedRequestsOnCancel` proves cancellation is not an excuse
to drop buffered work: every request already on the channel still gets
answered, coalesced the same way as the steady-state path.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight) — the standard library-adjacent implementation of this exact pattern.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the `default` case makes a receive non-blocking.
- [Cache stampede (Wikipedia)](https://en.wikipedia.org/wiki/Cache_stampede) — the failure mode request coalescing exists to prevent.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-admission-control-load-shedding.md](29-admission-control-load-shedding.md) | Next: [31-distributed-trace-span-propagation.md](31-distributed-trace-span-propagation.md)
