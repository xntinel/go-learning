# Exercise 6: Catch a data race and control the test cache

Concurrent fan-out — check N URLs at once and tally the results — is code a senior
backend engineer writes constantly, and it is exactly where a data race hides
until production. This module writes the racy version, sees the detector catch it,
fixes it with atomics, and explains why `-race` and `-count=1` belong in CI.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
fanout/                     independent module: example.com/fanout
  go.mod                    go 1.26
  fanout.go                 Client, Summary, CheckAll (race-free with atomics)
  cmd/demo/main.go          runnable fan-out over an in-process server
  fanout_test.go            correctness across many iterations, race-clean
```

Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
Implement: `CheckAll(ctx, client, urls)` that checks every URL concurrently and
returns a `Summary{OK, Failed}`, accumulating counts with `sync/atomic` (not a
bare shared map).
Test: a correctness test that runs the fan-out over many iterations and asserts
the tallies always sum to `len(urls)`, passing under `go test -race -count=1`.
Verify: `go test -count=1 -race ./...` must pass; `gofmt -l` empty.

Set up the module:

```bash
mkdir -p ~/go-exercises/fanout/cmd/demo
cd ~/go-exercises/fanout
go mod init example.com/fanout
```

### The bug the race detector exists to find

The obvious way to tally results from N goroutines is to write into a shared
variable. Here is the version that looks fine and is broken:

```go
// BROKEN: unsynchronized writes to shared ints from many goroutines.
func CheckAll(ctx context.Context, client Client, urls []string) Summary {
	var s Summary
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if healthy(ctx, client, u) {
				s.OK++ // data race: read-modify-write from many goroutines
			} else {
				s.Failed++ // data race
			}
		}()
	}
	wg.Wait()
	return s
}
```

`s.OK++` is a read-modify-write. Two goroutines can read the same value, each add
one, and each write back — losing an increment. The result is a tally that is
*usually* right and occasionally low, which is the worst kind of bug: it passes
casual testing and corrupts data under load. The compiler accepts it. A serial
test passes. Run it under the detector, though, and:

```bash
go test -race ./...
# ==================
# WARNING: DATA RACE
# Write at 0x... by goroutine 9:
#   example.com/fanout.CheckAll.func1 ...
# ==================
# FAIL
```

The race detector is a *runtime* instrument: it flags a race only on a path that
actually executes concurrently during the test. That is why the test must
exercise the concurrent path, and why `-race` in CI is non-negotiable — a race
that never ran under `-race` ships.

### The fix: atomic counters

The correct version accumulates into `atomic.Int64` counters, whose `Add` is a
single indivisible operation, then reads them once after `wg.Wait()`:

Create `fanout.go`:

```go
package fanout

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// Client is the one method of *http.Client that CheckAll needs.
type Client interface {
	Do(*http.Request) (*http.Response, error)
}

// Summary is the aggregate outcome of a fan-out check.
type Summary struct {
	OK     int
	Failed int
}

// CheckAll checks every url concurrently and returns the tally. Counts are
// accumulated with atomic operations, so the fan-out is race-free.
func CheckAll(ctx context.Context, client Client, urls []string) Summary {
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if healthy(ctx, client, u) {
				ok.Add(1)
			} else {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	return Summary{OK: int(ok.Load()), Failed: int(failed.Load())}
}

func healthy(ctx context.Context, client Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode < 500
}
```

`atomic.Int64` (Go 1.19+) is the typed atomic: `ok.Add(1)` and `ok.Load()` need
no mutex and cannot lose an increment. A `sync.Mutex` around a plain `int` would
be equally correct; atomics are the lighter choice for a pure counter.

### The runnable demo

The demo fans out over an in-process server that returns `200` for every request,
so the tally is deterministic.

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

	urls := make([]string, 5)
	for i := range urls {
		urls[i] = srv.URL
	}

	s := fanout.CheckAll(context.Background(), srv.Client(), urls)
	fmt.Printf("ok=%d failed=%d\n", s.OK, s.Failed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok=5 failed=0
```

### Tests

The correctness test runs the fan-out many times and asserts the tallies always
sum to the number of URLs and match the expected split. Because it runs under
`-race`, a regression to unsynchronized counters would trip the detector; because
it runs many iterations, a lost increment is caught by the sum invariant even if
the detector somehow missed the interleaving.

Create `fanout_test.go`:

```go
package fanout

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckAllTally(t *testing.T) {
	t.Parallel()

	// Half the servers return 200 (ok), half return 503 (failed).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okSrv.Close)
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(badSrv.Close)

	urls := []string{okSrv.URL, badSrv.URL, okSrv.URL, badSrv.URL, okSrv.URL}
	const wantOK, wantFailed = 3, 2

	for i := range 50 {
		s := CheckAll(t.Context(), okSrv.Client(), urls)
		if s.OK+s.Failed != len(urls) {
			t.Fatalf("iter %d: OK+Failed = %d, want %d (lost an increment?)", i, s.OK+s.Failed, len(urls))
		}
		if s.OK != wantOK || s.Failed != wantFailed {
			t.Fatalf("iter %d: got OK=%d Failed=%d, want OK=%d Failed=%d", i, s.OK, s.Failed, wantOK, wantFailed)
		}
	}
}

func TestCheckAllEmpty(t *testing.T) {
	t.Parallel()
	s := CheckAll(t.Context(), http.DefaultClient, nil)
	if s.OK != 0 || s.Failed != 0 {
		t.Fatalf("CheckAll(nil) = %+v, want zero", s)
	}
}
```

The client passed in the tally test is `okSrv.Client()`, but because each URL is
absolute, the client's base does not matter; both servers are reached by their own
absolute URLs. On the caching point: run the suite twice and the second prints
`ok  example.com/fanout  (cached)`. Add `-count=1` and it executes again — proof
the cache serves unchanged results and that `-count=1` forces a real run
(`go clean -testcache` clears the cache entirely).

## Review

The fan-out is correct when the tally is a pure function of the responses no
matter how the goroutines interleave: `OK + Failed == len(urls)` on every
iteration, achieved because `atomic.Int64.Add` cannot lose an increment. The proof
is running it under `-race` across many iterations — the detector guards the
concurrency and the sum invariant guards the arithmetic. If you swap the atomics
back to a bare `s.OK++`, `go test -race` fails immediately; that failure is the
lesson.

The traps are the two CI flags. A race that never ran under `-race` is invisible,
so the race detector must exercise the concurrent path — a serial test vouches for
nothing. And a green `go test` may be a cache hit, not a run; when you need to
observe real execution, use `-count=1`. Both belong in the CI command, together.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` works and what it catches.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and its `Add`/`Load`.
- [cmd/go: test caching](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-count`, and why results are cached.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — waiting for a fan-out to complete.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-error-contract-is-as-join.md](05-error-contract-is-as-join.md) | Next: [07-benchmarks-and-fuzzing.md](07-benchmarks-and-fuzzing.md)
