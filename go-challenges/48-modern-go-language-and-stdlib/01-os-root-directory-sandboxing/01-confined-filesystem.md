# Exercise 1: A Directory-Confined Filesystem

This module builds `safestore`, a small package that serves and stores files under one directory and confines every operation beneath it with `os.Root`. It is a single cohesive package: confined reads and writes, a nested sub-root, a one-shot open via `os.OpenInRoot`, a confined directory listing through `Root.FS()`, and safe archive extraction that rejects zip-slip.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
safestore.go      Store{root *os.Root}; Open, Close, Name, ReadFile, WriteFile, Sub; ErrEmptyName
oncereader.go     ReadOnce(dir, name) via os.OpenInRoot (one-shot, no Store)
listing.go        (*Store) List() via Root.FS() + io/fs
extract.go        (*Store) Extract(entries) — rejects zip-slip
cmd/
  demo/
    main.go       extract, list, and prove a traversal is blocked
safestore_test.go table-driven confinement tests, in-root symlink, -race
example_test.go   runnable Example with // Output:
```

- Files: `safestore.go`, `oncereader.go`, `listing.go`, `extract.go`, `cmd/demo/main.go`, `safestore_test.go`, `example_test.go`.
- Implement: `Store` with `Open`, `Close`, `Name`, `ReadFile`, `WriteFile`, `Sub`, `List`, `Extract`; the package function `ReadOnce`; and the `ErrEmptyName` sentinel.
- Test: `safestore_test.go` plants a secret outside the root and asserts every escape attempt (`..`, escaping symlink, absolute symlink, zip-slip) both errors and never leaks the secret; it pins in-root symlink following, the not-exist distinction, and a concurrent read under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

This is a library, not a program: there is no `main` in the package itself. The `cmd/demo` binary and the tests are how you exercise it.

### The Store and its one confined handle

The whole package is built around a single `*os.Root`. `os.OpenRoot(dir)` opens the directory and holds a descriptor to it; every method then resolves names *against that descriptor* rather than against a string path, so the kernel enforces that results stay beneath the root. The `Store` owns that handle, which is why `Open` returns something you must `Close`.

`ReadFile` is the first confined operation, and it sets two conventions the rest of the package follows: it validates input against an exported sentinel (`ErrEmptyName`) and wraps every failure with `%w`, so callers can branch with `errors.Is`. Note what is *not* here: no `filepath.Clean`, no prefix check, no symlink resolution — the confinement is a property of the handle, so there is nothing to sanitize.

Confinement is not read-only, and the write path is where it matters most. A traversal bug on the *write* path is worse than on the read path: instead of leaking a file it lets an attacker overwrite one (`../../etc/cron.d/x`). `WriteFile` rejects an escaping name *before* any bytes reach the disk, so a malicious name cannot plant a file outside the tree. `Sub` shows the second idea: `Root.OpenRoot` opens a subdirectory as its own, independently confined `*os.Root`. A nested store cannot climb back above its parent, so you can hand one component (a per-tenant directory, say) an even tighter sandbox without trusting it. Both methods reuse the `ErrEmptyName` sentinel and `%w` wrapping.

Create `safestore.go`:

```go
package safestore

import (
	"errors"
	"fmt"
	"os"
)

// ErrEmptyName is returned when a caller passes an empty file name.
var ErrEmptyName = errors.New("name must not be empty")

// Store serves files from a single directory tree. Every method confines
// access beneath the root: a relative ".." chain, an absolute symlink, and a
// symlink whose target escapes the root are all rejected, not followed.
type Store struct {
	root *os.Root
}

// Open opens dir as a confinement root. The returned Store owns a directory
// handle and must be closed.
func Open(dir string) (*Store, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("safestore: open root %q: %w", dir, err)
	}
	return &Store{root: root}, nil
}

// Close releases the directory handle held by the root.
func (s *Store) Close() error {
	return s.root.Close()
}

// Name reports the directory the store was opened on.
func (s *Store) Name() string {
	return s.root.Name()
}

// ReadFile reads the named file, interpreted relative to the store root. Any
// attempt to escape the root fails instead of reading the outside file.
func (s *Store) ReadFile(name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("safestore: %w", ErrEmptyName)
	}
	data, err := s.root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("safestore: read %q: %w", name, err)
	}
	return data, nil
}

// WriteFile writes data to the named file inside the root, creating it with
// perm if needed. A name that escapes the root is rejected before any write
// happens, so an attacker cannot plant a file outside the tree.
func (s *Store) WriteFile(name string, data []byte, perm os.FileMode) error {
	if name == "" {
		return fmt.Errorf("safestore: %w", ErrEmptyName)
	}
	if err := s.root.WriteFile(name, data, perm); err != nil {
		return fmt.Errorf("safestore: write %q: %w", name, err)
	}
	return nil
}

// Sub opens a subdirectory of the store as its own nested Store. The nested
// store is independently confined: it cannot climb back above the parent.
func (s *Store) Sub(name string) (*Store, error) {
	if name == "" {
		return nil, fmt.Errorf("safestore: %w", ErrEmptyName)
	}
	sub, err := s.root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("safestore: sub %q: %w", name, err)
	}
	return &Store{root: sub}, nil
}
```

### One-shot opens with os.OpenInRoot

`os.OpenRoot` is the right tool when you will perform several operations under one directory: you open the root once and amortize the directory handle across many calls. But a lot of code only needs to open *one* attacker-named file and move on — a request handler reading a single uploaded document, say. Keeping a `*os.Root` alive (and remembering to `Close` it) just for that is overhead.

`os.OpenInRoot(dir, name)` is the one-shot form. It is equivalent to `os.OpenRoot(dir)` followed by opening `name`, and is subject to the exact same escape checks — a `..` chain, an escaping symlink, or an absolute symlink target is rejected — but it hands you a plain `*os.File` and manages the root internally, so there is no handle for you to track. The trade-off is the mirror image of `OpenRoot`: convenient for a single open, wasteful if you call it in a loop over the same directory (each call re-opens the root), where a reused `*os.Root` is cheaper. Because it returns an `*os.File`, you read it with the ordinary `io` toolkit — here `io.ReadAll` after a `defer f.Close()`.

Create `oncereader.go`:

```go
package safestore

import (
	"io"
	"os"
)

// ReadOnce reads a single file from dir without keeping a Store open.
// os.OpenInRoot opens name confined to dir, so a traversing name is rejected
// exactly as a Store method would reject it.
func ReadOnce(dir, name string) ([]byte, error) {
	f, err := os.OpenInRoot(dir, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
```

### Listing through Root.FS()

`Root.FS()` bridges `os.Root` to the `io/fs` abstraction: it returns an `fs.FS` whose concrete value also implements `fs.StatFS`, `fs.ReadFileFS`, `fs.ReadDirFS`, and (since Go 1.25) `fs.ReadLinkFS`. The payoff is reuse: any code written against `io/fs` — `fs.ReadDir`, `fs.WalkDir`, `fs.Glob`, a `html/template` loader — runs *inside the confinement* when you hand it `root.FS()`, even though that code knows nothing about `os.Root`. You inherit traversal-resistance in libraries that were never written with it in mind. `List` uses the simplest such consumer, `fs.ReadDir`, to enumerate the root's top-level entries and returns them sorted with `slices.Sort`.

Create `listing.go`:

```go
package safestore

import (
	"io/fs"
	"slices"
)

// List returns the names of the entries directly under the store root, sorted.
// It reads them through Root.FS(), an fs.FS that is itself confined, so the
// io/fs traversal cannot escape either.
func (s *Store) List() ([]string, error) {
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	slices.Sort(names)
	return names, nil
}
```

### Safe archive extraction (zip-slip)

Extraction is the archetypal reason `os.Root` was added. Unpacking a zip or tar means writing files whose *names come straight from the archive*, and a malicious archive embeds names like `../../etc/cron.d/x` to escape the extraction directory — the "zip-slip" vulnerability that has hit countless tools. The naive `filepath.Join(dir, entry.Name)` does nothing to stop it: `Join` cleans the path lexically but still happily produces a path outside `dir`.

Writing each entry *through the root* turns every escaping name into a rejected write. `Extract` also creates parent directories with `Root.MkdirAll`, which is confined for the same reason — a `../` in a directory component is refused too. So the whole unpack is safe by construction, with no per-name validation.

Create `extract.go`:

```go
package safestore

import "path"

// Extract writes a set of archive-style entries into the store. Entry names are
// slash-separated, as in a zip or tar. Any entry whose name escapes the root
// (the classic "zip-slip") is rejected before its bytes are written, so a
// malicious archive cannot plant files outside the tree.
func (s *Store) Extract(entries map[string][]byte) error {
	for name, data := range entries {
		if dir := path.Dir(name); dir != "." {
			if err := s.root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := s.WriteFile(name, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
```

### The runnable demo

A test proves a property in the abstract; a demo makes it concrete. This one creates a temp directory with one file, extracts an archive-style entry into a subdirectory, lists what the store now holds, then tries a deep traversal and prints the denial it gets back.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	safestore "example.com/confined-filesystem"
)

func main() {
	dir, err := os.MkdirTemp("", "safestore-demo")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("quarterly numbers"), 0o644); err != nil {
		log.Fatal(err)
	}

	store, err := safestore.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if err := store.Extract(map[string][]byte{"logs/app.log": []byte("started")}); err != nil {
		log.Fatal(err)
	}

	names, err := store.List()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("entries:", names)

	if _, err := store.ReadFile("../../../etc/passwd"); err != nil {
		fmt.Printf("traversal blocked: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
entries: [logs report.txt]
traversal blocked: safestore: read "../../../etc/passwd": openat ../../../etc/passwd: path escapes from parent
```

### Tests

A security property is only real if a test would fail when it breaks. These tests do not eyeball a `printf`; `makeTree` builds a real directory tree with `t.TempDir()`, plants a secret *outside* the root, and for every escape attempt (`..`, an escaping symlink, an absolute symlink, a zip-slip entry) asserts two things: the call returns an error, *and* the secret's bytes never come back. They also pin the non-obvious behaviors from the concepts — an in-root symlink is still followed (`TestInRootSymlinkIsFollowed`), a missing file reports `os.ErrNotExist` while an escape does not — and run a concurrent read under `-race`, since `os.Root` is documented safe for concurrent use.

Create `safestore_test.go`:

```go
package safestore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// makeTree builds, returning the path to the store root (<base>/data):
//
//	<base>/secret.txt            out-of-root secret
//	<base>/data/                 the store root
//	<base>/data/ok.txt           -> "in-root"
//	<base>/data/inside.lnk       -> ok.txt            (stays inside, followed)
//	<base>/data/escape.lnk       -> ../secret.txt     (escapes, rejected)
//	<base>/data/abs.lnk          -> <base>/secret.txt (absolute, rejected)
//	<base>/data/nested/inner.txt -> "deep"
func makeTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(base, "secret.txt"), "TOPSECRET")
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(data, "ok.txt"), "in-root")
	write(filepath.Join(data, "nested", "inner.txt"), "deep")
	link("ok.txt", filepath.Join(data, "inside.lnk"))
	link(filepath.Join("..", "secret.txt"), filepath.Join(data, "escape.lnk"))
	link(filepath.Join(base, "secret.txt"), filepath.Join(data, "abs.lnk"))
	return data
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(makeTree(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReadConfinement(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{"in-root file", "ok.txt", "in-root", false},
		{"symlink staying inside", "inside.lnk", "in-root", false},
		{"relative traversal", "../secret.txt", "", true},
		{"escaping symlink", "escape.lnk", "", true},
		{"absolute symlink", "abs.lnk", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.ReadFile(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ReadFile(%q) = %q, want error: escape must be denied", tt.path, got)
				}
				if bytes.Contains(got, []byte("TOPSECRET")) {
					t.Fatalf("ReadFile(%q) leaked the out-of-root secret", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", tt.path, err)
			}
			if string(got) != tt.want {
				t.Fatalf("ReadFile(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestInRootSymlinkIsFollowed(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	if err := os.Symlink("ok.txt", filepath.Join(s.Name(), "latest.lnk")); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile("latest.lnk")
	if err != nil || string(got) != "in-root" {
		t.Fatalf("ReadFile(latest.lnk) = %q, err = %v; want in-root followed", got, err)
	}
}

func TestMissingFileReportsNotExist(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	_, err := s.ReadFile("nope.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile(missing) err = %v, want os.ErrNotExist", err)
	}
}

func TestEmptyNameRejected(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	if _, err := s.ReadFile(""); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("ReadFile(empty) err = %v, want ErrEmptyName", err)
	}
	if err := s.WriteFile("", nil, 0o644); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("WriteFile(empty) err = %v, want ErrEmptyName", err)
	}
}

func TestWriteIsConfined(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	if err := s.WriteFile("../planted.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile(../planted.txt) succeeded; escape must be denied")
	}
	outside := filepath.Join(filepath.Dir(s.Name()), "planted.txt")
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape write planted a file at %s", outside)
	}

	if err := s.WriteFile("out.txt", []byte("written"), 0o644); err != nil {
		t.Fatalf("WriteFile(out.txt) = %v", err)
	}
	got, err := s.ReadFile("out.txt")
	if err != nil {
		t.Fatalf("ReadFile(out.txt) = %v", err)
	}
	if string(got) != "written" {
		t.Fatalf("round-trip = %q, want %q", got, "written")
	}
}

func TestSubStoreStaysConfined(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	sub, err := s.Sub("nested")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if got, err := sub.ReadFile("inner.txt"); err != nil || string(got) != "deep" {
		t.Fatalf("sub ReadFile = %q, err = %v", got, err)
	}
	if _, err := sub.ReadFile("../../secret.txt"); err == nil {
		t.Fatal("sub store escaped to the parent's secret")
	}
}

func TestReadOnce(t *testing.T) {
	t.Parallel()
	dir := makeTree(t)

	got, err := ReadOnce(dir, "ok.txt")
	if err != nil || string(got) != "in-root" {
		t.Fatalf("ReadOnce(ok.txt) = %q, %v", got, err)
	}
	if _, err := ReadOnce(dir, "../secret.txt"); err == nil {
		t.Fatal("ReadOnce followed a traversal; escape must be denied")
	}
}

func TestListReturnsTopLevelEntries(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	names, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	has := func(want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	if !has("ok.txt") || !has("nested") {
		t.Fatalf("List() = %v, want it to include ok.txt and nested", names)
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	if err := s.Extract(map[string][]byte{
		"docs/readme.txt": []byte("hello"),
		"data.bin":        []byte("bytes"),
	}); err != nil {
		t.Fatalf("Extract(safe) error = %v", err)
	}
	if got, _ := s.ReadFile("docs/readme.txt"); string(got) != "hello" {
		t.Fatalf("extracted docs/readme.txt = %q", got)
	}

	if err := s.Extract(map[string][]byte{"../evil.txt": []byte("x")}); err == nil {
		t.Fatal("Extract followed a zip-slip name; escape must be denied")
	}
	outside := filepath.Join(filepath.Dir(s.Name()), "evil.txt")
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zip-slip planted a file at %s", outside)
	}
}

func TestConcurrentReadsAreSafe(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.ReadFile("ok.txt"); err != nil || string(got) != "in-root" {
				t.Errorf("concurrent ReadFile = %q, err = %v", got, err)
			}
		}()
	}
	wg.Wait()
}
```

The `Example` doubles as auto-verified documentation: its `// Output:` block is checked by `go test`, so the read-then-deny narrative cannot drift from the code.

Create `example_test.go`:

```go
package safestore

import (
	"fmt"
	"os"
	"path/filepath"
)

func Example() {
	dir, err := os.MkdirTemp("", "safestore-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("public"), 0o644); err != nil {
		panic(err)
	}

	s, err := Open(dir)
	if err != nil {
		panic(err)
	}
	defer s.Close()

	data, err := s.ReadFile("note.txt")
	fmt.Printf("read: %s err: %v\n", data, err)

	_, err = s.ReadFile("../../../etc/passwd")
	fmt.Printf("escape denied: %t\n", err != nil)

	// Output:
	// read: public err: <nil>
	// escape denied: true
}
```

## Review

The package is sound when confinement is a property of the handle, not of any string check, and the tests are what make that property real rather than asserted. `makeTree` plants `secret.txt` *outside* the root and the confinement tests demand both halves for each escape — an error *and* the absence of `TOPSECRET` in the returned bytes — so a regression that "succeeds" by leaking the file fails just as loudly as one that panics. Confirm the write path is guarded the same way the read path is: `TestWriteIsConfined` checks that `../planted.txt` neither succeeds nor leaves a file on disk outside the tree, and `TestExtractRejectsZipSlip` does the same for an archive entry. The race detector is load-bearing here, not decorative: `os.Root` is documented safe for concurrent use and `TestConcurrentReadsAreSafe` is the only thing that would catch a regression that broke it.

Common mistakes for this feature. The first is reaching for `filepath.Clean` plus a prefix check instead of anchoring the open: `Clean` collapses `..` lexically but does not resolve symlinks, so a symlinked component slips straight through — the entire point of `os.Root` is that you stop classifying paths yourself. The second is expecting a typed escape error: there is no exported sentinel, the escape wraps an unexported value, and `errors.Is(err, os.ErrNotExist)` is deliberately *false* for an escape, so the only correct contract is "any error means denied" (use the not-exist check only to distinguish a genuine 404). The third is treating `os.Root` as a full sandbox: it confines path *resolution*, not what a confined path may *name*, so it does not block `/proc`, bind mounts, or device files — pair it with namespaces or seccomp when you need those.

## Resources

- [`os.Root`](https://pkg.go.dev/os#Root) — the confinement type, its methods, and the per-method version notes (1.24 vs 1.25).
- [`os.OpenInRoot`](https://pkg.go.dev/os#OpenInRoot) — the one-shot confined open used by `ReadOnce`.
- [Traversal-resistant file APIs (Go blog)](https://go.dev/blog/osroot) — the design rationale, the TOCTOU problem, and the zip-slip motivation.
- [Go 1.25 release notes: os](https://go.dev/doc/go1.25#os) — the `ReadFile`/`WriteFile`/`FS`/`ReadLinkFS` additions this package relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../02-testing-synctest-deterministic-concurrency/00-concepts.md](../02-testing-synctest-deterministic-concurrency/00-concepts.md)
