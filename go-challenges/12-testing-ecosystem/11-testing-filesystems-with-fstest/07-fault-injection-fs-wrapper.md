# Exercise 7: Inject read and permission errors with a custom fs.FS

`fstest.MapFS` has one deliberate blind spot: every stored file reads cleanly, so
it can never exercise the error branches your I/O code exists to handle. This
exercise closes that gap by hand-writing a `faultFS` (and a faulty `fs.File`)
that injects a permission error on `Open` or an unexpected-EOF mid-`Read` for a
named path, composed over a normal `MapFS`. It then uses that wrapper to prove a
config loader surfaces the error instead of returning a half-filled result.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
faultfs/                     independent module: example.com/faultfs
  go.mod                     go 1.26
  faultfs.go                 faultFS, faultyFile; Load(fs.FS, name) config loader
  cmd/
    demo/
      main.go                load a good file, then an Open-failing and a Read-failing one
  faultfs_test.go            errors.Is against injected sentinels; no partial config
```

- Files: `faultfs.go`, `cmd/demo/main.go`, `faultfs_test.go`.
- Implement: `faultFS` wrapping an `fs.FS` that returns a configured error from
  `Open` for one path and wraps another path's file so `Read` fails mid-stream;
  a `Load(fsys fs.FS, name string) (Config, error)` that reads via `fs.ReadFile`.
- Test: compose `faultFS` over `MapFS` so one `Open` returns `fs.ErrPermission`
  and one `Read` returns `io.ErrUnexpectedEOF`; assert `Load` surfaces each via
  `errors.Is` and returns a zero `Config`, never a partial one.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/faultfs/cmd/demo
cd ~/go-exercises/faultfs
go mod init example.com/faultfs
```

### Why a wrapper, and what an fs.File actually is

`MapFS` cannot fail a read because a map has no I/O to fail — so the "read
returned an error" and "permission denied on open" branches of your loader stay
uncovered no matter how many `MapFS` fixtures you write. The fix is a wrapper
that implements `fs.FS` and injects the failure you want, delegating everything
else to a real `MapFS` so the rest of the tree behaves normally.

Two failure modes matter, and they live at different layers. An *open* failure
is easy: `faultFS.Open(name)` checks whether `name` is the designated
open-failing path and, if so, returns `nil` and a `*fs.PathError` wrapping the
injected sentinel (here `fs.ErrPermission`) before delegating. A *read* failure
is subtler because it happens after `Open` succeeds: you must return a real,
openable file whose `Read` fails partway. That means implementing `fs.File` —
the three-method interface `Read(p []byte) (int, error)`, `Stat() (fs.FileInfo,
error)`, `Close() error`. Our `faultyFile` delegates `Stat` and `Close` to the
underlying file but overrides `Read` to return the injected error (here
`io.ErrUnexpectedEOF`), simulating a stream that dies mid-transfer.

The point of the exercise is what this coverage *proves* about the loader. When
`fs.ReadFile` hits an `Open` error or a `Read` error, it returns that error, and
a correct loader wraps it with `%w` and returns a *zero* `Config` — never a
struct half-populated from the bytes read before the failure. The test asserts
both: the error is `errors.Is`-matchable against the injected sentinel, and the
returned `Config` is the zero value. A loader that returned partial state on a
read error would be a real bug, and only a fault-injecting FS can catch it.

Create `faultfs.go`. It also exposes a small `LoadDemo` helper (used by the
runnable demo, since the fault wrapper itself is unexported) that composes the
wrapper over a `MapFS` and loads a named file:

```go
package faultfs

import (
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing/fstest"
)

// faultFS wraps an fs.FS and injects failures for specific paths: openErr is
// returned from Open(openFail); readFail's file opens but its Read returns
// readErr mid-stream. All other paths delegate unchanged.
type faultFS struct {
	inner    fs.FS
	openFail string
	openErr  error
	readFail string
	readErr  error
}

func (f faultFS) Open(name string) (fs.File, error) {
	if f.openErr != nil && name == f.openFail {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.openErr}
	}
	file, err := f.inner.Open(name)
	if err != nil {
		return nil, err
	}
	if f.readErr != nil && name == f.readFail {
		return faultyFile{File: file, readErr: f.readErr}, nil
	}
	return file, nil
}

// faultyFile opens normally but fails its first Read, simulating a stream that
// dies mid-transfer. Stat and Close delegate to the real file.
type faultyFile struct {
	fs.File
	readErr error
}

func (f faultyFile) Read([]byte) (int, error) {
	return 0, f.readErr
}

// Config is the parsed key=value config.
type Config struct {
	Host string
	Port int
}

// Load reads and parses a key=value config from fsys via fs.ReadFile. Any I/O
// error is wrapped and returned with a zero Config; the loader never returns a
// partially populated Config on failure.
func Load(fsys fs.FS, name string) (Config, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Config{}, fmt.Errorf("load %s: %w", name, err)
	}
	var cfg Config
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "host":
			cfg.Host = strings.TrimSpace(val)
		case "port":
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return Config{}, fmt.Errorf("load %s: invalid port: %w", name, err)
			}
			cfg.Port = n
		}
	}
	return cfg, nil
}

// LoadDemo builds a MapFS with a fault wrapper (denied.txt fails Open, torn.txt
// fails Read) and loads the named file, for the runnable demo.
func LoadDemo(name string) (Config, error) {
	base := fstest.MapFS{
		"good.txt":   {Data: []byte("host=db.local\nport=5432\n")},
		"denied.txt": {Data: []byte("host=secret\n")},
		"torn.txt":   {Data: []byte("host=db.local\n")},
	}
	fsys := faultFS{
		inner:    base,
		openFail: "denied.txt",
		openErr:  fs.ErrPermission,
		readFail: "torn.txt",
		readErr:  io.ErrUnexpectedEOF,
	}
	return Load(fsys, name)
}
```

### The runnable demo

The demo composes a `faultFS` over a `MapFS` with three files (via the exported
`LoadDemo` helper, since the wrapper type is unexported), then loads each: the
good one succeeds, the open-failing one surfaces a permission error, and the
read-failing one surfaces an unexpected EOF.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/faultfs"
)

func main() {
	for _, name := range []string{"good.txt", "denied.txt", "torn.txt"} {
		cfg, err := faultfs.LoadDemo(name)
		if err != nil {
			fmt.Printf("%s -> error: %v\n", name, err)
			continue
		}
		fmt.Printf("%s -> host=%s port=%d\n", name, cfg.Host, cfg.Port)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good.txt -> host=db.local port=5432
denied.txt -> error: load denied.txt: open denied.txt: permission denied
torn.txt -> error: load torn.txt: unexpected EOF
```

### Tests

`TestOpenFailureSurfaces` composes a `faultFS` whose `denied.txt` fails `Open`
with `fs.ErrPermission` and asserts `Load` returns an error satisfying
`errors.Is(err, fs.ErrPermission)` and a zero `Config`.
`TestReadFailureSurfaces` does the same for a `torn.txt` whose `Read` fails with
`io.ErrUnexpectedEOF`, asserting the error and the zero `Config` — the case
`MapFS` alone can never produce. `TestGoodFileStillWorks` proves the wrapper
delegates untouched paths.

Create `faultfs_test.go`:

```go
package faultfs

import (
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

func composed() faultFS {
	base := fstest.MapFS{
		"good.txt":   {Data: []byte("host=db.local\nport=5432\n")},
		"denied.txt": {Data: []byte("host=secret\n")},
		"torn.txt":   {Data: []byte("host=db.local\n")},
	}
	return faultFS{
		inner:    base,
		openFail: "denied.txt",
		openErr:  fs.ErrPermission,
		readFail: "torn.txt",
		readErr:  io.ErrUnexpectedEOF,
	}
}

func TestOpenFailureSurfaces(t *testing.T) {
	t.Parallel()

	cfg, err := Load(composed(), "denied.txt")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want errors.Is fs.ErrPermission", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("Config = %+v, want zero on error", cfg)
	}
}

func TestReadFailureSurfaces(t *testing.T) {
	t.Parallel()

	cfg, err := Load(composed(), "torn.txt")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want errors.Is io.ErrUnexpectedEOF", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("Config = %+v, want zero on error", cfg)
	}
}

func TestGoodFileStillWorks(t *testing.T) {
	t.Parallel()

	cfg, err := Load(composed(), "good.txt")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.local" || cfg.Port != 5432 {
		t.Fatalf("Config = %+v, want {db.local 5432}", cfg)
	}
}
```

## Review

The wrapper is correct when it injects exactly the configured failures and
delegates everything else, and the loader is correct when it surfaces those
failures with `%w` and returns a zero `Config` — never partial state. The whole
point is coverage `MapFS` cannot give: the `Open`-permission and mid-stream-`Read`
branches only run against a hand-written fault FS. The `fs.File` interface is
three methods (`Read`, `Stat`, `Close`); the faulty file embeds a real one and
overrides only `Read`, which is the minimal way to corrupt exactly one behavior.
The assertion that pays off most is `cfg != (Config{})` on the error paths — it
catches a loader that leaks half-read state.

## Resources

- [`fs.File`](https://pkg.go.dev/io/fs#File) — the `Read`/`Stat`/`Close` interface a fault file must satisfy.
- [`fs.ErrPermission` and `fs.PathError`](https://pkg.go.dev/io/fs#pkg-variables) — the sentinel and the error type to wrap.
- [`io.ErrUnexpectedEOF`](https://pkg.go.dev/io#pkg-variables) — the mid-stream read failure to inject.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-glob-config-discovery.md](06-glob-config-discovery.md) | Next: [08-optional-interface-fast-path.md](08-optional-interface-fast-path.md)
