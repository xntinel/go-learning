# 16. Implementing a Custom io.Reader

Build a custom `io.Reader` that applies ROT13 to bytes from another reader. The lesson focuses on the `Read` contract: return the number of bytes produced, preserve EOF behavior, and avoid hiding errors from the underlying reader.

## Concepts

### Read Mutates The Provided Buffer

An `io.Reader` receives a caller-owned byte slice and fills up to `len(p)` bytes. It returns `n` and an error. The caller must only inspect `p[:n]`.

### EOF Is A Signal, Not Data

A reader commonly returns `n > 0` with `err == nil`, then returns `0, io.EOF` on the next call. Do not convert EOF into another error.

### Wrapping Readers Should Preserve Contracts

A transforming reader should call the underlying reader, transform only the bytes actually read, and return the same `n` and error unless it has its own validation failure.

## Exercises

### Exercise 1: Implement The Reader

Create `reader.go`:

```go
package rotreader

import (
	"fmt"
	"io"
)

type Reader struct {
	r io.Reader
}

func New(r io.Reader) (*Reader, error) {
	if r == nil {
		return nil, fmt.Errorf("new rot reader: %w", ErrNilReader)
	}
	return &Reader{r: r}, nil
}

func (r *Reader) Read(p []byte) (int, error) {
	if r == nil || r.r == nil {
		return 0, fmt.Errorf("read rot: %w", ErrNilReader)
	}
	n, err := r.r.Read(p)
	for i := 0; i < n; i++ {
		p[i] = rotate(p[i])
	}
	return n, err
}

func rotate(b byte) byte {
	switch {
	case b >= 'a' && b <= 'z':
		return 'a' + (b-'a'+13)%26
	case b >= 'A' && b <= 'Z':
		return 'A' + (b-'A'+13)%26
	default:
		return b
	}
}
```

Create `errors.go`:

```go
package rotreader

import "errors"

var ErrNilReader = errors.New("reader must not be nil")
```

### Exercise 2: Test With iotest

Create `reader_test.go`:

```go
package rotreader

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReaderTransformsBytes(t *testing.T) {
	t.Parallel()

	r, err := New(strings.NewReader("Hello, Go!"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "Uryyb, Tb!" {
		t.Fatalf("data = %q", data)
	}
}

func TestReaderSatisfiesIOTReaderContract(t *testing.T) {
	t.Parallel()

	r, err := New(strings.NewReader("abcXYZ"))
	if err != nil {
		t.Fatal(err)
	}
	if err := iotest.TestReader(r, []byte("nopKLM")); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsNilReader(t *testing.T) {
	t.Parallel()

	_, err := New(nil)
	if !errors.Is(err, ErrNilReader) {
		t.Fatalf("err = %v, want ErrNilReader", err)
	}
}

func ExampleReader() {
	r, _ := New(strings.NewReader("abc"))
	data, _ := io.ReadAll(r)
	fmt.Println(string(data))
	// Output: nop
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"io"
	"log"
	"os"
	"strings"

	"example.com/rotreader"
)

func main() {
	r, err := rotreader.New(strings.NewReader("Hello, Go!\n"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := io.Copy(os.Stdout, r); err != nil {
		log.Fatal(err)
	}
}
```

## Common Mistakes

### Transforming The Whole Buffer

Wrong: loop over `range p` after the underlying read.

Fix: transform only `p[:n]`; the rest of the buffer is not new data.

### Swallowing io.EOF

Wrong: replace every underlying error with nil.

Fix: return the same error from the underlying reader unless the wrapper itself failed.

### Skipping Reader Contract Tests

Wrong: test only one `io.ReadAll` happy path.

Fix: use `testing/iotest.TestReader` to exercise short reads and EOF behavior.

## Verification

Run this from `~/go-exercises/rotreader`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test proving applying ROT13 twice returns the original text.

## Summary

- A custom reader must honor the `Read(p []byte) (n int, err error)` contract.
- Transform only bytes actually read.
- Preserve EOF and underlying errors.
- `testing/iotest` helps verify reader behavior.

## What's Next

Next: [Structured Logging with Rotation](../17-structured-logging-rotation/17-structured-logging-rotation.md).

## Resources

- [io.Reader](https://pkg.go.dev/io#Reader)
- [testing/iotest](https://pkg.go.dev/testing/iotest)
- [io.ReadAll](https://pkg.go.dev/io#ReadAll)
