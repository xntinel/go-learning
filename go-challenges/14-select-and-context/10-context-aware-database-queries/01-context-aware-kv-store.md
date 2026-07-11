# Exercise 1: A Context-Aware Store That Cancels In-Flight Reads and Writes

Before touching a real SQL driver, build the mental model in miniature: a store
whose reads and writes race their own latency against `ctx.Done()`, so a
cancelled context returns immediately instead of after the "query" finishes. This
is exactly how a `*Context` driver method behaves, and it is the foundational
artifact the rest of the chapter reuses.

## What you'll build

```text
simkv/                       independent module: example.com/simkv
  go.mod                     go 1.25
  simkv.go                   DB with Get/Set that select on latency vs ctx.Done; ErrNotFound sentinel
  cmd/
    demo/
      main.go                success read, then a read cancelled mid-flight
  simkv_test.go              value / not-found / context-cancellation contracts, -race
```

Files: `simkv.go`, `cmd/demo/main.go`, `simkv_test.go`.
Implement: a `DB` with `Get(ctx, key)` and `Set(ctx, key, value)` whose latency races `ctx.Done()`, plus an `ErrNotFound` sentinel wrapped with `%w`.
Test: success returns the value, a missing key returns a wrapped `ErrNotFound` asserted via `errors.Is`, and a 10ms-deadline context against 50ms latency returns `context.DeadlineExceeded` with elapsed shorter than the latency.
Verify: `go test -count=1 -race ./...`

### Why a `select` on `time.After` versus `ctx.Done()` models the driver

A real driver method spends its time waiting on a socket for the server's
response. While it waits, it is also watching the context: if `ctx.Done()` fires
first, the driver abandons the wait, tells the server to cancel, and returns a
context error immediately. We model that with the smallest possible mechanism —

```
select {
case <-time.After(d.latency):   // the "query" finished
case <-ctx.Done():              // the caller gave up first
    return "", fmt.Errorf("simkv.Get(%s): %w", key, ctx.Err())
}
```

The important property this pins down is *short-circuit*. When the deadline is
10ms and the latency is 50ms, the context branch wins after ~10ms; the method
does not wait the full 50ms and then report failure. The test measures elapsed
time to prove this: a method that ignored cancellation would still take 50ms and
merely return a different error. That distinction — "returns fast" versus
"returns a context error eventually" — is the entire point of context-aware I/O,
and it is invisible unless the test times it.

Note the two return contracts. On cancellation the method wraps `ctx.Err()` with
`%w` so the caller can assert `errors.Is(err, context.DeadlineExceeded)`. On a
missing key it wraps the package sentinel `ErrNotFound` the same way. Both are
recoverable through `errors.Is`; neither leaks an internal string the caller
would have to match on.

Set up the module:

```bash
mkdir -p ~/go-exercises/simkv/cmd/demo
cd ~/go-exercises/simkv
go mod init example.com/simkv
go mod edit -go=1.25
```

Create `simkv.go`:

```go
package simkv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is the sentinel for a missing key. Callers assert it with
// errors.Is; they never string-match the driver's message.
var ErrNotFound = errors.New("simkv: key not found")

// DB is a concurrency-safe in-memory store whose Get/Set model the *Context
// driver methods: each operation races a simulated latency against ctx.Done, so
// a cancelled context returns immediately instead of after the latency elapses.
type DB struct {
	mu      sync.RWMutex
	data    map[string]string
	latency time.Duration
}

// New returns a DB seeded with kvs, where each operation takes latency to
// complete unless the context is cancelled first.
func New(latency time.Duration, kvs map[string]string) *DB {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	return &DB{data: cp, latency: latency}
}

// Get returns the value for key. It returns a wrapped context error if ctx is
// cancelled before the read completes, or a wrapped ErrNotFound if key is absent.
func (d *DB) Get(ctx context.Context, key string) (string, error) {
	select {
	case <-time.After(d.latency):
	case <-ctx.Done():
		return "", fmt.Errorf("simkv.Get(%s): %w", key, ctx.Err())
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.data[key]
	if !ok {
		return "", fmt.Errorf("simkv.Get(%s): %w", key, ErrNotFound)
	}
	return v, nil
}

// Set stores value under key. It returns a wrapped context error if ctx is
// cancelled before the write completes.
func (d *DB) Set(ctx context.Context, key, value string) error {
	select {
	case <-time.After(d.latency):
	case <-ctx.Done():
		return fmt.Errorf("simkv.Set(%s): %w", key, ctx.Err())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[key] = value
	return nil
}
```

### The runnable demo

The demo shows both outcomes against the wall clock: a generous deadline lets a
read succeed, and a deadline shorter than the latency makes the next read return
a context error fast.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/simkv"
)

func main() {
	db := simkv.New(20*time.Millisecond, map[string]string{"u1": "alice"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, err := db.Get(ctx, "u1")
	fmt.Printf("read ok: v=%s err=%v\n", v, err)

	fast, cancelFast := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelFast()
	start := time.Now()
	_, err = db.Get(fast, "u1")
	fmt.Printf("read cancelled: is-deadline=%v fast=%v\n",
		errors.Is(err, context.DeadlineExceeded), time.Since(start) < 20*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read ok: v=alice err=<nil>
read cancelled: is-deadline=true fast=true
```

### Tests

The tests pin three contracts per read and confirm cancellation short-circuits by
timing it. `TestGetRespectsContext` is the one that matters most: it asserts both
that the error is `context.DeadlineExceeded` *and* that the call returned in well
under the 50ms latency, which is the only way to distinguish real cancellation
from a slow failure.

Create `simkv_test.go`:

```go
package simkv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestGetReturnsValue(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, map[string]string{"u1": "alice"})
	v, err := db.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Get: err = %v, want nil", err)
	}
	if v != "alice" {
		t.Fatalf("Get: v = %q, want alice", v)
	}
}

func TestGetReturnsNotFoundSentinel(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, nil)
	_, err := db.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: err = %v, want ErrNotFound", err)
	}
}

func TestGetRespectsContext(t *testing.T) {
	t.Parallel()
	db := New(50*time.Millisecond, map[string]string{"u1": "alice"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := db.Get(ctx, "u1")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get: err = %v, want DeadlineExceeded", err)
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("Get ignored context: took %v (>= latency), want short-circuit", elapsed)
	}
}

func TestSetRespectsContext(t *testing.T) {
	t.Parallel()
	db := New(50*time.Millisecond, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := db.Set(ctx, "k", "v")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Set: err = %v, want DeadlineExceeded", err)
	}
}

func TestSetThenGet(t *testing.T) {
	t.Parallel()
	db := New(time.Millisecond, nil)
	if err := db.Set(context.Background(), "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := db.Get(context.Background(), "k")
	if err != nil || v != "v" {
		t.Fatalf("Get after Set = %q,%v; want v,nil", v, err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	db := New(0, nil)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			_ = db.Set(context.Background(), key, "v")
			_, _ = db.Get(context.Background(), key)
		}()
	}
	wg.Wait()
}

func ExampleDB_Get() {
	db := New(time.Millisecond, map[string]string{"u1": "alice"})
	v, _ := db.Get(context.Background(), "u1")
	fmt.Println(v)
	// Output: alice
}
```

## Review

The store is correct when cancellation is a race the context can win: with a
deadline shorter than the latency, `Get` returns a wrapped
`context.DeadlineExceeded` and returns *before* the latency elapses. Timing the
call is not optional here — an implementation that waited the full latency and
then returned `ctx.Err()` would pass an error-type assertion while failing the
whole purpose, so the test asserts `elapsed < latency`. The two sentinel
contracts are equally load-bearing: a missing key wraps `ErrNotFound` and a
cancellation wraps `ctx.Err()`, both with `%w`, so callers use `errors.Is` and
never depend on message text. Run with `-race` to confirm the `RWMutex` actually
guards the map under concurrent `Set`/`Get`.

## Resources

- [database/sql: DB.QueryContext](https://pkg.go.dev/database/sql#DB.QueryContext) — the real method this `Get` models.
- [context package](https://pkg.go.dev/context) — `Context`, `WithTimeout`, and `Err`.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) — the `select`-on-`Done` idiom.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-transactional-transfer-rollback.md](02-transactional-transfer-rollback.md)
