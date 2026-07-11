# Exercise 9: Bounded, Cancellable Request-Body Guard

Reading a request body is a trust boundary. An unbounded `io.ReadAll(r.Body)` is a
memory-exhaustion vector, and an unbounded read of an abandoned upload wastes a
goroutine. This exercise builds a guard that caps payload size with
`http.MaxBytesReader` (returning `413` on oversize) and reads under `r.Context()`
so a client disconnect stops the read — while carefully distinguishing a
`*http.MaxBytesError` from a genuine context cancellation.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
bodyguard/                 independent module: example.com/bodyguard
  go.mod                   go 1.26
  guard.go                 ReadBounded; GuardedHandler (413 / 200 / bail-on-cancel)
  cmd/
    demo/
      main.go              an oversize POST (413) and a well-formed POST (200)
  guard_test.go            413 + errors.As test, happy-path decode, cancel-mid-upload
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `ReadBounded(w, r, limit)` wrapping `r.Body` in `http.MaxBytesReader` and reading under `r.Context()`; `GuardedHandler(limit)` translating `*http.MaxBytesError` to `413`, a context error to a quiet bail, and a good body to a `200` decode.
Test: oversize POST asserts `413` and `errors.As(err, *http.MaxBytesError)`; well-formed POST asserts `200`+decode; a cancelled slow upload asserts a context error, not a garbage decode.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bodyguard/cmd/demo
cd ~/go-exercises/bodyguard
go mod init example.com/bodyguard
```

## The design

`http.MaxBytesReader(w, r.Body, limit)` returns a reader that fails with a
`*http.MaxBytesError` once more than `limit` bytes are read; it also signals the
server to close the connection so a hostile client cannot keep streaming. That
handles the size half of the trust boundary. The cancellation half is `r.Context()`:
an abandoned upload should stop buffering the moment the client is gone, not read
to some natural end that may never come.

To honor cancellation *deterministically*, `ReadBounded` runs the `io.ReadAll` in
a goroutine and selects on `r.Context().Done()`. The subtlety is making the result
unambiguous: if the context is cancelled, the read goroutine may return a net
error (connection reset) at almost the same instant, and a bare `select` could
pick either branch. So both branches funnel through the same rule — *if the
context is done, return the context error* — which makes the outcome deterministic
regardless of scheduling. The read goroutine's channel is buffered (size 1) so it
never blocks sending even after `ReadBounded` has returned on the cancel path.

There is no `Decoder.DecodeContext` method in the standard library — do not invent
one. JSON decoding is the ordinary `json.Unmarshal` over the bytes returned by the
size-capped, context-honored read. `GuardedHandler` branches on the error: an
`errors.As` match on `*http.MaxBytesError` becomes `413`; a `context.Canceled`
means the client is gone, so it bails quietly (writing into a dead connection is
pointless); anything else is a `400`.

Create `guard.go`:

```go
package bodyguard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// ReadBounded reads r.Body under a size cap (http.MaxBytesReader) and honors
// r.Context(): if the context is cancelled it returns the context error rather
// than a partial read. A *http.MaxBytesError is returned when the body exceeds
// limit. The context rule is applied on both select branches so the outcome is
// deterministic regardless of goroutine scheduling.
func ReadBounded(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r.Body)
		ch <- result{b, err}
	}()

	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case res := <-ch:
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return res.b, res.err
	}
}

// GuardedHandler decodes a JSON body under ReadBounded. Oversize bodies become
// 413; a client disconnect (context error) bails quietly; a malformed body is
// 400; a good body is 200.
func GuardedHandler(limit int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := ReadBounded(w, r, limit)
		if err != nil {
			var maxErr *http.MaxBytesError
			switch {
			case errors.As(err, &maxErr):
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			case errors.Is(err, context.Canceled):
				// Client is gone; nothing useful to write.
			default:
				http.Error(w, "bad request", http.StatusBadRequest)
			}
			return
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
```

## The runnable demo

The demo drives the guard with an oversize POST (which must yield `413`) and a
well-formed small POST (which must yield `200`) through an in-process server.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"example.com/bodyguard"
)

func post(url, body string) int {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func main() {
	srv := httptest.NewServer(bodyguard.GuardedHandler(32)) // 32-byte cap
	defer srv.Close()

	oversize := `{"note":"` + strings.Repeat("x", 100) + `"}`
	fmt.Printf("oversize -> %d\n", post(srv.URL, oversize))
	fmt.Printf("wellformed -> %d\n", post(srv.URL, `{"ok":true}`))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
oversize -> 413
wellformed -> 200
```

## Tests

`TestReadBoundedOversize` calls `ReadBounded` directly with a `NewRecorder` and an
over-limit body and asserts `errors.As(err, *http.MaxBytesError)` holds — the unit
proof of the size branch. `TestGuardedHandler413And200` drives the full handler
over `httptest` and asserts `413` on oversize and `200` on a well-formed body.
`TestCancelledUploadReturnsContextError` streams a slow body through an
`io.Pipe`, cancels the client context mid-upload, and asserts `ReadBounded` on the
server returns a context error — not a partial or garbage decode.

Create `guard_test.go`:

```go
package bodyguard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadBoundedOversize(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	body := strings.NewReader(strings.Repeat("a", 1000))
	r := httptest.NewRequest(http.MethodPost, "/", body)

	_, err := ReadBounded(w, r, 100)
	if err == nil {
		t.Fatal("ReadBounded returned nil error for an over-limit body")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("err = %v, want a *http.MaxBytesError", err)
	}
	if maxErr.Limit != 100 {
		t.Fatalf("MaxBytesError.Limit = %d, want 100", maxErr.Limit)
	}
}

func TestGuardedHandler413And200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(GuardedHandler(32))
	defer srv.Close()

	post := func(t *testing.T, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := post(t, `{"note":"`+strings.Repeat("x", 100)+`"}`); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", got)
	}
	if got := post(t, `{"ok":true}`); got != http.StatusOK {
		t.Fatalf("well-formed status = %d, want 200", got)
	}
}

func TestCancelledUploadReturnsContextError(t *testing.T) {
	t.Parallel()

	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := ReadBounded(w, r, 1<<20)
		got <- err
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, pr)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.ContentLength = 1 << 20 // claim a large body we never finish sending

	go func() { _, _ = http.DefaultClient.Do(req) }()

	_, _ = pw.Write([]byte("{")) // send a sliver, then abandon
	cancel()                     // client disconnects mid-upload
	_ = pw.Close()

	select {
	case err := <-got:
		if err == nil {
			t.Fatal("ReadBounded returned nil, want a context error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client cancel")
	}
}
```

## Review

The guard is correct when both halves of the trust boundary hold: the size cap
returns a `*http.MaxBytesError` (translated to `413`), and the read honors
`r.Context()` so an abandoned upload returns a context error deterministically —
achieved by applying the "if context is done, return the context error" rule on
both `select` branches. The two mistakes it prevents are an unbounded
`io.ReadAll(r.Body)` (memory exhaustion) and conflating the oversize error with a
cancellation (they need different responses: `413` versus a quiet bail). Do not
reach for a `Decoder.DecodeContext` — it does not exist; decode the size-capped,
context-honored bytes with ordinary `json.Unmarshal`. Run with `-race`: the read
goroutine's result channel is buffered so it never blocks after a cancel.

## Resources

- [`http.MaxBytesReader`](https://pkg.go.dev/net/http#MaxBytesReader) — capping a request body and signaling the server to close.
- [`http.MaxBytesError`](https://pkg.go.dev/net/http#MaxBytesError) — the typed error whose `Limit` field carries the cap.
- [`encoding/json.Unmarshal`](https://pkg.go.dev/encoding/json#Unmarshal) — decoding the size-capped bytes (there is no `DecodeContext`).

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-outbound-httptrace-timing.md](08-outbound-httptrace-timing.md) | Next: [10-server-basecontext-shutdown-linkage.md](10-server-basecontext-shutdown-linkage.md)
