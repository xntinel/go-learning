# Exercise 23: Message Deduplicator With Time-Window Expiry and GC Policy

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A message deduplicator remembers every hash it has seen for a sliding time
window, and something has to periodically sweep the expired ones out or
memory grows forever. This module builds that deduplicator through options,
checking that the GC sweep runs at least as often as entries expire — a
looser GC interval would let expired entries sit around for far longer than
the window they were supposed to be bounded by.

## What you'll build

```text
dedupe/                          independent module: example.com/dedupe
  go.mod                         go 1.24
  dedupe.go                      Deduper, Option, New, WithWindow, WithGCInterval,
                                  WithHashAlgorithm, WithClock, SHA256Hash, FNV1aHash,
                                  Seen, Len
  cmd/
    demo/
      main.go                    manual clock drives a duplicate, an expiry, and a GC sweep
  dedupe_test.go                  table test over options plus expiry, GC, and -race concurrency
```

- Files: `dedupe.go`, `cmd/demo/main.go`, `dedupe_test.go`.
- Implement: `New(opts ...Option) (*Deduper, error)` whose `Seen` reports and records a message's hash under a single lock, refilling on a GC schedule, validating the GC interval never exceeds the dedup window and the hash algorithm is never nil.
- Test: every option-validation case, expiry within and after the window, GC sweeping expired entries, both hash algorithms, and a `-race` concurrency check proving exactly one goroutine ever sees a shared message as new.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the GC interval must fit inside the window

`WithWindow` and `WithGCInterval` are unrelated options — a caller can set
either without the other, in any order. But a GC interval longer than the
window defeats the point of a periodic sweep: entries would routinely sit
in memory well past their expiry, waiting for a GC pass that only comes
around less often than they go stale. `New` checks `gcInterval > window`
after every option has run and rejects it — the same "compare two durations
that came from two different options" shape used for the event store's
snapshot interval and the key rotator's deprecation window elsewhere in
this chapter.

### One lock, one check-then-act

`Seen` does everything under a single `sync.Mutex` critical section: run
GC if due, hash the message, check whether that hash is still within its
recorded expiry, and either report a duplicate or record a fresh expiry.
Splitting the "is it a duplicate" check from the "record it" write across
two separate lock acquisitions would let two concurrent calls both observe
"not seen" for the same message and both proceed as if they were first —
exactly the race `TestConcurrentSeenNeverDoubleCountsAMessage` guards
against. Because the whole operation is one critical section, exactly one
caller can ever be the one that discovers a message is new.

### Two real hash algorithms, injected

`WithHashAlgorithm` takes a `func([]byte) string`, and the module ships two
ready-to-use ones built on real stdlib hash packages:
`SHA256Hash` (`crypto/sha256`, collision-resistant) and `FNV1aHash`
(`hash/fnv`, faster and non-cryptographic). Injecting the algorithm rather
than hardcoding one is the same pattern as injecting a clock: it lets a
caller trade correctness guarantees for speed without the deduplicator's
own logic ever changing.

Create `dedupe.go`:

```go
package dedupe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// Deduper is a concurrency-safe, time-windowed message deduplicator with a
// periodic garbage-collection sweep.
type Deduper struct {
	mu         sync.Mutex
	window     time.Duration
	gcInterval time.Duration
	hashFn     func([]byte) string
	now        func() time.Time

	seen   map[string]time.Time // hash -> expiry
	lastGC time.Time
}

// Option configures a Deduper and may reject invalid input.
type Option func(*Deduper) error

// New seeds defaults, applies opts in order, then validates the cross-field
// invariant no single option could see: the GC interval must not exceed the
// dedup window, or expired entries would sit in memory far longer than the
// window they were supposed to be bounded by.
func New(opts ...Option) (*Deduper, error) {
	d := &Deduper{
		window:     5 * time.Minute,
		gcInterval: time.Minute,
		hashFn:     SHA256Hash,
		now:        time.Now,
		seen:       make(map[string]time.Time),
	}
	for _, opt := range opts {
		if err := opt(d); err != nil {
			return nil, err
		}
	}

	if d.gcInterval > d.window {
		return nil, fmt.Errorf("GC interval %s exceeds dedup window %s", d.gcInterval, d.window)
	}

	d.lastGC = d.now()
	return d, nil
}

// WithWindow sets how long a message hash is remembered (> 0).
func WithWindow(d time.Duration) Option {
	return func(dd *Deduper) error {
		if d <= 0 {
			return fmt.Errorf("window must be positive, got %s", d)
		}
		dd.window = d
		return nil
	}
}

// WithGCInterval sets how often expired hashes are swept out (> 0).
func WithGCInterval(d time.Duration) Option {
	return func(dd *Deduper) error {
		if d <= 0 {
			return fmt.Errorf("GC interval must be positive, got %s", d)
		}
		dd.gcInterval = d
		return nil
	}
}

// WithHashAlgorithm injects the function used to fingerprint a message.
func WithHashAlgorithm(fn func([]byte) string) Option {
	return func(dd *Deduper) error {
		if fn == nil {
			return fmt.Errorf("hash algorithm is nil")
		}
		dd.hashFn = fn
		return nil
	}
}

// WithClock injects the clock used to time expiry and GC.
func WithClock(now func() time.Time) Option {
	return func(dd *Deduper) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		dd.now = now
		return nil
	}
}

// SHA256Hash is a ready-to-use, collision-resistant hash algorithm.
func SHA256Hash(msg []byte) string {
	sum := sha256.Sum256(msg)
	return hex.EncodeToString(sum[:])
}

// FNV1aHash is a faster, non-cryptographic hash algorithm suitable when
// dedup only needs to be probabilistically correct.
func FNV1aHash(msg []byte) string {
	h := fnv.New64a()
	h.Write(msg)
	return hex.EncodeToString(h.Sum(nil))
}

// Seen reports whether msg was already seen within the current window and
// records it either way, refreshing its expiry. The check and the record
// happen under a single lock so concurrent callers can never both observe
// "not seen" for the same message.
func (d *Deduper) Seen(msg []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	d.maybeGCLocked(now)

	hash := d.hashFn(msg)
	if expiry, ok := d.seen[hash]; ok && now.Before(expiry) {
		return true
	}
	d.seen[hash] = now.Add(d.window)
	return false
}

// Len reports how many hashes are currently tracked, including any not yet
// swept by GC.
func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

func (d *Deduper) maybeGCLocked(now time.Time) {
	if now.Sub(d.lastGC) < d.gcInterval {
		return
	}
	for hash, expiry := range d.seen {
		if !now.Before(expiry) {
			delete(d.seen, hash)
		}
	}
	d.lastGC = now
}
```

### The runnable demo

The demo sends the same message twice immediately (the second is a
duplicate), then advances a manual clock past the window and shows both the
message no longer flagged as a duplicate and the GC sweep having reduced the
tracked hash count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/dedupe"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	d, err := dedupe.New(
		dedupe.WithWindow(time.Minute),
		dedupe.WithGCInterval(30*time.Second),
		dedupe.WithHashAlgorithm(dedupe.FNV1aHash),
		dedupe.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	msg := []byte("order-created:42")
	fmt.Printf("first sighting is duplicate: %t\n", d.Seen(msg))
	fmt.Printf("immediate resend is duplicate: %t\n", d.Seen(msg))

	current = current.Add(90 * time.Second) // past the window
	fmt.Printf("resend after window is duplicate: %t\n", d.Seen(msg))
	fmt.Printf("tracked hashes after GC: %d\n", d.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first sighting is duplicate: false
immediate resend is duplicate: true
resend after window is duplicate: false
tracked hashes after GC: 1
```

### Tests

`TestNewValidation` tables the GC-interval/window invariant, including the
exact-boundary case where they are equal. `TestSeenWithinAndAfterWindow`
and `TestGCSweepsExpiredEntries` drive a fake clock through expiry and a GC
sweep. `TestHashAlgorithmsAreDistinguishable` proves both `SHA256Hash` and
`FNV1aHash` work end to end. `TestConcurrentSeenNeverDoubleCountsAMessage`
runs `-race` over 100 goroutines calling `Seen` on the same message and
asserts exactly one reports it as new.

Create `dedupe_test.go`:

```go
package dedupe

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "GC interval exceeds window", opts: []Option{
			WithWindow(time.Minute), WithGCInterval(2 * time.Minute),
		}, wantErr: true},
		{name: "GC interval equal to window is allowed", opts: []Option{
			WithWindow(time.Minute), WithGCInterval(time.Minute),
		}},
		{name: "zero window", opts: []Option{WithWindow(0)}, wantErr: true},
		{name: "zero GC interval", opts: []Option{WithGCInterval(0)}, wantErr: true},
		{name: "nil hash algorithm rejected", opts: []Option{WithHashAlgorithm(nil)}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSeenWithinAndAfterWindow(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	d, err := New(
		WithWindow(time.Minute),
		WithGCInterval(30*time.Second),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	msg := []byte("payload")
	if d.Seen(msg) {
		t.Fatal("first sighting reported as duplicate")
	}
	if !d.Seen(msg) {
		t.Fatal("immediate resend not reported as duplicate")
	}

	current = base.Add(90 * time.Second) // past the window
	if d.Seen(msg) {
		t.Fatal("resend after window expired still reported as duplicate")
	}
}

func TestGCSweepsExpiredEntries(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	d, err := New(
		WithWindow(time.Minute),
		WithGCInterval(30*time.Second),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		d.Seen([]byte(fmt.Sprintf("msg-%d", i)))
	}
	if got := d.Len(); got != 5 {
		t.Fatalf("Len() = %d, want 5", got)
	}

	current = base.Add(90 * time.Second) // all 5 expired, GC interval elapsed
	d.Seen([]byte("trigger-gc"))
	if got := d.Len(); got != 1 {
		t.Fatalf("Len() after GC = %d, want 1 (only the triggering message)", got)
	}
}

func TestHashAlgorithmsAreDistinguishable(t *testing.T) {
	t.Parallel()

	for _, algo := range []func([]byte) string{SHA256Hash, FNV1aHash} {
		d, err := New(WithHashAlgorithm(algo))
		if err != nil {
			t.Fatal(err)
		}
		if d.Seen([]byte("a")) {
			t.Fatal("first sighting reported as duplicate")
		}
		if !d.Seen([]byte("a")) {
			t.Fatal("resend not reported as duplicate")
		}
	}
}

func TestConcurrentSeenNeverDoubleCountsAMessage(t *testing.T) {
	t.Parallel()

	d, err := New()
	if err != nil {
		t.Fatal(err)
	}

	const n = 100
	var firstCount int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !d.Seen([]byte("shared-message")) {
				mu.Lock()
				firstCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if firstCount != 1 {
		t.Fatalf("firstCount = %d, want exactly 1 (only one goroutine should see it as new)", firstCount)
	}
}
```

## Review

The deduplicator is correct when GC never lags so far behind the window
that expired entries linger indefinitely, and when the duplicate check and
the record it makes are atomic with respect to every other caller. The
`window`/`gcInterval` comparison is a direct one, unlike the paginated
lister's derived product earlier in this chapter, but it belongs in the
same place for the same reason: neither option can see the other's value.
`TestConcurrentSeenNeverDoubleCountsAMessage` is the test that actually
proves the single-lock design matters — with the check and the write split
across two lock acquisitions, this test would flake under `-race` and
sometimes report more than one goroutine as "first."

## Resources

- [pkg.go.dev: crypto/sha256](https://pkg.go.dev/crypto/sha256)
- [pkg.go.dev: hash/fnv](https://pkg.go.dev/hash/fnv)
- [Kafka: idempotent producer and deduplication](https://kafka.apache.org/documentation/#semantics)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-sql-migration-executor-strategy.md](22-sql-migration-executor-strategy.md) | Next: [24-metrics-event-aggregator.md](24-metrics-event-aggregator.md)
