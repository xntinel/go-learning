# Exercise 7: Mocking http.RoundTripper — A Weather API Client Without a Socket

To test the branch logic of an HTTP client — status handling, body decoding, error
mapping — you do not need a server. `http.RoundTripper` is a one-method seam
(`RoundTrip(*http.Request) (*http.Response, error)`); inject a fake transport that
returns canned `*http.Response` values and the client runs its full logic with zero
network. This module builds a typed weather-API client and exercises every branch
through a fake transport.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. No external dependencies, no network.

## What you'll build

```text
weather/                     independent module: example.com/weather
  go.mod                     go 1.26
  weather.go                 Client over *http.Client; Report; ErrNotFound; *ServerError
  cmd/
    demo/
      main.go                runnable demo using an injected fake transport
  weather_test.go            fake RoundTripper; table over status/body; request-capture; transport error
```

- Files: `weather.go`, `cmd/demo/main.go`, `weather_test.go`.
- Implement: `Client.Current(ctx, city)` that builds a request, decodes 200 JSON into `Report`, maps 404 to `ErrNotFound`, 5xx to a typed `*ServerError`, and surfaces transport errors.
- Test: a fake `http.RoundTripper` returning canned responses (body via `io.NopCloser`); a table over 200-good / 200-malformed / 404 / 5xx / transport-error; assert the outgoing request URL and method via a captured `*http.Request`.
- Verify: `go test -count=1 -race ./...`

### The client and its error mapping

`Client` wraps an `*http.Client` and a base URL, both injected. `Current` builds a
`GET /weather?city=...` request with the caller's context, sends it through the
injected client, and maps the response:

- 200: decode the JSON body into a `Report`; a malformed body is a decode error.
- 404: the city does not exist -> the sentinel `ErrNotFound` (callers match with
  `errors.Is`).
- 5xx: a server-side failure -> a typed `*ServerError` carrying the status code
  (callers extract it with `errors.As`); it is retryable, unlike the 4xx cases.
- a transport-level error (the `RoundTrip` itself failed — DNS, connection refused,
  a cancelled context): wrapped and returned so `errors.Is` still reaches the cause.

The reason to test this through a fake `RoundTripper` rather than an
`httptest.Server` is isolation and speed: you are testing *this client's* mapping
logic, not the transport, so you feed it exact `*http.Response` values and never
open a socket. (When you specifically want the real transport exercised — TLS,
redirects, connection reuse — `httptest.Server` is the right tool; a note at the end
of the tests says so.)

Create `weather.go`:

```go
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrNotFound is returned when the city is unknown (HTTP 404).
var ErrNotFound = errors.New("city not found")

// ServerError is a typed, retryable error carrying the upstream 5xx status.
type ServerError struct {
	StatusCode int
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("weather server error: status %d", e.StatusCode)
}

// Retryable reports whether the caller should retry; 5xx responses are.
func (e *ServerError) Retryable() bool { return true }

// Report is the decoded 200 response body.
type Report struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_c"`
}

// Client is a typed weather-API client over an injected *http.Client.
type Client struct {
	http    *http.Client
	baseURL string
}

// New injects the HTTP client and base URL. Tests pass a client whose Transport
// is a fake RoundTripper.
func New(hc *http.Client, baseURL string) *Client {
	return &Client{http: hc, baseURL: baseURL}
}

// Current fetches the weather for a city, mapping status codes to typed errors.
func (c *Client) Current(ctx context.Context, city string) (Report, error) {
	u := c.baseURL + "/weather?city=" + url.QueryEscape(city)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Report{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Report{}, fmt.Errorf("weather request: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var r Report
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return Report{}, fmt.Errorf("decode weather body: %w", err)
		}
		return r, nil
	case resp.StatusCode == http.StatusNotFound:
		return Report{}, ErrNotFound
	case resp.StatusCode >= 500:
		return Report{}, &ServerError{StatusCode: resp.StatusCode}
	default:
		return Report{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}
```

### The runnable demo

The demo injects a fake transport right here in `package main` so you can watch the
client decode a canned response with no network — the same seam the tests use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/weather"
)

// stubTransport returns one canned response for any request.
type stubTransport struct {
	status int
	body   string
}

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func main() {
	hc := &http.Client{Transport: stubTransport{
		status: http.StatusOK,
		body:   `{"city":"Paris","temp_c":12.5}`,
	}}
	c := weather.New(hc, "http://weather.test")

	r, err := c.Current(context.Background(), "Paris")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s: %.1fC\n", r.City, r.TempC)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Paris: 12.5C
```

### The fake transport and the tests

`fakeTransport` implements `RoundTripper`: it returns a preprogrammed response (or a
preprogrammed transport error) and captures the outgoing `*http.Request` so a test
can assert what the client built. Every canned body is wrapped in
`io.NopCloser(strings.NewReader(...))` — the client calls `resp.Body.Close()`, and a
nil or non-`ReadCloser` body would panic; this is the single most common mistake
with `RoundTripper` fakes.

The table covers all five branches: a 200 with valid JSON decodes to the `Report`;
a 200 with malformed JSON yields a decode error (asserted as non-nil, not a
sentinel); a 404 maps to `ErrNotFound` (via `errors.Is`); a 503 maps to a
`*ServerError` with `StatusCode == 503` (via `errors.As`, and it reports
`Retryable`); and a transport-level error surfaces wrapped so `errors.Is` reaches
the cause. A separate test uses the captured request to assert the method is `GET`
and the URL carries the escaped city query.

Create `weather_test.go`:

```go
package weather

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport is a canned RoundTripper that also captures the request.
type fakeTransport struct {
	resp    *http.Response
	err     error
	lastReq *http.Request
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.lastReq = req
	return f.resp, f.err
}

// resp builds a canned *http.Response with a safe, closable body.
func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func clientWith(ft *fakeTransport) *Client {
	return New(&http.Client{Transport: ft}, "http://weather.test")
}

func TestCurrentBranches(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("connection refused")

	tests := []struct {
		name     string
		resp     *http.Response
		transErr error
		wantCity string
		checkErr func(t *testing.T, err error)
	}{
		{
			name:     "200 valid json",
			resp:     resp(http.StatusOK, `{"city":"Paris","temp_c":12.5}`),
			wantCity: "Paris",
			checkErr: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		},
		{
			name: "200 malformed json",
			resp: resp(http.StatusOK, `{not json`),
			checkErr: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("want decode error, got nil")
				}
			},
		},
		{
			name: "404 not found",
			resp: resp(http.StatusNotFound, ``),
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "503 server error",
			resp: resp(http.StatusServiceUnavailable, ``),
			checkErr: func(t *testing.T, err error) {
				var se *ServerError
				if !errors.As(err, &se) {
					t.Fatalf("err = %v, want *ServerError", err)
				}
				if se.StatusCode != http.StatusServiceUnavailable || !se.Retryable() {
					t.Fatalf("ServerError = %+v, want 503 retryable", se)
				}
			},
		},
		{
			name:     "transport error",
			transErr: transportErr,
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, transportErr) {
					t.Fatalf("err = %v, want to wrap the transport error", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ft := &fakeTransport{resp: tc.resp, err: tc.transErr}
			got, err := clientWith(ft).Current(context.Background(), "Paris")
			tc.checkErr(t, err)
			if tc.wantCity != "" && got.City != tc.wantCity {
				t.Fatalf("city = %q, want %q", got.City, tc.wantCity)
			}
		})
	}
}

func TestCurrentBuildsRequest(t *testing.T) {
	t.Parallel()

	ft := &fakeTransport{resp: resp(http.StatusOK, `{"city":"Sao Paulo","temp_c":20}`)}
	if _, err := clientWith(ft).Current(context.Background(), "Sao Paulo"); err != nil {
		t.Fatalf("Current: %v", err)
	}

	req := ft.lastReq
	if req == nil {
		t.Fatal("transport never received a request")
	}
	if req.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", req.Method)
	}
	if got, want := req.URL.Path, "/weather"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if got := req.URL.Query().Get("city"); got != "Sao Paulo" {
		t.Fatalf("city query = %q, want %q", got, "Sao Paulo")
	}
}
```

## Review

The client is correct when each status maps to its documented result: 200 decodes,
404 is `ErrNotFound`, 5xx is a retryable `*ServerError`, and a transport failure is
wrapped so the cause survives `errors.Is`. The fake `RoundTripper` is what makes all
of that testable with no server — one method, canned responses, and a captured
request to assert the URL and method the client built. The two non-negotiables for
the fake: every body is `io.NopCloser(strings.NewReader(...))` (the client *will*
close it) and every response has a valid `StatusCode`. Reach for `httptest.Server`
instead only when you need the real transport exercised end-to-end; for pure
client-side mapping, the `RoundTripper` seam is cheaper and more precise. Run
`go test -race`.

## Resources

- [net/http RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — the one-method transport seam.
- [io.NopCloser](https://pkg.go.dev/io#NopCloser) — wrapping a reader as a closable response body.
- [errors.As](https://pkg.go.dev/errors#As) — extracting the typed `*ServerError` in the 5xx test.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — the alternative when you want the real transport exercised.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-clock-interface-deterministic-backoff.md](08-clock-interface-deterministic-backoff.md)
