# Exercise 22: HTTP Response Trailer Headers Injection on Exit

HTTP trailers — headers sent *after* the body, declared up front via a
`Trailer` header — are the right place to report metadata that is only known
once handling is finished, like a final processing status. This exercise
builds a handler helper that always attaches trailers describing the
outcome, computed by a deferred closure that reads the function's named
`status` and `err` results after the body has already been written.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde de error del handler).

## What you'll build

```text
trailerresp/                    independent module: example.com/trailerresp
  go.mod
  trailerresp.go                 ServeWithTrailer (named status/err, deferred trailer injection)
  cmd/demo/
    main.go                      runnable demo: a real HTTP round trip, success and failure
  trailerresp_test.go             httptest server, trailers asserted after reading the body
```

- Files: `trailerresp.go`, `cmd/demo/main.go`, `trailerresp_test.go`.
- Implement: `ServeWithTrailer(w http.ResponseWriter, handle func() (string, error)) (status int, err error)` that declares and populates `X-Process-Status`/`X-Process-Error` trailers regardless of whether `handle` succeeds.
- Test: a real `httptest.NewServer` round trip for both a successful and a failing `handle`, reading the trailers only after fully consuming the response body (as HTTP trailers require).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/22-http-response-trailer-header-injection/cmd/demo
cd go-solutions/04-functions/02-named-return-values/22-http-response-trailer-header-injection
go mod edit -go=1.24
```

### Trailers computed from named results, injected exactly once

HTTP trailers have a two-step protocol: declare their names via the
`Trailer` header before `WriteHeader`, then set their values any time before
the handler returns — the `net/http` `ResponseWriter` docs describe this
directly. `ServeWithTrailer` declares the trailer names up front and then
lets a single deferred closure fill them in from whatever `status` and `err`
end up being:

```go
w.Header().Set("Trailer", "X-Process-Status, X-Process-Error")

defer func() {
    w.Header().Set("X-Process-Status", strconv.Itoa(status))
    if err != nil {
        w.Header().Set("X-Process-Error", err.Error())
    } else {
        w.Header().Set("X-Process-Error", "")
    }
}()

body, herr := handle()
if herr != nil {
    err = herr
    status = http.StatusInternalServerError
    w.WriteHeader(status)
    return
}
status = http.StatusOK
w.WriteHeader(status)
_, _ = io.WriteString(w, body)
return
```

Because `status` and `err` are named results, the deferred closure sees
whichever branch actually ran without needing its own copy of that logic —
success sets `status = 200` and leaves `err` nil, failure sets both, and the
trailer-population code does not care which happened. Any future third
branch added to `ServeWithTrailer` inherits correct trailers automatically,
as long as it also sets the two named results before returning.

Create `trailerresp.go`:

```go
package trailerresp

import (
	"io"
	"net/http"
	"strconv"
)

// ServeWithTrailer writes handle's result to w and always attaches two HTTP
// trailer headers describing the outcome: X-Process-Status (the numeric HTTP
// status actually written) and X-Process-Error (empty on success). Trailers
// must be declared via the "Trailer" header before the body is written and
// then set after — see the net/http docs on ResponseWriter — so callers
// cannot simply set them before WriteHeader and be done.
//
// status and err are named results so a single deferred closure can populate
// the trailers from whatever those results end up being, regardless of which
// branch below set them. That guarantees the trailers are always sent — on
// the success path, the handler-error path, or any future branch someone
// adds — because emitting them lives in one place tied to the named results
// instead of being duplicated at every return statement.
func ServeWithTrailer(w http.ResponseWriter, handle func() (string, error)) (status int, err error) {
	w.Header().Set("Trailer", "X-Process-Status, X-Process-Error")

	defer func() {
		w.Header().Set("X-Process-Status", strconv.Itoa(status))
		if err != nil {
			w.Header().Set("X-Process-Error", err.Error())
		} else {
			w.Header().Set("X-Process-Error", "")
		}
	}()

	body, herr := handle()
	if herr != nil {
		err = herr
		status = http.StatusInternalServerError
		w.WriteHeader(status)
		return
	}

	status = http.StatusOK
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/trailerresp"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = trailerresp.ServeWithTrailer(w, func() (string, error) {
			return "hello", nil
		})
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		_, _ = trailerresp.ServeWithTrailer(w, func() (string, error) {
			return "", errors.New("boom")
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/ok", "/fail"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			fmt.Println("request error:", err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body) // must read the body fully to see trailers
		resp.Body.Close()
		fmt.Printf("%s: status=%d trailer-status=%s trailer-error=%q\n",
			path, resp.StatusCode, resp.Trailer.Get("X-Process-Status"), resp.Trailer.Get("X-Process-Error"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/ok: status=200 trailer-status=200 trailer-error=""
/fail: status=500 trailer-status=500 trailer-error="boom"
```

### Tests

Trailers only appear on the client's `resp.Trailer` map after the body has
been fully read, per how HTTP trailers are transmitted — the test reads the
body to `io.Discard` before inspecting them.

Create `trailerresp_test.go`:

```go
package trailerresp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServeWithTrailer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		handle        func() (string, error)
		wantStatus    int
		wantTrailer   string
		wantErrHeader string
	}{
		{
			name:          "success",
			handle:        func() (string, error) { return "hello", nil },
			wantStatus:    http.StatusOK,
			wantTrailer:   "200",
			wantErrHeader: "",
		},
		{
			name:          "handler error",
			handle:        func() (string, error) { return "", errors.New("boom") },
			wantStatus:    http.StatusInternalServerError,
			wantTrailer:   "500",
			wantErrHeader: "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = ServeWithTrailer(w, tt.handle)
			}))
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("reading body: %v", err)
			}

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Trailer.Get("X-Process-Status"); got != tt.wantTrailer {
				t.Fatalf("trailer X-Process-Status = %q, want %q", got, tt.wantTrailer)
			}
			if got := resp.Trailer.Get("X-Process-Error"); got != tt.wantErrHeader {
				t.Fatalf("trailer X-Process-Error = %q, want %q", got, tt.wantErrHeader)
			}
		})
	}
}
```

## Review

`ServeWithTrailer` is correct when both the success and failure paths declare
the same two trailer names up front and populate them from the final
`status`/`err` values regardless of which branch ran. The named results are
what let a single deferred closure own that population logic, rather than
duplicating `w.Header().Set(...)` calls at every return point inside the
function — a duplication that would be easy to get out of sync as branches
are added. The mistake to avoid is setting the trailer values *before*
`WriteHeader` and the body write: trailers are only meaningful once the
handler has actually decided the outcome, which is exactly why the
population must happen in a defer that runs after the body-writing code, not
inline before it.

## Resources

- [`net/http` package docs (ResponseWriter and trailers)](https://pkg.go.dev/net/http#ResponseWriter)
- [`net/http/httptest` package docs](https://pkg.go.dev/net/http/httptest)
- [RFC 7230 §4.1.2: Chunked Transfer Coding (trailer part)](https://www.rfc-editor.org/rfc/rfc7230#section-4.1.2)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-batch-operation-success-position-log.md](21-batch-operation-success-position-log.md) | Next: [23-cryptographic-key-cache-invalidate.md](23-cryptographic-key-cache-invalidate.md)
