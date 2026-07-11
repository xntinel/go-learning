# Exercise 3: Inspect the full response with ResponseRecorder.Result

A recorder's `Code`, `Body`, and `Header()` are live fields mutated as the handler
runs. To inspect the response the way a client would have received it — status,
committed headers, parsed cookies — call `rec.Result()` for a real
`*http.Response` snapshot. This module builds a handler that sets headers, a
cookie, and a non-200 status, then proves the difference between the live header
map and the committed snapshot.

## What you'll build

```text
resultinspect/                  independent module: example.com/result-response-inspection
  go.mod                        go 1.26
  handler.go                    AcceptHandler: headers + Set-Cookie + 202, one header set too late
  cmd/
    demo/
      main.go                   runs the handler, prints the Result() snapshot
  handler_test.go               asserts StatusCode, Header snapshot, Cookies(), Flushed, Body
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `AcceptHandler`, which sets a `Content-Type`, an `X-Request-Id`, a session cookie, writes `202`, then sets one header *after* `WriteHeader` to demonstrate the snapshot boundary.
- Test: `res := rec.Result(); defer res.Body.Close()`; assert `res.StatusCode`, `res.Header.Get(...)`, `res.Cookies()` and a specific cookie's attributes; contrast the too-late header's absence from `res.Header` with its presence in the live `rec.Header()`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/resultinspect/cmd/demo
cd ~/go-exercises/resultinspect
go mod init example.com/result-response-inspection
```

### The snapshot boundary: why Result() is the truth

An `http.ResponseWriter` commits its status and headers the moment `WriteHeader`
is called (or the first `Write`, which implies `WriteHeader(200)`). Any header you
set *after* that point is written into the live map but never sent to the client —
the status line and headers are already on their way. The recorder models this
faithfully: `rec.WriteHeader(code)` takes a *snapshot* of the header map at that
instant, and `rec.Result()` builds its `*http.Response` from that snapshot. So
`res.Header` reflects exactly what a client would have received, while the live
`rec.Header()` map keeps accumulating whatever the handler set afterwards.

This is why asserting on the live fields can lull you into a false pass: a header
set too late shows up in `rec.Header()` and your test goes green, but in
production that header never reaches the client. `rec.Result()` catches the bug.
`Result()` also gives you two conveniences the live map does not: `res.Cookies()`
parses the `Set-Cookie` headers into `[]*http.Cookie` so you can assert on a
cookie's `HttpOnly`/`MaxAge`/`Path` without string-parsing, and `res.Body` is a
real `io.ReadCloser` — which means you must `Close()` it (a no-op for the recorder,
but the habit matters and keeps the code copy-safe when the same assertions run
against a real response).

`AcceptHandler` below sets its committed headers and cookie, calls
`WriteHeader(http.StatusAccepted)`, and *then* sets `X-Too-Late`. The test asserts
`X-Too-Late` is absent from `res.Header` (the snapshot) yet present in
`rec.Header()` (the live map) — making the boundary concrete.

Create `handler.go`:

```go
package handler

import "net/http"

// AcceptHandler commits a 202 with headers and a session cookie, then sets one
// header AFTER WriteHeader to demonstrate that late headers never reach the
// client. It models an endpoint that accepts work for async processing.
func AcceptHandler(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-Id", "req-123")
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "s3cr3t",
		Path:     "/",
		MaxAge:   3600,
		HttpOnly: true,
	})

	w.WriteHeader(http.StatusAccepted)

	// Set after WriteHeader: recorded in the live header map, but NOT part of
	// the committed response, so it is absent from Result().Header.
	h.Set("X-Too-Late", "ignored")

	_, _ = w.Write([]byte(`{"accepted":true}`))
}
```

### The demo

The demo runs the handler against a recorder and prints the `Result()` snapshot,
including the parsed cookie.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/result-response-inspection"
)

func main() {
	req := httptest.NewRequest(http.MethodPost, "/jobs", nil)
	rec := httptest.NewRecorder()
	handler.AcceptHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	fmt.Printf("status: %d\n", res.StatusCode)
	fmt.Printf("content-type: %s\n", res.Header.Get("Content-Type"))
	fmt.Printf("x-too-late in snapshot: %q\n", res.Header.Get("X-Too-Late"))
	for _, c := range res.Cookies() {
		fmt.Printf("cookie: %s=%s httpOnly=%v maxAge=%d\n", c.Name, c.Value, c.HttpOnly, c.MaxAge)
	}
	fmt.Printf("body: %s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 202
content-type: application/json
x-too-late in snapshot: ""
cookie: session=s3cr3t httpOnly=true maxAge=3600
body: {"accepted":true}
```

### Tests

The test runs the handler once, takes the `Result()` snapshot, and asserts across
status, headers, cookies, and body. The pointed assertion is the pair on
`X-Too-Late`: absent from `res.Header` (proving the snapshot boundary), present in
`rec.Header()` (proving the live map kept mutating). It also asserts
`rec.Flushed == false` — the handler never called `Flush`, so there was no
streaming.

Create `handler_test.go`:

```go
package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcceptHandlerResult(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/jobs", nil)
	rec := httptest.NewRecorder()
	AcceptHandler(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("StatusCode = %d, want %d", res.StatusCode, http.StatusAccepted)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := res.Header.Get("X-Request-Id"); got != "req-123" {
		t.Fatalf("X-Request-Id = %q, want req-123", got)
	}

	// The snapshot boundary: X-Too-Late was set after WriteHeader, so it is
	// absent from the committed snapshot but present in the live header map.
	if got := res.Header.Get("X-Too-Late"); got != "" {
		t.Fatalf("Result().Header has X-Too-Late = %q; want it absent from the snapshot", got)
	}
	if got := rec.Header().Get("X-Too-Late"); got != "ignored" {
		t.Fatalf("live rec.Header() X-Too-Late = %q, want ignored", got)
	}

	if rec.Flushed {
		t.Fatal("Flushed = true, want false (handler never streamed)")
	}

	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("len(Cookies) = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "session" || c.Value != "s3cr3t" {
		t.Fatalf("cookie = %s=%s, want session=s3cr3t", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Fatal("cookie HttpOnly = false, want true")
	}
	if c.MaxAge != 3600 {
		t.Fatalf("cookie MaxAge = %d, want 3600", c.MaxAge)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"accepted":true}` {
		t.Fatalf("body = %s, want {\"accepted\":true}", body)
	}
}
```

As a "your turn" addition, extend `AcceptHandler` to also set a `Location` header
before `WriteHeader` and assert it survives in `res.Header`, contrasting with the
too-late header.

## Review

The lesson is that the live recorder fields and the `Result()` snapshot answer
different questions, and only the snapshot matches what a client sees. The
`X-Too-Late` pair is the whole point: it is in the live map and absent from the
snapshot, so a test that asserted on `rec.Header()` alone would wrongly pass while
production dropped the header. Always close `res.Body` even on a recorder — the
type is a real `io.ReadCloser`, and building the habit means the same assertions
work unchanged against a real `*http.Response`. `res.Cookies()` is the right way to
assert cookie attributes; hand-parsing `Set-Cookie` is fragile and usually wrong on
the `HttpOnly`/`Secure`/`SameSite` flags.

## Resources

- [httptest `ResponseRecorder.Result`](https://pkg.go.dev/net/http/httptest#ResponseRecorder.Result) — the snapshot semantics and the deprecated `HeaderMap`.
- [net/http `Response.Cookies`](https://pkg.go.dev/net/http#Response.Cookies) — parsing `Set-Cookie` into `[]*http.Cookie`.
- [net/http `SetCookie`](https://pkg.go.dev/net/http#SetCookie) — writing a cookie and its attributes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-newserver-e2e-handler.md](02-newserver-e2e-handler.md) | Next: [04-testing-outbound-client.md](04-testing-outbound-client.md)
