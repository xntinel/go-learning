# 4. Middleware Chains

Middleware is ordinary Go code that wraps an `http.Handler` with cross-cutting behavior. This lesson builds a reusable middleware package with logging, bearer-token authentication, recovery, a chain helper, examples, a demo command, and `httptest` coverage.

## Concepts

The `net/http` package represents server behavior with `http.Handler`, whose `ServeHTTP` method receives a `ResponseWriter` and a `Request`. `http.HandlerFunc` adapts a function to that interface. Middleware follows the shape `func(http.Handler) http.Handler`: it receives the next handler and returns a new handler.

Ordering matters. In `Chain(handler, Recovery(...), Logging(...), Auth(...))`, recovery is outermost, logging wraps authentication and the final handler, and authentication can stop unauthorized requests before the final handler runs. Expected authentication failures should use a sentinel error wrapped with `%w`; callers and tests should check it with `errors.Is`.

Use `httptest.NewRequest` and `httptest.NewRecorder` to test handlers without listening on a real port. Use `ResponseRecorder.Result` after the handler finishes to inspect the response.

## Exercises

Create this module layout:

```text
middleware/
  go.mod
  middleware.go
  middleware_example_test.go
  middleware_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/middleware

go 1.26
```

Create `middleware.go`:

```go
package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

var ErrUnauthorized = errors.New("unauthorized")

type Middleware func(http.Handler) http.Handler

func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

func Logging(logf func(string, ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			if logf != nil {
				logf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
			}
		})
	}
}

func ValidateBearer(header, want string) error {
	if header != "Bearer "+want {
		return fmt.Errorf("validate bearer token: %w", ErrUnauthorized)
	}
	return nil
}

func Auth(token string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := ValidateBearer(r.Header.Get("Authorization"), token); errors.Is(err, ErrUnauthorized) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func Recovery(logf func(string, ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if logf != nil {
						logf("panic recovered: %v", recovered)
					}
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
```

Create `middleware_example_test.go`:

```go
package middleware_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/middleware"
)

func ExampleChain() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "private")
	})

	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()

	wrapped := middleware.Chain(handler, middleware.Auth("secret-token"))
	wrapped.ServeHTTP(rec, req)

	fmt.Println(strings.TrimSpace(rec.Body.String()))

	// Output:
	// private
}
```

Create `middleware_test.go`:

```go
package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{name: "valid", header: "Bearer secret-token"},
		{name: "missing", header: "", wantErr: true},
		{name: "wrong", header: "Bearer wrong", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateBearer(tt.header, "secret-token")
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatalf("ValidateBearer error = %v, want ErrUnauthorized", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateBearer returned error: %v", err)
			}
		})
	}
}

func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		header     string
		statusCode int
		body       string
	}{
		{name: "authorized", header: "Bearer secret-token", statusCode: http.StatusOK, body: "ok"},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: "unauthorized"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ok"))
			})
			req := httptest.NewRequest(http.MethodGet, "/private", nil)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()

			Auth("secret-token")(handler).ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			if res.StatusCode != tt.statusCode {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.statusCode)
			}
			if !strings.Contains(rec.Body.String(), tt.body) {
				t.Fatalf("body = %q, want substring %q", rec.Body.String(), tt.body)
			}
		})
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	t.Parallel()

	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, format)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	Recovery(logf)(handler).ServeHTTP(rec, req)

	if rec.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Result().StatusCode)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
}

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var calls []string
	middlewareFor := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, name+" before")
				next.ServeHTTP(w, r)
				calls = append(calls, name+" after")
			})
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "handler")
	})

	Chain(handler, middlewareFor("a"), middlewareFor("b")).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
	)

	want := []string{"a before", "b before", "handler", "b after", "a after"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http"

	"example.com/middleware"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "public")
	})
	mux.Handle("GET /private", middleware.Auth("secret-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "private")
	})))
	mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
		panic("demo panic")
	})

	handler := middleware.Chain(
		mux,
		middleware.Recovery(log.Printf),
		middleware.Logging(log.Printf),
	)

	log.Println("listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}
```

## Common Mistakes

Putting recovery inside authentication means a panic in outer middleware can escape. Put recovery first in the `Chain` argument list so it becomes outermost.

Forgetting `next.ServeHTTP(w, r)` stops the request permanently. Only skip `next` intentionally, such as when authentication fails.

Writing headers after `next.ServeHTTP` may be too late because the handler may already have written the response. Set request-derived values before calling `next`, and wrap `ResponseWriter` when response inspection is required.

Comparing authentication errors with `==` breaks once errors are wrapped. Use `errors.Is(err, ErrUnauthorized)`.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

Middleware composes behavior around `http.Handler`. `Chain` applies middleware in a predictable order, `Auth` can stop requests before the final handler, `Recovery` prevents handler panics from crashing the server, and `httptest` keeps middleware tests fast and isolated.

## What's Next

Next: [Request Body Parsing and Validation](../05-request-body-parsing-and-validation/05-request-body-parsing-and-validation.md).

## Resources

- [http.Handler](https://pkg.go.dev/net/http#Handler)
- [http.HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc)
- [httptest.ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder)
- [errors.Is](https://pkg.go.dev/errors#Is)
