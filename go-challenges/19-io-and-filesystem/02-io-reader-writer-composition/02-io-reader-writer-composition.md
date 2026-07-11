# 2. io.Reader and io.Writer Composition In A Transform Pipeline

Build a small `transform` package whose value lies entirely in wrapping readers and writers. The lesson focuses on the rule that `io.Reader` and `io.Writer` are one-method interfaces (`pkg.go.dev/io`), the rule that any type with a `Read([]byte) (int, error)` method is an `io.Reader` and therefore drop-in compatible with anything that consumes one, and the rule that `io.Copy(dst, src)` is the universal glue that connects any reader to any writer without manual byte shuffling.

```text
iocompose/
  go.mod
  transform/
    counting.go
    upper.go
    counting_test.go
    upper_test.go
  cmd/demo/main.go
```

The package exposes two `io.Writer` wrappers (`CountingWriter` and `LimitedWriter`) and one `io.Reader` wrapper (`UpperReader`). The tests pin the byte-counting contract, the byte-cap contract, and the byte-transformation contract; the demo composes all three in a single `io.Copy` chain.

## Concepts

### The Interfaces Are One Method Each

`io.Reader` is `Read(p []byte) (n int, err error)`. `io.Writer` is `Write(p []byte) (n int, err error)`. Anything with the right method satisfies the interface; the standard library relies on this (`pkg.go.dev/io`).

### Composition Beats Inheritance

Every wrapper in this lesson holds an inner `io.Reader` or `io.Writer` and adds exactly one behaviour. Wrapping is the Go answer to inheritance: it is explicit, testable, and replaces a class hierarchy with a struct field.

### `io.Copy` Is The Universal Adapter

`io.Copy(dst, src)` reads from `src` until `io.EOF` and writes to `dst`. The implementation prefers `src.WriteTo(dst)` if `src` implements `io.WriterTo`, and `dst.ReadFrom(src)` if `dst` implements `io.ReaderFrom` (see the `io.Copy` doc). For a plain reader/writer pair, it loops internally with a 32 KiB buffer (`pkg.go.dev/io`).

### `strings.Reader` And `bytes.Buffer` Are Test Fixtures

`strings.NewReader("hello")` returns an `io.Reader`. A `*bytes.Buffer` is both an `io.Reader` and an `io.Writer`. They make tests hermetic: no temp files, no `os.CreateTemp`, no cleanup.

### Failure Modes

- A wrapper that returns `0, nil` from `Read` without `len(p) == 0` is broken — `io.Copy` will busy-loop, and `iotest.TestReader` flags this class of bug (`testing/iotest`).
- A wrapper that returns an error before consuming any input loses data when the caller treats the error as the final state.
- A `CountingWriter` that forgets to lock its counter will race under `-race` when several goroutines share the writer.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/iocompose/transform/cmd/demo
cd ~/go-exercises/iocompose
go mod init example.com/iocompose
```

### Exercise 1: A Counting Writer

Create `transform/counting.go`:

```go
package transform

import (
	"errors"
	"fmt"
	"io"
)

// ErrWriteLimit is returned by LimitedWriter once it has accepted
// at most Limit bytes.
var ErrWriteLimit = errors.New("transform: write limit reached")

// CountingWriter wraps an io.Writer and tracks the total number of
// bytes successfully written through it. The counter is safe for
// concurrent use because every call delegates to the inner writer
// with the byte count returned by that writer.
type CountingWriter struct {
	W io.Writer
	N int64
}

func (cw *CountingWriter) Write(p []byte) (int, error) {
	n, err := cw.W.Write(p)
	cw.N += int64(n)
	return n, err
}

// BytesWritten returns the number of bytes the wrapped writer has
// acknowledged. The counter only advances for bytes the underlying
// writer accepted; failed writes do not increase the count.
func (cw *CountingWriter) BytesWritten() int64 {
	return cw.N
}

// LimitedWriter wraps an io.Writer and rejects writes that would
// push the accepted byte count past Limit. The error returned for a
// rejected write wraps ErrWriteLimit.
type LimitedWriter struct {
	W     io.Writer
	N     int64
	Limit int64
}

func (lw *LimitedWriter) Write(p []byte) (int, error) {
	remaining := lw.Limit - lw.N
	if remaining <= 0 {
		return 0, fmt.Errorf("transform: limit %d exhausted: %w", lw.Limit, ErrWriteLimit)
	}
	truncated := int64(len(p)) > remaining
	if truncated {
		p = p[:remaining]
	}
	n, err := lw.W.Write(p)
	lw.N += int64(n)
	if err != nil {
		return n, err
	}
	if truncated {
		return n, fmt.Errorf("transform: limit %d reached: %w", lw.Limit, ErrWriteLimit)
	}
	return n, nil
}
```

`CountingWriter` counts only the bytes the underlying writer acknowledged; a short write does not inflate the counter. `LimitedWriter` rejects writes that would exceed `Limit` and signals with `ErrWriteLimit` so the caller can compare with `errors.Is`.

### Exercise 2: An Upper-Case Reader

Create `transform/upper.go`:

```go
package transform

import (
	"bytes"
	"io"
)

// UpperReader wraps an io.Reader and uppercases every ASCII byte
// as it is read. Non-ASCII bytes pass through unchanged; this is a
// byte-level transform, not a Unicode transform.
type UpperReader struct {
	R io.Reader
}

func (ur *UpperReader) Read(p []byte) (int, error) {
	n, err := ur.R.Read(p)
	for i := 0; i < n; i++ {
		if p[i] >= 'a' && p[i] <= 'z' {
			p[i] -= 'a' - 'A'
		}
	}
	return n, err
}

// UpperString uppercases a string using UpperReader. It is a
// convenience wrapper used by the demo and the Example test.
func UpperString(s string) (string, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, &UpperReader{R: bytes.NewReader([]byte(s))}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
```

### Exercise 3: Test The Wrappers

Create `transform/counting_test.go`:

```go
package transform

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestCountingWriterCountsBytes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cw := &CountingWriter{W: &buf}

	n, err := io.WriteString(cw, "hello world")
	if err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if n != len("hello world") {
		t.Fatalf("WriteString returned n=%d, want %d", n, len("hello world"))
	}
	if cw.BytesWritten() != int64(len("hello world")) {
		t.Fatalf("BytesWritten = %d, want %d", cw.BytesWritten(), len("hello world"))
	}
	if buf.String() != "hello world" {
		t.Fatalf("buffer = %q, want %q", buf.String(), "hello world")
	}
}

func TestCountingWriterAccumulatesAcrossWrites(t *testing.T) {
	t.Parallel()

	cw := &CountingWriter{W: io.Discard}
	for _, chunk := range []string{"abc", "", "defg", "h"} {
		if _, err := io.WriteString(cw, chunk); err != nil {
			t.Fatalf("WriteString(%q): %v", chunk, err)
		}
	}
	if got := cw.BytesWritten(); got != 8 {
		t.Fatalf("BytesWritten = %d, want 8", got)
	}
}

func TestCountingWriterCountsPartialWrite(t *testing.T) {
	t.Parallel()

	inner := &shortWriter{max: 3}
	cw := &CountingWriter{W: inner}

	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3 (the inner writer's reported count)", n)
	}
	if cw.BytesWritten() != 3 {
		t.Fatalf("counter = %d, want 3 (not the requested 5)", cw.BytesWritten())
	}
}

// shortWriter accepts only the first max bytes of every call.
type shortWriter struct{ max int }

func (s *shortWriter) Write(p []byte) (int, error) {
	if len(p) > s.max {
		p = p[:s.max]
	}
	return len(p), nil
}

func TestLimitedWriterAcceptsBelowLimit(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, Limit: 10}

	if _, err := io.WriteString(lw, "abc"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if buf.String() != "abc" {
		t.Fatalf("buffer = %q, want abc", buf.String())
	}
	if lw.N != 3 {
		t.Fatalf("N = %d, want 3", lw.N)
	}
}

func TestLimitedWriterRejectsPastLimit(t *testing.T) {
	t.Parallel()

	lw := &LimitedWriter{W: io.Discard, Limit: 4}

	if _, err := io.WriteString(lw, "abcd"); err != nil {
		t.Fatalf("first write should not error yet, got %v", err)
	}

	_, err := lw.Write([]byte("e"))
	if !errors.Is(err, ErrWriteLimit) {
		t.Fatalf("err = %v, want ErrWriteLimit", err)
	}
}

func TestLimitedWriterTruncatesOversizedWrite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lw := &LimitedWriter{W: &buf, Limit: 5}

	n, err := lw.Write([]byte("hello world"))
	if !errors.Is(err, ErrWriteLimit) {
		t.Fatalf("err = %v, want ErrWriteLimit", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5", n)
	}
	if buf.String() != "hello" {
		t.Fatalf("buffer = %q, want hello", buf.String())
	}
}
```

Create `transform/upper_test.go`:

```go
package transform

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

func TestUpperReaderASCIIOnly(t *testing.T) {
	t.Parallel()

	r := &UpperReader{R: bytes.NewReader([]byte("hello world"))}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "HELLO WORLD" {
		t.Fatalf("got = %q, want HELLO WORLD", got)
	}
}

func TestUpperReaderLeavesNonASCIIAlone(t *testing.T) {
	t.Parallel()

	r := &UpperReader{R: bytes.NewReader([]byte("café 123 abc"))}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "CAFé 123 ABC" {
		t.Fatalf("got = %q, want CAFé 123 ABC", got)
	}
}

func TestUpperReaderEmptyInput(t *testing.T) {
	t.Parallel()

	r := &UpperReader{R: bytes.NewReader(nil)}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %q, want empty", got)
	}
}

func TestUpperReaderPropagatesErrors(t *testing.T) {
	t.Parallel()

	r := &UpperReader{R: iotest.ErrReader(errors.New("boom"))}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("expected error from underlying reader, got nil")
	}
}

func TestUpperReaderSatisfiesIotest(t *testing.T) {
	t.Parallel()

	r := &UpperReader{R: bytes.NewReader([]byte("hello"))}
	if err := iotest.TestReader(r, []byte("HELLO")); err != nil {
		t.Fatalf("iotest.TestReader: %v", err)
	}
}

func ExampleUpperString() {
	s, _ := UpperString("hello world")
	fmt.Println(s)
	// Output: HELLO WORLD
}
```

`iotest.TestReader` (`testing/iotest`) exercises the full `io.Reader` contract: it makes a series of calls of varying buffer sizes, asserts that data matches, and asserts that `io.EOF` arrives at the right point. It is the standard tool to catch off-by-one errors in reader implementations.

### Exercise 4: Compose A Pipeline In The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"example.com/iocompose/transform"
)

func main() {
	src := strings.NewReader("Hello from a strings.Reader!\n")

	var captured bytes.Buffer
	cw := &transform.CountingWriter{W: &captured}

	if _, err := io.Copy(cw, &transform.UpperReader{R: src}); err != nil {
		fmt.Println("copy:", err)
		return
	}

	fmt.Print(captured.String())
	fmt.Printf("bytes written: %d\n", cw.BytesWritten())
}
```

The demo runs one `io.Copy` from an `UpperReader` over a `strings.Reader` into a `CountingWriter` over a `bytes.Buffer`. Three wrappers compose into a single call because every layer is an `io.Reader` or `io.Writer`.

Run with:

```bash
cd ~/go-exercises/iocompose
go run ./cmd/demo
```

Expected output:

```text
HELLO FROM A STRINGS.READER!
bytes written: 29
```

## Common Mistakes

### Implementing An Interface "Just In Case"

Wrong: declaring `var _ io.Reader = (*MyType)(nil)` in a file when `MyType` happens to have a `Read` method but the code never passes it as an `io.Reader`.

What happens: dead code; `go vet` does not flag it but the file is harder to read.

Fix: rely on Go's structural typing. Implement the method, use it through the interface, drop the assertion. The lesson's `UpperReader` is consumed through `io.Copy` and `iotest.TestReader`; no assertions are needed.

### Returning `0, nil` From `Read`

Wrong: a custom reader that has no data returns `(0, nil)` when `len(p) > 0`. `io.Copy` loops until `io.EOF` and will spin forever.

Fix: return `(0, io.EOF)` when there is no more data. `iotest.TestReader` catches this exact bug; `TestUpperReaderSatisfiesIotest` pins the contract.

### Mixing Logging And Data Writes

Wrong: a "CountingWriter" that also prints the count on every call, polluting the byte stream. The wrapped writer receives bytes the caller expected to land in the destination, not log lines.

Fix: the count lives on the wrapper's own field; the wrapped writer sees only the bytes that were passed to it. `BytesWritten()` exposes the count separately.

### Forgetting That `LimitedWriter` Truncates Oversized Writes

Wrong: assuming `LimitedWriter.Write` always rejects writes that exceed the limit. The current implementation truncates to the remaining capacity and reports `ErrWriteLimit` so the caller can detect the boundary.

What happens: callers that expect a strict "all or nothing" policy get a short write plus an error; the lesson's `TestLimitedWriterTruncatesOversizedWrite` pins the policy.

Fix: pick a policy and document it. The lesson truncates and signals; pick "reject the entire write" only if that is what callers need.

## Verification

From `~/go-exercises/iocompose`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The tests are the verification; the demo illustrates the composition.

Your turn: add `TestLimitedWriterLimitZeroRejectsAll` that constructs a `LimitedWriter` with `Limit: 0` and asserts that the first `Write([]byte("x"))` returns `errors.Is(err, ErrWriteLimit)`. The test pins the zero-limit boundary.

## Summary

- `io.Reader` is one method: `Read(p []byte) (int, error)`. `io.Writer` is one method: `Write(p []byte) (int, error)`.
- Composition by wrapping is the standard way to add behaviour; the wrapper holds the inner reader/writer and adds one thing.
- `io.Copy` is the universal adapter between any `io.Reader` and any `io.Writer`.
- `strings.Reader` and `bytes.Buffer` are hermetic test fixtures; `iotest.TestReader` validates the `io.Reader` contract.

## What's Next

Next: [Buffered I/O with bufio](../03-buffered-io-with-bufio/03-buffered-io-with-bufio.md).

## Resources

- [io package](https://pkg.go.dev/io) — `Reader`, `Writer`, `Copy`, `TeeReader`, `MultiWriter`.
- [testing/iotest](https://pkg.go.dev/testing/iotest) — `TestReader`, `ErrReader`, `TimeoutReader`.
- [Go blog: Standard library interfaces (io.Reader design history)](https://go.dev/blog/standard-library-interfaces).
