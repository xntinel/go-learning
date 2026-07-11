# Exercise 6: A MaxBytes Body Guard That Rejects Oversized Streams

An unbounded request body is a denial-of-service vector: a client that streams
gigabytes into a handler that calls `io.ReadAll` will OOM the process. The guard is
a reader that caps total bytes and distinguishes a clean under-limit stream from a
limit breach. This exercise builds that guard on `net/http.MaxBytesReader`,
contrasts it with the silently-truncating `io.LimitReader`, and grounds it in an
`httptest`-backed handler.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
bodyguard/                  independent module: example.com/bodyguard
  go.mod                    module example.com/bodyguard
  guard.go                  ReadBounded (MaxBytesReader) and ReadTruncating (LimitReader) for contrast
  cmd/
    demo/
      main.go               reads under, at, and over the limit; shows both behaviors
  guard_test.go             under/at read clean; over -> *http.MaxBytesError via errors.As; LimitReader truncates; httptest handler
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `ReadBounded(body io.ReadCloser, limit int64) ([]byte, error)` using `http.MaxBytesReader`, and `ReadTruncating(r io.Reader, limit int64) ([]byte, error)` using `io.LimitReader` for the contrast.
Test: payloads of `limit-1`, `limit`, `limit+1` — under/at read to `io.EOF`, over returns `*http.MaxBytesError` via `errors.As`; `io.LimitReader` truncates silently; an `httptest` handler enforces the cap.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/bodyguard/cmd/demo
cd ~/go-exercises/bodyguard
go mod init example.com/bodyguard
```

### Two caps that look alike and behave oppositely

`io.LimitReader(r, n)` and `http.MaxBytesReader(w, r, n)` both stop after `n`
bytes, but their behavior on an oversized stream is the difference between a bug
and a guard. `io.LimitReader` reads at most `n` bytes and then returns `io.EOF`
with **no error**: an oversized body reads as a valid short body, silently
truncated. That is correct when you deliberately want a prefix ("read the first 512
bytes to sniff a content type") and catastrophic as a body-size limit, because the
handler never learns the client sent too much and processes a corrupted, truncated
request as if it were complete.

`http.MaxBytesReader` allows exactly `n` bytes and returns a typed
`*http.MaxBytesError` the moment the client tries to send more. That typed error is
the whole point: the handler can `errors.As` it, respond `413 Request Entity Too
Large`, and reject the request instead of accepting a truncation. It also flags the
underlying `http.ResponseWriter` (when one is passed) so the server stops reading
the connection. In a test we pass `nil` for the writer, which is safe — the guard
still returns the typed error.

The boundary behavior is exact and worth pinning: at `limit` bytes the read
completes to `io.EOF` with no error; at `limit + 1` the read returns
`*http.MaxBytesError` whose `Limit` field equals the configured cap. The test
drives `limit-1`, `limit`, and `limit+1` to lock all three.

Create `guard.go`:

```go
package bodyguard

import (
	"io"
	"net/http"
)

// ReadBounded reads body but rejects a stream that exceeds limit bytes, returning
// a *http.MaxBytesError once the client sends more than limit. A stream of at most
// limit bytes reads cleanly. Pass a nil ResponseWriter outside a live server.
func ReadBounded(body io.ReadCloser, limit int64) ([]byte, error) {
	limited := http.MaxBytesReader(nil, body, limit)
	return io.ReadAll(limited)
}

// ReadTruncating reads at most limit bytes and then stops with no error. It is the
// UNSAFE choice for a body cap: an oversized stream is silently truncated. Shown
// here only to contrast with ReadBounded.
func ReadTruncating(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
```

### The runnable demo

The demo runs a payload of `limit+1` bytes through both functions to show the
difference: the guard reports a `*http.MaxBytesError`; the truncating reader
silently returns `limit` bytes with no error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/bodyguard"
)

func main() {
	const limit = 8
	payload := strings.Repeat("x", limit+1) // one byte over

	_, err := bodyguard.ReadBounded(io.NopCloser(strings.NewReader(payload)), limit)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		fmt.Printf("guard: rejected, limit=%d\n", maxErr.Limit)
	}

	out, err := bodyguard.ReadTruncating(strings.NewReader(payload), limit)
	fmt.Printf("truncating: read %d bytes, err=%v\n", len(out), err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
guard: rejected, limit=8
truncating: read 8 bytes, err=<nil>
```

### Tests

`TestBoundaryCases` drives `limit-1`, `limit`, and `limit+1` through `ReadBounded`,
asserting the first two read cleanly and the third returns a `*http.MaxBytesError`
via `errors.As`. `TestLimitReaderTruncatesSilently` proves `io.LimitReader` returns
`limit` bytes and a nil error on an oversized stream — the unsafe behavior.
`TestHandlerRejectsOversizedBody` mounts a real handler over `httptest` that caps
the body and writes `413` on a breach.

Create `guard_test.go`:

```go
package bodyguard

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const limit = 16

func body(n int) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Repeat("a", n)))
}

func TestBoundaryCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"under limit", limit - 1, false},
		{"at limit", limit, false},
		{"over limit", limit + 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadBounded(body(tc.size), limit)
			if tc.wantErr {
				var maxErr *http.MaxBytesError
				if !errors.As(err, &maxErr) {
					t.Fatalf("err = %v, want *http.MaxBytesError", err)
				}
				if maxErr.Limit != limit {
					t.Fatalf("Limit = %d, want %d", maxErr.Limit, limit)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != tc.size {
				t.Fatalf("read %d bytes, want %d", len(got), tc.size)
			}
		})
	}
}

func TestLimitReaderTruncatesSilently(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("a", limit+10)
	got, err := ReadTruncating(strings.NewReader(oversized), limit)
	if err != nil {
		t.Fatalf("LimitReader returned err %v, want nil (it truncates silently)", err)
	}
	if len(got) != limit {
		t.Fatalf("read %d bytes, want %d (silently truncated)", len(got), limit)
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	h := func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if _, err := io.ReadAll(r.Body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", limit+5)))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
```

## Review

The guard is correct when it returns a typed `*http.MaxBytesError` on breach so the
caller can reject rather than silently accept a truncated body. The contrast test
is the lesson's spine: `io.LimitReader` returns a nil error on an oversized stream,
which is exactly why it must never be used as a body cap even though it looks
interchangeable. The boundary triple — `limit-1`, `limit`, `limit+1` — pins the
off-by-one that separates "read cleanly" from "reject". The `httptest` handler
grounds it: a real handler wraps `r.Body` in `MaxBytesReader`, checks with
`errors.As`, and answers `413`. Run `go test -race` to confirm all three.

## Resources

- [`net/http.MaxBytesReader`](https://pkg.go.dev/net/http#MaxBytesReader) — the body cap that returns a typed error.
- [`net/http.MaxBytesError`](https://pkg.go.dev/net/http#MaxBytesError) — the `Limit` field checked with `errors.As`.
- [`io.LimitReader`](https://pkg.go.dev/io#LimitReader) — the silently-truncating reader to contrast against.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-timeout-reader-retry-loop.md](05-timeout-reader-retry-loop.md) | Next: [07-line-framing-scanner-across-boundaries.md](07-line-framing-scanner-across-boundaries.md)
