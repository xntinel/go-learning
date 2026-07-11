# Exercise 4: Batch Importer — Escaping the defer-in-a-loop File-Handle Leak

A `defer` is scoped to the enclosing function, not the loop iteration. Put
`defer f.Close()` inside a `range` over N sources and you hold all N handles open
until the whole import returns — a file-descriptor leak that passes a small test
and exhausts the process in production. This module builds a batch importer both
ways and uses an instrumented fake resource to prove the difference: the fixed
version keeps at most one handle live; the naive version keeps N.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
importer/                   independent module: example.com/importer
  go.mod
  importer/importer.go       Tracker + Resource gauge; ImportAll (fixed), NaiveImportAll (leaky)
  cmd/demo/main.go           import 1000 sources, print peak concurrent open
  importer/importer_test.go  fixed keeps max-open == 1; naive keeps max-open == N; all closed once
```

- Files: `importer/importer.go`, `cmd/demo/main.go`, `importer/importer_test.go`.
- Implement: `ImportAll(sources, open)` that delegates each source to a helper `importOne` whose `defer r.Close()` fires at the end of *its* call; and `NaiveImportAll` that defers inside the loop, kept as a labelled anti-pattern to measure the leak.
- Test: a `Tracker` fake counts live-open resources and records the maximum ever seen; feed 1000 sources and assert `ImportAll` peaks at 1 concurrent open while `NaiveImportAll` peaks at N, and that every resource's `Close` ran exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/importer/importer ~/go-exercises/importer/cmd/demo
cd ~/go-exercises/importer
go mod init example.com/importer
```

### Function scope is the whole story

`defer` runs when the *function* returns, not when a block or a loop iteration
ends. So this:

```go
for _, src := range sources {
	r, _ := open(src)
	defer r.Close()   // does NOT close at end of iteration
	r.Import()
}
```

registers one deferred `Close` per iteration and runs none of them until the
enclosing function returns. With ten thousand sources you accumulate ten thousand
open handles, and the operating system's per-process file-descriptor limit — a
few thousand by default — kills you long before the loop finishes. The bug is
invisible in a unit test with three inputs and fatal in production with a real
batch.

The fix is a scope change, not a cleverness change: extract the open/use/close of
a single source into its own function. Now the `defer` is scoped to *that*
function, so it fires at the end of each call, and at most one resource is live at
any instant. The importer becomes a thin loop that calls the per-source helper:

```go
for _, src := range sources {
	if err := importOne(src, open); err != nil {
		return err
	}
}
```

To make the leak measurable rather than merely asserted, the fake resource
carries a shared `Tracker` that increments a live-open gauge on `Open`, decrements
it on `Close`, and remembers the maximum the gauge ever reached. After importing
1000 sources, the fixed importer's peak is 1; the naive importer's peak is 1000.
That gap is the leak, quantified.

Create `importer/importer.go`:

```go
package importer

import (
	"fmt"
	"sync/atomic"
)

// Tracker instruments resource lifetimes: it counts live (opened-but-not-closed)
// resources and remembers the peak, so a test can prove how many handles a
// strategy holds at once.
type Tracker struct {
	live   atomic.Int64
	max    atomic.Int64
	closes atomic.Int64
}

// Open records a new live resource and updates the peak.
func (t *Tracker) Open(name string) *Resource {
	n := t.live.Add(1)
	for {
		m := t.max.Load()
		if n <= m || t.max.CompareAndSwap(m, n) {
			break
		}
	}
	return &Resource{tracker: t, name: name}
}

func (t *Tracker) MaxLive() int64 { return t.max.Load() }
func (t *Tracker) Live() int64    { return t.live.Load() }
func (t *Tracker) Closes() int64  { return t.closes.Load() }

// Resource is a fake file handle whose open/close is counted by its Tracker.
type Resource struct {
	tracker *Tracker
	name    string
	closed  bool
}

// Read stands in for importing a source's contents.
func (r *Resource) Read() (int, error) { return 1, nil }

// Close returns the handle. It is idempotent so a double close does not
// undercount the live gauge.
func (r *Resource) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.tracker.live.Add(-1)
	r.tracker.closes.Add(1)
	return nil
}

// OpenFunc opens a source by name.
type OpenFunc func(name string) (*Resource, error)

// ImportAll processes every source, keeping at most one resource live by
// delegating each to importOne, whose defer fires at the end of its own call.
func ImportAll(sources []string, open OpenFunc) (int, error) {
	total := 0
	for _, src := range sources {
		n, err := importOne(src, open)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func importOne(src string, open OpenFunc) (n int, err error) {
	r, err := open(src)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", src, err)
	}
	defer r.Close() // fires at the end of THIS function, i.e. this iteration
	return r.Read()
}

// NaiveImportAll is the anti-pattern: it defers Close inside the loop, so every
// handle stays open until the whole function returns. It exists only to measure
// the leak in a test. Do not ship this shape.
func NaiveImportAll(sources []string, open OpenFunc) (int, error) {
	total := 0
	for _, src := range sources {
		r, err := open(src)
		if err != nil {
			return total, fmt.Errorf("open %s: %w", src, err)
		}
		defer r.Close() // BUG: function-scoped, holds every handle at once
		n, err := r.Read()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/importer/importer"
)

func main() {
	tr := &importer.Tracker{}
	open := func(name string) (*importer.Resource, error) { return tr.Open(name), nil }

	sources := make([]string, 1000)
	for i := range sources {
		sources[i] = fmt.Sprintf("src-%d", i)
	}

	total, err := importer.ImportAll(sources, open)
	if err != nil {
		panic(err)
	}
	fmt.Println("imported:", total)
	fmt.Println("peak concurrent open:", tr.MaxLive())
	fmt.Println("closes:", tr.Closes())
	fmt.Println("still live:", tr.Live())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
imported: 1000
peak concurrent open: 1
closes: 1000
still live: 0
```

### Tests

The tests feed 1000 sources through both importers and read the peak the tracker
recorded. The fixed importer peaks at 1; the naive one peaks at N. Both close
every handle exactly once — the difference is *when*, and how many are live at the
peak.

Create `importer/importer_test.go`:

```go
package importer

import (
	"fmt"
	"testing"
)

func makeSources(n int) []string {
	s := make([]string, n)
	for i := range s {
		s[i] = fmt.Sprintf("src-%d", i)
	}
	return s
}

func TestImportAllKeepsOneHandleLive(t *testing.T) {
	t.Parallel()

	const n = 1000
	tr := &Tracker{}
	open := func(name string) (*Resource, error) { return tr.Open(name), nil }

	total, err := ImportAll(makeSources(n), open)
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Errorf("total = %d, want %d", total, n)
	}
	if tr.MaxLive() != 1 {
		t.Errorf("peak concurrent open = %d, want 1", tr.MaxLive())
	}
	if tr.Closes() != n {
		t.Errorf("closes = %d, want %d", tr.Closes(), n)
	}
	if tr.Live() != 0 {
		t.Errorf("still live = %d, want 0", tr.Live())
	}
}

func TestNaiveImportAllLeaksAllHandles(t *testing.T) {
	t.Parallel()

	const n = 1000
	tr := &Tracker{}
	open := func(name string) (*Resource, error) { return tr.Open(name), nil }

	total, err := NaiveImportAll(makeSources(n), open)
	if err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Errorf("total = %d, want %d", total, n)
	}
	// The leak, quantified: every handle was held open at once.
	if tr.MaxLive() != n {
		t.Errorf("peak concurrent open = %d, want %d (the leak)", tr.MaxLive(), n)
	}
	// They do all close eventually, at function return.
	if tr.Live() != 0 {
		t.Errorf("still live after return = %d, want 0", tr.Live())
	}
}

func Example() {
	tr := &Tracker{}
	open := func(name string) (*Resource, error) { return tr.Open(name), nil }
	total, _ := ImportAll([]string{"a", "b", "c"}, open)
	fmt.Println(total, tr.MaxLive())
	// Output: 3 1
}
```

## Review

The importer is correct when it imports every source and keeps at most one
resource live at a time; the tracker's `MaxLive()` of 1 is the proof. The naive
version imports the same sources with the same final state — everything closes at
return — but its `MaxLive()` of N is the leak the test exists to expose, and in
production that N is file descriptors the OS will refuse to grant past its limit.
The fix is never "add a manual `r.Close()` before `continue`" — that breaks on any
early return or panic within the iteration; it is the scope change of extracting a
per-source function so `defer` fires once per call, on every exit path. Run
`go test -race` to confirm the tracker's atomics report a consistent peak under
the detector.

## Resources

- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer)
- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64)
- [Go Wiki: Common Mistakes](https://go.dev/wiki/CommonMistakes)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-defer-close-error-into-named-return.md](03-defer-close-error-into-named-return.md) | Next: [05-mutex-defer-unlock-critical-section.md](05-mutex-defer-unlock-critical-section.md)
