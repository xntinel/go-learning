# Exercise 1: A TTL Cache With a Deliberate Public Surface

A production in-memory TTL cache is a perfect first artifact for the export
boundary: the value it protects (a map plus a lock plus per-entry expiry) must
never be reachable directly, and the only mutation paths callers get are methods
that validate and lock first. This exercise builds that cache with an exported
surface of exactly `New`, `Get`, `Set`, `Delete`, `Len`, and a set of sentinel
errors, and keeps everything else, the `entry` type, the `data` map, the `set`
helper, and the `now` clock seam, unexported.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
cache/                     independent module: example.com/cache
  go.mod                   go 1.26
  cache.go                 Cache with Get/Set/Delete/Len; entry/data/set/now unexported
  cmd/
    demo/
      main.go              runnable demo: set, read, delete, look up missing
  cache_test.go            whitebox package cache: TTL via now seam, sentinels, unexported access
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: an exported `Cache` with `New`, `Get`, `Set`, `Delete`, `Len`; an unexported `entry` type, `data` map, `set` helper the exported `Set` calls after validating, and an unexported `now func() time.Time` clock seam.
- Test: a white-box test in `package cache` that forces expiry by reassigning `c.now`, asserts `ErrNotFound`/`ErrExpired`, rejects an empty key, reaches the unexported `set`/`data` directly, and pins the `ttl=0` no-expiry contract.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cache/cmd/demo
cd ~/go-exercises/cache
go mod init example.com/cache
go mod edit -go=1.26
```

### Why this shape

The cache's whole reason to exist is to hold a map safely across goroutines with
per-entry expiry. That invariant, "every read and write is serialized by the lock,
and expiry is computed from a single clock", only holds if callers cannot touch the
map or the clock directly. So the exported surface is the smallest set of operations
a caller needs: store a value with a TTL, read it back, delete it, count it. The
`data` map, the `entry` type, and the `set` helper stay unexported because exporting
any of them would hand a caller a way to write to the map without the lock or without
validating the key, and that capability could never be withdrawn.

`now` is the interesting one. It is an unexported field of type `func() time.Time`,
defaulting to `time.Now`. In production it is `time.Now` and nobody thinks about it.
In a white-box test we reassign it to a fixed clock so we can prove TTL expiry
deterministically, without sleeping real seconds. Because it is unexported, it is not
part of the shipped API and no caller can depend on it; because the test is in
`package cache`, the test can still reach it. That is the white-box seam: an internal
knob that tests use and callers cannot.

`Set` validates before it mutates. It rejects an empty key up front, then takes the
write lock and calls the unexported `set` helper, which computes the expiry and writes
the entry. Splitting validation (in `Set`) from mutation (in `set`) keeps the mutating
code path in one small place; making `set` unexported guarantees the only way to reach
it from outside the package is through the validating `Set`.

Expiry is evaluated on read. `Get` returns `ErrExpired` when `now()` is after the
entry's `expiresAt`, but it does not delete the entry, so `Len` counts stored entries
regardless of expiry. A TTL of zero means "never expires": `set` leaves `expiresAt` as
the zero `time.Time`, and `Get` skips the expiry check when `expiresAt.IsZero()`.

Create `cache.go`:

```go
package cache

import (
	"errors"
	"sync"
	"time"
)

// Exported sentinel errors are part of the public contract: callers match them
// with errors.Is rather than substring-matching a message.
var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: entry expired")
)

// entry is unexported: callers never name it, so its fields can change freely.
type entry struct {
	value     []byte
	expiresAt time.Time
}

// Cache is a concurrency-safe map whose entries expire after a TTL. Its state
// (data) and clock (now) are unexported, so the only mutation paths are the
// exported methods, which validate and lock first.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

// New is the only construction path. It seeds the map and wires now to
// time.Now; tests reassign now to a fixed clock.
func New() *Cache {
	return &Cache{
		data: make(map[string]entry),
		now:  time.Now,
	}
}

// Get returns the stored value, or ErrNotFound if absent, or ErrExpired if the
// entry's TTL has elapsed. Expiry is evaluated here and does not delete.
func (c *Cache) Get(key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && c.now().After(e.expiresAt) {
		return nil, ErrExpired
	}
	return e.value, nil
}

// Set validates the key, then delegates the mutation to the unexported set
// helper under the write lock. A ttl of 0 means the entry never expires.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) error {
	if key == "" {
		return errors.New("cache: empty key")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(key, value, ttl)
	return nil
}

// set is the unexported mutation helper. Because it is unexported, callers can
// only reach it through the validating Set.
func (c *Cache) set(key string, value []byte, ttl time.Duration) {
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

// Delete removes a key if present; deleting a missing key is a no-op.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

// Len reports the number of stored entries, expired or not.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

### The runnable demo

The demo touches only the exported API, because `cmd/demo` is a separate
`package main`. It stores two sessions, reads one, counts, deletes it, and shows the
missing-key path resolving to `ErrNotFound`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/cache"
)

func main() {
	c := cache.New()
	_ = c.Set("session:alice", []byte("token-123"), time.Minute)
	_ = c.Set("session:bob", []byte("token-456"), 0)

	if v, err := c.Get("session:alice"); err == nil {
		fmt.Printf("alice -> %s\n", v)
	}

	fmt.Printf("entries: %d\n", c.Len())

	c.Delete("session:alice")
	if _, err := c.Get("session:alice"); errors.Is(err, cache.ErrNotFound) {
		fmt.Println("alice -> not found after delete")
	}

	if _, err := c.Get("session:missing"); errors.Is(err, cache.ErrNotFound) {
		fmt.Println("missing -> not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice -> token-123
entries: 2
alice -> not found after delete
missing -> not found
```

### Tests

The tests are white-box (`package cache`) because two of them must reach unexported
names: `TestGetReturnsExpired` reassigns `c.now` to a fixed clock and advances it to
prove TTL expiry deterministically, and `TestUnexportedSetAndDataAreReachable` calls
the unexported `set` and reads the unexported `data` directly to demonstrate
same-package access. The rest drive the exported surface and assert the sentinels with
`errors.Is`. `TestGetReturnsValueWithNoTTL` pins the `ttl=0` contract: an entry stored
with no TTL survives an arbitrarily large clock advance. Run with `-race`, since the
cache is mutex-guarded and `TestConcurrentSetGet` hammers it from 100 goroutines.

Create `cache_test.go`:

```go
package cache

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	c := New()
	if err := c.Set("k1", []byte("v1"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := c.Get("k1")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "v1" {
		t.Fatalf("v = %q, want v1", v)
	}
}

func TestGetReturnsNotFound(t *testing.T) {
	t.Parallel()

	c := New()
	if _, err := c.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	c := New()
	if err := c.Set("", []byte("v"), 0); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestGetReturnsExpired(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.now = func() time.Time { return now }

	if err := c.Set("k1", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := c.Get("k1"); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestGetReturnsValueWithNoTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.now = func() time.Time { return now }

	if err := c.Set("k1", []byte("v1"), 0); err != nil {
		t.Fatal(err)
	}

	now = now.Add(1000 * time.Hour) // ttl=0 means never expires
	v, err := c.Get("k1")
	if err != nil {
		t.Fatalf("err = %v, want nil for ttl=0 entry", err)
	}
	if string(v) != "v1" {
		t.Fatalf("v = %q, want v1", v)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	c := New()
	_ = c.Set("k1", []byte("v1"), 0)
	c.Delete("k1")
	if _, err := c.Get("k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLen(t *testing.T) {
	t.Parallel()

	c := New()
	_ = c.Set("k1", []byte("v1"), 0)
	_ = c.Set("k2", []byte("v2"), 0)
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// TestUnexportedSetAndDataAreReachable proves a same-package (white-box) test
// can reach the unexported set helper and data map directly.
func TestUnexportedSetAndDataAreReachable(t *testing.T) {
	t.Parallel()

	c := New()
	c.set("k", []byte("v"), 0)
	if got := c.data["k"].value; string(got) != "v" {
		t.Fatalf("data[k] = %q, want v", got)
	}
}

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()

	c := New()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			_ = c.Set(key, []byte("v"), time.Minute)
			_, _ = c.Get(key)
		}()
	}
	wg.Wait()
}

func ExampleCache() {
	c := New()
	_ = c.Set("answer", []byte("42"), time.Minute)
	v, _ := c.Get("answer")
	fmt.Printf("%s\n", v)
	// Output: 42
}
```

## Review

The cache is correct when expiry is a pure function of `now()` and the entry's stored
`expiresAt`: `Get` returns `ErrExpired` exactly when `expiresAt` is non-zero and
`now()` is after it, `ErrNotFound` when the key is absent, and the value otherwise; a
`ttl` of 0 leaves `expiresAt` zero so the check is skipped forever. The export
discipline is the lesson: `data`, `entry`, `set`, and `now` are unexported, so a caller
has no way to write the map without the lock or to bypass the empty-key check, and the
white-box test proves those internals are reachable from the same package while the
demo (a separate `package main`) is confined to the exported surface. The two easiest
mistakes here are exporting `set` "because a test needs it", which you avoid by
white-box testing, and exporting the `data` map through an accessor, which would let a
caller corrupt the cache behind its own lock. Run `go test -race` to confirm the mutex
actually serializes the 100-goroutine `Set`/`Get` storm.

## Resources

- [Go Spec: Exported identifiers](https://go.dev/ref/spec#Exported_identifiers) — the exact lexical rule the whole lesson rests on.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock guarding the map.
- [`time.Time.After` and `IsZero`](https://pkg.go.dev/time#Time.After) — the expiry comparison and the zero-time check that encodes "no TTL".
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the exported sentinels by identity.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-json-field-visibility.md](02-json-field-visibility.md)
