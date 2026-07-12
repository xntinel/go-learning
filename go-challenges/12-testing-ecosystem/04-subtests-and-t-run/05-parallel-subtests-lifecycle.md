# Exercise 5: Parallel Subtests and the Run-Returns-After-Children Guarantee

Independent endpoint checks are the textbook case for parallelism: three readiness
probes have no reason to run in sequence. But the moment you make subtests
parallel, teardown becomes subtle — the code after your loop runs *before* the
children do. This exercise runs three probes concurrently inside a wrapper
`t.Run("group", …)` and uses the guarantee that `Run` does not return until its
parallel children finish to place teardown at a deterministic point.

This module is fully self-contained: its own `go mod init`, probe, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
healthprobe/                independent module: example.com/healthprobe
  go.mod                    go 1.26
  probe.go                  func Probe(ctx, client, url) (int, error)
  cmd/
    demo/
      main.go               runnable demo: probe an in-memory endpoint
  probe_test.go             wrapper-group parallel probes; post-group teardown; -race
```

- Files: `probe.go`, `cmd/demo/main.go`, `probe_test.go`.
- Implement: `Probe(ctx context.Context, client *http.Client, url string) (int, error)`
  returning the response status code.
- Test: three `t.Parallel` children inside a `t.Run("group", …)`; assert the code
  after the group observes all three completed; close servers there.
- Verify: `go test -count=1 -race ./...`

### The pause-resume model and why the wrapper group exists

`t.Parallel()` does not start a subtest running concurrently on the spot. It
*pauses* the subtest and hands control back to the parent; the parent runs to the
end of its function, starting the remaining siblings (which also pause); and only
when the parent function returns do all the paused siblings resume together. The
practical trap: code written *after* the loop that spawns parallel children runs
while those children are still paused, so a naive `srv.Close()` on the next line
tears down the servers before a single probe has run.

The fix is the guarantee that `t.Run(name, f)` does not return until every parallel
subtest started inside `f` has completed. Wrap the parallel children in a
`t.Run("group", …)`: the parent of the parallel children is now the *group*
function, and the `t.Run("group", …)` call in the enclosing test blocks until the
children finish. The line after that call is therefore a deterministic teardown
point — every probe has run, so it is safe to close the servers and assert
coverage. (The equivalent second idiom is `t.Cleanup`, which also runs after all
subtests; the wrapper group is preferred when you want teardown at a specific
place in the test body rather than deferred to the end.)

`Probe` builds a context-carrying request so a cancelled `ctx` aborts the probe —
the readiness-check shape you want in production.

Create `probe.go`:

```go
package healthprobe

import (
	"context"
	"fmt"
	"net/http"
)

// Probe issues a GET to url and returns the response status code. It is a
// readiness check: callers treat a 2xx code as healthy. The context lets a
// caller bound or cancel the probe.
func Probe(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("probe %s: %w", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/healthprobe"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	code, err := healthprobe.Probe(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		panic(err)
	}
	fmt.Printf("readiness: %d\n", code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
readiness: 200
```

### Tests

`TestEndpoints` builds three in-memory endpoints, probes them in three parallel
children wrapped in `t.Run("group", …)`, and records each result in a
mutex-guarded map. The code after the group is reached only once all three
children have finished, so that is where the servers are closed and the coverage
assertion (all three probed, all `200`) runs. `-race` proves the concurrent map
access is properly guarded and no probe raced its server's teardown.

Create `probe_test.go`:

```go
package healthprobe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestEndpoints(t *testing.T) {
	names := []string{"auth", "billing", "search"}
	srvs := make(map[string]*httptest.Server, len(names))
	for _, n := range names {
		srvs[n] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}

	var mu sync.Mutex
	probed := make(map[string]int)

	t.Run("group", func(t *testing.T) {
		for _, n := range names {
			t.Run(n, func(t *testing.T) {
				t.Parallel()
				code, err := Probe(t.Context(), srvs[n].Client(), srvs[n].URL)
				if err != nil {
					t.Fatalf("probe %s: %v", n, err)
				}
				mu.Lock()
				probed[n] = code
				mu.Unlock()
			})
		}
	})

	// Reached only after all three parallel children have completed: the
	// deterministic teardown point. Close the servers and assert coverage here.
	for _, s := range srvs {
		s.Close()
	}
	if len(probed) != len(names) {
		t.Fatalf("probed %d endpoints, want %d", len(probed), len(names))
	}
	for _, n := range names {
		if probed[n] != http.StatusOK {
			t.Fatalf("endpoint %s status = %d, want 200", n, probed[n])
		}
	}
}

func ExampleProbe() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code, _ := Probe(context.Background(), srv.Client(), srv.URL)
	fmt.Println(code)
	// Output: 204
}
```

## Review

The correctness of the teardown depends entirely on where it sits. Because the
three probes call `t.Parallel` and are wrapped in `t.Run("group", …)`, that `Run`
call blocks until all three complete, so the server-close and coverage assertion
on the following lines see a finished group. Move those probes out of the wrapper
so they are direct parallel children of `TestEndpoints`, and the close would run
while they are still paused — a use-after-close race that `-race` would catch. The
mutex around the `probed` map is not optional either: parallel children write it
concurrently, and read-modify-write of a map is a data race without it. Run
`go test -race -count=1` and treat any report as a failure.

## Resources

- [testing.T.Parallel — pkg.go.dev](https://pkg.go.dev/testing#T.Parallel)
- [testing.T.Run — pkg.go.dev](https://pkg.go.dev/testing#T.Run)
- [Using Subtests and Sub-benchmarks — Go Blog](https://go.dev/blog/subtests)

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-shared-fixture-parallel.md](06-shared-fixture-parallel.md)
