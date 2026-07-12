# Exercise 1: Implement The Concurrency-Safe TTL Cache Under Test

This is the system under test for the whole lesson: an in-memory cache that every
later module tests from a different angle. It exposes `Get`/`Set`/`Delete`/`Len`
over a `map` guarded by a `sync.RWMutex`, expires entries after a TTL, reads the
clock through an injectable `now func() time.Time` so tests can freeze time, and
signals absence through two sentinel errors.

## What you'll build

```text
suite/                      independent module: example.com/suite
  go.mod
  cache.go                  Cache: Get/Set/Delete/Len; ErrNotFound/ErrExpired; injectable now
  cmd/
    demo/
      main.go               runnable demo: set, read, delete, count
  cache_test.go             smoke test pinning the four-method contract + an Example
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a `Cache` backed by `map[string]entry` + `sync.RWMutex`, with `New`, `Get`, `Set(key, value, ttl)`, `Delete`, `Len`, an injectable `now func() time.Time`, and sentinel errors `ErrNotFound`/`ErrExpired`.
Test: a contract smoke test asserting the four methods, the empty-value-is-not-an-error rule, and `errors.Is` against both sentinels.
Verify: `go test -count=1 -race ./...`

### The API contract the rest of the suite pins

Four methods and two sentinel errors are the entire surface, and every design
choice here exists to make the type both correct in production and honest under
test. The clock is read through an unexported `now func() time.Time` field
initialized to `time.Now`; a test overrides it with a frozen instant so expiry is
deterministic without sleeping. Expiry is encoded as a zero-valued `expiresAt`
meaning "never expires" (a TTL `<= 0` on `Set`), so `time.Time.IsZero` is the
guard that distinguishes a permanent entry from a timed one. `Get` returns
`ErrNotFound` when the key is absent and `ErrExpired` when it exists but
`now().After(expiresAt)` — two distinct signals because the handler in a later
module maps them to different HTTP status codes (404 versus 410).

The concurrency contract is a `sync.RWMutex`: `Get` and `Len` take the read lock
so concurrent reads do not serialize, while `Set` and `Delete` take the write
lock. An empty value is explicitly *not* an error — `Set(key, []byte(""), 0)`
followed by `Get(key)` returns `("", nil)`, not `ErrNotFound`. That distinction
(a stored empty value versus an absent key) is a real contract that a caching
layer must honor, and a later module has a dedicated subtest pinning it.

Create `cache.go`:

```go
package suite

import (
	"errors"
	"sync"
	"time"
)

// Sentinel errors let callers distinguish "never stored" from "stored but
// expired" with errors.Is; the HTTP handler maps them to 404 and 410.
var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
)

type entry struct {
	value     []byte
	expiresAt time.Time // zero means never expires
}

// Cache is a concurrency-safe in-memory cache with per-entry TTL. It reads the
// clock through the now field so tests can freeze time without sleeping.
type Cache struct {
	mu   sync.RWMutex
	data map[string]entry
	now  func() time.Time
}

// New returns an empty cache that reads the real wall clock.
func New() *Cache {
	return &Cache{data: make(map[string]entry), now: time.Now}
}

// Get returns the stored value, ErrNotFound if the key was never stored, or
// ErrExpired if it was stored but its TTL has elapsed. An empty stored value is
// returned as ("", nil), not as an error.
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

// Set stores value under key. A ttl <= 0 means the entry never expires.
func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

// Delete removes key if present; deleting an absent key is a no-op.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

// Len reports the number of entries stored, expired or not (expiry is lazy).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

### The runnable demo

The demo drives the four methods against the real clock so you can see the API in
motion: store two keys, read one back, delete it, and print the count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/suite"
)

func main() {
	c := suite.New()
	c.Set("user:1", []byte("alice"), 0)
	c.Set("user:2", []byte("bob"), 0)

	if v, err := c.Get("user:1"); err == nil {
		fmt.Printf("user:1 = %s\n", v)
	}

	c.Delete("user:1")
	if _, err := c.Get("user:1"); errors.Is(err, suite.ErrNotFound) {
		fmt.Println("user:1 deleted")
	}

	fmt.Printf("entries: %d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user:1 = alice
user:1 deleted
entries: 1
```

### The contract test

Before any later module layers on golden files or fuzzing, this smoke test pins
the four-method contract in place: a stored value reads back, a deleted key
reports `ErrNotFound`, an absent key reports `ErrNotFound`, and an empty value is
returned without error. It sets the injected clock to a frozen instant so the
result never depends on wall time.

Create `cache_test.go`:

```go
package suite

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestContract(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.now = func() time.Time { return frozen }

	c.Set("k", []byte("v"), 0)
	if v, err := c.Get("k"); err != nil || string(v) != "v" {
		t.Fatalf("Get(k) = %q, %v; want \"v\", nil", v, err)
	}

	c.Delete("k")
	if _, err := c.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v; want ErrNotFound", err)
	}

	if _, err := c.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) err = %v; want ErrNotFound", err)
	}

	c.Set("empty", []byte(""), 0)
	if v, err := c.Get("empty"); err != nil || string(v) != "" {
		t.Fatalf("Get(empty) = %q, %v; want \"\", nil", v, err)
	}
}

func TestExpired(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New()
	c.now = func() time.Time { return frozen }

	c.Set("k", []byte("v"), time.Minute)
	c.now = func() time.Time { return frozen.Add(2 * time.Minute) }
	if _, err := c.Get("k"); !errors.Is(err, ErrExpired) {
		t.Fatalf("Get(k) after TTL err = %v; want ErrExpired", err)
	}
}

func Example() {
	c := New()
	c.Set("answer", []byte("42"), 0)
	v, _ := c.Get("answer")
	fmt.Printf("%s\n", v)
	// Output: 42
}
```

## Review

The cache is correct when absence and expiry are the only two failure signals and
each is a distinct sentinel: `Get` returns `ErrNotFound` exactly when the key is
not in the map, `ErrExpired` exactly when it is in the map but `now().After` its
non-zero `expiresAt`, and `(value, nil)` otherwise — including for a stored empty
value. The injected `now` is the seam that makes `TestExpired` deterministic:
freeze it, `Set` with a TTL, advance it past the deadline, and expiry is a pure
function with no wall-clock dependence. Keep expiry lazy — `Len` counts an expired
entry until something reads or deletes it, which is correct, not a bug. Run
`go test -race` to confirm the `RWMutex` actually guards the map; every later
module inherits this same contract.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write locking semantics for a concurrent map.
- [`time.Time.IsZero` and `time.Time.After`](https://pkg.go.dev/time#Time.After) — the guards behind "never expires" and "past deadline".
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching against sentinel errors instead of comparing strings.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-helpers-and-t-helper.md](02-helpers-and-t-helper.md)
