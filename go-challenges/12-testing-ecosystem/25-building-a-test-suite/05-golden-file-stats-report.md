# Exercise 5: Golden-File Test A Cache Stats Report With -update

Serialized output — a stats report, a JSON API body, a rendered template — rots
the moment you hand-write its expected bytes in a string literal. The golden-file
pattern compares the rendered output against a checked-in `testdata/*.golden`
fixture, regenerated on demand by a custom `-update` flag. This module adds a
`Cache.Stats()` renderer and golden-tests it.

## What you'll build

```text
statscache/                 independent module: example.com/statscache
  go.mod
  cache.go                  Cache + Stats() rendering deterministic JSON
  cmd/
    demo/
      main.go               prints the stats report
  cache_test.go             golden test with an -update flag
  testdata/
    stats.golden            the fixture (bootstrapped on first run)
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: `NewWithClock`, `Set`, `Stats()` returning deterministic indented JSON (sorted keys, frozen clock).
Test: render the report and byte-compare against `testdata/stats.golden`; `-update` regenerates it.
Verify: `go test -count=1 -race ./...` and `go test -run TestStatsGolden -update ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/statscache/cmd/demo
cd ~/go-exercises/statscache
go mod init example.com/statscache
```

### Determinism is the whole prerequisite

A golden test is only as trustworthy as the determinism of the output it pins. Two
things make a report flap between runs: unsorted map iteration and a real
timestamp. `Stats()` closes both holes. It collects per-key entries into a slice
and sorts them by key with `slices.SortFunc`, so the order is fixed regardless of
Go's randomized map iteration. And it reads the clock through the injected `now`
so the `active`/`expired` counts depend on the test's frozen instant, not on when
the test happens to run. Output is rendered with `json.MarshalIndent` — stable
field order comes from the struct definition, not a map — plus a trailing newline
so the file ends cleanly and diffs stay small.

The `-update` workflow is a package-level `flag.Bool`. Running
`go test -run TestStatsGolden -update` writes the freshly rendered bytes to
`testdata/stats.golden`; a normal run reads that file and byte-compares. The
`testdata` directory is special: the `go` tool ignores it when building, so it
ships as a fixture and is never mistaken for a package. The failure message prints
both sides and reminds the reader to run `-update` — you regenerate a golden with
the flag, never by hand-editing, because hand-editing is how a wrong expectation
gets frozen in.

One honesty note on this self-contained lesson: the test *bootstraps* the golden
if it is missing (first run in a fresh tree), so the module gates without a
pre-committed fixture. In a real repository you commit `testdata/stats.golden` and
turn a missing file into a hard failure, forcing an explicit `-update` — otherwise
a deleted fixture silently "passes". The bootstrap branch is commented as the spot
to tighten.

Create `cache.go`:

```go
package statscache

import (
	"cmp"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

type entry struct {
	value     []byte
	expiresAt time.Time
}

type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

// NewWithClock is the testability seam: it injects a clock so Stats is
// deterministic under test and in the demo.
func NewWithClock(now func() time.Time) *Cache {
	c := New()
	c.now = now
	return c
}

func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

// KeyStat is one row of the report.
type KeyStat struct {
	Key     string `json:"key"`
	Bytes   int    `json:"bytes"`
	Expired bool   `json:"expired"`
}

// Report is the serialized shape golden-tested by the suite.
type Report struct {
	Entries int       `json:"entries"`
	Active  int       `json:"active"`
	Expired int       `json:"expired"`
	Bytes   int       `json:"bytes"`
	Keys    []KeyStat `json:"keys"`
}

// Stats renders a deterministic JSON report: keys sorted, counts computed
// against the injected clock, trailing newline for clean diffs.
func (c *Cache) Stats() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.now()
	rep := Report{Keys: make([]KeyStat, 0, len(c.data))}
	for k, e := range c.data {
		expired := !e.expiresAt.IsZero() && now.After(e.expiresAt)
		rep.Entries++
		rep.Bytes += len(e.value)
		if expired {
			rep.Expired++
		} else {
			rep.Active++
		}
		rep.Keys = append(rep.Keys, KeyStat{Key: k, Bytes: len(e.value), Expired: expired})
	}
	slices.SortFunc(rep.Keys, func(a, b KeyStat) int { return cmp.Compare(a.Key, b.Key) })

	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
```

### The runnable demo

The demo freezes a clock, seeds three entries (one of which expires before the
report is rendered), and prints the report. A mutable `now` variable captured by
the clock closure lets the demo advance time after seeding.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"time"

	"example.com/statscache"
)

func main() {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := epoch
	c := statscache.NewWithClock(func() time.Time { return now })

	c.Set("alpha", []byte("hello"), 0)
	c.Set("beta", []byte("world!!"), time.Hour)
	c.Set("gamma", []byte("x"), time.Minute)

	now = epoch.Add(2 * time.Minute) // gamma's 1-minute TTL is now past

	report, err := c.Stats()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s", report)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "entries": 3,
  "active": 2,
  "expired": 1,
  "bytes": 13,
  "keys": [
    {
      "key": "alpha",
      "bytes": 5,
      "expired": false
    },
    {
      "key": "beta",
      "bytes": 7,
      "expired": false
    },
    {
      "key": "gamma",
      "bytes": 1,
      "expired": true
    }
  ]
}
```

### The golden test

Create `cache_test.go`:

```go
package statscache

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "regenerate golden files")

func TestStatsGolden(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := epoch
	c := NewWithClock(func() time.Time { return now })
	c.Set("alpha", []byte("hello"), 0)
	c.Set("beta", []byte("world!!"), time.Hour)
	c.Set("gamma", []byte("x"), time.Minute)
	now = epoch.Add(2 * time.Minute)

	got, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats() err = %v", err)
	}

	golden := filepath.Join("testdata", "stats.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", golden)
	}

	want, err := os.ReadFile(golden)
	if errors.Is(err, os.ErrNotExist) {
		// Fresh tree: bootstrap the fixture so the lesson gates standalone.
		// In a committed repo, replace this branch with t.Fatalf to require
		// an explicit -update when the golden is missing.
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		want = got
	} else if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("stats report mismatch (run: go test -run TestStatsGolden -update)\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
```

## Review

The golden test is trustworthy only because `Stats()` is deterministic: keys
sorted with `slices.SortFunc`, counts computed against the injected clock, and a
stable JSON shape from a struct rather than a map. Regenerate the fixture with
`go test -run TestStatsGolden -update` and never by editing the file; the failure
message points you at exactly that command. The bootstrap branch keeps this
standalone lesson gating without a committed fixture, but the moment this ships in
a real repo you commit `testdata/stats.golden` and make a missing file fail, so a
deleted or forgotten fixture cannot silently pass. Run `-race` because `Stats()`
reads the same `RWMutex` the writers hold.

## Resources

- [`encoding/json.MarshalIndent`](https://pkg.go.dev/encoding/json#MarshalIndent) — stable, indented serialization.
- [`flag.Bool`](https://pkg.go.dev/flag#Bool) — the `-update` flag the golden workflow depends on.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — deterministic ordering that keeps the golden stable.
- [Go source: `cmd/gofmt` uses testdata golden files](https://cs.opensource.google/go/go/+/master:src/cmd/gofmt/) — the pattern in the standard library itself.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-table-driven-get.md](04-table-driven-get.md) | Next: [06-testmain-and-short.md](06-testmain-and-short.md)
