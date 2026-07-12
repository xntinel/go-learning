# Exercise 10: Suite-Level Setup with TestMain vs Per-Test t.Cleanup

Some dependencies are too expensive to build per test — a shared broker stub, a
seeded server — but must exist for the whole package. `TestMain` is where they
live: build before `m.Run`, tear down after. Per-test state still belongs in
`t.Cleanup`. This exercise draws that boundary explicitly.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
suitelc/                     independent module: example.com/suitelc
  go.mod                     go 1.24
  broker.go                  Handler (fake broker /health) + Client with Ping/CloseIdle
  cmd/
    demo/
      main.go                runnable demo: spin the handler, ping it
  broker_test.go             TestMain builds one shared server; tests add per-test cleanup
```

- Files: `broker.go`, `cmd/demo/main.go`, `broker_test.go`.
- Implement: a `Handler()` serving `/health`, and a `Client` with `Ping` and `CloseIdle`.
- Test: a `TestMain` that builds a package-shared `httptest.Server` before `m.Run` and closes it after; tests that use `t.Cleanup` for per-test state on top of it.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Scope selects the mechanism

The rule is a clean split by lifetime. A resource whose lifetime is the *whole
package* — expensive to build, shared by every test — belongs in `TestMain`: build
it before `m.Run()`, tear it down after `m.Run()` returns. A resource whose
lifetime is *one test* belongs in that test's `t.Cleanup`. The two are not
interchangeable. There is no `t.Cleanup` in `TestMain` — `TestMain` receives a
`*testing.M`, not a `*testing.T` — and its teardown runs only *after every test has
finished*, so a per-test `t.Cleanup` can never reach the shared resource to close
it. Conversely, putting a package-lifetime resource in a per-test cleanup rebuilds
it for every test (slow) or tears it down mid-suite (broken for the tests that
follow).

`TestMain` runs in the main goroutine and wraps `m.Run()`. `m.Run()` executes all
the package's tests and returns an exit code. In modern Go you do not have to call
`os.Exit` yourself: per the `testing` docs, "If TestMain returns, the test wrapper
will pass the result of m.Run to os.Exit itself." So `TestMain` can build the
shared server, capture `code := m.Run()`, close the server, and simply return —
the framework exits with the code. (The classic `os.Exit(m.Run())` form is
equally valid; the return form lets any deferred teardown run, since `os.Exit`
skips defers.)

To prove setup ran before any test, `TestMain` sets a package-level flag after
building the server, and the first test asserts it. To show the shared server
persists across tests, two tests both `Ping` it successfully and each registers a
*per-test* `t.Cleanup` (releasing that test's idle connections) — teardown that is
scoped to the test, layered on top of the suite-lifetime server that only
`TestMain` will close.

Create `broker.go`:

```go
package suitelc

import (
	"io"
	"net/http"
)

// Handler is the fake broker's HTTP surface: a single health endpoint. It stands
// in for an expensive shared dependency built once for the whole test package.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	return mux
}

// Client talks to a broker at baseURL.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client targeting baseURL.
func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{}}
}

// Ping calls /health and returns its body.
func (c *Client) Ping() (string, error) {
	resp, err := c.http.Get(c.baseURL + "/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CloseIdle releases this client's idle connections. It is a per-test-scoped
// teardown, distinct from the suite-lifetime server torn down in TestMain.
func (c *Client) CloseIdle() {
	c.http.CloseIdleConnections()
}
```

### The runnable demo

The demo spins the handler on a local server, pings it, and shuts it down — the
same shape as `TestMain`, but outside the testing framework.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http/httptest"

	"example.com/suitelc"
)

func main() {
	srv := httptest.NewServer(suitelc.Handler())
	defer srv.Close()

	c := suitelc.NewClient(srv.URL)
	got, err := c.Ping()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ping: %s\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ping: ok
```

### The tests

`TestMain` builds one `httptest.Server` for the whole package, sets `setupRan`,
runs the suite, then closes the server and returns. `TestSharedServerSetupRan`
asserts setup ran before it. `TestClientPingUsesSharedServer` and
`TestSharedServerSurvivesAcrossTests` both hit the shared server and register a
per-test `t.Cleanup` (`CloseIdle`) — proving the server persists across tests while
each test cleans up only its own state.

Create `broker_test.go`:

```go
package suitelc

import (
	"fmt"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// sharedServer is a suite-lifetime dependency: built once in TestMain, closed
// after all tests. It is deliberately NOT a per-test resource.
var (
	sharedServer *httptest.Server
	setupRan     atomic.Bool
)

func TestMain(m *testing.M) {
	// Suite setup: build the expensive shared dependency once.
	sharedServer = httptest.NewServer(Handler())
	setupRan.Store(true)

	code := m.Run()

	// Suite teardown: runs after ALL tests. No t.Cleanup exists here, and no
	// per-test t.Cleanup could reach this shared server.
	sharedServer.Close()
	if code != 0 {
		fmt.Fprintf(os.Stderr, "suite failed with code %d\n", code)
	}
	// TestMain returns; the framework passes m.Run's result to os.Exit.
}

func TestSharedServerSetupRan(t *testing.T) {
	if !setupRan.Load() {
		t.Fatal("TestMain setup did not run before tests")
	}
	if sharedServer == nil {
		t.Fatal("shared server was not built")
	}
}

func TestClientPingUsesSharedServer(t *testing.T) {
	t.Parallel()
	c := NewClient(sharedServer.URL)
	t.Cleanup(c.CloseIdle) // per-test teardown; the server itself is TestMain's job

	got, err := c.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got != "ok" {
		t.Errorf("Ping = %q, want ok", got)
	}
}

func TestSharedServerSurvivesAcrossTests(t *testing.T) {
	t.Parallel()
	// If a per-test cleanup had closed the shared server, this test would fail:
	// it proves the server's lifetime spans the whole suite, not one test.
	c := NewClient(sharedServer.URL)
	t.Cleanup(c.CloseIdle)

	got, err := c.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got != "ok" {
		t.Errorf("Ping = %q, want ok", got)
	}
}

func ExampleClient() {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	c := NewClient(srv.URL)
	got, _ := c.Ping()
	fmt.Println(got)
	// Output: ok
}
```

## Review

The boundary is correct when the shared server is built and closed only in
`TestMain` while every test scopes its own teardown to `t.Cleanup`.
`TestSharedServerSetupRan` proves setup preceded the tests via the `setupRan` flag,
and `TestSharedServerSurvivesAcrossTests` proves no per-test cleanup closed the
shared server. The mistakes to avoid: do not put a suite-lifetime resource in a
per-test `t.Cleanup` — it rebuilds per test or tears down mid-suite; and do not
expect a per-test cleanup to close the shared server, because `TestMain`'s teardown
runs only after all tests and is unreachable from `t.Cleanup`. In modern Go,
`TestMain` may capture `m.Run`'s code, tear down, and return — the framework calls
`os.Exit` with that code, and returning (rather than `os.Exit`) lets deferred
teardown run. Run `go test -race -count=1` to confirm the shared server serves the
parallel tests cleanly.

## Resources

- [`testing.M`](https://pkg.go.dev/testing#M) and the [Main section](https://pkg.go.dev/testing#hdr-Main) — package-level setup/teardown and the return-vs-`os.Exit` contract.
- [`testing.M.Run`](https://pkg.go.dev/testing#M.Run) — runs the package's tests and returns the exit code.
- [`net/http/httptest.NewServer`](https://pkg.go.dev/net/http/httptest#NewServer) — the shared server built once in `TestMain`.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — per-test teardown layered on top of the shared resource.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-leak-guard-lifo-invariant.md](09-leak-guard-lifo-invariant.md) | Next: [../13-build-tags-for-test-separation/00-concepts.md](../13-build-tags-for-test-separation/00-concepts.md)
