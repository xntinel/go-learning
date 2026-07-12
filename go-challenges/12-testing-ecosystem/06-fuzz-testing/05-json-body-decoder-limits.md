# Exercise 5: Fuzz A Bounded JSON Request-Body Decoder

An API handler decodes a JSON request body into a struct. Two things must hold no
matter what a client sends: the decoder must never read more than a fixed byte
budget (an unbounded body is a memory-exhaustion attack), and it must never
panic on malformed input — only ever return a typed error. This module builds a
generic `DecodeBody` and fuzzes it with arbitrary bytes, using a counting reader
to prove the budget is never exceeded.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
apireq/                    independent module: example.com/apireq
  go.mod                   module path
  decode.go                DecodeBody[T any](io.Reader, int64) (T, error)
  cmd/
    demo/
      main.go              decode a good body and a too-large body, print results
  decode_test.go           TestDecodeBody, FuzzDecodeBody (byte budget), Example
```

Files: `decode.go`, `cmd/demo/main.go`, `decode_test.go`.
Implement: `DecodeBody[T any](r io.Reader, max int64) (T, error)` limiting the
reader and using a strict `json.Decoder`.
Test: a table test asserting typed errors; `FuzzDecodeBody` proving no panic and
`bytesRead <= max`.
Verify: `go test -race ./...`, then `go test -fuzz=FuzzDecodeBody -fuzztime=2s`.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/06-fuzz-testing/05-json-body-decoder-limits/cmd/demo
cd go-solutions/12-testing-ecosystem/06-fuzz-testing/05-json-body-decoder-limits
```

### Limiting the reader and rejecting unknown fields

`DecodeBody` wraps the incoming reader in `io.LimitReader(r, max)` so the decoder
can pull at most `max` bytes from the underlying stream, then builds a
`json.Decoder` and calls `DisallowUnknownFields` so a body with fields the struct
does not declare is rejected rather than silently dropped. That strictness is a
real API-hardening choice: it turns a client typo or an injection attempt into a
`400` instead of a silently-ignored field.

The limiting deserves a precise note, because it differs from the other common
tool. `io.LimitReader` returns `io.EOF` once `max` bytes are consumed, so an
over-budget body looks *identical to a truncated one* — the decoder reports an
"unexpected EOF". `http.MaxBytesReader`, by contrast, returns a distinct
"request body too large" error and is the right choice inside a real
`net/http` handler because it also caps the `ResponseWriter`. For the robustness
property we care about here — never read past the budget, never panic — either
limiter works, and `io.LimitReader` keeps the module dependency-free.

The fuzz property is a robustness property with a resource bound. The body feeds
arbitrary bytes through a `countingReader` (which records exactly how many bytes
the decoder pulled) into `DecodeBody`, and asserts two things: the call returned
(a value or an error, never a panic), and `countingReader.n <= max`. The counting
reader is the honest way to measure the budget: it sits *beneath* the
`io.LimitReader` and sees every real read, so if the limit were ever bypassed the
count would exceed `max` and the test would fail.

Create `decode.go`:

```go
package apireq

import (
	"encoding/json"
	"io"
)

// DecodeBody reads at most max bytes from r and decodes a single JSON value of
// type T, rejecting unknown fields. It never reads past max bytes and returns a
// typed json error (never a panic) on malformed input.
func DecodeBody[T any](r io.Reader, max int64) (T, error) {
	var v T
	dec := json.NewDecoder(io.LimitReader(r, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/apireq"
)

type user struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func main() {
	const max = 64

	u, err := apireq.DecodeBody[user](strings.NewReader(`{"name":"alice","age":30}`), max)
	fmt.Printf("good: %+v err=%v\n", u, err)

	big := `{"name":"` + strings.Repeat("x", 200) + `"}`
	_, err = apireq.DecodeBody[user](strings.NewReader(big), max)
	fmt.Printf("over budget: err=%v\n", err)

	_, err = apireq.DecodeBody[user](strings.NewReader(`{"name":"a","role":"admin"}`), max)
	fmt.Printf("unknown field: err=%v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: {Name:alice Age:30} err=<nil>
over budget: err=unexpected EOF
unknown field: err=json: unknown field "role"
```

### Tests

Create `decode_test.go`:

```go
package apireq

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

type payload struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// countingReader records how many bytes are actually read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestDecodeBody(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		got, err := DecodeBody[payload](bytes.NewReader([]byte(`{"name":"go","age":3}`)), 1024)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (payload{Name: "go", Age: 3}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("syntax error is typed", func(t *testing.T) {
		t.Parallel()
		_, err := DecodeBody[payload](bytes.NewReader([]byte(`{"name":]`)), 1024)
		var se *json.SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("err = %v, want *json.SyntaxError", err)
		}
	})
	t.Run("unknown field rejected", func(t *testing.T) {
		t.Parallel()
		_, err := DecodeBody[payload](bytes.NewReader([]byte(`{"name":"x","extra":1}`)), 1024)
		if err == nil {
			t.Fatal("want error for unknown field, got nil")
		}
	})
	t.Run("empty body is EOF", func(t *testing.T) {
		t.Parallel()
		_, err := DecodeBody[payload](bytes.NewReader(nil), 1024)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	})
}

func FuzzDecodeBody(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"name":"go","age":3}`),
		[]byte(`{"name":"x"`),
		[]byte(`[[[[[[]]]]]]`),
		[]byte("\x00\xff not json at all"),
		bytes.Repeat([]byte("A"), 4096),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		const max = 128
		cr := &countingReader{r: bytes.NewReader(data)}
		_, _ = DecodeBody[payload](cr, max) // must not panic; error is fine
		if cr.n > max {
			t.Fatalf("decoder read %d bytes, budget was %d", cr.n, max)
		}
	})
}

func Example() {
	u, err := DecodeBody[payload](bytes.NewReader([]byte(`{"name":"go","age":1}`)), 64)
	fmt.Printf("%+v %v\n", u, err)
	// Output: {Name:go Age:1} <nil>
}
```

## Review

The decoder is correct when arbitrary bytes always produce either a decoded value
or a typed `json` error, never a panic, and the counting reader never sees more
than `max` bytes. The byte-budget property is the one that would otherwise slip
through example tests: a handler that forgets the limiter passes every unit test
and falls over the first time a client streams a gigabyte. Note the deliberate
choice of `io.LimitReader` (over-budget looks like truncation) versus
`http.MaxBytesReader` (distinct error) — the demo's "unexpected EOF" for the
over-budget body is the visible consequence. Run `go test -race ./...`, then
`go test -fuzz=FuzzDecodeBody -fuzztime=2s`.

## Resources

- [`encoding/json.Decoder`](https://pkg.go.dev/encoding/json#Decoder) — `NewDecoder`, `Decode`, and `DisallowUnknownFields`.
- [`io.LimitReader`](https://pkg.go.dev/io#LimitReader) — the budget-enforcing wrapper.
- [`net/http.MaxBytesReader`](https://pkg.go.dev/net/http#MaxBytesReader) — the handler-side limiter with a distinct over-budget error.

---

Back to [04-no-panic-header-parser.md](04-no-panic-header-parser.md) | Next: [06-regression-corpus-from-crash.md](06-regression-corpus-from-crash.md)
