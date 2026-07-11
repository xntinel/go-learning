# Exercise 9: Gating Slow Integration Tests Behind a Build Tag

A fast unit suite must stay fast, so the slow tests that touch a real filesystem
or a spun-up dependency belong in a separate, tag-gated binary. This module splits
a file-backed store's tests into a default unit set and a `//go:build integration`
set, and contrasts the tag with `testing.Short()`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test. Nothing here imports another exercise.

## What you'll build

```text
filestore/                     module example.com/filestore
  go.mod                       package filestore
  store.go                     Store; New, Put, Get, encodeKey; ErrInvalidKey
  store_test.go                fast unit tests (no tag): key encoding, unsafe-key rejection
  integration_test.go          //go:build integration -> real filesystem round-trip
  cmd/demo/main.go             writes and reads back a value in a temp dir
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`, `integration_test.go`.
- Implement: a file-backed store with a safe key encoder that rejects path separators, wrapping `ErrInvalidKey` with `%w`.
- Test: fast unit tests always; a filesystem round-trip only under `-tags integration`.
- Verify: `go test -race ./...` runs unit only; `go test -tags integration ./...` adds the round-trip; `go list -tags integration -f '{{.TestGoFiles}}'` shows the split.

Set up the module:

```bash
mkdir -p ~/go-exercises/filestore/cmd/demo
cd ~/go-exercises/filestore
go mod init example.com/filestore
```

### Compiled out versus compiled-and-skipped

There are two ways to keep a slow test out of the fast path, and they are not
equivalent. `testing.Short()` with `go test -short` leaves the test *compiled into
the binary* and skips it at runtime — the test's code, and its dependencies, are
still built. A `//go:build integration` tag instead compiles the test out entirely
unless the tag is set: the fast `go test ./...` never even builds
`integration_test.go`, so a heavy test-only dependency (a database driver, a
container SDK) never enters the default test binary. For a genuinely separate
integration stage — one that pulls in infrastructure the unit box does not have —
the build tag is the right tool; `testing.Short()` is right when the slow test
shares all the same code and you only want to skip it sometimes.

The store itself is deliberately small but real: it persists values as files, and
its `encodeKey` rejects empty keys, `.`/`..`, and path separators so a key can
never escape the store directory — the kind of check a security review demands.
The rejection wraps a sentinel `ErrInvalidKey`, so callers assert it with
`errors.Is`. The unit tests cover the pure encoding logic and the unsafe-key path
(fast, no meaningful I/O); the integration test does the actual write-then-read
round trip against `t.TempDir`.

Create `store.go`:

```go
// Package filestore is a tiny file-backed key/value store. Its pure key-encoding
// logic is covered by fast unit tests; the real filesystem round-trip is covered
// by an integration test gated behind //go:build integration.
package filestore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidKey is returned when a key cannot be encoded to a safe file name.
var ErrInvalidKey = errors.New("filestore: invalid key")

// Store persists values as files under a directory.
type Store struct{ dir string }

// New returns a Store rooted at dir.
func New(dir string) *Store { return &Store{dir: dir} }

// encodeKey maps a key to a safe file name, rejecting empty keys and path
// separators so a key can never escape the store directory.
func encodeKey(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, "/\\") || key == "." || key == ".." {
		return "", ErrInvalidKey
	}
	return key + ".val", nil
}

// Put writes value under key.
func (s *Store) Put(key string, value []byte) error {
	name, err := encodeKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), value, 0o600)
}

// Get reads the value stored under key.
func (s *Store) Get(key string) ([]byte, error) {
	name, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.dir, name))
}
```

### The runnable demo

The demo writes a value into a temp directory, reads it back, and shows that an
unsafe key is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/filestore"
)

func main() {
	dir, _ := os.MkdirTemp("", "filestore-demo")
	defer os.RemoveAll(dir)

	s := filestore.New(dir)
	if err := s.Put("session", []byte("alice")); err != nil {
		fmt.Println("put error:", err)
		return
	}
	v, err := s.Get("session")
	if err != nil {
		fmt.Println("get error:", err)
		return
	}
	fmt.Printf("session=%s\n", v)

	if err := s.Put("../escape", nil); err != nil {
		fmt.Println("rejected unsafe key:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
session=alice
rejected unsafe key: filestore: invalid key
```

### The tests

`store_test.go` has no build tag, so it is the fast suite CI runs on every commit:
it exercises `encodeKey` directly and asserts the unsafe-key path returns
`ErrInvalidKey` via `errors.Is`. `integration_test.go` carries `//go:build
integration` and runs the real round trip; it is compiled only under the tag.

Create `store_test.go`:

```go
package filestore

import (
	"errors"
	"testing"
)

func TestEncodeKey(t *testing.T) {
	t.Parallel()
	ok := []string{"session", "user:42", "a.b.c"}
	for _, k := range ok {
		if _, err := encodeKey(k); err != nil {
			t.Errorf("encodeKey(%q) = %v; want no error", k, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", "a\\b"}
	for _, k := range bad {
		if _, err := encodeKey(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("encodeKey(%q) err = %v; want ErrInvalidKey", k, err)
		}
	}
}

func TestPutRejectsUnsafeKey(t *testing.T) {
	t.Parallel()
	s := New(t.TempDir())
	if err := s.Put("../escape", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put unsafe key err = %v; want ErrInvalidKey", err)
	}
}
```

Create `integration_test.go` — compiled only with `-tags integration`:

```go
//go:build integration

package filestore

import (
	"bytes"
	"testing"
)

func TestFilesystemRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	want := []byte("integration payload")
	if err := s.Put("k", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get = %q; want %q", got, want)
	}
}
```

Run the two stages and confirm the split:

```bash
go test ./...                    # fast: unit only
go test -tags integration ./...  # adds the round-trip
go list -f '{{.TestGoFiles}}' .
go list -tags integration -f '{{.TestGoFiles}}' .
```

```text
[store_test.go]
[integration_test.go store_test.go]
```

## Review

The split is correct when the default `go test` compiles and runs only
`store_test.go` (proved by the `go list` diff) and the tagged run additionally
executes the round trip. The design decision to internalize: reach for
`//go:build integration` when the slow test drags in dependencies or infrastructure
the unit box should not build, and reach for `testing.Short()` when the slow test
shares all its code with the fast ones and you only want to skip it on demand. The
security-relevant detail — `encodeKey` rejecting `..` and separators, asserted with
`errors.Is` against a `%w`-wrapped sentinel — is exactly the kind of check you want
in the *fast* suite so it runs on every commit, not deferred to the integration
stage.

## Resources

- [Build constraints on test files (cmd/go)](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — gating a `_test.go` file with a tag.
- [`testing.Short` and `go test -short`](https://pkg.go.dev/testing#Short) — the runtime-skip alternative and when it applies.
- [`testing.T.TempDir`](https://pkg.go.dev/testing#T.TempDir) — the per-test temp directory the integration test uses.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-reproducible-release-matrix.md](10-reproducible-release-matrix.md)
