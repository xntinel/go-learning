# Exercise 8: Implement ReadFileFS/ReadDirFS fast paths and prove they are used

The top-level helpers `fs.ReadFile` and `fs.ReadDir` dispatch to optional
interfaces — `ReadFileFS`, `ReadDirFS` — when the concrete filesystem implements
them, and fall back to `Open` otherwise. On a hot config path that difference is
an extra `Open`+`Stat` per read. This exercise builds a `bundleFS` that
implements the fast-path interfaces and a plain `Open`-only FS, instruments both
with atomic counters, and proves which path each helper takes.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
fastpath/                    independent module: example.com/fastpath
  go.mod                     go 1.26
  fastpath.go                counters; bundleFS (ReadFileFS+ReadDirFS); plainFS (Open-only)
  cmd/
    demo/
      main.go                read via both FS types and print the call counters
  fastpath_test.go           assert bundleFS uses ReadFile/ReadDir; plainFS uses Open
```

- Files: `fastpath.go`, `cmd/demo/main.go`, `fastpath_test.go`.
- Implement: a `bundleFS` implementing `fs.ReadFileFS` and `fs.ReadDirFS` over a
  `MapFS`, and a `plainFS` implementing only `Open`, both counting method calls
  with atomic counters; a helper using `fs.FileInfoToDirEntry`.
- Test: `fs.ReadFile(bundle, name)` increments the `ReadFile` counter and not
  `Open`; `fs.ReadFile(plain, name)` increments `Open` (the fallback);
  `fs.ReadDir` likewise for `ReadDir` vs `Open`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/08-optional-interface-fast-path/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/08-optional-interface-fast-path
```

### How the helpers choose a path, and why it is measurable

`fs.ReadFile(fsys, name)` has exactly one branch at the top:

```text
if fsys implements ReadFileFS { return fsys.ReadFile(name) }
// else: Open(name); Stat (for size); io.ReadAll; Close
```

`fs.ReadDir` is the same shape against `ReadDirFS`, falling back to
`Open(name)` + the file's `ReadDirFile.ReadDir(-1)`. So a filesystem that
implements the optional interface gets its method called directly — one call, no
`Open` — while an `Open`-only filesystem pays the full open/stat/read cycle. This
is not a micro-optimization myth; for a filesystem that fronts a network store or
a decompressor, an `embed.FS`, or a custom bundle format, the fast path can
collapse several syscalls or allocations into one, and on a config path read on
every request that adds up.

You can *prove* which path runs by instrumenting the wrapper with atomic
counters and inspecting them after the call. `bundleFS` implements `Open`,
`ReadFile`, and `ReadDir`, each bumping its own counter before delegating to the
underlying `MapFS`; because `bundleFS` satisfies `ReadFileFS`, `fs.ReadFile`
calls `bundleFS.ReadFile` and the `Open` counter stays at zero. `plainFS`
implements only `Open`; `fs.ReadFile` cannot find a `ReadFileFS`, so it falls
back and the `Open` counter increments. The counters make an otherwise invisible
dispatch decision observable in a test — which is the whole point: a custom FS on
a hot path should be *tested* to confirm it is actually taking the fast path, not
assumed to.

The `EntryFor` helper shows `fs.FileInfoToDirEntry`, which converts a
`fs.FileInfo` (from `Stat`) into a `fs.DirEntry` — useful when you have stat
information and need to hand back the cheaper `DirEntry` shape a `ReadDir` caller
expects.

Create `fastpath.go`:

```go
package fastpath

import (
	"io/fs"
	"sync/atomic"
)

// counters records how each optional-interface method was reached.
type counters struct {
	open     atomic.Int64
	readFile atomic.Int64
	readDir  atomic.Int64
}

// bundleFS implements the optional fast-path interfaces ReadFileFS and
// ReadDirFS in addition to Open, so fs.ReadFile and fs.ReadDir dispatch
// straight to its methods rather than falling back to Open.
type bundleFS struct {
	inner fs.FS
	c     *counters
}

func newBundleFS(inner fs.FS) (bundleFS, *counters) {
	c := &counters{}
	return bundleFS{inner: inner, c: c}, c
}

func (b bundleFS) Open(name string) (fs.File, error) {
	b.c.open.Add(1)
	return b.inner.Open(name)
}

func (b bundleFS) ReadFile(name string) ([]byte, error) {
	b.c.readFile.Add(1)
	return fs.ReadFile(b.inner, name)
}

func (b bundleFS) ReadDir(name string) ([]fs.DirEntry, error) {
	b.c.readDir.Add(1)
	return fs.ReadDir(b.inner, name)
}

// plainFS implements only Open, forcing fs.ReadFile / fs.ReadDir onto their
// fallback paths (Open + read).
type plainFS struct {
	inner fs.FS
	c     *counters
}

func newPlainFS(inner fs.FS) (plainFS, *counters) {
	c := &counters{}
	return plainFS{inner: inner, c: c}, c
}

func (p plainFS) Open(name string) (fs.File, error) {
	p.c.open.Add(1)
	return p.inner.Open(name)
}

// EntryFor stats name and returns the cheaper DirEntry view via
// fs.FileInfoToDirEntry.
func EntryFor(fsys fs.FS, name string) (fs.DirEntry, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	return fs.FileInfoToDirEntry(info), nil
}
```

### The runnable demo

The demo reads the same file through both filesystems and prints the counters,
so you can see the fast path take one `ReadFile` call with zero `Open`, and the
plain path take an `Open`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io/fs"
	"testing/fstest"

	"example.com/fastpath"
)

func main() {
	base := fstest.MapFS{"config.yaml": {Data: []byte("k: v")}}

	bundle, bc := fastpath.NewBundle(base)
	_, _ = fs.ReadFile(bundle, "config.yaml")
	fmt.Printf("bundle: readFile=%d open=%d\n", bc.ReadFileCount(), bc.OpenCount())

	plain, pc := fastpath.NewPlain(base)
	_, _ = fs.ReadFile(plain, "config.yaml")
	fmt.Printf("plain:  readFile=%d open=%d\n", pc.ReadFileCount(), pc.OpenCount())
}
```

The demo needs exported constructors and counter accessors (the wrapper types
and counters are unexported).

Append to `fastpath.go`:

```go
// NewBundle returns a fast-path FS over inner and its call counters.
func NewBundle(inner fs.FS) (fs.FS, *Counters) {
	b, c := newBundleFS(inner)
	return b, &Counters{c}
}

// NewPlain returns an Open-only FS over inner and its call counters.
func NewPlain(inner fs.FS) (fs.FS, *Counters) {
	p, c := newPlainFS(inner)
	return p, &Counters{c}
}

// Counters exposes the internal call counts for the demo and tests.
type Counters struct{ c *counters }

func (x *Counters) OpenCount() int64     { return x.c.open.Load() }
func (x *Counters) ReadFileCount() int64 { return x.c.readFile.Load() }
func (x *Counters) ReadDirCount() int64  { return x.c.readDir.Load() }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bundle: readFile=1 open=0
plain:  readFile=0 open=1
```

### Tests

`TestBundleUsesFastPath` calls `fs.ReadFile` against the bundle and asserts the
`ReadFile` counter is 1 and `Open` is 0 — the fast path was taken.
`TestPlainUsesFallback` calls `fs.ReadFile` against the plain FS and asserts
`Open` incremented — the fallback ran. `TestReadDirDispatch` does the parallel
check for `fs.ReadDir` against `ReadDirFS`. Same-package tests reach the
unexported wrappers directly.

Create `fastpath_test.go`:

```go
package fastpath

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func base() fstest.MapFS {
	return fstest.MapFS{
		"config.yaml":    {Data: []byte("k: v")},
		"conf.d/a.yaml":  {Data: []byte("a: 1")},
		"conf.d/b.yaml":  {Data: []byte("b: 2")},
	}
}

func TestBundleUsesFastPath(t *testing.T) {
	t.Parallel()

	b, c := newBundleFS(base())
	if _, err := fs.ReadFile(b, "config.yaml"); err != nil {
		t.Fatal(err)
	}
	if got := c.readFile.Load(); got != 1 {
		t.Fatalf("readFile count = %d, want 1", got)
	}
	if got := c.open.Load(); got != 0 {
		t.Fatalf("open count = %d, want 0 (fast path should not Open)", got)
	}
}

func TestPlainUsesFallback(t *testing.T) {
	t.Parallel()

	p, c := newPlainFS(base())
	if _, err := fs.ReadFile(p, "config.yaml"); err != nil {
		t.Fatal(err)
	}
	if got := c.open.Load(); got < 1 {
		t.Fatalf("open count = %d, want >= 1 (fallback must Open)", got)
	}
}

func TestReadDirDispatch(t *testing.T) {
	t.Parallel()

	b, c := newBundleFS(base())
	if _, err := fs.ReadDir(b, "conf.d"); err != nil {
		t.Fatal(err)
	}
	if got := c.readDir.Load(); got != 1 {
		t.Fatalf("readDir count = %d, want 1", got)
	}
	if got := c.open.Load(); got != 0 {
		t.Fatalf("open count = %d, want 0 on ReadDir fast path", got)
	}
}
```

## Review

The dispatch is correct when `fs.ReadFile` against a `ReadFileFS` bumps only the
`ReadFile` counter and never `Open`, while against an `Open`-only FS it bumps
`Open` — and the same for `fs.ReadDir`/`ReadDirFS`. The lesson is a design
discipline: a custom `fs.FS` on a hot path should implement the optional
interfaces, and you should *test* that the fast path is actually taken rather
than trusting it. Atomic counters make the invisible dispatch observable; the
`Open == 0` assertion is the one that would catch a wrapper that accidentally
forces the slow path. `fs.FileInfoToDirEntry` is the small utility for handing a
stat result back as the cheaper `DirEntry`.

## Resources

- [`fs.ReadFileFS` / `fs.ReadFile`](https://pkg.go.dev/io/fs#ReadFileFS) — the optional interface and the helper that dispatches to it.
- [`fs.ReadDirFS` / `fs.ReadDir`](https://pkg.go.dev/io/fs#ReadDirFS) — the directory fast path.
- [`fs.FileInfoToDirEntry`](https://pkg.go.dev/io/fs#FileInfoToDirEntry) — convert a `FileInfo` into a `DirEntry`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-fault-injection-fs-wrapper.md](07-fault-injection-fs-wrapper.md) | Next: [09-modtime-hot-reload-cache.md](09-modtime-hot-reload-cache.md)
