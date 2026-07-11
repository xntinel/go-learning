# Exercise 6: Lazy Singletons with sync.OnceValue Instead of init()

When a shared resource is expensive to build but should not be paid for at import
time, `sync.OnceValue` is the modern tool: the factory runs at most once, even
under concurrent callers, and the result is skippable — a test that never touches
the singleton never builds it. This exercise contrasts that lazy path with an
`init()`-built global.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
lazyclient/                  independent module: example.com/lazyclient
  go.mod                     module example.com/lazyclient
  client.go                  sync.OnceValue-backed *http.Client singleton + build counter
  cmd/demo/main.go           calls Client() a few times, shows one build
  client_test.go             concurrent callers -> exactly one build, same pointer; -race
```

Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
Implement: a lazily-built `*http.Client` with a tuned `*http.Transport`, constructed through `sync.OnceValue` so the factory runs exactly once on first use.
Test: many concurrent callers see the factory run exactly once (atomic counter) and get the same pointer; the singleton is never built if never called.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lazyclient/cmd/demo
cd ~/go-exercises/lazyclient
go mod init example.com/lazyclient
```

### Why lazy beats an init()-built global

Consider a shared `*http.Client` with a carefully tuned `*http.Transport`
(connection pool sizes, timeouts). The naive approach is a package-level
`var Client = buildClient()` or a `func init() { client = buildClient() }`. Both run
at import time, unconditionally: every binary that imports the package — including a
unit test that never makes an HTTP call, and a linter or code generator that only
needs the package's types — pays to construct the transport. Worse, the global is
frozen at import time and cannot be reset or substituted by a test.

`sync.OnceValue` fixes both problems. `sync.OnceValue(f)` returns a function that
runs `f` at most once and caches its single return value; every later call returns
that cached value without re-running `f`. It is safe under concurrent access — if a
thousand goroutines call it simultaneously, exactly one runs the factory and the
rest block until the value is ready — and it propagates a panic from `f` to all
callers. The result: the transport is built on the first `Client()` call, not at
import; a test that never calls `Client()` never builds it; and the "build exactly
once under concurrency" guarantee is the library's job, not yours.

The typed `Once*` helpers (Go 1.21) are the point. Before them you wrote a
`sync.Once` plus a package var plus a nil check, and it was easy to get the memory
visibility or the double-check wrong. `sync.OnceValue` (single value),
`sync.OnceValues` (value plus error), and `sync.OnceFunc` (side effect only) encode
the correct pattern once. Reach for them instead of hand-rolling.

An atomic build counter lets the test *prove* the factory ran once. In production
you would not keep the counter; here it is the observable that turns "runs once"
from a claim into an assertion.

Create `client.go`:

```go
// client.go
package lazyclient

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// builds counts how many times the factory ran. Production code would not keep
// this; the test uses it to prove the factory runs exactly once.
var builds atomic.Int64

// Builds reports how many times the client factory has executed.
func Builds() int64 { return builds.Load() }

// client is the lazily-built singleton. sync.OnceValue guarantees buildClient
// runs at most once, on first call, even under concurrent callers.
var client = sync.OnceValue(buildClient)

func buildClient() *http.Client {
	builds.Add(1)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Client returns the shared HTTP client, building it on first use. The cost is
// paid lazily, not at import time, so a caller that never needs it never pays.
func Client() *http.Client { return client() }
```

### The runnable demo

The demo calls `Client()` several times and prints the build count, showing that
repeated calls share one instance built exactly once.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/lazyclient"
)

func main() {
	fmt.Println("builds before first use:", lazyclient.Builds())

	a := lazyclient.Client()
	b := lazyclient.Client()
	c := lazyclient.Client()

	fmt.Println("same instance:", a == b && b == c)
	fmt.Println("builds after 3 calls:", lazyclient.Builds())
	fmt.Println("timeout:", a.Timeout)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
builds before first use: 0
same instance: true
builds after 3 calls: 1
timeout: 10s
```

### Tests

`TestBuiltExactlyOnceConcurrently` fans many goroutines into `Client()` and asserts
the factory ran exactly once and every caller got the same pointer — the core
`OnceValue` guarantee, verified under `-race`. Because the counter is process-wide
and the singleton is shared, this test does not call `t.Parallel()` alongside other
tests that also touch `Client()`; instead it is the single place that observes the
build, and the pointer-identity test reads the already-built value.

Note the deliberate contrast in the comment: an `init()`-built global would show a
build count of 1 *before any test ran*, because import alone would have built it —
the lazy path lets a test observe "0 builds until first use".

Create `client_test.go`:

```go
// client_test.go
package lazyclient

import (
	"net/http"
	"sync"
	"testing"
)

// TestLazyBeforeUse asserts the singleton is not built merely by importing the
// package. With an init()-built global this would already be 1. It runs first
// (source order, no t.Parallel) so no other test has called Client() yet.
func TestLazyBeforeUse(t *testing.T) {
	if got := Builds(); got != 0 {
		t.Fatalf("Builds() = %d before any Client() call; want 0 (was it built at import?)", got)
	}
}

// TestBuiltExactlyOnceConcurrently drives many concurrent callers and proves the
// factory ran once and all callers share one pointer.
func TestBuiltExactlyOnceConcurrently(t *testing.T) {
	const n = 100
	results := make([]*http.Client, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = Client()
		}()
	}
	wg.Wait()

	if got := Builds(); got != 1 {
		t.Fatalf("Builds() = %d after %d concurrent calls; want 1", got, n)
	}
	base := Client()
	for i := range n {
		if results[i] != base {
			t.Fatalf("caller %d got a different client instance", i)
		}
	}
}
```

## Review

The lazy singleton is correct when three things hold: the factory has not run
before the first `Client()` call (`TestLazyBeforeUse` sees 0 builds), it runs
exactly once no matter how many goroutines race into it (`Builds() == 1`), and
every caller receives the same pointer. `sync.OnceValue` provides all three; your
job is only to pass it a factory. Run `go test -race` to confirm there is no data
race on the shared value — the helper's internal locking is what makes the
concurrent access safe.

The mistake to avoid is reaching back for an `init()`-built global or a hand-rolled
`sync.Once` + `bool` + value + mutex. The former pays the construction cost at
import time in every binary, including tests that never use it, and cannot be
observed as "not yet built"; the latter is easy to get subtly wrong around memory
visibility and re-entry. `sync.OnceValue`/`OnceValues`/`OnceFunc` encode the correct
lazy-singleton pattern, including panic propagation, so use them.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — run a factory at most once and cache its value (Go 1.21+).
- [sync.OnceValues and sync.OnceFunc](https://pkg.go.dev/sync#OnceValues) — the value+error and side-effect-only variants.
- [http.Transport](https://pkg.go.dev/net/http#Transport) — the tuned transport the singleton wraps.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-fail-fast-invariant-checks-at-init.md](07-fail-fast-invariant-checks-at-init.md)
