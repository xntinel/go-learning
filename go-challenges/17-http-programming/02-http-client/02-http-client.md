# 2. HTTP Client

HTTP client code belongs behind package APIs, not directly in `main`. This lesson builds a small reusable client package that uses `net/http`, reads and closes response bodies, classifies status codes, and wraps validation errors so callers can use `errors.Is`.

## Concepts

The official `net/http` package provides package-level helpers such as `http.Get` and `http.Post`, plus the reusable `http.Client` type. The package documentation states that clients are safe for concurrent use and should be reused for efficiency. For request-level control, build a request with `http.NewRequestWithContext`, set headers through `Request.Header`, and execute it with `Client.Do`.

Every successful `Client.Do` call returns a response with a body. The caller must close `Response.Body` when finished. Read the body with `io.ReadAll` when you need it in memory, or drain it with `io.Copy(io.Discard, resp.Body)` when rejecting a response but still want the transport to reuse the connection.

Use sentinel errors for expected validation failures such as non-2xx responses. Wrap them with `%w` so callers can use `errors.Is(err, ErrBadStatus)` while still receiving context such as the URL and status code.

## Exercises

Create this module layout:

```text
http-client/
  go.mod
  client.go
  client_example_test.go
  client_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/httpclient

go 1.26
```

Create `client.go`:

```go
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

var ErrBadStatus = errors.New("bad HTTP status")

func FetchJSON(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create GET request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send GET request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("GET %s returned %d: %w", url, resp.StatusCode, ErrBadStatus)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read GET response: %w", err)
	}
	return body, nil
}

func PostJSON(ctx context.Context, client *http.Client, url string, payload any) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request JSON: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create POST request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send POST request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("POST %s returned %d: %w", url, resp.StatusCode, ErrBadStatus)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read POST response: %w", err)
	}
	return body, nil
}

func StatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "success"
	case code >= 400 && code < 500:
		return "client error"
	case code >= 500 && code < 600:
		return "server error"
	default:
		return "other"
	}
}
```

Create `client_example_test.go`:

```go
package httpclient_test

import (
	"fmt"

	"example.com/httpclient"
)

func ExampleStatusClass() {
	fmt.Println(httpclient.StatusClass(200))
	fmt.Println(httpclient.StatusClass(404))
	fmt.Println(httpclient.StatusClass(503))

	// Output:
	// success
	// client error
	// server error
}
```

Create `client_test.go`:

```go
package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "success", code: http.StatusOK, want: "success"},
		{name: "client error", code: http.StatusNotFound, want: "client error"},
		{name: "server error", code: http.StatusInternalServerError, want: "server error"},
		{name: "redirect", code: http.StatusFound, want: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := StatusClass(tt.code); got != tt.want {
				t.Fatalf("StatusClass(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestFetchJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "success", statusCode: http.StatusOK},
		{name: "bad status", statusCode: http.StatusTeapot, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "application/json" {
					t.Fatalf("Accept header = %q", r.Header.Get("Accept"))
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()

			body, err := FetchJSON(context.Background(), server.Client(), server.URL)
			if tt.wantErr {
				if !errors.Is(err, ErrBadStatus) {
					t.Fatalf("FetchJSON error = %v, want ErrBadStatus", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchJSON returned error: %v", err)
			}
			if string(body) != `{"ok":true}` {
				t.Fatalf("body = %s", body)
			}
		})
	}
}

func TestPostJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q", r.Header.Get("Content-Type"))
		}

		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["name"] != "Gopher" {
			t.Fatalf("payload name = %q", payload["name"])
		}
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer server.Close()

	body, err := PostJSON(context.Background(), server.Client(), server.URL, map[string]string{"name": "Gopher"})
	if err != nil {
		t.Fatalf("PostJSON returned error: %v", err)
	}
	if string(body) != `{"created":true}` {
		t.Fatalf("body = %s", body)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/httpclient"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"method":%q,"path":%q}`, r.Method, r.URL.Path)
	}))
	defer server.Close()

	body, err := httpclient.FetchJSON(context.Background(), server.Client(), server.URL+"/demo")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(string(body))
	fmt.Println(httpclient.StatusClass(http.StatusOK))
}
```

## Common Mistakes

Closing `resp.Body` before checking the request error can panic because `resp` may be nil. Check `err` first, then defer `resp.Body.Close()`.

Creating a new `http.Client` for every request defeats connection reuse. Reuse one client, or accept one as a dependency so callers control transport and timeout behavior.

Returning only `fmt.Errorf("status %d", code)` makes validation hard to inspect. Wrap a sentinel error with `%w` and test it with `errors.Is`.

Using live internet services in tests makes tests slow and flaky. Use `net/http/httptest` to run local handlers in-process.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

HTTP client code should reuse `http.Client`, create requests with context when callers need cancellation, set explicit headers, read and close response bodies, and wrap expected failures with sentinel errors. Local `httptest` servers let you verify headers, methods, status handling, and JSON bodies without network dependencies.

## What's Next

Next: [ServeMux Routing and Patterns](../03-servemux-routing-and-patterns/03-servemux-routing-and-patterns.md).

## Resources

- [net/http package](https://pkg.go.dev/net/http)
- [http.Client](https://pkg.go.dev/net/http#Client)
- [http.NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext)
- [httptest package](https://pkg.go.dev/net/http/httptest)
- [errors.Is](https://pkg.go.dev/errors#Is)
