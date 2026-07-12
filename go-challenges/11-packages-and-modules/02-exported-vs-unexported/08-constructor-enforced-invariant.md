# Exercise 8: Make the Zero Value Illegal: Force Callers Through New

Some types are useful at their zero value (`sync.Mutex`, `bytes.Buffer`); others must
be built through `New` because a required dependency lives in an unexported field with
no exported setter, so the zero value is deliberately unusable. This exercise builds an
API `Client` of the second kind: its base URL and HTTP client are unexported and only
`New` can set them, so a misused zero value fails with a clear guard instead of a
nil-panic deep in production.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
apiclient/                 independent module: example.com/apiclient
  go.mod                   go 1.26
  apiclient.go             Client with unexported baseURL/http; New validates; Health guards zero value
  cmd/
    demo/
      main.go              spins an httptest server, builds via New, calls Health
  apiclient_test.go        package apiclient_test: New works, zero value guarded, New validates input
```

- Files: `apiclient.go`, `cmd/demo/main.go`, `apiclient_test.go`.
- Implement: a `Client` whose required dependencies (`baseURL`, `http`) are unexported with no exported setter; `New(baseURL, *http.Client) (*Client, error)` validates and is the only construction path; a `Health` method that returns `ErrNotConstructed` if the zero value is used.
- Test: prove a `New`-built client works (against an `httptest` server), prove the zero value is guarded, and prove `New` rejects invalid input; document that the type cannot be usefully built from a struct literal in the test package.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/11-packages-and-modules/02-exported-vs-unexported/08-constructor-enforced-invariant/cmd/demo
cd go-solutions/11-packages-and-modules/02-exported-vs-unexported/08-constructor-enforced-invariant
go mod edit -go=1.26
```

### When the zero value should be illegal

Zero-value design is a real choice with two right answers depending on the type. A
`sync.Mutex`, a `bytes.Buffer`, and a `strings.Builder` are all useful with no
constructor: `var mu sync.Mutex` is a ready-to-use unlocked mutex, and that is a feature,
it removes a `New` from every caller's path and there is no required dependency to
supply. Pick zero-value-usable when the zero state is meaningful and nothing is required.

An API client is the opposite. It cannot function without a base URL and an HTTP client,
and those are dependencies the caller must provide. If we made them exported fields, a
caller could build a `Client{}` or a `Client{baseURL: "x"}`, forget the HTTP client, and
get a nil-pointer panic on the first request, far from the mistake and hard to trace. So
we keep `baseURL` and `http` unexported with no exported setter: the only way to obtain a
usable `Client` is `New`, which validates that `baseURL` is non-empty and defaults the
HTTP client if nil. A `Client{}` obtained some other way (a zero value, an accidental
struct literal inside the package) is guarded: `Health` checks its own fields and returns
`ErrNotConstructed` rather than dereferencing a nil client. That converts a class of
misuse from a production nil-panic into either a compile-time obstacle (you cannot set the
unexported fields from another package) or an explicit, early, well-defined error.

The guard is coherent, not paranoid: it checks exactly the invariant `New` establishes
(non-empty URL, non-nil HTTP client), returns a documented sentinel, and never panics.
That is the difference between "the zero value is illegal" done well and a scattering of
nil-checks.

Create `apiclient.go`:

```go
package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrNotConstructed guards the illegal zero value: a Client not built by New has
// no baseURL and no http client, and Health reports this instead of panicking.
var ErrNotConstructed = errors.New("apiclient: Client must be created with New")

// Client's required dependencies are unexported with no exported setter, so the
// only way to a usable value is New. The zero value is deliberately unusable.
type Client struct {
	baseURL string
	http    *http.Client
}

// New is the only construction path. It validates the base URL and defaults the
// HTTP client, so a returned *Client is always usable.
func New(baseURL string, hc *http.Client) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("apiclient: baseURL is required")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: baseURL, http: hc}, nil
}

// Health calls GET {baseURL}/health. It guards the zero value first: a Client not
// built by New fails with ErrNotConstructed rather than dereferencing nil.
func (c *Client) Health(ctx context.Context) (string, error) {
	if c == nil || c.baseURL == "" || c.http == nil {
		return "", ErrNotConstructed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("apiclient: health status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
```

### The runnable demo

The demo stands up an in-process `httptest` server so the run is self-contained and
deterministic, builds a client with `New`, and calls `Health`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/apiclient"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, "ok")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, srv.Client())
	if err != nil {
		panic(err)
	}
	status, err := c.Health(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Printf("health: %s\n", status)

	// The zero value is guarded, not a panic.
	var zero apiclient.Client
	_, err = zero.Health(context.Background())
	fmt.Printf("zero value: %v\n", errors.Is(err, apiclient.ErrNotConstructed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
health: ok
zero value: true
```

### Tests

The black-box test builds a client with `New` against an `httptest` server and asserts
`Health` returns the body, proving the constructed value works. `TestZeroValueGuarded`
proves a zero-value `Client` returns `ErrNotConstructed` rather than panicking.
`TestNewRejectsEmptyURL` proves the constructor validates. The commented block records
that the unexported fields cannot be set from a struct literal in the test package, so
`New` really is the only usable path.

Create `apiclient_test.go`:

```go
package apiclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/apiclient"
)

func TestNewClientWorks(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != "ok" {
		t.Fatalf("Health = %q, want ok", got)
	}
}

func TestZeroValueGuarded(t *testing.T) {
	t.Parallel()

	var c apiclient.Client // zero value: never went through New
	if _, err := c.Health(context.Background()); !errors.Is(err, apiclient.ErrNotConstructed) {
		t.Fatalf("zero value err = %v, want ErrNotConstructed", err)
	}
}

func TestNewRejectsEmptyURL(t *testing.T) {
	t.Parallel()

	if _, err := apiclient.New("", nil); err == nil {
		t.Fatal("New with empty baseURL should error")
	}
}

// The unexported fields cannot be set from a struct literal outside the package,
// so New is the only path to a usable Client:
//
//	c := apiclient.Client{baseURL: "http://x"}  // unknown field baseURL (or: cannot refer to unexported field)
//
// A bare apiclient.Client{} compiles but is guarded by ErrNotConstructed.
```

## Review

The type is correct when a `New`-built client succeeds against the `httptest` server,
when a zero-value `Client` returns `ErrNotConstructed` instead of panicking, and when
`New` rejects an empty base URL. The design decision to make the zero value illegal is
appropriate precisely because the client has a required dependency (the HTTP transport and
target) with no sensible default, contrast `sync.Mutex`, whose zero value is intentionally
usable because it has no required dependency. Keeping `baseURL` and `http` unexported with
no exported setter means a caller in another package cannot half-construct the type via a
struct literal, so the failure mode is either a compile error or a clear early guard, not
a nil-panic in production. The guard checks exactly the invariant `New` establishes, which
is what keeps it coherent rather than a scattering of defensive nil-checks.

## Resources

- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — the in-process server used to test the client without a network.
- [`http.NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — the request constructor the client uses.
- [Effective Go: Allocation with `new`](https://go.dev/doc/effective_go#allocation_new) — how to design a type whose zero value is useful, and when a constructor is required instead.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the `ErrNotConstructed` guard sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-sentinel-errors-and-error-type.md](07-sentinel-errors-and-error-type.md) | Next: [09-embedding-and-api-surface-leak.md](09-embedding-and-api-surface-leak.md)
