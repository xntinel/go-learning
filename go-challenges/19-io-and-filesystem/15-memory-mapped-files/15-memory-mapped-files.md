# 15. Memory-Mapped Files

Build a portable page-window reader that models the access pattern of memory-mapped files without using platform-specific `mmap` calls. Go's standard library does not expose a cross-platform mmap API, so the lesson teaches the invariants around offsets, bounds, and close state using `io.ReaderAt`.

## Concepts

### mmap Is Platform-Specific

Real memory mapping is an operating-system facility. In Go, production code usually uses platform-specific syscalls or a maintained external package. This exercise stays portable and standard-library-only.

### Random Access Needs Bounds

Memory mapping makes random access cheap, but offsets still need validation. Negative offsets, zero-length windows, and reads beyond the file should return typed errors.

### Close State Is Part Of The Contract

Real mapped regions must be unmapped. This model includes `Close` so tests can pin the invariant that closed mappings cannot be read.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/19-io-and-filesystem/15-memory-mapped-files/15-memory-mapped-files/cmd/demo
cd go-solutions/19-io-and-filesystem/15-memory-mapped-files/15-memory-mapped-files
```

### Exercise 1: Implement A Mapping Model

Create `mapping.go`:

```go
package mmapmodel

import (
	"fmt"
	"io"
)

type Mapping struct {
	r      io.ReaderAt
	size   int64
	closed bool
}

func New(r io.ReaderAt, size int64) (*Mapping, error) {
	if r == nil {
		return nil, fmt.Errorf("new mapping: %w", ErrNilReaderAt)
	}
	if size < 0 {
		return nil, fmt.Errorf("new mapping: %w", ErrNegativeSize)
	}
	return &Mapping{r: r, size: size}, nil
}

func (m *Mapping) ReadWindow(offset int64, length int) ([]byte, error) {
	if m == nil || m.closed {
		return nil, fmt.Errorf("read window: %w", ErrClosed)
	}
	if offset < 0 || length < 1 {
		return nil, fmt.Errorf("read window: %w", ErrInvalidWindow)
	}
	end := offset + int64(length)
	if end > m.size || end < offset {
		return nil, fmt.Errorf("read window: %w", ErrWindowOutOfRange)
	}
	buf := make([]byte, length)
	if _, err := m.r.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("read window at %d: %w", offset, err)
	}
	return buf, nil
}

func (m *Mapping) Close() error {
	if m == nil || m.closed {
		return fmt.Errorf("close mapping: %w", ErrClosed)
	}
	m.closed = true
	return nil
}
```

Create `errors.go`:

```go
package mmapmodel

import "errors"

var (
	ErrNilReaderAt      = errors.New("reader-at must not be nil")
	ErrNegativeSize     = errors.New("size must not be negative")
	ErrInvalidWindow    = errors.New("window offset and length are invalid")
	ErrWindowOutOfRange = errors.New("window exceeds mapping size")
	ErrClosed           = errors.New("mapping is closed")
)
```

### Exercise 2: Test Bounds And Close State

Create `mapping_test.go`:

```go
package mmapmodel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadWindow(t *testing.T) {
	t.Parallel()

	m, err := New(strings.NewReader("abcdef"), 6)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadWindow(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cde" {
		t.Fatalf("window = %q", got)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil", err: newNil(), want: ErrNilReaderAt},
		{name: "negative", err: newNegative(), want: ErrNegativeSize},
		{name: "invalid", err: readInvalid(), want: ErrInvalidWindow},
		{name: "range", err: readRange(), want: ErrWindowOutOfRange},
		{name: "closed", err: readClosed(), want: ErrClosed},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, tt.err, tt.want)
		}
	}
}

func newNil() error {
	_, err := New(nil, 0)
	return err
}

func newNegative() error {
	_, err := New(strings.NewReader(""), -1)
	return err
}

func readInvalid() error {
	m, _ := New(strings.NewReader("abc"), 3)
	_, err := m.ReadWindow(0, 0)
	return err
}

func readRange() error {
	m, _ := New(strings.NewReader("abc"), 3)
	_, err := m.ReadWindow(2, 2)
	return err
}

func readClosed() error {
	m, _ := New(strings.NewReader("abc"), 3)
	_ = m.Close()
	_, err := m.ReadWindow(0, 1)
	return err
}

func ExampleMapping_ReadWindow() {
	m, _ := New(strings.NewReader("abcdef"), 6)
	data, _ := m.ReadWindow(1, 2)
	fmt.Println(string(data))
	// Output: bc
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"example.com/mmapmodel"
)

func main() {
	m, err := mmapmodel.New(strings.NewReader("memory-map-model"), int64(len("memory-map-model")))
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()
	data, err := m.ReadWindow(7, 3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
}
```

## Common Mistakes

### Claiming Portable mmap In The Standard Library

Wrong: teach a nonexistent cross-platform `os.Mmap` API.

Fix: explain that real mmap requires platform-specific syscalls or an external package.

### Ignoring Offset Overflow

Wrong: compute `offset + length` and only check whether it is greater than size.

Fix: also check whether the computed end is less than the offset.

### Reading After Close

Wrong: let a closed mapping keep serving data.

Fix: return `ErrClosed` after `Close`.

## Verification

Run this from `~/go-exercises/mmapmodel`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that closes a mapping twice and asserts `errors.Is(err, ErrClosed)`.

## Summary

- Go has no portable standard-library mmap API.
- Random-access windows need strict offset and length validation.
- Close state is a real part of mapping APIs.
- `io.ReaderAt` is a portable way to model mmap-like random access in tests.

## What's Next

Next: [Implementing a Custom io.Reader](../16-implementing-custom-io-reader/16-implementing-custom-io-reader.md).

## Resources

- [io.ReaderAt](https://pkg.go.dev/io#ReaderAt)
- [os package](https://pkg.go.dev/os)
- [syscall package](https://pkg.go.dev/syscall)
