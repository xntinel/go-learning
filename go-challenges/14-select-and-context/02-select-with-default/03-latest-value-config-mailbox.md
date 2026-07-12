# Exercise 3: Coalescing Single-Slot Mailbox for Config-Reload Notifications

When a config file changes three times in a row before your reload goroutine wakes
up, it should reload once, with the newest config — not three times, and not with a
stale snapshot. The coalescing mailbox delivers only the most recent value to a
slow consumer, with bounded memory: one slot, latest wins. It is a size-1 channel
plus a non-blocking send-with-evict.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
mailbox/                    independent module: example.com/mailbox
  go.mod                    go 1.26
  mailbox.go                type Mailbox[T]; Store (evict-then-send), Load (try-recv)
  cmd/
    demo/
      main.go               stores three configs, loads the newest
  mailbox_test.go           coalescing, non-blocking Store, concurrent -race
```

- Files: `mailbox.go`, `cmd/demo/main.go`, `mailbox_test.go`.
- Implement: `Mailbox[T any]` with `New[T]()`, `Store(v)` (evict the stale value then send the new one, never blocks), and `Load() (T, bool)` (non-blocking receive).
- Test: three `Store`s then one `Load` returns the third value; a second `Load` returns `false`; `Store` never blocks even when the slot is full; concurrent `Store`/`Load` under `-race` neither deadlocks nor loses the newest.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Evict the stale, not the new

The naive "notify" channel is `make(chan T, 1)` with a plain non-blocking send:
`select { case ch <- v: ; default: }`. It has a fatal flaw for latest-wins state:
when the slot is already full, the `default` fires and the *new* value is dropped,
leaving the *stale* one in the slot. A consumer that reads later sees old config.
That is backwards — coalescing must keep the newest, not the oldest.

The fix is send-with-evict: try to send; if the slot is full, do a non-blocking
receive to discard the stale value, then retry the send. `Store` is a small loop
because under concurrency the slot's state can change between the failed send and
the evict — another goroutine may have drained it, or refilled it — so `Store`
retries until its send lands. Each iteration makes progress: it either sends (and
returns) or evicts one value (freeing the slot for the retry). It never blocks: both
the send and the receive are non-blocking `select`s.

`Load` is a plain non-blocking receive. If a value is present it returns
`(v, true)` and empties the slot; if not, `(zero, false)`. A slow consumer that
loads infrequently only ever observes the latest published value — the earlier ones
were evicted by later `Store`s — which is exactly the memory-bounded, latest-wins
semantics an fsnotify or etcd-watch reload loop wants.

Create `mailbox.go`:

```go
package mailbox

// Mailbox is a single-slot, latest-wins mailbox. Store publishes a value,
// evicting any unread previous value; Load reads the latest value if present.
// Both operations are non-blocking. It is safe for concurrent use.
type Mailbox[T any] struct {
	ch chan T
}

// New returns an empty Mailbox.
func New[T any]() *Mailbox[T] {
	return &Mailbox[T]{ch: make(chan T, 1)}
}

// Store publishes v as the latest value. If the slot already holds an unread
// value, that stale value is evicted first so the newest always wins. Store
// never blocks.
func (m *Mailbox[T]) Store(v T) {
	for {
		select {
		case m.ch <- v:
			return
		default:
			// Slot is full: evict the stale value, then retry the send.
			select {
			case <-m.ch:
			default:
			}
		}
	}
}

// Load returns the latest value and true if one is present, or the zero value
// and false if the mailbox is empty. It never blocks and empties the slot.
func (m *Mailbox[T]) Load() (T, bool) {
	select {
	case v := <-m.ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}
```

### The runnable demo

The demo models a config that is rewritten three times before the reader gets to
it. The reader sees only the third version — proof that intermediate values were
coalesced away.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/mailbox"
)

type Config struct {
	Version  int
	MaxConns int
}

func main() {
	m := mailbox.New[Config]()

	// Three rapid config changes, no reader in between.
	m.Store(Config{Version: 1, MaxConns: 10})
	m.Store(Config{Version: 2, MaxConns: 20})
	m.Store(Config{Version: 3, MaxConns: 30})

	if cfg, ok := m.Load(); ok {
		fmt.Printf("reloaded: version=%d maxConns=%d\n", cfg.Version, cfg.MaxConns)
	}
	if _, ok := m.Load(); !ok {
		fmt.Println("no pending change")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the first `Load` consumes the coalesced value, the second finds
the slot empty):

```
reloaded: version=3 maxConns=30
no pending change
```

### Tests

`TestCoalescesToLatest` is the core contract: three `Store`s with no `Load`
between, then one `Load` must return the third value, and a second `Load` must
report empty. `TestStoreNeverBlocksWhenFull` fills the slot and asserts a second
`Store` returns promptly (behind a timeout, so a regression to a blocking send fails
rather than hangs). `TestConcurrentStoreLoad` hammers the mailbox from many
goroutines under `-race`, asserting no deadlock and no panic; a final `Store`
followed by `Load` confirms the newest value still wins after the storm.

Create `mailbox_test.go`:

```go
package mailbox

import (
	"sync"
	"testing"
	"time"
)

func TestCoalescesToLatest(t *testing.T) {
	t.Parallel()

	m := New[int]()
	m.Store(1)
	m.Store(2)
	m.Store(3)

	v, ok := m.Load()
	if !ok {
		t.Fatal("Load after Stores: ok = false, want true")
	}
	if v != 3 {
		t.Fatalf("Load = %d, want 3 (latest wins)", v)
	}

	if _, ok := m.Load(); ok {
		t.Fatal("second Load: ok = true, want false (slot drained)")
	}
}

func TestStoreNeverBlocksWhenFull(t *testing.T) {
	t.Parallel()

	m := New[int]()
	m.Store(1) // slot now full

	done := make(chan struct{})
	go func() {
		m.Store(2) // must evict 1 and send 2 without blocking
		m.Store(3)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Store blocked on a full slot; it must be non-blocking")
	}

	if v, _ := m.Load(); v != 3 {
		t.Fatalf("Load = %d, want 3", v)
	}
}

func TestConcurrentStoreLoad(t *testing.T) {
	t.Parallel()

	m := New[int]()
	var wg sync.WaitGroup

	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				m.Store(w*1000 + i)
				m.Load()
			}
		}()
	}
	wg.Wait()

	// After the storm, latest-wins still holds for a fresh publish.
	m.Store(42)
	if v, ok := m.Load(); !ok || v != 42 {
		t.Fatalf("Load = %d,%v after concurrent storm, want 42,true", v, ok)
	}
}
```

## Review

The mailbox is correct when a `Load` after any run of `Store`s returns the value of
the *last* `Store` and reports empty thereafter. The defining mistake is the plain
non-blocking send that drops the new value on a full slot: it compiles, it never
blocks, and it silently serves stale config forever — `TestCoalescesToLatest`
catches it because it would return `1`, not `3`. The second risk is a `Store` that
blocks or deadlocks under contention; the concurrent test plus `-race` proves the
evict-then-retry loop always makes progress and never parks. In production this
type feeds a reload goroutine that `Load`s on a debounce timer: bursts of file
writes collapse into a single reload of the newest config, with exactly one slot of
memory.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — non-blocking send and receive.
- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded buffering and coalescing between stages.
- [Go by Example: Non-Blocking Channel Operations](https://gobyexample.com/non-blocking-channel-operations) — the try-send/try-recv idiom the evict loop is built from.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-token-bucket-allow-limiter.md](04-token-bucket-allow-limiter.md)
