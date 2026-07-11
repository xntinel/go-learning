# Exercise 3: A Semaphore-Bounded HTTP Fan-Out

A service that fans out to a downstream API must never send more concurrent requests than that downstream is provisioned to handle; exceeding its budget turns your throughput into its outage. This module builds a bounded fan-out client that issues many HTTP GETs but holds the number in flight at any instant to a fixed ceiling N, and proves the ceiling by measuring concurrency at the downstream itself.

This module is fully self-contained. It depends on nothing but the standard library and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fanout.go             Result, Fetch
cmd/
  demo/
    main.go           fan out to an in-process server and summarize
fanout_test.go        downstream-observed peak <= N under -race; error capture
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Result` and `Fetch(ctx, client, urls, maxConcurrency)`.
- Test: `fanout_test.go` runs a downstream that records its own peak concurrency and asserts it never exceeds N; a second test confirms transport errors are captured per-URL.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p bounded-http-fanout/cmd/demo && cd bounded-http-fanout
go mod init example.com/bounded-http-fanout
```

### The design, and why the proof must come from the server

`Fetch` takes a list of URLs and a concurrency ceiling. It allocates a results slice indexed one-to-one with the input, so the output preserves input order without any locking — each goroutine owns its own slot and no two goroutines touch the same index. The bound is the familiar buffered-channel semaphore: acquire a token in the loop before launching, release it in a deferred receive. Every request is built with `http.NewRequestWithContext` so a cancelled parent aborts the in-flight HTTP calls rather than waiting them out, and the response body is drained and closed so the transport can reuse the connection — a fan-out that leaks bodies will exhaust its connection pool and stall under load.

The subtle part is how you prove the bound actually holds, and the answer is that the client cannot prove it about itself convincingly. A client-side in-flight counter measures when the client thinks a request is outstanding, but the thing that matters is how many requests are simultaneously hitting the downstream, which is a property observed at the downstream. So the test stands up an `httptest.Server` whose handler increments an atomic counter on entry, records the high-water mark with a compare-and-swap loop, sleeps long enough to force overlap, then decrements. The peak that handler records is the real concurrency the downstream experienced, and asserting it is at most N is the only assertion that means what the exercise claims. Running it under `-race` is mandatory: the handler's counter is touched by many server goroutines at once, and the detector would flag a non-atomic version.

A per-URL `Result` carries the status code and any transport error separately. An HTTP 500 is a successful round-trip with a server-error status, so it sets `Status` and leaves `Err` nil; a connection refused or a DNS failure never produced a status, so it sets `Err`. Collapsing those two cases into one field is a classic fan-out bug that makes a downstream's 500s indistinguishable from the network being down.

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// Result is the outcome of one request. Status is the HTTP status code when a
// response was received (including 4xx/5xx); Err is non-nil only when no
// response was obtained at all (transport error, cancellation, bad URL).
type Result struct {
	URL    string
	Status int
	Err    error
}

// Fetch issues a GET for every URL with at most maxConcurrency requests in
// flight at any instant. Results are returned in the same order as urls. A
// cancelled ctx aborts in-flight requests.
func Fetch(ctx context.Context, client *http.Client, urls []string, maxConcurrency int) []Result {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	if client == nil {
		client = http.DefaultClient
	}
	results := make([]Result, len(urls))
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	for i, url := range urls {
		sem <- struct{}{} // acquire BEFORE launching; this is the bound.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = fetchOne(ctx, client, url)
		}()
	}
	wg.Wait()
	return results
}

func fetchOne(ctx context.Context, client *http.Client, url string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{URL: url, Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{URL: url, Err: err}
	}
	// Drain and close so the connection can be reused; a leaked body
	// exhausts the transport's connection pool under load.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return Result{URL: url, Status: resp.StatusCode}
}
```

### The runnable demo

The demo is fully in-process: it stands up an `httptest.Server` that records its own peak concurrency, fans out forty requests to it at a ceiling of five, and prints how many succeeded along with the peak the server saw. Because the server is local there is no external dependency and the demo is hermetic, yet it exercises the real `net/http` transport. The printed peak is the honest measurement: it comes from the downstream, and it is at most the ceiling.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/bounded-http-fanout/fanout"
)

func main() {
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const n = 40
	const limit = 5
	urls := make([]string, n)
	for i := range urls {
		urls[i] = srv.URL
	}

	results := fanout.Fetch(context.Background(), srv.Client(), urls, limit)
	ok := 0
	for _, r := range results {
		if r.Err == nil && r.Status == http.StatusOK {
			ok++
		}
	}
	fmt.Printf("requests=%d limit=%d ok=%d downstream_peak=%d\n", n, limit, ok, peak.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (downstream peak is bounded by the limit and reaches it here):

```text
requests=40 limit=5 ok=40 downstream_peak=5
```

### Tests

The peak test is the contract: a downstream that records its own concurrency, a fan-out at ceiling N, and an assertion that the server never saw more than N at once. It runs under `-race` because the handler's counter is shared across server goroutines. The error test points the fan-out at a closed server's address so every request fails at the transport, and asserts each `Result` carries a non-nil `Err` and a zero `Status` — proving transport failures are reported per-URL and not silently dropped or confused with a status code.

Create `fanout_test.go`:

```go
package fanout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchBoundsDownstreamConcurrency(t *testing.T) {
	t.Parallel()

	const limit = 4
	const n = 30
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := inFlight.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	urls := make([]string, n)
	for i := range urls {
		urls[i] = srv.URL
	}

	results := Fetch(context.Background(), srv.Client(), urls, limit)
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("result %d: unexpected error %v", i, r.Err)
		}
		if r.Status != http.StatusOK {
			t.Fatalf("result %d: status %d, want 200", i, r.Status)
		}
	}
	if got := peak.Load(); got > int64(limit) {
		t.Fatalf("downstream peak = %d, want <= %d", got, limit)
	}
}

func TestFetchCapturesTransportErrors(t *testing.T) {
	t.Parallel()

	// Start a server, capture its URL, then close it so every request fails
	// at the transport with no status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	client := srv.Client()
	srv.Close()

	urls := []string{dead, dead, dead}
	results := Fetch(context.Background(), client, urls, 2)
	if len(results) != len(urls) {
		t.Fatalf("got %d results, want %d", len(results), len(urls))
	}
	for i, r := range results {
		if r.Err == nil {
			t.Fatalf("result %d: want a transport error, got nil", i)
		}
		if r.Status != 0 {
			t.Fatalf("result %d: status = %d, want 0 on transport error", i, r.Status)
		}
	}
}
```

## Review

The fan-out is correct when the downstream — not the client — never sees more than N concurrent requests, when results stay in input order, and when transport errors are reported per-URL distinct from HTTP status codes. The decisive design choice is measuring the peak at the server; a client-side counter proves the client's bookkeeping, not the downstream's load, and the two can differ. Read the error test to confirm a failed round-trip yields a non-nil `Err` and a zero `Status`, and the success test to confirm a 500 would set `Status` and leave `Err` nil — folding those together hides outages. Drain and close every response body; a fan-out that leaks bodies stalls once the connection pool is exhausted, which the race detector will not catch but a load test will. Keep the handler's concurrency counter atomic and run under `-race`.

## Resources

- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — the in-process server used to observe downstream concurrency without an external dependency.
- [net/http: Client and request cancellation](https://pkg.go.dev/net/http#Client) — `http.NewRequestWithContext` and why bodies must be drained and closed for connection reuse.
- [golang.org/x/sync/errgroup](https://pkg.go.dev/golang.org/x/sync/errgroup) — `SetLimit`, the packaged way to bound a fan-out, mirroring this semaphore by hand.

---

Back to [02-weighted-semaphore.md](02-weighted-semaphore.md) | Next: [04-bounded-object-processing.md](04-bounded-object-processing.md)
