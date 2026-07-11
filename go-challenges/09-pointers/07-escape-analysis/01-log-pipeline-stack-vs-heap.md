# Exercise 1: Log Pipeline: Value-on-Stack vs Pointer-to-Heap

The clearest way to see escape analysis is to build the same log entry two ways —
one that returns a value and stays on the stack, one that returns a pointer and is
forced to the heap — and then read the compiler's verdict on each. This module
builds a small in-memory log pipeline around that contrast.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
logpipeline/                  independent module: example.com/logpipeline
  go.mod                      go 1.26
  log.go                      Entry; NewEntry, WithField, BuildStack (value),
                              BuildHeap (*Entry); Pipeline.Add/Snapshot (defensive copy)
  cmd/
    demo/
      main.go                 builds entries both ways; shows snapshot independence
  log_test.go                 field init, WithField lazy map, value vs pointer,
                              Snapshot independence
```

Files: `log.go`, `cmd/demo/main.go`, `log_test.go`.
Implement: `Entry` with `NewEntry`, `WithField`; `BuildStack` returning `Entry`
(does not escape); `BuildHeap` returning `*Entry` (moved to heap); a `Pipeline`
with `Add` and a copy-independent `Snapshot`.
Test: field initialization, lazy `Fields` map init, `BuildStack` value semantics,
`BuildHeap` non-nil pointer, and `Snapshot` independence after mutation.
Verify: `go test -count=1 -race ./...`, then observe the escape decisions with
`go build -gcflags=-m ./... 2>&1 | grep -E 'moved to heap|does not escape'`.

Set up the module:

```bash
mkdir -p ~/go-exercises/logpipeline/cmd/demo
cd ~/go-exercises/logpipeline
go mod init example.com/logpipeline
```

### The two constructors, and why they differ

`BuildStack` builds an `Entry` and returns it by value. Its lifetime is bounded by
the call: the caller receives a copy, and the local `e` inside `BuildStack` can be
reclaimed the instant the function returns. The compiler can prove this, so `e`
stays on the stack — the allocation is a pointer bump, free of any GC cost.

`BuildHeap` builds the identical `Entry` but returns `&e`. Now the value must
survive the return, because the caller holds a pointer into it. The compiler
cannot let `e` die with the frame, so it moves `e` to the heap. This is the
single most important escape rule to internalize: returning the address of a
local forces that local onto the heap, every time. It is not a copy you avoided;
it is a heap allocation you incurred.

The trap this exercise inoculates against is "return `*Entry` to avoid copying the
struct". On a hot path where the caller only reads the entry, the value return is
strictly cheaper: a stack copy of a handful of words versus a heap allocation plus
a GC-scannable object. Reach for the pointer only when the entry must be shared or
mutated through the pointer.

`WithField` shows a second idea: the `Fields` map is created lazily, on first use,
so an entry with no structured fields carries a `nil` map and allocates nothing
for it. `Pipeline.Snapshot` shows a third: it returns a defensive copy of the
internal slice, so a caller iterating a snapshot is not exposed to concurrent
`Add`s mutating the backing array underneath it.

Create `log.go`:

```go
package log

import "time"

// Entry is a single structured log record. Its fields are exported so the demo
// (a separate package) and JSON encoders can read them.
type Entry struct {
	Time    time.Time
	Level   string
	Message string
	Fields  map[string]any
}

// NewEntry builds an Entry with the current time. Returned by value.
func NewEntry(level, msg string) Entry {
	return Entry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
}

// WithField returns a copy of e with k=v added. The Fields map is created lazily
// on first use, so a field-less entry allocates no map.
func (e Entry) WithField(k string, v any) Entry {
	if e.Fields == nil {
		e.Fields = make(map[string]any, 1)
	}
	e.Fields[k] = v
	return e
}

// BuildStack returns an Entry by value. The local does not escape: it stays on
// the stack and is reclaimed when the function returns.
func BuildStack(level, msg string) Entry {
	e := Entry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
	return e
}

// BuildHeap returns a pointer to a local Entry. Because the caller holds the
// address, the value must outlive the frame, so it is moved to the heap.
func BuildHeap(level, msg string) *Entry {
	e := Entry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
	return &e
}

// Pipeline is an in-memory sink that accumulates entries.
type Pipeline struct {
	entries []Entry
}

func NewPipeline() *Pipeline {
	return &Pipeline{}
}

// Add appends an entry to the pipeline.
func (p *Pipeline) Add(e Entry) {
	p.entries = append(p.entries, e)
}

// Len reports how many entries are stored.
func (p *Pipeline) Len() int {
	return len(p.entries)
}

// Snapshot returns an independent copy of the stored entries, so a caller is not
// exposed to later Add calls mutating the backing array.
func (p *Pipeline) Snapshot() []Entry {
	out := make([]Entry, len(p.entries))
	copy(out, p.entries)
	return out
}
```

### The runnable demo

The demo builds an entry each way, adds a couple to a pipeline, takes a snapshot,
then adds one more to prove the snapshot did not grow. It avoids printing the
timestamp so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	logpkg "example.com/logpipeline"
)

func main() {
	v := logpkg.BuildStack("info", "value on stack")
	p := logpkg.BuildHeap("warn", "pointer to heap")
	fmt.Printf("stack: %s %s\n", v.Level, v.Message)
	fmt.Printf("heap:  %s %s\n", p.Level, p.Message)

	entry := logpkg.NewEntry("info", "charge").WithField("cents", 1999)
	fmt.Printf("field: cents=%v\n", entry.Fields["cents"])

	pipe := logpkg.NewPipeline()
	pipe.Add(logpkg.NewEntry("info", "a"))
	pipe.Add(logpkg.NewEntry("error", "b"))
	snap := pipe.Snapshot()
	pipe.Add(logpkg.NewEntry("info", "c"))
	fmt.Printf("snapshot len=%d pipeline len=%d\n", len(snap), pipe.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stack: info value on stack
heap:  warn pointer to heap
field: cents=1999
snapshot len=2 pipeline len=3
```

### Tests

The tests exercise the two allocation patterns and the copy semantics. They do
not assert *where* a value lives — that is Exercise 2's job with `AllocsPerRun`;
here the tests assert behavior, and the escape decisions are observed separately
with `-gcflags=-m`.

Create `log_test.go`:

```go
package log

import (
	"fmt"
	"testing"
)

func TestNewEntryInitializesFields(t *testing.T) {
	t.Parallel()
	e := NewEntry("info", "hello")
	if e.Level != "info" {
		t.Errorf("Level = %q, want info", e.Level)
	}
	if e.Message != "hello" {
		t.Errorf("Message = %q, want hello", e.Message)
	}
	if e.Time.IsZero() {
		t.Error("Time is zero; NewEntry must stamp time.Now")
	}
	if e.Fields != nil {
		t.Errorf("Fields = %v, want nil before WithField", e.Fields)
	}
}

func TestWithFieldLazyMap(t *testing.T) {
	t.Parallel()
	e := NewEntry("info", "hello")
	if e.Fields != nil {
		t.Fatal("Fields should be nil until WithField is called")
	}
	e = e.WithField("user", "alice").WithField("tenant", "acme")
	if got := e.Fields["user"]; got != "alice" {
		t.Errorf("Fields[user] = %v, want alice", got)
	}
	if got := e.Fields["tenant"]; got != "acme" {
		t.Errorf("Fields[tenant] = %v, want acme", got)
	}
}

func TestBuildStackReturnsValue(t *testing.T) {
	t.Parallel()
	e := BuildStack("info", "hello")
	if e.Message != "hello" || e.Level != "info" {
		t.Errorf("BuildStack = %+v, want level=info message=hello", e)
	}
}

func TestBuildHeapReturnsPointer(t *testing.T) {
	t.Parallel()
	p := BuildHeap("info", "hello")
	if p == nil {
		t.Fatal("BuildHeap returned nil")
	}
	if p.Message != "hello" {
		t.Errorf("Message = %q, want hello", p.Message)
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	t.Parallel()
	p := NewPipeline()
	p.Add(NewEntry("info", "a"))
	snap := p.Snapshot()
	p.Add(NewEntry("info", "b"))
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d after later Add; want 1 (must be a copy)", len(snap))
	}
	// Mutating the snapshot must not touch the pipeline's stored entry.
	snap[0].Message = "mutated"
	if again := p.Snapshot(); again[0].Message != "a" {
		t.Errorf("pipeline entry = %q after snapshot mutation; want a", again[0].Message)
	}
}

func Example() {
	e := NewEntry("info", "login").WithField("user", "alice")
	fmt.Printf("%s user=%v\n", e.Message, e.Fields["user"])
	// Output: login user=alice
}
```

## Review

The pipeline is correct when `Snapshot` is genuinely independent of the pipeline:
appending after a snapshot must not lengthen it, and mutating a snapshot element
must not alter the stored entry. Both follow from `make`+`copy` rather than
returning `p.entries` directly. The escape contract is separate and observable:
run `go build -gcflags=-m ./... 2>&1 | grep -E 'moved to heap|does not escape'`
and you will see `e` in `BuildHeap` reported as `moved to heap` while `BuildStack`
is not — the returned-pointer rule made visible. The mistake to avoid is reaching
for `BuildHeap` "to avoid copying the struct"; on a read-only hot path the value
return is cheaper, and Exercise 2 turns that claim into a measured, CI-enforced
guarantee. Run `go test -race` to confirm the copy semantics under the race
detector.

## Resources

- [Go Blog: Escape analysis](https://go.dev/blog/escape-analysis) — how the compiler decides stack vs heap.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when to return a value and when a pointer.
- [cmd/compile: gcflags](https://pkg.go.dev/cmd/compile) — the `-m` optimization-decision flags.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-observe-escapes-in-tests.md](02-observe-escapes-in-tests.md)
