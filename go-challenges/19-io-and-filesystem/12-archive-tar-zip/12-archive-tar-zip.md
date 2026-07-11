# 12. Archive Formats -- tar and zip

Build a zip archive package that writes named files to an `io.Writer` and reads them back from bytes. The same design principles apply to tar, but zip is enough to show path validation, deterministic metadata, and standard-library archive APIs.

## Concepts

### Archives Store Names, Not Just Bytes

Each entry has a path-like name. Unsafe names such as absolute paths or `..` segments can cause path traversal when extracting. Validate names before writing or extracting.

### zip.Writer Must Be Closed

`zip.Writer` writes its central directory on `Close`. If you forget to close it, readers may see a corrupt archive.

### Deterministic Metadata Helps Tests

Archive entries can contain timestamps and modes. Set the fields you care about so output is reproducible enough to test.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/archivezip/cmd/demo
cd ~/go-exercises/archivezip
go mod init example.com/archivezip
```

### Exercise 1: Create And Read A Zip Archive

Create `zip.go`:

```go
package archivezip

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

type File struct {
	Name string
	Data []byte
}

func Create(w io.Writer, files []File) error {
	if w == nil {
		return fmt.Errorf("create zip: %w", ErrNilWriter)
	}
	zw := zip.NewWriter(w)
	for _, file := range sorted(files) {
		if err := validateName(file.Name); err != nil {
			_ = zw.Close()
			return err
		}
		h := &zip.FileHeader{Name: file.Name, Method: zip.Deflate}
		h.SetModTime(time.Unix(0, 0).UTC())
		entry, err := zw.CreateHeader(h)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("create entry %q: %w", file.Name, err)
		}
		if _, err := entry.Write(file.Data); err != nil {
			_ = zw.Close()
			return fmt.Errorf("write entry %q: %w", file.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close zip: %w", err)
	}
	return nil
}

func Read(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	out := map[string][]byte{}
	for _, file := range zr.File {
		if err := validateName(file.Name); err != nil {
			return nil, err
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open entry %q: %w", file.Name, err)
		}
		content, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read entry %q: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close entry %q: %w", file.Name, closeErr)
		}
		out[file.Name] = content
	}
	return out, nil
}

func validateName(name string) error {
	clean := path.Clean(name)
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("archive name %q: %w", name, ErrUnsafeName)
	}
	return nil
}

func sorted(files []File) []File {
	out := append([]File(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

Create `errors.go`:

```go
package archivezip

import "errors"

var (
	ErrNilWriter  = errors.New("writer must not be nil")
	ErrUnsafeName = errors.New("archive entry name is unsafe")
)
```

### Exercise 2: Test Archives

Create `zip_test.go`:

```go
package archivezip

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestCreateAndRead(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := Create(&buf, []File{{Name: "b.txt", Data: []byte("b")}, {Name: "a.txt", Data: []byte("a")}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := Read(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if string(files["a.txt"]) != "a" || string(files["b.txt"]) != "b" {
		t.Fatalf("files = %+v", files)
	}
}

func TestCreateRejectsUnsafeNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "/abs.txt", "../secret.txt"} {
		var buf bytes.Buffer
		err := Create(&buf, []File{{Name: name, Data: []byte("x")}})
		if !errors.Is(err, ErrUnsafeName) {
			t.Errorf("name %q: err = %v, want ErrUnsafeName", name, err)
		}
	}
}

func TestCreateRejectsNilWriter(t *testing.T) {
	t.Parallel()

	err := Create(nil, nil)
	if !errors.Is(err, ErrNilWriter) {
		t.Fatalf("err = %v, want ErrNilWriter", err)
	}
}

func ExampleCreate() {
	var buf bytes.Buffer
	_ = Create(&buf, []File{{Name: "hello.txt", Data: []byte("hello")}})
	fmt.Println(buf.Len() > 0)
	// Output: true
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

	"example.com/archivezip"
)

func main() {
	var buf bytes.Buffer
	if err := archivezip.Create(&buf, []archivezip.File{{Name: "demo.txt", Data: []byte("demo")}}); err != nil {
		log.Fatal(err)
	}
	fmt.Println(buf.Len() > 0)
}
```

## Common Mistakes

### Forgetting To Close zip.Writer

Wrong: create entries and return without `zw.Close()`.

Fix: close the writer and return any close error.

### Trusting Archive Names

Wrong: extract `../secret.txt` relative to a destination directory.

Fix: reject absolute paths and parent traversal before writing or extracting.

### Depending On Map Order

Wrong: write archive entries from a map and expect stable order.

Fix: sort file names before writing.

## Verification

Run this from `~/go-exercises/archivezip`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that passes invalid zip bytes to `Read` and expects an error.

## Summary

- The standard library provides `archive/zip` and `archive/tar`.
- Validate archive entry names to prevent traversal bugs.
- Close archive writers so final metadata is written.
- Sort entries when deterministic output matters.

## What's Next

Next: [io/fs Virtual Filesystems](../13-io-fs-virtual-filesystems/13-io-fs-virtual-filesystems.md).

## Resources

- [archive/zip package](https://pkg.go.dev/archive/zip)
- [archive/tar package](https://pkg.go.dev/archive/tar)
- [path.Clean](https://pkg.go.dev/path#Clean)
