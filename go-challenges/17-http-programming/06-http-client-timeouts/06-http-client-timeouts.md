# 6. HTTP Client Timeouts

HTTP clients need explicit deadlines. This lesson builds a reusable `clienttimeouts` package that configures `http.Client`, `http.Transport`, and per-request `context.Context` deadlines, then verifies timeout behavior with `net/http/httptest` instead of depending on the public internet.

## Concepts

The `net/http` package documents that `Client` and `Transport` are safe for concurrent use and should be reused. `http.Client.Timeout` applies to the whole request, including connection time, redirects, and reading the response body. `http.Transport` exposes phase-level settings such as `DialContext`, `TLSHandshakeTimeout`, `ResponseHeaderTimeout`, `IdleConnTimeout`, `MaxIdleConns`, and `MaxIdleConnsPerHost`. `http.NewRequestWithContext` lets a single request carry its own deadline.

Timeout failures are errors, not status codes. Classify them with `errors.Is(err, context.DeadlineExceeded)` and, when appropriate, `errors.As` into `net.Error` and check `Timeout()`.

## Exercises

Create this module layout:

```text
client-timeouts/
    go.mod
    timeout.go
    timeout_example_test.go
    timeout_test.go
    cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/client-timeouts

go 1.26
```

Create `timeout.go`:

```go
package clienttimeouts

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var (
	ErrInvalidURL    = errors.New("invalid url")
	ErrRequestFailed = errors.New("request failed")
)

type Result struct {
	StatusCode int
	Bytes      int64
}

func NewClient(totalTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: totalTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   3 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

func Fetch(ctx context.Context, client *http.Client, url string, requestTimeout time.Duration) (Result, error) {
	if url == "" {
		return Result{}, fmt.Errorf("%w: empty url", ErrInvalidURL)
	}
	if client == nil {
		client = NewClient(10 * time.Second)
	}
	if requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrRequestFailed, err)
	}
	defer resp.Body.Close()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("%w: read body: %v", ErrRequestFailed, err)
	}

	return Result{StatusCode: resp.StatusCode, Bytes: n}, nil
}

func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
```

Create `timeout_example_test.go`:

```go
package clienttimeouts_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	clienttimeouts "example.com/client-timeouts"
)

func ExampleFetch() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer server.Close()

	result, err := clienttimeouts.Fetch(context.Background(), clienttimeouts.NewClient(time.Second), server.URL, 500*time.Millisecond)
	fmt.Println(result.StatusCode, result.Bytes, err == nil)

	// Output:
	// 200 5 true
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
	"time"

	clienttimeouts "example.com/client-timeouts"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "demo")
	}))
	defer server.Close()

	result, err := clienttimeouts.Fetch(context.Background(), clienttimeouts.NewClient(time.Second), server.URL, 200*time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("status=%d bytes=%d\n", result.StatusCode, result.Bytes)
}
```

Create `timeout_test.go`:

```go
package clienttimeouts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "bad scheme", url: "http://%zz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Fetch(context.Background(), nil, tt.url, 0)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("expected ErrInvalidURL, got %v", err)
			}
		})
	}
}

func TestFetchTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, "late")
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), NewClient(time.Second), server.URL, 10*time.Millisecond)
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("expected ErrRequestFailed, got %v", err)
	}
	if !IsTimeout(err) {
		t.Fatalf("expected timeout classification, got %v", err)
	}
}

func TestFetchSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "short", body: "ok"},
		{name: "longer", body: "response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			result, err := Fetch(context.Background(), NewClient(time.Second), server.URL, 0)
			if err != nil {
				t.Fatalf("Fetch returned error: %v", err)
			}
			if result.StatusCode != http.StatusOK || result.Bytes != int64(len(tt.body)) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
```

## Common Mistakes

- Using `http.Get` or a zero-value `http.Client` for production calls with no timeout.
- Creating a new `http.Transport` for every request instead of reusing one.
- Forgetting `defer cancel()` after `context.WithTimeout`.
- Matching timeout errors by string instead of using `errors.Is` or `errors.As`.
- Testing timeouts against public endpoints, which makes tests slow and flaky.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

You built a library package that reuses a configured `http.Client`, sets transport phase timeouts, applies per-request deadlines with `http.NewRequestWithContext`, wraps sentinel errors with `%w`, and tests timeout behavior with `httptest`.

## What's Next

Next: [Cookie and Session Management](../07-cookie-and-session-management/07-cookie-and-session-management.md).

## Resources

- [net/http Client](https://pkg.go.dev/net/http#Client)
- [net/http Transport](https://pkg.go.dev/net/http#Transport)
- [net/http NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext)
- [context WithTimeout](https://pkg.go.dev/context#WithTimeout)
