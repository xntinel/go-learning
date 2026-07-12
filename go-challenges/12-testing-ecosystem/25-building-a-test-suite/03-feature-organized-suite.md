# Exercise 3: Organize The Suite By Feature With Parallel Subtests

The suite is laid out feature-per-function — `TestSet`, `TestGet`, `TestDelete`,
`TestLen` — with each case a named subtest under `t.Run`. This is the layout that
lets `go test -run 'TestGet/expired'` run one case and keeps one failure from
burying the rest. It also forces a design decision the `-race` detector will
grade: which fixtures can be shared across parallel subtests and which cannot.

## What you'll build

```text
suite/                      independent module: example.com/suite
  go.mod
  cache.go                  the cache under test
  cmd/
    demo/
      main.go               tiny runnable demo of the cache
  helpers_test.go           newTestCache, assertValue, assertNoValue
  cache_test.go             TestSet/TestGet/TestDelete/TestLen with parallel subtests
```

Files: `cache.go`, `cmd/demo/main.go`, `helpers_test.go`, `cache_test.go`.
Implement: reuse the module-1 cache and module-2 helpers.
Test: `TestSet` (success, overwrites), `TestGet` (success, not_found, expired, empty_value), `TestDelete` (success, missing), `TestLen` (empty, after_set), all with `t.Parallel()`.
Verify: `go test -count=1 -race ./...` and `go test -run 'TestGet/expired' ./...`

### The sharing decision that -race grades

A parent test seeds one `*Cache` and its subtests run in parallel against it. That
is safe for exactly the operations the cache synchronizes internally: `Set`,
`Get`, and `Delete` all take the `RWMutex`, so concurrent parallel subtests
calling them on *disjoint keys* race on nothing. The moment a subtest needs to
mutate an *unsynchronized* field — the injected `now` clock, to simulate the
passage of time for the `expired` case — sharing breaks: reassigning `c.now` while
another parallel subtest reads it through `Get` is a data race the detector flags.
The fix is local isolation: the `expired` subtest builds its *own* cache with
`newTestCache`, advances that cache's clock, and never touches the shared one.

`TestLen` needs the same isolation for a different reason. Its `empty` subtest
asserts `Len() == 0` while its `after_set` subtest inserts two keys; run in
parallel against a shared cache, `empty` might observe the inserts and flake — not
a data race, but a logical one. Each `Len` subtest therefore gets its own cache.
The general rule the layout teaches: share a fixture across parallel subtests only
when every subtest's effect on it is both synchronized and non-conflicting;
otherwise isolate.

The `empty_value` subtest under `TestGet` pins a contract that is easy to get
wrong: a stored empty value must read back as `("", nil)`, distinct from an absent
key's `ErrNotFound`. A cache that conflates "stored empty" with "missing" corrupts
any caller that legitimately caches empty results (an empty search result, a
tombstone), so the contract earns a dedicated subtest.

Create `cache.go`:

```go
package suite

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("cache: key not found")
	ErrExpired  = errors.New("cache: key expired")
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

func (c *Cache) Set(key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}
	c.data[key] = entry{value: value, expiresAt: expiresAt}
}

func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}
```

Create `helpers_test.go`:

```go
package suite

import (
	"bytes"
	"testing"
	"time"
)

func newTestCache(now time.Time) *Cache {
	c := New()
	c.now = func() time.Time { return now }
	return c
}

func assertValue(t *testing.T, c *Cache, key, want string) {
	t.Helper()
	got, err := c.Get(key)
	if err != nil {
		t.Fatalf("Get(%q) err = %v; want nil", key, err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("Get(%q) = %q; want %q", key, got, want)
	}
}

func assertNoValue(t *testing.T, c *Cache, key string) {
	t.Helper()
	if _, err := c.Get(key); err == nil {
		t.Fatalf("Get(%q) err = nil; want non-nil", key)
	}
}
```

### The runnable demo

The demo builds a real binary next to the suite; it stores and reads one entry.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/suite"
)

func main() {
	c := suite.New()
	c.Set("k", []byte("v"), 0)
	v, _ := c.Get("k")
	fmt.Printf("k = %s, len = %d\n", v, c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
k = v, len = 1
```

### The feature-organized suite

Create `cache_test.go`:

```go
package suite

import (
	"errors"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestSet(t *testing.T) {
	t.Parallel()
	c := newTestCache(epoch)

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		c.Set("k1", []byte("v1"), 0)
		assertValue(t, c, "k1", "v1")
	})

	t.Run("overwrites", func(t *testing.T) {
		t.Parallel()
		c.Set("k2", []byte("v2a"), 0)
		c.Set("k2", []byte("v2b"), 0)
		assertValue(t, c, "k2", "v2b")
	})
}

func TestGet(t *testing.T) {
	t.Parallel()
	c := newTestCache(epoch)
	c.Set("k1", []byte("v1"), 0)
	c.Set("empty", []byte(""), 0)

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		assertValue(t, c, "k1", "v1")
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		assertNoValue(t, c, "missing")
	})

	t.Run("empty_value", func(t *testing.T) {
		t.Parallel()
		// Contract: a stored empty value is ("", nil), not ErrNotFound.
		assertValue(t, c, "empty", "")
	})

	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		// Own cache: this subtest mutates the clock, which is unsynchronized.
		local := newTestCache(epoch)
		local.Set("k", []byte("v"), time.Minute)
		local.now = func() time.Time { return epoch.Add(2 * time.Minute) }
		if _, err := local.Get("k"); !errors.Is(err, ErrExpired) {
			t.Fatalf("Get(k) after TTL err = %v; want ErrExpired", err)
		}
	})
}

func TestDelete(t *testing.T) {
	t.Parallel()
	c := newTestCache(epoch)
	c.Set("k1", []byte("v1"), 0)

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		c.Delete("k1")
		assertNoValue(t, c, "k1")
	})

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		c.Delete("does-not-exist") // no-op, must not panic
	})
}

func TestLen(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(epoch) // own cache: isolated count
		if got := c.Len(); got != 0 {
			t.Fatalf("Len = %d; want 0", got)
		}
	})

	t.Run("after_set", func(t *testing.T) {
		t.Parallel()
		c := newTestCache(epoch)
		c.Set("a", []byte("1"), 0)
		c.Set("b", []byte("2"), 0)
		if got := c.Len(); got != 2 {
			t.Fatalf("Len = %d; want 2", got)
		}
	})
}
```

Run the whole suite and one selected case:

```bash
go test -count=1 -race ./...
go test -run 'TestGet/expired' ./...
```

Expected output:

```
ok  	example.com/suite	0.3s
```

## Review

The suite is correctly organized when each behavior is one top-level function and
each case is one named subtest, so `-run` can target a single case and a failure
names the exact behavior. The design lesson is in the sharing decisions: `TestSet`
and `TestGet` share one cache because their parallel subtests only ever call
synchronized methods on disjoint keys, while `TestGet/expired` and both `TestLen`
subtests isolate their own cache because they mutate the unsynchronized clock or
would observe each other's inserts. Get this wrong and the suite passes locally
but the `-race` detector or a busy CI machine surfaces the flake. Run
`go test -race` to confirm the shared caches are safe, and use the `-run` form to
prove selective execution actually works.

## Resources

- [`(*testing.T).Run`](https://pkg.go.dev/testing#T.Run) — subtests and the `Parent/child` naming that `-run` targets.
- [`(*testing.T).Parallel`](https://pkg.go.dev/testing#T.Parallel) — parallel-subtest scheduling and its ordering guarantees.
- [Go Blog: Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests) — the canonical write-up of this layout.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-helpers-and-t-helper.md](02-helpers-and-t-helper.md) | Next: [04-table-driven-get.md](04-table-driven-get.md)
