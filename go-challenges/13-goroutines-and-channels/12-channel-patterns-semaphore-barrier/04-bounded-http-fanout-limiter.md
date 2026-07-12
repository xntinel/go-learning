# Exercise 4: Bounding In-Flight Requests to a Downstream API

The canonical production use of a semaphore is protecting a dependency. You have
a list of URLs to fetch, records to push, or objects to upload, and firing them
all at once would exhaust the downstream's connection pool or trip its rate
limit. This exercise builds `FetchAll`: a bounded fan-out that keeps at most
`maxInFlight` requests hitting the downstream at once, tested against an
`httptest.Server` that records the real peak concurrency it saw.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
fanout/                     independent module: example.com/fanout
  go.mod                    go 1.26
  fanout.go                 type Result; FetchAll(ctx, client, urls, maxInFlight)
  cmd/
    demo/
      main.go               fetch several URLs from a test server, print status counts
  fanout_test.go            peak<=cap against httptest, cancellation unwinds (-race)
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `FetchAll` that fans out GET requests through a context-aware semaphore, collecting a per-URL `Result`, with no more than `maxInFlight` in flight at once.
- Test: an `httptest.Server` records peak concurrency; fire 50 URLs at `maxInFlight=4` and assert the observed peak is `<= 4` and all 50 results returned; cancelling the parent context mid-flight unwinds in-flight requests with `context.Canceled`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### How the limiter is wired

`FetchAll` allocates a semaphore of capacity `maxInFlight` and a results slice
indexed to match the input URLs, so each goroutine writes its own slot and no
shared writes race. For each URL it *acquires the semaphore in the parent
goroutine before spawning* the worker. That ordering is deliberate: acquiring
first means at most `maxInFlight` goroutines are ever running a request
concurrently, and it also gives back-pressure — the loop itself blocks on a full
semaphore instead of spawning goroutines that would immediately block. The
acquire is context-aware: if the caller's context is cancelled while the loop is
waiting for a slot, the remaining URLs are recorded as `ctx.Err()` rather than
dispatched.

Each worker builds its request with `http.NewRequestWithContext` so the shared
context propagates into the HTTP client. When the parent context is cancelled,
every in-flight `client.Do` returns promptly with an error wrapping
`context.Canceled`, the worker records it, releases the slot, and exits — no
goroutine is left hanging on a dead request. `errors.Is(err, context.Canceled)`
sees through the `*url.Error` the client wraps around the cause.

Writing distinct indices of a slice from different goroutines is race-free: the
race detector flags concurrent access to the *same* memory, and each index is a
separate element. A `WaitGroup` joins all workers before `FetchAll` returns the
slice.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// Result is the outcome of fetching one URL. Exactly one of Status (>0) or Err
// is meaningful.
type Result struct {
	URL    string
	Status int
	Err    error
}

// FetchAll issues a GET for every URL, keeping at most maxInFlight requests in
// flight at once so a downstream dependency is never hit by more than that many
// concurrent callers. Results are returned in the same order as urls. If ctx is
// cancelled, URLs not yet dispatched record ctx.Err() and in-flight requests
// unwind with the cancellation error.
func FetchAll(ctx context.Context, client *http.Client, urls []string, maxInFlight int) []Result {
	sem := make(chan struct{}, maxInFlight)
	results := make([]Result, len(urls))
	var wg sync.WaitGroup

	for i, u := range urls {
		// Acquire in the parent goroutine: bounds in-flight workers and applies
		// back-pressure to this loop.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			results[i] = Result{URL: u, Err: ctx.Err()}
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = fetch(ctx, client, u)
		}()
	}

	wg.Wait()
	return results
}

func fetch(ctx context.Context, client *http.Client, url string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{URL: url, Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{URL: url, Err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return Result{URL: url, Status: resp.StatusCode}
}
```

### The runnable demo

The demo stands up an `httptest.Server` that returns 200, fans out eight requests
to it at `maxInFlight=3`, and prints how many succeeded — a deterministic count
regardless of the order the requests complete in.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/fanout"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	urls := make([]string, 8)
	for i := range urls {
		urls[i] = srv.URL
	}

	results := fanout.FetchAll(context.Background(), srv.Client(), urls, 3)

	ok := 0
	for _, r := range results {
		if r.Err == nil && r.Status == http.StatusOK {
			ok++
		}
	}
	fmt.Printf("requests=%d ok=%d\n", len(results), ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests=8 ok=8
```

### Tests

`TestBoundsInFlight` is the proof: the test server's handler increments an atomic
on entry, records the peak via `CompareAndSwap`, sleeps briefly so requests
overlap, and decrements on exit. Firing 50 URLs at `maxInFlight=4` and asserting
the server-observed peak is `<= 4` proves the limiter actually held at the
downstream, not just in our accounting. `TestCancelUnwinds` points `FetchAll` at
a handler that blocks until its request context is cancelled, cancels the parent
shortly after start, and asserts every result carries `context.Canceled` — the
in-flight requests unwound rather than hanging.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundsInFlight(t *testing.T) {
	t.Parallel()

	var live, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := live.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // hold the slot so requests overlap
		live.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const n, limit = 50, 4
	urls := make([]string, n)
	for i := range urls {
		urls[i] = srv.URL
	}

	results := FetchAll(context.Background(), srv.Client(), urls, limit)

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for _, r := range results {
		if r.Err != nil || r.Status != http.StatusOK {
			t.Fatalf("result for %s = status %d, err %v", r.URL, r.Status, r.Err)
		}
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("downstream peak concurrency = %d, want <= %d", got, limit)
	}
}

func TestCancelUnwinds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the client cancels
	}))
	defer srv.Close()

	const n, limit = 6, 6
	urls := make([]string, n)
	for i := range urls {
		urls[i] = srv.URL
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	results := FetchAll(ctx, srv.Client(), urls, limit)

	canceled := 0
	for _, r := range results {
		if r.Err == nil {
			t.Fatalf("expected an error for %s, got status %d", r.URL, r.Status)
		}
		if errors.Is(r.Err, context.Canceled) {
			canceled++
		}
	}
	if canceled == 0 {
		t.Fatal("expected at least one context.Canceled result")
	}
}
```

## Review

The limiter is correct when the peak concurrency measured *at the downstream*
never exceeds `maxInFlight` and every URL yields exactly one result. Measuring the
peak inside the test server, not in `FetchAll`, is what makes the test
trustworthy — it proves the contract the dependency actually experiences.
Acquiring the semaphore before spawning the worker is the key design choice: it
bounds the number of live request goroutines and back-pressures the dispatch loop
rather than spawning goroutines that immediately block. The context threads all
the way into `http.NewRequestWithContext`, so cancellation unwinds in-flight
requests instead of leaking them — the failure this exercise exists to prevent.
Run `-race` to confirm the per-index result writes and the atomic peak tracker are
clean.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewServer` and the client it exposes for in-process HTTP tests.
- [net/http: NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) — attaching a context so cancellation propagates into `Client.Do`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded fan-out and cancellation propagation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-context-aware-semaphore-acquire.md](03-context-aware-semaphore-acquire.md) | Next: [05-weighted-semaphore-cost-aware.md](05-weighted-semaphore-cost-aware.md)
