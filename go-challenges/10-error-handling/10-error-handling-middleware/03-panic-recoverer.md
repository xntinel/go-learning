# Exercise 3: Recover panics at the middleware boundary

An unrecovered panic in a handler crashes the whole server process. This exercise
builds the `Recoverer` middleware that `defer`s a `recover`, logs the value plus
`debug.Stack()` through `slog`, and writes a 500 — with the carve-out that
separates a correct recoverer from a dangerous one: it must *not* swallow
`http.ErrAbortHandler` or `http.ErrHandlerTimeout`, which `net/http` uses
deliberately to abort a connection quietly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
recoverer/                   independent module: example.com/recoverer
  go.mod                     go 1.26
  recoverer.go               Recoverer middleware; recover + carve-out; slog + debug.Stack; 500 JSON
  cmd/
    demo/
      main.go                runnable demo: a panicking handler -> 500 JSON
  recoverer_test.go          panic -> 500 test; ErrAbortHandler re-panic test; -race
```

Files: `recoverer.go`, `cmd/demo/main.go`, `recoverer_test.go`.
Implement: `Recoverer(http.Handler) http.Handler` that recovers, re-panics the two
abort sentinels, logs the rest with `debug.Stack()`, and writes a 500 JSON body.
Test: a handler that panics -> 500 with a JSON `internal error`; a handler that
panics with `http.ErrAbortHandler` -> the middleware re-panics (asserted by a
recovering test wrapper).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/recoverer/cmd/demo
cd ~/go-exercises/recoverer
go mod init example.com/recoverer
```

### Why recover, and why the carve-out

`http.Server` does have a last-ditch per-connection recover, but it logs the panic
and closes the connection *without writing a response* — the client gets a dropped
connection, not a 500, and the log line is buried. A `Recoverer` middleware you
control turns a panic into a clean, observable 500 with a body, a logged stack,
and (in later exercises) a correlation id. Placed outermost in the stack it also
catches panics thrown by *inner* middleware, which the connection-level recover
cannot distinguish from anything else.

The carve-out is the part people miss. `net/http` reserves two sentinel values as
deliberate control signals:

- `http.ErrAbortHandler` is *panicked* to abort a handler and close the connection
  silently. The stdlib documents it exactly this way: "panicking with
  ErrAbortHandler ... aborts the handler so the client sees an interrupted
  response but the server does not log an error." Reverse proxies use it when the
  upstream disappears. If your `Recoverer` converts it to a 500, you have turned a
  deliberate quiet abort into a logged error and a bogus response body.
- `http.ErrHandlerTimeout` is the error `http.TimeoutHandler`'s substitute writer
  returns after the deadline; a `Recoverer` that sees it should let it pass rather
  than reinterpret it.

So the recover function first checks whether the recovered value is one of those
sentinels and, if so, *re-panics* it, letting `net/http` do what it intended. Only
a non-abort panic is logged with its stack and rendered as a 500. `recover()`
returns `any`, so the comparison is `rec == http.ErrAbortHandler` — a plain
interface equality against the sentinel value.

The stack is captured with `runtime/debug.Stack()`, which returns the formatted
stack of the *calling goroutine* — the one that panicked and is now unwinding
through the deferred recover. That is exactly the goroutine you want. It is logged
with `slog`, never written to the client: the stack is internal detail.

Create `recoverer.go`:

```go
package recoverer

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recoverer catches panics from next (and anything it wraps), logs the value and
// stack, and writes a 500. It re-panics net/http's deliberate abort sentinels
// instead of converting them to a 500. Place it outermost in the stack.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// net/http uses these panics deliberately to abort a connection
			// quietly; converting them to a 500 corrupts that contract.
			if rec == http.ErrAbortHandler || rec == http.ErrHandlerTimeout {
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "recovered panic",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
		}()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo silences the log to `io.Discard` for clean output, wraps a handler that
panics, and drives it through an in-process server so you can see the 500 body a
real panic produces. (It does *not* demonstrate `ErrAbortHandler` in `main`,
because that re-panic would abort the request rather than print a line — the test
covers it instead.)

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/recoverer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := recoverer.Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]int
		m["boom"] = 1 // panic: assignment to entry in nil map
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=500 body={"error":"internal error"}
```

### Tests

`TestRecoversToInternalError` drives a panicking handler through an `httptest`
recorder and asserts a 500 with the JSON `internal error` body.
`TestRepanicsAbortHandler` is the carve-out proof: it installs its *own* recover in
a deferred function, drives a handler that panics with `http.ErrAbortHandler`, and
asserts the value that reaches its recover is `http.ErrAbortHandler` — i.e. the
middleware re-panicked instead of writing a 500. A parallel case does the same for
`http.ErrHandlerTimeout`. Run with `-race`; recovery and response writing must be
race-clean.

Create `recoverer_test.go`:

```go
package recoverer

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRecoversToInternalError(t *testing.T) {
	t.Parallel()

	h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated bug")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "internal error" {
		t.Fatalf("body error = %q, want %q", body["error"], "internal error")
	}
}

func TestRepanicsAbortSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sentinel error
	}{
		{"abort", http.ErrAbortHandler},
		{"timeout", http.ErrHandlerTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(tc.sentinel)
			}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			defer func() {
				got := recover()
				if got != tc.sentinel {
					t.Fatalf("recovered value = %v, want re-panicked %v", got, tc.sentinel)
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want no 500 written (default 200)", rec.Code)
				}
			}()

			h.ServeHTTP(rec, req)
			t.Fatal("expected Recoverer to re-panic the abort sentinel")
		})
	}
}

func ExampleRecoverer() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := Recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(rec.Code)
	// Output: 500
}
```

## Review

The recoverer is correct on two axes. First, an ordinary panic becomes a clean
500 with a JSON body and a logged stack, and never leaks the panic value or stack
to the client — `TestRecoversToInternalError` pins that. Second, and more subtly,
the two abort sentinels pass straight through: `TestRepanicsAbortSentinels`
asserts the middleware re-panics them, so `net/http`'s deliberate quiet-abort and
timeout semantics survive. Drop the carve-out and that test fails because the
value reaching the outer recover would be `nil` (the middleware would have
swallowed the panic and written a 500). Keep `Recoverer` outermost in the real
stack so it also catches panics from inner middleware, and keep `debug.Stack()`
going to the log and never to the response. Run `-race`: the deferred recover and
the response write must be clean under the detector.

## Resources

- [`net/http#ErrAbortHandler`](https://pkg.go.dev/net/http#ErrAbortHandler) — the panic sentinel a recoverer must re-panic.
- [`runtime/debug#Stack`](https://pkg.go.dev/runtime/debug#Stack) — the formatted stack of the panicking goroutine.
- [`log/slog#Logger.ErrorContext`](https://pkg.go.dev/log/slog#Logger.ErrorContext) — context-aware structured error logging.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-middleware-stack-composition.md](04-middleware-stack-composition.md)
