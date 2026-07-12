# Exercise 9: Hot-reload config cache driven by fs.Stat ModTime

A service that hot-reloads its config re-parses the file only when it actually
changed — comparing the file's `ModTime` against the cached snapshot. Testing
that logic normally means writing a file, sleeping, rewriting it, and hoping the
mtime advanced. `MapFile.ModTime` makes the time dimension an explicit value:
swapping the `MapFS` entry for one with a later `ModTime` simulates an on-disk
edit with no sleeps and no wall clock. This exercise builds that cache and tests
every branch deterministically.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
reloadcfg/                   independent module: example.com/reloadcfg
  go.mod                     go 1.26
  reload.go                  ReloadingConfig; New(fs.FS, name); Get() re-parses on newer ModTime
  cmd/
    demo/
      main.go                Get, swap the MapFile ModTime, Get again; show parse count
  reload_test.go             no-reparse, reparse-on-change, stat-error, concurrency tests
```

- Files: `reload.go`, `cmd/demo/main.go`, `reload_test.go`.
- Implement: `New(fsys fs.FS, name string) *ReloadingConfig` and `Get() (Config,
  error)` that `fs.Stat`s the file, returns the cached value when `ModTime` is
  unchanged, and re-parses only when `ModTime` is newer; a `Stat` error is
  surfaced, not swallowed.
- Test: first `Get` parses; a second `Get` with unchanged `ModTime` does not
  re-parse (parse counter stable); replacing the entry with a later `ModTime`
  causes exactly one re-parse and the new value; a `Stat` error is returned.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/09-modtime-hot-reload-cache/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/09-modtime-hot-reload-cache
```

### ModTime is the reload trigger; make it an explicit value

The cache memoizes a parsed `Config` plus the `ModTime` it was parsed from. On
`Get`, it `fs.Stat`s the file and compares: if the file's `ModTime` is not after
the cached one, the cached `Config` is still current and is returned without
re-parsing; if it is newer, the file changed on disk and the cache re-parses and
updates its snapshot. That single comparison is the whole hot-reload contract,
and it is a *pure function of the stat result* — which is exactly what makes it
testable without a real clock. In production the `ModTime` comes from the real
file's mtime; in a test you set it in the `MapFile` and, to simulate an edit, you
replace the map entry with a new `MapFile` carrying a later `ModTime`. No
`time.Sleep`, no flakiness, and the "did it re-parse?" question is answered by a
parse counter you increment on each parse.

Two design points a senior review would flag. First, concurrency: `Get` may be
called from many request goroutines, so the snapshot is guarded by a
`sync.RWMutex` — the common case (unchanged file) takes only a read lock, and the
write lock is taken only to install a new parse. Because two goroutines could
both observe a newer `ModTime` before either reloads, the code *re-checks* the
condition after acquiring the write lock, so the file is parsed once, not once
per racer (the standard double-checked pattern). Second, error handling: a
`Stat` failure must be *surfaced*, not swallowed into a stale value. A cache that
silently keeps serving the last-good config when the file vanished hides a real
operational problem; `Get` returns the error so the caller decides.

Create `reload.go`:

```go
package reloadcfg

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is the parsed key=value config.
type Config struct {
	Host string
	Port int
}

// ReloadingConfig memoizes a parsed Config and re-parses only when the file's
// ModTime is newer than the cached snapshot. It is safe for concurrent Get.
type ReloadingConfig struct {
	fsys fs.FS
	name string

	mu      sync.RWMutex
	cfg     Config
	modTime time.Time
	loaded  bool

	parses atomic.Int64 // observability: how many times the file was parsed
}

// New returns a cache that reloads name from fsys when its ModTime advances.
func New(fsys fs.FS, name string) *ReloadingConfig {
	return &ReloadingConfig{fsys: fsys, name: name}
}

// Parses reports how many times the file has been parsed (for tests/metrics).
func (c *ReloadingConfig) Parses() int64 { return c.parses.Load() }

// Get returns the current config, re-parsing only if the file's ModTime is
// newer than the cached snapshot. A Stat error is surfaced, not swallowed.
func (c *ReloadingConfig) Get() (Config, error) {
	info, err := fs.Stat(c.fsys, c.name)
	if err != nil {
		return Config{}, fmt.Errorf("reload stat %s: %w", c.name, err)
	}
	mod := info.ModTime()

	c.mu.RLock()
	if c.loaded && !mod.After(c.modTime) {
		cfg := c.cfg
		c.mu.RUnlock()
		return cfg, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && !mod.After(c.modTime) { // re-check under the write lock
		return c.cfg, nil
	}
	cfg, err := parse(c.fsys, c.name)
	if err != nil {
		return Config{}, err
	}
	c.cfg = cfg
	c.modTime = mod
	c.loaded = true
	c.parses.Add(1)
	return cfg, nil
}

func parse(fsys fs.FS, name string) (Config, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Config{}, fmt.Errorf("reload read %s: %w", name, err)
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
				return Config{}, fmt.Errorf("reload %s: invalid port: %w", name, err)
			}
			cfg.Port = n
		}
	}
	return cfg, nil
}
```

### The runnable demo

The demo builds a `MapFS` with an initial `ModTime`, reads it (one parse), reads
again (no new parse), then replaces the entry with a later `ModTime` and new
bytes and reads again (one more parse, new value) — a full reload cycle with no
sleeps.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"testing/fstest"

	"example.com/reloadcfg"
)

func main() {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=old.local\nport=1111\n"), ModTime: t0},
	}
	cache := reloadcfg.New(fsys, "config.txt")

	c1, _ := cache.Get()
	c2, _ := cache.Get() // unchanged: served from cache
	fmt.Printf("first:  host=%s parses=%d\n", c1.Host, cache.Parses())
	fmt.Printf("cached: host=%s parses=%d\n", c2.Host, cache.Parses())

	// Simulate an on-disk edit: newer ModTime, new bytes.
	fsys["config.txt"] = &fstest.MapFile{
		Data:    []byte("host=new.local\nport=2222\n"),
		ModTime: t0.Add(time.Hour),
	}

	c3, _ := cache.Get()
	fmt.Printf("reload: host=%s parses=%d\n", c3.Host, cache.Parses())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first:  host=old.local parses=1
cached: host=old.local parses=1
reload: host=new.local parses=2
```

### Tests

`TestNoReparseWhenUnchanged` calls `Get` twice against a fixed `ModTime` and
asserts `Parses()` stays 1. `TestReparseOnNewerModTime` swaps the `MapFile` for
one with a later `ModTime` and asserts exactly one additional parse and the new
value. `TestStatErrorSurfaced` points the cache at an absent file and asserts
`Get` returns an error rather than a zero-but-nil-error stale value.
`TestConcurrentGet` hammers `Get` from many goroutines under `-race` to prove the
`RWMutex` guards the snapshot.

Create `reload_test.go`:

```go
package reloadcfg

import (
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestNoReparseWhenUnchanged(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=a.local\nport=1\n"), ModTime: epoch},
	}
	c := New(fsys, "config.txt")

	if _, err := c.Get(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(); err != nil {
		t.Fatal(err)
	}
	if got := c.Parses(); got != 1 {
		t.Fatalf("Parses = %d, want 1 (unchanged file must not re-parse)", got)
	}
}

func TestReparseOnNewerModTime(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=old.local\nport=1\n"), ModTime: epoch},
	}
	c := New(fsys, "config.txt")

	first, err := c.Get()
	if err != nil {
		t.Fatal(err)
	}
	if first.Host != "old.local" {
		t.Fatalf("first host = %q, want old.local", first.Host)
	}

	fsys["config.txt"] = &fstest.MapFile{
		Data:    []byte("host=new.local\nport=2\n"),
		ModTime: epoch.Add(time.Hour),
	}

	second, err := c.Get()
	if err != nil {
		t.Fatal(err)
	}
	if second.Host != "new.local" || second.Port != 2 {
		t.Fatalf("second = %+v, want {new.local 2}", second)
	}
	if got := c.Parses(); got != 2 {
		t.Fatalf("Parses = %d, want 2 (one reload)", got)
	}
}

func TestStatErrorSurfaced(t *testing.T) {
	t.Parallel()

	c := New(fstest.MapFS{}, "absent.txt")
	if _, err := c.Get(); err == nil {
		t.Fatal("Get returned nil error for a missing file")
	}
}

func TestConcurrentGet(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"config.txt": {Data: []byte("host=a.local\nport=1\n"), ModTime: epoch},
	}
	c := New(fsys, "config.txt")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := c.Parses(); got != 1 {
		t.Fatalf("Parses = %d, want 1 under concurrent Get", got)
	}
}
```

## Review

The cache is correct when re-parsing is a pure function of the stat comparison:
an unchanged `ModTime` serves the memoized `Config` with no parse, a newer
`ModTime` triggers exactly one re-parse and installs the new value, and a `Stat`
failure is returned rather than hidden behind a stale snapshot. The
double-checked re-check under the write lock is what keeps a concurrent burst to
a single parse — `TestConcurrentGet` asserts that under `-race`. The whole
exercise hinges on `MapFile.ModTime` being a value you set: it turns a
time-dependent reload path into a deterministic, instant test, with no
`time.Sleep` anywhere.

## Resources

- [`fs.Stat` and `fs.FileInfo`](https://pkg.go.dev/io/fs#Stat) — the stat call and the `ModTime` it exposes.
- [`fstest.MapFile`](https://pkg.go.dev/testing/fstest#MapFile) — the `ModTime` field that drives the reload deterministically.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read-mostly guarding for the cached snapshot.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-optional-interface-fast-path.md](08-optional-interface-fast-path.md) | Next: [../12-t-cleanup-patterns/00-concepts.md](../12-t-cleanup-patterns/00-concepts.md)
