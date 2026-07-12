# Exercise 4: Mock an Outbound API Client by Injecting http.RoundTripper

An API client's real logic is not the socket — it is building the right URL,
setting the right headers, and mapping status codes to typed errors. All of that
is testable with no network if you inject an `http.RoundTripper` into the
`*http.Client`: the test returns canned `*http.Response` values and captures the
outgoing `*http.Request`. This module builds a typed client and doubles its
transport.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
apiclient/                   independent module: example.com/apiclient
  go.mod                     go 1.26
  client.go                  Client.GetUser; ErrUserNotFound, ErrUpstream
  cmd/
    demo/
      main.go                runnable demo driven by a canned RoundTripper
  client_test.go            roundTripperFunc table cases; request-capturing spy; httptest server
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: a `Client` over an injected `*http.Client` with `GetUser(ctx, id) (User, error)`, mapping 200 to a decoded user, 404 to `ErrUserNotFound`, 5xx to `ErrUpstream`, and a malformed body to a decode error.
- Test: a `roundTripperFunc` adapter returning canned responses (200/404/500/malformed) table-driven; a spy `RoundTripper` capturing the request to assert path, method, and headers; an `httptest.NewServer` integration variant.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/11-mock-interfaces-for-testing/04-mock-http-client-roundtripper/cmd/demo
cd go-solutions/08-interfaces/11-mock-interfaces-for-testing/04-mock-http-client-roundtripper
```

### The seam: http.RoundTripper

`http.Client` delegates every request to its `Transport`, an `http.RoundTripper`
with a single method: `RoundTrip(*http.Request) (*http.Response, error)`. That one
method is the entire outbound-network seam. Instead of injecting a custom
interface, you inject a `*http.Client` whose `Transport` you control — so the
client code stays exactly what production runs (`c.http.Do(req)`), and the test
swaps the transport underneath it. This is the network analogue of injecting a
`Clock`: hoist the hidden dependency behind an interface the stdlib already
defines.

A one-line adapter turns a function into a `RoundTripper`:

```go
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
```

Now a test can hand the client any canned behavior. The client's real logic —
URL construction, `Accept` header, and the status-to-error mapping — runs fully,
and nothing touches a socket.

### Two things the fake transport must get right

First, the response `Body`. A real `http.Response.Body` is an
`io.ReadCloser`, and `http.Client` (and your client's `defer resp.Body.Close()`)
will call `Close`. A `*strings.Reader` is a `Reader` but not a `Closer`, so wrap
it: `io.NopCloser(strings.NewReader(body))`. Forgetting this either fails to
compile (wrong type) or, if you improvise, panics on `Close`. The fake must honor
the same lifecycle the real client expects.

Second, do not return a shared mutable body across calls — each response gets its
own reader, because a `Body` is consumed (read to EOF) once. In a table test each
case constructs its own response, which sidesteps this entirely.

### Status-to-error mapping is the contract

`GetUser` maps outcomes to typed results: 200 decodes the JSON body into a `User`;
404 returns the sentinel `ErrUserNotFound`; any 5xx returns `ErrUpstream` wrapped
with the status so callers can classify a transient upstream failure; a 200 with a
malformed body returns a decode error. That mapping is the client's real job, and
each branch is exercised by one table row. Sentinels are wrapped with `%w` so a
caller uses `errors.Is(err, ErrUserNotFound)` rather than inspecting status codes
itself.

Create `client.go`:

```go
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Sentinels callers classify with errors.Is.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrUpstream     = errors.New("upstream error")
)

// User is the decoded response payload.
type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Client calls an outbound users API through an injected *http.Client.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client. A nil hc uses http.DefaultClient.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: baseURL, http: hc}
}

// GetUser fetches GET {base}/users/{id} and maps the status to a typed result.
func (c *Client) GetUser(ctx context.Context, id int) (User, error) {
	url := fmt.Sprintf("%s/users/%d", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return User{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return User{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var u User
		if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
			return User{}, fmt.Errorf("decode user: %w", err)
		}
		return u, nil
	case resp.StatusCode == http.StatusNotFound:
		return User{}, fmt.Errorf("get user %d: %w", id, ErrUserNotFound)
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, resp.Body)
		return User{}, fmt.Errorf("get user %d: %w (status %d)", id, ErrUpstream, resp.StatusCode)
	default:
		return User{}, fmt.Errorf("get user %d: unexpected status %d", id, resp.StatusCode)
	}
}
```

### The runnable demo

The demo builds a client whose transport always returns a canned 200 with a JSON
user, so `go run ./cmd/demo` needs no network and prints a deterministic result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/apiclient"
)

type cannedTransport struct{}

func (cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":7,"name":"alice"}`)),
		Header:     make(http.Header),
	}, nil
}

func main() {
	hc := &http.Client{Transport: cannedTransport{}}
	c := apiclient.New("https://api.example.com", hc)

	u, err := c.GetUser(context.Background(), 7)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("user: %d %s\n", u.ID, u.Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user: 7 alice
```

### Tests

`TestGetUserStatusMapping` is table-driven over the four outcomes, each returning a
canned response via `roundTripperFunc`. `TestGetUserSendsCorrectRequest` uses a
capturing spy transport to assert the outgoing method, path, and `Accept` header
without any assertion on the response. `TestGetUserAgainstServer` is the contract
variant: a real `httptest.NewServer` exercises the full transport, guarding against
the mock lying about a detail the real stack would reject.

Create `client_test.go`:

```go
package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roundTripperFunc adapts a function to an http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// cannedResponse builds a *http.Response with a closable body.
func cannedResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func clientWith(rt http.RoundTripper) *Client {
	return New("https://api.example.com", &http.Client{Transport: rt})
}

func TestGetUserStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		wantUser User
		wantErr  error // sentinel to match with errors.Is, or nil
		wantAny  bool  // true when we only require a non-nil error
	}{
		{name: "ok", status: 200, body: `{"id":7,"name":"alice"}`, wantUser: User{ID: 7, Name: "alice"}},
		{name: "not found", status: 404, body: ``, wantErr: ErrUserNotFound},
		{name: "server error", status: 500, body: `boom`, wantErr: ErrUpstream},
		{name: "malformed body", status: 200, body: `{not json`, wantAny: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := clientWith(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return cannedResponse(tc.status, tc.body), nil
			}))

			got, err := c.GetUser(context.Background(), 7)

			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
			case tc.wantAny:
				if err == nil {
					t.Fatal("want a decode error, got nil")
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.wantUser {
					t.Fatalf("user = %+v, want %+v", got, tc.wantUser)
				}
			}
		})
	}
}

func TestGetUserSendsCorrectRequest(t *testing.T) {
	t.Parallel()

	var captured *http.Request
	c := clientWith(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		captured = r.Clone(r.Context())
		return cannedResponse(200, `{"id":1,"name":"bob"}`), nil
	}))

	if _, err := c.GetUser(context.Background(), 42); err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	if captured == nil {
		t.Fatal("transport was never called")
	}
	if captured.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", captured.Method)
	}
	if captured.URL.Path != "/users/42" {
		t.Errorf("path = %s, want /users/42", captured.URL.Path)
	}
	if got := captured.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
}

func TestGetUserAgainstServer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/9" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":9,"name":"carol"}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, srv.Client())

	got, err := c.GetUser(context.Background(), 9)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if want := (User{ID: 9, Name: "carol"}); got != want {
		t.Fatalf("user = %+v, want %+v", got, want)
	}

	if _, err := c.GetUser(context.Background(), 100); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user err = %v, want ErrUserNotFound", err)
	}
}
```

## Review

The client's logic is fully testable without a network because every branch flows
from a value the fake transport controls: the status code and body decide the
mapping, and the captured request proves the URL and headers. The table test pins
the status-to-error contract with `errors.Is`; the capturing spy pins the request
shape; and the `httptest.NewServer` variant is the antidote to a mock that lies —
it drives the real transport, real header parsing, and real body handling, so if
the canned responses ever drift from what the actual stack produces, the server
test catches it.

The recurring mistake is the response `Body`. Return `io.NopCloser(strings.New
Reader(...))`, never a bare reader, so `defer resp.Body.Close()` works exactly as
against a real server. And keep one server-backed test alongside the fast canned
ones: the fake gives you the four branches cheaply, but only the real transport
proves the client speaks HTTP correctly.

## Resources

- [`net/http.RoundTripper`](https://pkg.go.dev/net/http#RoundTripper) — the single-method transport seam.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewServer` and `Server.Client` for the contract test.
- [`io.NopCloser`](https://pkg.go.dev/io#NopCloser) — wrapping a reader as the closable response body.

---

Back to [03-fake-in-memory-repository.md](03-fake-in-memory-repository.md) | Next: [05-mock-clock-for-retry-backoff.md](05-mock-clock-for-retry-backoff.md)
