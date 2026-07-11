# Exercise 4: A Byte-Counting, Size-Limited Request Body

A struct that embeds the request body (`io.ReadCloser`), counts bytes as they are
read, and rejects reads past a maximum with a typed error — and whose `Close`
closes the underlying body exactly once. This is the wrapper behind every "413
Payload Too Large" and every ingress byte-count metric.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
meteredbody/                independent module: example.com/meteredbody
  go.mod                    go 1.26
  meteredbody.go            type Body (wraps io.ReadCloser); New, Read, Count, Close; ErrBodyTooLarge
  cmd/
    demo/
      main.go               read under the limit, then over it, then double-close
  meteredbody_test.go       under/over limit, idempotent Close, spy close-count, MaxBytesReader compare
```

- Files: `meteredbody.go`, `cmd/demo/main.go`, `meteredbody_test.go`.
- Implement: a `Body` wrapping an `io.ReadCloser` with a byte ceiling; `Read` returns `ErrBodyTooLarge` once the source exceeds the limit; `Count()` reports bytes delivered; `Close` closes the underlying body exactly once via `sync.Once`.
- Test: full read under the limit with correct `Count()`; the typed error over the limit with `Count()` pinned at the ceiling; idempotent `Close` with the underlying closer invoked exactly once (spy); a comparison test asserting `http.MaxBytesReader` yields `*http.MaxBytesError`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/meteredbody/cmd/demo
cd ~/go-exercises/meteredbody
go mod init example.com/meteredbody
```

### The overflow-detection subtlety and why Close uses sync.Once

Counting bytes is easy; detecting "too large" without lying about the count is the
subtle part. If you simply cap each read at `limit - count`, you can never tell a
body that is *exactly* the limit from one that is one byte over — both stop at the
ceiling. The technique here: while under the limit, read normally and accumulate
the count; once the count reaches the limit, do one small *probe* read of the
source. If the probe returns any bytes, the body exceeds the limit and `Read`
returns `ErrBodyTooLarge` with `Count()` pinned exactly at the ceiling; if the
probe returns EOF, the body was exactly the limit and `Read` reports a clean EOF.
The count therefore never exceeds `limit`, which is what the "413" path and the
metrics want.

`Close` must be idempotent because a request body is closed from the handler's
`defer` *and* sometimes from a helper that consumed it — a double close of a real
`http.Response.Body` can double-return a connection to the pool. A `sync.Once`
guarantees the underlying `Close` runs exactly once and every caller sees the same
recorded error.

`http.MaxBytesReader` does a closely related job and is the right tool when you have
an `http.ResponseWriter` (it can signal the client and close the connection). This
wrapper is the right tool when you only have a body and want a plain byte count plus
a typed limit error — for a queue consumer, a proxy, or a non-HTTP transport. The
comparison test documents that `MaxBytesReader` yields `*http.MaxBytesError`.

Create `meteredbody.go`:

```go
package meteredbody

import (
	"errors"
	"io"
	"sync"
)

// ErrBodyTooLarge is returned once the wrapped source exceeds the byte ceiling.
var ErrBodyTooLarge = errors.New("meteredbody: request body too large")

// Body wraps an io.ReadCloser (an HTTP request body), counts the bytes read, and
// refuses to deliver more than limit bytes. Close closes the underlying body once.
type Body struct {
	rc    io.ReadCloser
	limit int64
	count int64
	once  sync.Once
	err   error
}

// New wraps rc, enforcing a ceiling of limit bytes.
func New(rc io.ReadCloser, limit int64) *Body {
	return &Body{rc: rc, limit: limit}
}

// Read delivers up to the ceiling. Once the ceiling is reached it probes the
// source: any remaining data means the body is too large.
func (b *Body) Read(p []byte) (int, error) {
	if b.count >= b.limit {
		var probe [1]byte
		n, _ := b.rc.Read(probe[:])
		if n > 0 {
			return 0, ErrBodyTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.limit-b.count {
		p = p[:b.limit-b.count]
	}
	n, err := b.rc.Read(p)
	b.count += int64(n)
	return n, err
}

// Count reports the number of bytes delivered so far, never exceeding the limit.
func (b *Body) Count() int64 { return b.count }

// Close closes the underlying body exactly once, returning the recorded error.
func (b *Body) Close() error {
	b.once.Do(func() {
		b.err = b.rc.Close()
	})
	return b.err
}

// Static assertion: *Body satisfies the io.ReadCloser it wraps.
var _ io.ReadCloser = (*Body)(nil)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"strings"

	"example.com/meteredbody"
)

func main() {
	small := meteredbody.New(io.NopCloser(strings.NewReader("hello")), 1024)
	data, err := io.ReadAll(small)
	fmt.Printf("read %q count=%d err=%v\n", data, small.Count(), err)

	big := meteredbody.New(io.NopCloser(strings.NewReader("this body is far too long")), 8)
	_, err = io.ReadAll(big)
	fmt.Printf("over limit: count=%d err=%v\n", big.Count(), err)

	fmt.Println("close:", big.Close())
	fmt.Println("close again:", big.Close())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read "hello" count=5 err=<nil>
over limit: count=8 err=meteredbody: request body too large
close: <nil>
close again: <nil>
```

### Tests

Create `meteredbody_test.go`:

```go
package meteredbody

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// spyCloser records how many times Close was called.
type spyCloser struct {
	io.Reader
	closes int
}

func (s *spyCloser) Close() error {
	s.closes++
	return nil
}

func TestUnderLimit(t *testing.T) {
	t.Parallel()
	b := New(io.NopCloser(strings.NewReader("hello")), 1024)
	data, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", data)
	}
	if b.Count() != 5 {
		t.Fatalf("Count = %d, want 5", b.Count())
	}
}

func TestOverLimit(t *testing.T) {
	t.Parallel()
	b := New(io.NopCloser(strings.NewReader("0123456789")), 4)
	_, err := io.ReadAll(b)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if b.Count() != 4 {
		t.Fatalf("Count = %d, want 4 (ceiling)", b.Count())
	}
}

func TestExactlyLimitIsClean(t *testing.T) {
	t.Parallel()
	b := New(io.NopCloser(strings.NewReader("1234")), 4)
	data, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("exactly-limit body errored: %v", err)
	}
	if string(data) != "1234" {
		t.Fatalf("data = %q, want 1234", data)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	spy := &spyCloser{Reader: strings.NewReader("x")}
	b := New(spy, 16)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if spy.closes != 1 {
		t.Fatalf("underlying closed %d times, want 1", spy.closes)
	}
}

func TestMaxBytesReaderYieldsMaxBytesError(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789"))
	rec := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(rec, req.Body, 4)

	_, err := io.ReadAll(req.Body)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v, want *http.MaxBytesError", err)
	}
	if mbe.Limit != 4 {
		t.Fatalf("MaxBytesError.Limit = %d, want 4", mbe.Limit)
	}
}
```

## Review

The wrapper is correct when `Count()` is monotone, never exceeds `limit`, and
equals the exact number of bytes the caller received; when a body one byte over the
limit produces `ErrBodyTooLarge` while a body exactly at the limit produces a clean
EOF (`TestExactlyLimitIsClean` is the guard against an off-by-one that rejects
valid bodies); and when `Close` invokes the underlying closer exactly once no
matter how often it is called. The `spyCloser` count is the proof against the
connection-leak-or-double-free failure mode. The `MaxBytesReader` comparison test
is there so you reach for the stdlib tool on the HTTP path and this wrapper only
where you truly have just a body.

## Resources

- [`http.MaxBytesReader`](https://pkg.go.dev/net/http#MaxBytesReader) and [`http.MaxBytesError`](https://pkg.go.dev/net/http#MaxBytesError) — the HTTP-path size limiter and its typed error.
- [`io.LimitReader`](https://pkg.go.dev/io#LimitReader) and [`io.NopCloser`](https://pkg.go.dev/io#NopCloser) — the plain limiter and the reader-to-ReadCloser adapter.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — the guard that makes Close idempotent.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-responsecontroller-forwarding.md](03-responsecontroller-forwarding.md) | Next: [05-gzip-decoding-readcloser.md](05-gzip-decoding-readcloser.md)
