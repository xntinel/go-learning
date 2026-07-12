# Exercise 4: Test a downstream API client against an ephemeral server

The most common real-world use of `httptest` is not testing your handlers — it is
testing the code that *calls* other services. This module builds a typed payments
client and tests it against an `httptest.NewServer` whose handler asserts the
outgoing request (method, path, auth header, body) and returns canned responses,
verifying the client marshals the request and unmarshals the response correctly.

## What you'll build

```text
paymentsclient/                 independent module: example.com/testing-outbound-client
  go.mod                        go 1.26
  payments.go                   Client, ChargeRequest, ChargeResult, ErrUpstream; Charge
  cmd/
    demo/
      main.go                   spins a canned server, calls Charge, prints the result
  payments_test.go              server asserts the request; happy path, 5xx path, cancellation
```

- Files: `payments.go`, `cmd/demo/main.go`, `payments_test.go`.
- Implement: `Client.Charge(ctx, ChargeRequest) (*ChargeResult, error)` posting JSON with a bearer token; errors wrap the sentinel `ErrUpstream` with `%w`.
- Test: a server handler asserts `r.Method`, `r.URL.Path`, the `Authorization` header, and the decoded body, then writes a fixture; construct the client with `baseURL = srv.URL` and `srv.Client()`; assert the returned struct, the 500-to-`ErrUpstream` path, and context cancellation propagating into `Do`.
- Verify: `go test -count=1 -race ./...`

### Testing the outbound half: the server is your assertion surface

When you test a client, the roles invert: *your* code sends the request and *the
test's* handler receives it. That handler is where the interesting assertions live
— it is the only place you can check that the client set the right method, path,
`Content-Type`, `Authorization`, and request body. The server then returns a
canned response so you can check the client decodes it into the right domain
struct. Two failure paths matter as much as the happy one: a `5xx` from the
downstream must surface as a typed error your callers can branch on, and a
canceled context must abort the in-flight `Do` rather than hang.

The client takes its `*http.Client` as a dependency so the test can inject
`srv.Client()`. In the test, `baseURL` is `srv.URL`, so `c.baseURL + "/v1/charges"`
points at the ephemeral server. Errors wrap a package-level sentinel,
`ErrUpstream`, with `%w`, so callers write `errors.Is(err, payments.ErrUpstream)`
rather than string-matching. The body is always read to EOF and closed with a
`defer` so the connection returns to the pool even on the error path.

Create `payments.go`:

```go
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrUpstream is returned (wrapped) when the payments service responds with a
// non-2xx status. Callers match it with errors.Is.
var ErrUpstream = errors.New("payments: upstream error")

// ChargeRequest is the JSON body sent to the payments service.
type ChargeRequest struct {
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Source      string `json:"source"`
}

// ChargeResult is the JSON body returned by the payments service.
type ChargeResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Client calls a downstream payments API.
type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
}

// New builds a Client. A nil httpc falls back to http.DefaultClient.
func New(baseURL, token string, httpc *http.Client) *Client {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, httpc: httpc}
}

// Charge posts a charge and returns the created result. A non-200 status is
// reported as an error wrapping ErrUpstream.
func (c *Client) Charge(ctx context.Context, cr ChargeRequest) (*ChargeResult, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(cr); err != nil {
		return nil, fmt.Errorf("charge: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/charges", &buf)
	if err != nil {
		return nil, fmt.Errorf("charge: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("charge: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("charge: status %d: %s: %w", resp.StatusCode, bytes.TrimSpace(body), ErrUpstream)
	}

	var out ChargeResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("charge: decode: %w", err)
	}
	return &out, nil
}
```

### The demo

The demo starts a server that returns a fixed charge, calls the client, and prints
the decoded result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/testing-outbound-client"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ch_123","status":"succeeded"}`))
	}))
	defer srv.Close()

	c := payments.New(srv.URL, "test-token", srv.Client())
	res, err := c.Charge(context.Background(), payments.ChargeRequest{
		AmountCents: 4200,
		Currency:    "usd",
		Source:      "tok_visa",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("charge %s: %s\n", res.ID, res.Status)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
charge ch_123: succeeded
```

### Tests

The happy-path test's server handler is the assertion surface: it checks the
method, path, `Authorization` header, and decoded body before returning a fixture,
so a client that sent the wrong request fails there. The 500 test asserts the
error wraps `ErrUpstream` via `errors.Is`. The cancellation test cancels the
context before calling `Charge` and asserts the error wraps `context.Canceled`,
proving cancellation reaches `Do`. Tests base their contexts on `t.Context()` so
they cancel with the test.

Create `payments_test.go`:

```go
package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChargeSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/charges" {
			t.Errorf("path = %s, want /v1/charges", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		var got ChargeRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got.AmountCents != 4200 || got.Currency != "usd" || got.Source != "tok_visa" {
			t.Errorf("request body = %+v, want {4200 usd tok_visa}", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ch_123","status":"succeeded"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "test-token", srv.Client())
	res, err := c.Charge(t.Context(), ChargeRequest{AmountCents: 4200, Currency: "usd", Source: "tok_visa"})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if res.ID != "ch_123" || res.Status != "succeeded" {
		t.Fatalf("result = %+v, want {ch_123 succeeded}", res)
	}
}

func TestChargeUpstreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "test-token", srv.Client())
	_, err := c.Charge(t.Context(), ChargeRequest{AmountCents: 100, Currency: "usd", Source: "tok"})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want it to wrap ErrUpstream", err)
	}
}

func TestChargeContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","status":"succeeded"}`))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // canceled before the call

	c := New(srv.URL, "test-token", srv.Client())
	_, err := c.Charge(ctx, ChargeRequest{AmountCents: 1, Currency: "usd", Source: "tok"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
}
```

As a "your turn" addition, add a case where the server returns `200` with a
malformed JSON body and assert `Charge` returns a decode error (not a nil result).

## Review

Testing the outbound half puts the assertions in the *server* handler: it is the
only place to verify the client's method, path, headers, and body are exactly
right, and `t.Errorf` (not `t.Fatal`) is correct there because the handler runs on
the server's goroutine. Wrapping `ErrUpstream` with `%w` is what lets callers
branch on the failure with `errors.Is` instead of scraping strings, and the 500
test pins that contract. The cancellation test is the cheap insurance that
`context` actually threads through `http.NewRequestWithContext` into `Do` — a
canceled context must abort the call, and here it surfaces as
`context.Canceled`. Draining and closing the body on every path (via `defer`)
keeps the client from silently disabling keep-alive under load.

## Resources

- [net/http `Client.Do`](https://pkg.go.dev/net/http#Client.Do) — request execution and error semantics.
- [net/http `NewRequestWithContext`](https://pkg.go.dev/net/http#NewRequestWithContext) — building a client request bound to a context.
- [errors `Is`](https://pkg.go.dev/errors#Is) and [`fmt.Errorf` `%w`](https://pkg.go.dev/fmt#Errorf) — sentinel wrapping and matching.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-result-response-inspection.md](03-result-response-inspection.md) | Next: [05-roundtripper-stub.md](05-roundtripper-stub.md)
