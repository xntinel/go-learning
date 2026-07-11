# Exercise 8: Lock-Free Read-Mostly Snapshot Publisher

The read-mostly pattern is everywhere: a background goroutine rebuilds a stats or
config snapshot every few seconds, and every request-serving goroutine reads it. A
mutex here would put a lock on the hottest read path in the service. `atomic.Pointer[T]`
gives you wait-free reads and a single-swap publish, using immutability and
copy-on-write. This exercise builds that publisher and confronts the stale-read
trade-off head on.

This module is fully self-contained.

## What you'll build

```text
snappub/                   independent module: example.com/snappub
  go.mod
  publisher.go             type Snapshot; type Publisher; Publish (Store), Load, Previous (Swap)
  cmd/
    demo/
      main.go              publishes two snapshots, reads them back
  publisher_test.go        writer/readers coherence test, monotonic-version test, Example
```

- Files: `publisher.go`, `cmd/demo/main.go`, `publisher_test.go`.
- Implement: an immutable `Snapshot` struct and a `Publisher` over `atomic.Pointer[Snapshot]`; `Publish` stores a new snapshot, `Load` reads one wait-free, `Previous` swaps and returns the old.
- Test: a writer swaps monotonically-versioned snapshots while many readers `Load`; each reader always sees a fully-coherent, non-nil snapshot with a non-decreasing version.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/snappub/cmd/demo
cd ~/go-exercises/snappub
go mod init example.com/snappub
```

### Immutable snapshots and the single-swap publish

The type published is *immutable by convention*: once a `Snapshot` is built and its
pointer stored, no one ever mutates its fields. To publish new numbers you build a
brand-new `Snapshot` and `Store` the pointer to it — copy-on-write. That is what
makes the reads safe without a lock: a reader that `Load`s the pointer holds a
`*Snapshot` that no one will ever change under it, so it can read every field freely
and see a coherent set of values that all belong to the same publish.

The correctness rests on the memory-model edge from the concepts. The writer fully
initializes the struct (all fields written) *before* `Store`ing the pointer. A reader
that `Load`s that pointer is guaranteed by the happens-before edge to see all those
field writes. There is no torn read: you never observe a snapshot with its `Version`
from one publish and its `Requests` from another, because the fields are never
mutated in place — they are frozen at construction and exposed atomically as a single
pointer.

`Load` is wait-free and never blocks the writer; `Store` is a single instruction and
never blocks readers. `Previous` uses `Swap` to install a new snapshot and hand back
the old pointer in one atomic step — useful when you want to diff against or drain the
snapshot you just replaced.

The trade-off you must own: each `Publish` allocates a fresh `Snapshot`, and a reader
may `Load` the *previous* snapshot right up until the `Store` lands. That stale window
is fully consistent — the reader sees a real, coherent past snapshot, never a torn one
— but it is stale. For stats, config, and dashboards that is exactly right. For a value
that must reflect the absolute latest state with a hard ordering guarantee, this is the
wrong tool.

Create `publisher.go`:

```go
package snappub

import (
	"sync/atomic"
	"time"
)

// Snapshot is an immutable point-in-time view of service stats. Once published,
// its fields are never mutated; a new view is a brand-new Snapshot.
type Snapshot struct {
	Version  uint64
	Requests int64
	Errors   int64
	At       time.Time
}

// Publisher holds the current Snapshot behind an atomic pointer. Readers Load it
// wait-free; a writer Publishes a replacement with a single Store.
type Publisher struct {
	cur atomic.Pointer[Snapshot]
}

// New returns a Publisher seeded with an initial zero-version snapshot, so Load
// never returns nil.
func New() *Publisher {
	p := &Publisher{}
	p.cur.Store(&Snapshot{Version: 0, At: time.Now()})
	return p
}

// Publish installs s as the current snapshot. Build s fully before calling; do
// not mutate it afterward.
func (p *Publisher) Publish(s *Snapshot) {
	p.cur.Store(s)
}

// Load returns the current snapshot wait-free. The returned pointer is safe to
// read from; it is never mutated in place.
func (p *Publisher) Load() *Snapshot {
	return p.cur.Load()
}

// Previous installs s and returns the snapshot it replaced, in one atomic step.
func (p *Publisher) Previous(s *Snapshot) *Snapshot {
	return p.cur.Swap(s)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/snappub"
)

func main() {
	p := snappub.New()
	fmt.Println("initial version:", p.Load().Version)

	p.Publish(&snappub.Snapshot{Version: 1, Requests: 100, Errors: 2})
	s := p.Load()
	fmt.Printf("v%d: requests=%d errors=%d\n", s.Version, s.Requests, s.Errors)

	old := p.Previous(&snappub.Snapshot{Version: 2, Requests: 250, Errors: 5})
	fmt.Println("replaced version:", old.Version)
	fmt.Println("current version:", p.Load().Version)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial version: 0
v1: requests=100 errors=2
replaced version: 1
current version: 2
```

### Tests

`TestCoherentReads` runs one writer publishing snapshots with a built-in invariant
(`Requests == 2 * Errors`) and a strictly increasing `Version`, while eight readers
hammer `Load`. Every reader must see a non-nil snapshot, the invariant must hold on
every read (proving no torn value), and each reader's observed `Version` must be
non-decreasing (proving no reader ever moves backward within its own stream of
loads).

Create `publisher_test.go`:

```go
package snappub

import (
	"fmt"
	"sync"
	"testing"
)

func TestCoherentReads(t *testing.T) {
	t.Parallel()

	p := New()
	const updates = 2000
	const readers = 8

	var wg sync.WaitGroup

	// Writer: publish monotonically-versioned snapshots with the invariant
	// Requests == 2*Errors.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := uint64(1); v <= updates; v++ {
			p.Publish(&Snapshot{
				Version:  v,
				Errors:   int64(v),
				Requests: int64(2 * v),
			})
		}
	}()

	// Readers: each Load must be non-nil, internally coherent, and never regress.
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			for range updates {
				s := p.Load()
				if s == nil {
					t.Error("Load returned nil")
					return
				}
				if s.Requests != 2*s.Errors {
					t.Errorf("torn snapshot: v=%d requests=%d errors=%d",
						s.Version, s.Requests, s.Errors)
					return
				}
				if s.Version < last {
					t.Errorf("version regressed: %d after %d", s.Version, last)
					return
				}
				last = s.Version
			}
		}()
	}

	wg.Wait()

	if got := p.Load().Version; got != updates {
		t.Fatalf("final version = %d, want %d", got, updates)
	}
}

func TestPreviousReturnsOld(t *testing.T) {
	t.Parallel()

	p := New()
	p.Publish(&Snapshot{Version: 7})
	old := p.Previous(&Snapshot{Version: 8})
	if old.Version != 7 {
		t.Fatalf("Previous returned version %d, want 7", old.Version)
	}
	if got := p.Load().Version; got != 8 {
		t.Fatalf("current version = %d, want 8", got)
	}
}

func ExamplePublisher() {
	p := New()
	p.Publish(&Snapshot{Version: 1, Requests: 10, Errors: 1})
	s := p.Load()
	fmt.Printf("v%d requests=%d errors=%d\n", s.Version, s.Requests, s.Errors)
	// Output: v1 requests=10 errors=1
}
```

## Review

The publisher is correct when every `Load` returns a fully-coherent, non-nil
snapshot and no reader ever observes a torn mix of fields — `TestCoherentReads`
under `-race` proves it by checking the `Requests == 2*Errors` invariant on every
read across thousands of concurrent loads. The disciplines: build the `Snapshot`
completely before `Publish` and never mutate a published one (copy-on-write), and
seed the publisher so `Load` is never nil. Own the trade-off — readers may see the
previous snapshot until the store lands; that is fine for stats and config, wrong
for a value needing the absolute latest with hard ordering.

## Resources

- [`atomic.Pointer`](https://pkg.go.dev/sync/atomic#Pointer) — `Store`, `Load`, `Swap`, `CompareAndSwap`.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before edge that makes the published struct's fields visible.
- [Go 1.19 release notes: atomic types](https://go.dev/doc/go1.19#atomic_types) — the generic `atomic.Pointer[T]`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-exactly-once-guard.md](07-exactly-once-guard.md) | Next: [09-atomic-bitmask-feature-flags.md](09-atomic-bitmask-feature-flags.md)
