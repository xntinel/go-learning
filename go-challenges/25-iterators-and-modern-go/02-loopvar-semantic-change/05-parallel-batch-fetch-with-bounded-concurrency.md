# Exercise 5: Parallel Batch Fetch with Bounded Concurrency

A real batch job rarely launches one unbounded goroutine per item: it fetches N resources in parallel but caps how many run at once, captures the first failure, and cancels the rest. This exercise builds that job against an injected client, uses per-iteration capture to keep each fetch tied to its own URL, bounds concurrency with a semaphore, aggregates errgroup-style with a shared cancel, and proves all of it under the race detector.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs with only the standard library, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
fetch.go             Client, Result, FetchAll, ErrNoURLs, ErrInvalidLimit
cmd/
  demo/
    main.go          a fake client, fetch four urls with limit 2, print bodies
fetch_test.go        bounded-concurrency + alignment + error + validation, race-clean
example_test.go      ExampleFetchAll with a verified // Output block
```

- Files: `fetch.go`, `cmd/demo/main.go`, `fetch_test.go`, `example_test.go`.
- Implement: `FetchAll(context.Context, Client, []string, int) ([]Result, error)` that launches one goroutine per url capturing its own index and url, bounds in-flight work to `limit` with a semaphore channel, writes each body into its own results slot, and on the first error records it once and cancels the rest; plus the `Client` interface, the `Result` type, and the sentinels `ErrNoURLs` and `ErrInvalidLimit`.
- Test: assert results stay aligned with the input order, that observed in-flight concurrency never exceeds the limit, that a failing url surfaces a wrapped error, and that empty input and a non-positive limit return their sentinels — all under `-race`.
- Verify: `go test -count=1 -race ./...`

### The three jobs an errgroup-style fetch has to do at once

A batch fetcher has to solve three problems together, and the loop variable sits under all of them. First, **per-item correctness**: each goroutine must call the client with *its* url and store the body in *its* slot. The range loop `for i, url := range urls` gives each iteration its own `i` and `url` under Go 1.22, so the closure that runs later still refers to the right values; before 1.22 this needed `i, url := i, url` and was the single most common source of "all my fetches hit the last url" bugs. Second, **bounded concurrency**: launching ten thousand goroutines to fetch ten thousand urls exhausts sockets and memory, so a buffered channel used as a counting semaphore caps the number in flight — a goroutine acquires a slot by sending into `sem` before it works and releases it by receiving when done, and the main loop blocks on the send once `limit` slots are taken. Third, **fail-fast aggregation**: when one fetch fails, the job should stop launching useful work and report that error, which is exactly what `golang.org/x/sync/errgroup` with `WithContext` and `SetLimit` does; here it is built from the standard library so the module stays dependency-free, but the shape is identical — a shared `context.CancelFunc`, a `sync.Once` to record the first error, and goroutines that check `ctx.Err()` and bail.

The per-iteration capture is what lets these three mechanisms compose without a single `:=` copy. Each goroutine closes over its own `i` and `url`; the semaphore controls *how many* of those goroutines run at once but never changes *which* url each one holds; and the cancel path only decides *whether* a goroutine does its work, again without disturbing the captured value. If the capture were the old shared-variable kind, no amount of careful semaphore or context code would save you — every goroutine would fetch the loop's final url and write the final index. The `-race` test makes the claim concrete: it injects a client that counts how many calls are in flight simultaneously, asserts that count never exceeds `limit`, asserts the bodies line up with the urls, and runs under `-race` so any slip in the per-slot writes is caught.

`FetchAll` keeps results positional — `results[i]` is the body for `urls[i]` — so the output is deterministic without sorting and a caller can zip results back to inputs by index. On the first failure it wraps the error with the offending url, records it under a `sync.Once`, and calls `cancel`, so goroutines not yet started observe `ctx.Err()` and return immediately; `wg.Wait` then drains the in-flight ones before the function reads the recorded error. The semaphore is released with a deferred receive so a panicking or early-returning goroutine never leaks its slot.

Create `fetch.go`:

```go
package loopvar

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNoURLs is returned when there is nothing to fetch.
var ErrNoURLs = errors.New("urls must not be empty")

// ErrInvalidLimit is returned when the concurrency limit is not positive.
var ErrInvalidLimit = errors.New("concurrency limit must be positive")

// Client is the downstream HTTP-like dependency. It is an interface so tests
// can inject a fake that counts in-flight calls or fails a chosen url.
type Client interface {
	Get(ctx context.Context, url string) (string, error)
}

// Result is the body fetched for one url, kept positional with the input.
type Result struct {
	URL  string
	Body string
}

// FetchAll fetches every url in parallel, capping the number of in-flight
// requests at limit. Each goroutine captures its own iteration index and url
// (per-iteration scope under go 1.22), writes its body into its own results
// slot, and on the first error records it once and cancels the rest, so the job
// fails fast. It is race-free under go test -race and results stay aligned with
// the input order.
func FetchAll(ctx context.Context, client Client, urls []string, limit int) ([]Result, error) {
	if len(urls) == 0 {
		return nil, ErrNoURLs
	}
	if limit <= 0 {
		return nil, ErrInvalidLimit
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]Result, len(urls))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i, url := range urls {
		sem <- struct{}{} // blocks once limit slots are taken
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			body, err := client.Get(ctx, url)
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("fetch %s: %w", url, err)
					cancel()
				})
				return
			}
			results[i] = Result{URL: url, Body: body}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}
```

### The runnable demo

The demo injects a fake client that echoes each url into a body, fetches four urls with a limit of two, and prints the bodies in input order. Because results are positional rather than collected in completion order, the output is deterministic and safe to pin.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/loop-batch-fetch"
)

// echoClient is a stand-in for a real HTTP client.
type echoClient struct{}

func (echoClient) Get(ctx context.Context, url string) (string, error) {
	return "body:" + url, nil
}

func main() {
	urls := []string{"a", "b", "c", "d"}
	results, err := loopvar.FetchAll(context.Background(), echoClient{}, urls, 2)
	if err != nil {
		fmt.Println("fetch error:", err)
		return
	}
	for _, r := range results {
		fmt.Println(r.Body)
	}
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```
body:a
body:b
body:c
body:d
```

The `-race` flag is the point: at most two goroutines run at once, each writes only its own slot, and the positional bodies prove each fetch carried its own url.

### Tests

The tests run under `-race`. `TestFetchAllBoundsConcurrency` injects a client that tracks the maximum number of simultaneous in-flight calls and asserts it never exceeds the limit while every url is fetched once and the bodies line up. `TestFetchAllSurfacesError` fails one url and checks the wrapped error names it. `TestFetchAllValidation` pins both sentinels. The parallel subtests capture `tc` directly, with no `tc := tc` copy, relying on the per-iteration scope this chapter is about.

Create `fetch_test.go`:

```go
package loopvar

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingClient records the peak number of simultaneous in-flight calls and
// how many times each url was fetched, so a test can prove the limit holds.
type countingClient struct {
	mu        sync.Mutex
	inFlight  int
	maxFlight int
	seen      map[string]int
}

func (c *countingClient) Get(ctx context.Context, url string) (string, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.maxFlight {
		c.maxFlight = c.inFlight
	}
	c.seen[url]++
	c.mu.Unlock()

	time.Sleep(time.Millisecond) // widen the window so overlap is observable

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()

	return "body:" + url, nil
}

func TestFetchAllBoundsConcurrency(t *testing.T) {
	t.Parallel()

	client := &countingClient{seen: make(map[string]int)}
	urls := []string{"a", "b", "c", "d", "e", "f"}
	limit := 2

	got, err := FetchAll(context.Background(), client, urls, limit)
	if err != nil {
		t.Fatal(err)
	}

	if client.maxFlight > limit {
		t.Fatalf("max in-flight = %d, want <= %d", client.maxFlight, limit)
	}
	for i, url := range urls {
		want := Result{URL: url, Body: "body:" + url}
		if got[i] != want {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], want)
		}
		if client.seen[url] != 1 {
			t.Fatalf("url %q fetched %d times, want 1", url, client.seen[url])
		}
	}
}

func TestFetchAllAlignment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		urls  []string
		limit int
	}{
		{name: "limit-one", urls: []string{"x", "y", "z"}, limit: 1},
		{name: "limit-equals-len", urls: []string{"p", "q"}, limit: 2},
		{name: "limit-exceeds-len", urls: []string{"m"}, limit: 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &countingClient{seen: make(map[string]int)}
			got, err := FetchAll(context.Background(), client, tc.urls, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			bodies := make([]string, len(got))
			for i, r := range got {
				bodies[i] = r.Body
			}
			want := make([]string, len(tc.urls))
			for i, u := range tc.urls {
				want[i] = "body:" + u
			}
			if !reflect.DeepEqual(bodies, want) {
				t.Fatalf("bodies = %v, want %v", bodies, want)
			}
		})
	}
}

// erroringClient fails the single configured url and echoes the rest.
type erroringClient struct {
	badURL string
}

func (c erroringClient) Get(ctx context.Context, url string) (string, error) {
	if url == c.badURL {
		return "", errors.New("503 service unavailable")
	}
	return "body:" + url, nil
}

func TestFetchAllSurfacesError(t *testing.T) {
	t.Parallel()

	_, err := FetchAll(context.Background(), erroringClient{badURL: "c"}, []string{"a", "b", "c", "d"}, 2)
	if err == nil {
		t.Fatal("FetchAll error = nil, want an error for url c")
	}
	if !strings.Contains(err.Error(), "fetch c") {
		t.Fatalf("error %q does not mention the failing url", err.Error())
	}
}

func TestFetchAllValidation(t *testing.T) {
	t.Parallel()

	if _, err := FetchAll(context.Background(), erroringClient{}, nil, 2); !errors.Is(err, ErrNoURLs) {
		t.Fatalf("empty urls error = %v, want ErrNoURLs", err)
	}
	if _, err := FetchAll(context.Background(), erroringClient{}, []string{"a"}, 0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("zero limit error = %v, want ErrInvalidLimit", err)
	}
}
```

Create `example_test.go`:

```go
package loopvar

import (
	"context"
	"fmt"
)

type staticClient struct{}

func (staticClient) Get(ctx context.Context, url string) (string, error) {
	return "body:" + url, nil
}

func ExampleFetchAll() {
	results, _ := FetchAll(context.Background(), staticClient{}, []string{"a", "b"}, 2)
	for _, r := range results {
		fmt.Println(r.Body)
	}
	// Output:
	// body:a
	// body:b
}
```

## Review

The fetch is correct when, run under `go test -race`, it reports no data race, the bodies stay aligned with the urls, every url is fetched exactly once, and the observed peak concurrency never exceeds the limit. Three independent properties have to hold together: per-iteration capture keeps each goroutine tied to its own url and slot, the semaphore caps in-flight work, and the once-guarded cancel makes the job fail fast. The race detector validates the first; the in-flight counter validates the second; the error test validates the third. Each rests on Go 1.22 giving the loop its own `i` and `url` per iteration — compile the same code in a `go 1.21` module and the goroutines would race on the shared variables and the positional writes would scramble.

The common mistakes are three. Forgetting the semaphore release on the error path leaks a slot and eventually deadlocks the loop — the deferred `<-sem` prevents that. Recording every error instead of the first turns a single failure into a noisy join and races on the error variable — the `sync.Once` keeps it to one. And reaching for `i, url := i, url` out of habit is now redundant in a `go 1.26` module; the alignment and bounded-concurrency tests exist to show the copy-free form is correct. If you need the same pattern in production, `golang.org/x/sync/errgroup` with `SetLimit` packages exactly this — a bounded, context-cancelling, first-error group — but the standard-library build here shows there is no magic underneath.

## Resources

- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why per-iteration scope makes parallel loops over urls correct without a copy.
- [errgroup package](https://pkg.go.dev/golang.org/x/sync/errgroup) — the production form of bounded, context-cancelling, first-error aggregation this exercise rebuilds.
- [context package](https://pkg.go.dev/context) — how `WithCancel` propagates cancellation to in-flight and not-yet-started fetches.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` instruments and what a reported race means.

---

Back to [04-fan-out-to-a-downstream-service.md](04-fan-out-to-a-downstream-service.md) | Next: [../03-range-over-func-push-iterators/00-concepts.md](../03-range-over-func-push-iterators/00-concepts.md)
