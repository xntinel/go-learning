# Exercise 10: Killing Hidden Globals — Inject The HTTP Client Instead Of http.DefaultClient

`http.DefaultClient` is the most common hidden global in Go backends, and one of
the most dangerous: it has no timeout, shares one transport process-wide, and can
be pointed at nothing in a test. A client that reaches for it is both untestable
and prone to hung requests. The fix is injection — take an `*http.Client` or a
narrow `Doer` in the constructor — which enables `httptest`-based tests and
per-caller timeouts.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
apiclient/                  independent module: example.com/apiclient
  go.mod                    module example.com/apiclient
  client.go                 Doer interface; Client taking an injected Doer; GetUser
  cmd/
    demo/
      main.go               httptest.Server-backed client fetches a user
  client_test.go            httptest server; timeout honored; sentinel Doer proves no DefaultClient fallback
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: a `Doer` interface (`Do(*http.Request) (*http.Response, error)`); a `Client` that stores an injected `Doer` and a base URL; `GetUser(ctx, id)` building a request with `http.NewRequestWithContext` and parsing the JSON response.
- Test: an `httptest.Server`-backed `*http.Client` asserts request shaping and response parsing; a configured `Client.Timeout` is honored; a sentinel `Doer` proves the client uses the injected client and never falls back to `http.DefaultClient`.
- Verify: `go test -count=1 -race ./...`

### Why the default client is a hazard

`http.DefaultClient` is a package-level `*http.Client` with a zero `Timeout`. That
single fact is a production incident waiting to happen: a request to a downstream
that stops responding will block the calling goroutine forever, and under load the
blocked goroutines pile up until the process exhausts memory or connections. It
also shares one `Transport` — and thus one connection pool — across every unrelated
caller in the process, so one component's misconfiguration leaks into another's. And
because it is a global reached by name, a test cannot point it at a fake server
without mutating shared state that bleeds into other tests.

Injection dissolves all three problems. The `Client` takes a `Doer` — the one
method it needs, `Do(*http.Request) (*http.Response, error)`, which `*http.Client`
satisfies. In production the composition root injects an `*http.Client` with an
explicit `Timeout` and a tuned `Transport`; the timeout now lives at the seam where
it belongs and is set per caller. In tests, the seam accepts either an
`httptest.Server`'s client (a real client wired to an in-process server, so the full
HTTP round-trip is exercised with no network) or a hand-written `Doer` that returns
a canned response with no server at all. The `Doer` interface is narrower than
`*http.Client` on purpose: the client depends only on the ability to execute a
request, so a test fake is three lines.

`GetUser` is ordinary client code once the seam is in place: build the request with
`http.NewRequestWithContext` so the caller's context (and its cancellation and
deadline) flows into the request, call `c.doer.Do`, check the status, and decode the
JSON body. Nowhere does it name `http.DefaultClient` or `http.Get` — those are the
package-level conveniences that reintroduce the global. The sentinel-Doer test pins
exactly this: it injects a Doer that records it was called and asserts the client
went through it, which is only possible because the client has no fallback to the
default.

Create `client.go`:

```go
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Doer is the narrow HTTP seam: exactly the one method Client needs.
// *http.Client satisfies it, and so does a three-line test fake.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// User is the decoded response entity.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Client calls a downstream user API through an injected Doer. It never
// references http.DefaultClient.
type Client struct {
	base string
	doer Doer
}

// NewClient injects the Doer (typically a configured *http.Client) and the base
// URL. A nil Doer is a programming error, so it fails fast.
func NewClient(base string, doer Doer) (*Client, error) {
	if doer == nil {
		return nil, fmt.Errorf("apiclient: doer is required")
	}
	return &Client{base: base, doer: doer}, nil
}

// GetUser fetches a user by id. The caller's context flows into the request, so
// cancellation and deadlines are honored end to end.
func (c *Client) GetUser(ctx context.Context, id string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/users/"+id, nil)
	if err != nil {
		return User{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.doer.Do(req)
	if err != nil {
		return User{}, fmt.Errorf("get user %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("get user %s: status %d", id, resp.StatusCode)
	}

	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return User{}, fmt.Errorf("decode user %s: %w", id, err)
	}
	return u, nil
}
```

### The runnable demo

The demo stands up an `httptest.Server` that serves a user, injects the server's
own client (a configured `*http.Client`), and fetches through the real HTTP stack —
no external network, no `http.DefaultClient`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/apiclient"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","name":"Alice"}`))
	}))
	defer srv.Close()

	client, err := apiclient.NewClient(srv.URL, srv.Client())
	if err != nil {
		fmt.Println("new client:", err)
		return
	}

	u, err := client.GetUser(context.Background(), "u1")
	if err != nil {
		fmt.Println("get user:", err)
		return
	}
	fmt.Printf("id=%s name=%s\n", u.ID, u.Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=u1 name=Alice
```

### Tests

The three tests cover the three payoffs of injecting the client. `TestGetUserParses`
uses an `httptest.Server` to assert both request shaping (the server sees
`/users/u1`) and response parsing. `TestTimeoutHonored` injects an `*http.Client`
with a tiny `Timeout` against a slow server and asserts the request fails rather
than hanging. `TestUsesInjectedDoerNotDefault` injects a recording `Doer` that
returns a canned response and asserts the client went through it — proof there is no
`http.DefaultClient` fallback.

Create `client_test.go`:

```go
package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetUserParses(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"u1","name":"Alice"}`)
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	u, err := client.GetUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if gotPath != "/users/u1" {
		t.Fatalf("server saw path %q, want /users/u1", gotPath)
	}
	if u.ID != "u1" || u.Name != "Alice" {
		t.Fatalf("user = %+v, want {u1 Alice}", u)
	}
}

func TestTimeoutHonored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	// A configured client with a tight timeout, injected at the seam.
	hc := &http.Client{Timeout: 10 * time.Millisecond}
	client, err := NewClient(srv.URL, hc)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.GetUser(context.Background(), "u1"); err == nil {
		t.Fatal("GetUser: expected a timeout error, got nil")
	}
}

// recordingDoer returns a canned response and records that it was called,
// proving the client uses the injected Doer rather than http.DefaultClient.
type recordingDoer struct {
	called bool
}

func (d *recordingDoer) Do(*http.Request) (*http.Response, error) {
	d.called = true
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":"u1","name":"Bob"}`)),
		Header:     make(http.Header),
	}, nil
}

func TestUsesInjectedDoerNotDefault(t *testing.T) {
	t.Parallel()

	doer := &recordingDoer{}
	client, err := NewClient("http://unused.invalid", doer)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	u, err := client.GetUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !doer.called {
		t.Fatal("client did not use the injected Doer")
	}
	if u.Name != "Bob" {
		t.Fatalf("user name = %q, want Bob (from the injected Doer)", u.Name)
	}
}
```

## Review

The client is correct when it executes every request through the injected `Doer`
and names `http.DefaultClient` nowhere. `TestGetUserParses` proves the real HTTP
round-trip works against an in-process server; `TestTimeoutHonored` proves the
per-caller timeout set at the seam is enforced; `TestUsesInjectedDoerNotDefault`
proves there is no hidden fallback to the global. The mistakes to avoid are calling
`http.Get`/`http.DefaultClient` inside the client (no timeout, untestable) and
widening the seam to the full `*http.Client` when a one-method `Doer` is all the
client uses. Run `go test -race` to confirm the client is race-free across parallel
tests — which it is precisely because it holds no shared global. This closes the
lesson: every hidden global, from the clock to the logger to the HTTP client, is a
seam waiting to be injected.

## Resources

- [net/http.Client](https://pkg.go.dev/net/http#Client) — the `Timeout` field and why `DefaultClient` has none.
- [http.NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) — carrying the caller's context into the request.
- [net/http/httptest.Server](https://pkg.go.dev/net/http/httptest#Server) — an in-process server and its configured `Client` for hermetic tests.

---

Back to [09-health-check-aggregator.md](09-health-check-aggregator.md) | Next: [../11-mock-interfaces-for-testing/00-concepts.md](../11-mock-interfaces-for-testing/00-concepts.md)
