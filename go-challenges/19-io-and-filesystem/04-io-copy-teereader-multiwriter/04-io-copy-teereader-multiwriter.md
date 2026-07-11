# 4. io.Copy, TeeReader, and MultiWriter

Build a small streaming package that copies bytes once, computes a SHA-256 digest, and can fan the same stream out to multiple writers. The lesson focuses on composition with `io.Reader` and `io.Writer`: no manual buffer loops unless the standard library primitive cannot express the job.

## Concepts

### Streams Compose Around Interfaces

`io.Reader` and `io.Writer` let code transform data without knowing whether the source is a file, buffer, network connection, or compressor. `io.Copy` owns the loop: it reads from the source until EOF and writes each chunk to the destination, returning the number of bytes copied and the first error it sees.

### TeeReader Observes A Stream

`io.TeeReader(r, w)` returns a reader that reads from `r` and writes the same bytes to `w` as they are read. It is useful for hashing or auditing a stream while another operation consumes it. The write happens during reads, so errors from the tee writer are reported as read errors.

### MultiWriter Fans Out Writes

`io.MultiWriter(a, b, c)` returns a writer that writes the same bytes to each destination. It stops on the first error. That is usually correct: once destinations diverge, the caller should know the copy is not complete.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/streamcopy/cmd/demo
cd ~/go-exercises/streamcopy
go mod init example.com/streamcopy
```

### Exercise 1: Copy And Hash

Create `streamcopy.go`:

```go
package streamcopy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

type Result struct {
	Bytes  int64
	SHA256 string
}

func CopyWithHash(dst io.Writer, src io.Reader) (Result, error) {
	if dst == nil {
		return Result{}, fmt.Errorf("stream copy: %w", ErrNilWriter)
	}
	if src == nil {
		return Result{}, fmt.Errorf("stream copy: %w", ErrNilReader)
	}

	h := sha256.New()
	n, err := io.Copy(dst, io.TeeReader(src, h))
	if err != nil {
		return Result{}, fmt.Errorf("stream copy: %w", err)
	}
	return Result{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func CopyToAll(src io.Reader, dsts ...io.Writer) (Result, error) {
	if len(dsts) == 0 {
		return Result{}, fmt.Errorf("stream copy: %w", ErrNoWriters)
	}
	return CopyWithHash(io.MultiWriter(dsts...), src)
}
```

### Exercise 2: Add Sentinel Errors And An Example

Create `errors.go`:

```go
package streamcopy

import "errors"

var (
	ErrNilReader = errors.New("reader must not be nil")
	ErrNilWriter = errors.New("writer must not be nil")
	ErrNoWriters = errors.New("at least one writer is required")
)
```

Create `streamcopy_test.go`:

```go
package streamcopy

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCopyWithHash(t *testing.T) {
	t.Parallel()

	var dst bytes.Buffer
	got, err := CopyWithHash(&dst, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if dst.String() != "hello" {
		t.Fatalf("dst = %q, want hello", dst.String())
	}
	if got.Bytes != 5 || got.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("result = %+v", got)
	}
}

func TestCopyToAllWritesEveryDestination(t *testing.T) {
	t.Parallel()

	var a, b bytes.Buffer
	got, err := CopyToAll(strings.NewReader("data"), &a, &b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != 4 || a.String() != "data" || b.String() != "data" {
		t.Fatalf("bytes=%d a=%q b=%q", got.Bytes, a.String(), b.String())
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil reader", err: copyNilReader(), want: ErrNilReader},
		{name: "nil writer", err: copyNilWriter(), want: ErrNilWriter},
		{name: "no writers", err: copyNoWriters(), want: ErrNoWriters},
	}

	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, tt.err, tt.want)
		}
	}
}

func copyNilReader() error {
	_, err := CopyWithHash(&bytes.Buffer{}, nil)
	return err
}

func copyNilWriter() error {
	_, err := CopyWithHash(nil, strings.NewReader("x"))
	return err
}

func copyNoWriters() error {
	_, err := CopyToAll(strings.NewReader("x"))
	return err
}

func ExampleCopyWithHash() {
	var dst bytes.Buffer
	result, _ := CopyWithHash(&dst, strings.NewReader("go"))
	fmt.Println(dst.String())
	fmt.Println(result.Bytes)
	// Output:
	// go
	// 2
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"

	"example.com/streamcopy"
)

func main() {
	var first, second bytes.Buffer
	result, err := streamcopy.CopyToAll(strings.NewReader("demo"), &first, &second)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bytes=%d first=%q second=%q\n", result.Bytes, first.String(), second.String())
}
```

## Common Mistakes

### Hashing After Copying From The Same Reader


Wrong: call `io.Copy(dst, src)` and then try to read `src` again to hash it. Most readers are consumed after the first copy.

Fix: use `io.TeeReader` so the hasher observes bytes during the single copy.

### Ignoring MultiWriter Errors

Wrong: assume both destinations received all bytes even when `CopyToAll` returns an error.

Fix: treat an error as an incomplete fan-out. `io.MultiWriter` stops at the first failed destination.

### Matching Error Strings

Wrong: test `err.Error() == "reader must not be nil"`.

Fix: wrap sentinel errors with `%w` and assert with `errors.Is`.

## Verification

Run this from `~/go-exercises/streamcopy`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that uses a writer returning an error and asserts that `CopyWithHash` returns that wrapped error.

## Summary

- `io.Copy` is the standard loop for moving bytes from a reader to a writer.
- `io.TeeReader` lets another writer observe bytes as they are read.
- `io.MultiWriter` writes the same bytes to several destinations and fails on the first write error.
- Validation errors should be sentinel errors wrapped with `%w` and tested with `errors.Is`.

## What's Next

Next: [Walking Directory Trees](../05-walking-directory-trees/05-walking-directory-trees.md).

## Resources

- [io package](https://pkg.go.dev/io)
- [crypto/sha256 package](https://pkg.go.dev/crypto/sha256)
- [errors package](https://pkg.go.dev/errors)
