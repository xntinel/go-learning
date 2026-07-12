# Exercise 5: Instrumenting a Store by Embedding Its Interface

You have a `Store` interface with a dozen methods and you want to record latency
and a counter for just the write path. Reimplementing all twelve methods to add a
timer around one is wasteful and fragile. The idiomatic move is to embed the
`Store` interface in a decorator struct: the struct satisfies `Store` by
forwarding through the embedded value, and you override only `Put`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
storemetrics/               independent module: example.com/storemetrics
  go.mod                    module example.com/storemetrics
  storemetrics.go           Store interface; Decorator embedding Store, overriding Put
  cmd/
    demo/
      main.go               wrap a fake store; do Put/Get; print metrics
  storemetrics_test.go      Put intercepted, Get/Delete forwarded, nil-embed panic, compile check
```

Files: `storemetrics.go`, `cmd/demo/main.go`, `storemetrics_test.go`.
Implement: a `Store` interface (`Get`, `Put`, `Delete`), a `Decorator` struct
embedding `Store` and overriding `Put` to record a count and accumulated latency,
forwarding `Get`/`Delete` automatically, plus a `Metrics()` accessor and a `New`
constructor.
Test: `Put` is intercepted (counter and latency move) while `Get`/`Delete` pass
through to the inner store; calling a non-overridden method on a nil-embedded
decorator panics (the documented footgun); `var _ Store = (*Decorator)(nil)`
holds at compile time.
Verify: `go test -count=1 -race ./...`

### Why embedding the interface is the whole trick

`Decorator` embeds a `Store` *interface value*. That single embedded field makes
`Decorator` satisfy `Store` automatically: every method the interface requires is
promoted from the embedded value, so `d.Get(...)` and `d.Delete(...)` forward
straight to the wrapped store with no code from you. You then write exactly one
method — `Put` — on `*Decorator`, and because a method declared on the outer type
shadows the promoted one, your `Put` intercepts the call, times the inner
`d.Store.Put(...)`, bumps the counter, and returns. This is the partial-decorator
pattern: pay for only the methods you change; inherit the rest.

The overridden `Put` must call `d.Store.Put`, *not* `d.Put`, or it would recurse
into itself forever. And the metrics fields are guarded by a mutex because a store
is called concurrently; the `Metrics()` accessor takes the same lock to read a
consistent snapshot. Latency is measured through an injectable `clock` so the test
can assert an exact duration instead of a real elapsed time.

### The nil-embed footgun

Embedding an interface has a sharp edge: the embedded value can be nil. If you
construct a `Decorator{}` without setting the `Store`, the embedded interface is
nil, and any *non-overridden* promoted call — `d.Get(...)` — dereferences a nil
interface and panics. The overridden `Put` would panic too, at `d.Store.Put`. The
constructor exists precisely to prevent this: `New(inner)` sets the embedded
`Store`. The test documents the failure mode with `recover` so the behavior is
pinned rather than discovered in production.

Create `storemetrics.go`:

```go
package storemetrics

import (
	"context"
	"sync"
	"time"
)

// Store is the interface being decorated.
type Store interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Put(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

// Metrics is a snapshot of the write-path instrumentation.
type Metrics struct {
	Puts    int
	PutTime time.Duration
}

// Decorator embeds a Store, so it satisfies Store by forwarding every method
// through the embedded value. Only Put is overridden, to record metrics.
type Decorator struct {
	Store
	mu      sync.Mutex
	metrics Metrics
	clock   func() time.Time
}

// New wraps inner so the embedded Store is never nil.
func New(inner Store) *Decorator {
	return &Decorator{Store: inner, clock: time.Now}
}

// Put shadows the promoted method: it times and counts the inner Put, then
// forwards to the embedded store. It must call d.Store.Put, not d.Put.
func (d *Decorator) Put(ctx context.Context, key, value string) error {
	start := d.clock()
	err := d.Store.Put(ctx, key, value)
	elapsed := d.clock().Sub(start)

	d.mu.Lock()
	d.metrics.Puts++
	d.metrics.PutTime += elapsed
	d.mu.Unlock()
	return err
}

// Metrics returns a consistent snapshot of the recorded write metrics.
func (d *Decorator) Metrics() Metrics {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.metrics
}

// Compile-time proof that *Decorator satisfies the full Store interface even
// though it defines only Put.
var _ Store = (*Decorator)(nil)
```

### The runnable demo

The demo wraps an in-memory store, performs two `Put`s and a `Get`, and prints
the metrics to show the write path was intercepted while the read passed through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/storemetrics"
)

// memStore is a tiny concurrency-safe in-memory Store.
type memStore struct {
	m map[string]string
}

func (s *memStore) Get(_ context.Context, key string) (string, bool, error) {
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memStore) Put(_ context.Context, key, value string) error {
	s.m[key] = value
	return nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	delete(s.m, key)
	return nil
}

func main() {
	ctx := context.Background()
	d := storemetrics.New(&memStore{m: map[string]string{}})

	_ = d.Put(ctx, "a", "1")
	_ = d.Put(ctx, "b", "2")

	v, ok, _ := d.Get(ctx, "a") // forwarded, not counted
	fmt.Printf("get a: %s %v\n", v, ok)

	m := d.Metrics()
	fmt.Printf("puts: %d\n", m.Puts)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
get a: 1 true
puts: 2
```

### Tests

A recording fake counts calls to each method so the tests can prove `Put` was
intercepted (metrics moved) while `Get`/`Delete` reached the inner store
unchanged. An injected clock makes the accumulated latency exact. A separate test
uses `recover` to pin the nil-embed panic.

Create `storemetrics_test.go`:

```go
package storemetrics

import (
	"context"
	"testing"
	"time"
)

// countingStore records how many times each method is called.
type countingStore struct {
	gets, puts, deletes int
}

func (c *countingStore) Get(_ context.Context, key string) (string, bool, error) {
	c.gets++
	return "v", true, nil
}

func (c *countingStore) Put(_ context.Context, key, value string) error {
	c.puts++
	return nil
}

func (c *countingStore) Delete(_ context.Context, key string) error {
	c.deletes++
	return nil
}

func TestPutIsIntercepted(t *testing.T) {
	t.Parallel()

	inner := &countingStore{}
	d := New(inner)
	// Deterministic latency: each clock read advances 5ms.
	tick := time.Unix(0, 0)
	d.clock = func() time.Time {
		tick = tick.Add(5 * time.Millisecond)
		return tick
	}

	ctx := context.Background()
	if err := d.Put(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}

	if inner.puts != 1 {
		t.Fatalf("inner.puts = %d, want 1 (Put must forward)", inner.puts)
	}
	m := d.Metrics()
	if m.Puts != 1 {
		t.Fatalf("metrics.Puts = %d, want 1", m.Puts)
	}
	if m.PutTime != 5*time.Millisecond {
		t.Fatalf("metrics.PutTime = %v, want 5ms", m.PutTime)
	}
}

func TestGetAndDeleteAreForwarded(t *testing.T) {
	t.Parallel()

	inner := &countingStore{}
	d := New(inner)
	ctx := context.Background()

	if _, _, err := d.Get(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}

	if inner.gets != 1 || inner.deletes != 1 {
		t.Fatalf("forwarding failed: gets=%d deletes=%d, want 1,1", inner.gets, inner.deletes)
	}
	// Forwarded calls must not touch the write metrics.
	if m := d.Metrics(); m.Puts != 0 {
		t.Fatalf("metrics.Puts = %d after Get/Delete, want 0", m.Puts)
	}
}

func TestNilEmbedPanicsOnPromotedCall(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("calling a promoted method on a nil-embedded Store did not panic")
		}
	}()
	var d Decorator // embedded Store is nil
	_, _, _ = d.Get(context.Background(), "k")
}
```

## Review

The decorator is correct when `Put` intercepts (the counter and latency move and
the inner store still records the write) while `Get`/`Delete` forward untouched,
and when `var _ Store = (*Decorator)(nil)` compiles — the proof that embedding the
interface really did satisfy the whole contract. The traps this exercise pins:
calling `d.Put` instead of `d.Store.Put` inside the override, which recurses
forever; forgetting to override the method you meant to instrument, so the wrapper
silently adds nothing; and leaving the embedded interface nil, which turns the
first forwarded call into a panic — the reason `New` exists. Run `go test -race`;
the metrics mutex must hold under concurrent `Put`s.

## Resources

- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — embedding an interface in a struct to forward and override.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — how an embedded interface's methods are promoted.
- [Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — the composition mindset behind decorators like this one.

---

Prev: [04-base-repository-embedding.md](04-base-repository-embedding.md) | Back to [00-concepts.md](00-concepts.md) | Next: [06-grpc-forward-compat-embedding.md](06-grpc-forward-compat-embedding.md)
