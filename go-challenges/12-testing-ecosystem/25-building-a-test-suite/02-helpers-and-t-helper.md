# Exercise 2: Write Test Helpers With Correct Failure Attribution

Shared assertion helpers keep a suite short, but a helper that fails without
calling `t.Helper()` reports the failure at its own line, sending an on-call
engineer to the wrong file. This module builds `newTestCache`, `assertValue`, and
`assertNoValue` with correct attribution, plus a `t.Cleanup`-based helper that
demonstrates LIFO teardown ordering.

## What you'll build

```text
suite/                      independent module: example.com/suite
  go.mod
  cache.go                  the cache under test (same contract as module 01)
  cmd/
    demo/
      main.go               tiny runnable demo of the cache
  helpers_test.go           newTestCache, assertValue, assertNoValue (all t.Helper)
  suite_test.go             tests exercising the helpers + a LIFO t.Cleanup test
```

Files: `cache.go`, `cmd/demo/main.go`, `helpers_test.go`, `suite_test.go`.
Implement: `newTestCache(now time.Time)` injecting a frozen clock; `assertValue`/`assertNoValue` calling `t.Helper()`; `withCleanup` registering LIFO teardown.
Test: exercise the helpers on a real cache and assert `t.Cleanup` runs in LIFO order.
Verify: `go test -count=1 -race ./...`

### Why t.Helper is a correctness property

When a test fails, `go test` prints the file and line of the failing assertion.
If that assertion lives inside a shared helper, the reporter would naively point
at the line inside the helper — the same line for every one of its call sites.
`t.Helper()` fixes this: it marks the current function as a helper so the reporter
walks *past* its frame and reports the line in the test that *called* it. The
effect is only visible on failure, which is exactly why it is easy to forget and
expensive to omit: the suite passes in review, then during an incident every
failure points at `helpers_test.go:41` and tells the on-call nothing.

The discipline is mechanical: `t.Helper()` is the first statement of any function
that takes a `*testing.T` and can call `t.Fatalf`/`t.Errorf`. `newTestCache` does
not itself fail, so it does not strictly need it, but the two assertion helpers
do. To *see* the mechanism, add a deliberately failing assertion to a scratch test
and run `go test`: the reported line is the call site in your test, not the
`t.Fatalf` inside `assertValue`. Remove `t.Helper()` and rerun — the line jumps
into the helper. Then revert. (The shipped tests below all pass; the exercise is
to prove attribution locally, not to commit a failing test.)

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

### The runnable demo

The demo exists so the module builds a real binary alongside its tests; it stores
and reads one entry.

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
	fmt.Printf("k = %s\n", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
k = v
```

### The helpers

`newTestCache` injects a frozen clock so every test that uses it is deterministic.
`assertValue` fails unless `Get` returns the expected bytes with a nil error;
`assertNoValue` fails unless `Get` returns a non-nil error. Both call `t.Helper()`
first, and both use `bytes.Equal` rather than `string(got) == want` to compare on
the byte level (the value type is `[]byte`, and comparing bytes avoids an
allocation and is the honest comparison for binary values).

Create `helpers_test.go`:

```go
package suite

import (
	"bytes"
	"testing"
	"time"
)

// newTestCache returns a cache whose clock is frozen at now, so expiry is a pure
// function of what the test does, never of wall time.
func newTestCache(now time.Time) *Cache {
	c := New()
	c.now = func() time.Time { return now }
	return c
}

// assertValue fails the test unless Get(key) returns want with a nil error.
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

// assertNoValue fails the test unless Get(key) returns a non-nil error.
func assertNoValue(t *testing.T, c *Cache, key string) {
	t.Helper()
	if _, err := c.Get(key); err == nil {
		t.Fatalf("Get(%q) err = nil; want non-nil", key)
	}
}

// withCleanup registers a teardown callback and returns the cache. It shows the
// resource-teardown seam: real helpers register close/remove here.
func withCleanup(t *testing.T, now time.Time, record func(string)) *Cache {
	t.Helper()
	c := newTestCache(now)
	t.Cleanup(func() { record("cache torn down") })
	return c
}
```

### Tests

The first test exercises the assertion helpers against a real cache so their
happy paths are covered. The second proves `t.Cleanup` ordering: cleanups run in
last-in-first-out order, and because `t.Run` blocks until the subtest *and its
cleanups* complete, the parent can inspect the recorded order afterward — the one
place you can observe cleanup ordering from inside a test.

Create `suite_test.go`:

```go
package suite

import (
	"slices"
	"testing"
	"time"
)

var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestHelpersHappyPath(t *testing.T) {
	t.Parallel()
	c := newTestCache(testEpoch)
	c.Set("k", []byte("v"), 0)
	assertValue(t, c, "k", "v")
	assertNoValue(t, c, "missing")
}

func TestCleanupLIFO(t *testing.T) {
	t.Parallel()
	var order []string

	t.Run("inner", func(t *testing.T) {
		record := func(s string) { order = append(order, s) }
		_ = withCleanup(t, testEpoch, record)
		t.Cleanup(func() { record("first registered") })
		t.Cleanup(func() { record("second registered") })
	})

	// t.Run blocks until the subtest and all its cleanups have run, in LIFO
	// order: the last registered cleanup runs first.
	want := []string{"second registered", "first registered", "cache torn down"}
	if !slices.Equal(order, want) {
		t.Fatalf("cleanup order = %v; want %v", order, want)
	}
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

Expected output:

```
ok  	example.com/suite	0.2s
```

The timing varies; the leading `ok` and package path are what matter.

## Review

The helpers are correct when a failure they raise is attributed to the *test* that
called them, which `t.Helper()` guarantees by skipping the helper's stack frame.
Prove it once by introducing a temporary failure and watching the reported line
sit at the call site, then revert. `assertValue` comparing with `bytes.Equal` is
the honest comparison for a `[]byte` value and avoids a string allocation on the
hot path of a large suite. The `TestCleanupLIFO` test encodes the teardown
contract that real helpers depend on: register cleanups in setup order, and they
unwind in reverse, so a helper that opens then locks a resource can register close
then unlock and have them run unlock-then-close. Run `-race` because the helpers
touch the same `RWMutex`-guarded cache the production code does.

## Resources

- [`(*testing.T).Helper`](https://pkg.go.dev/testing#T.Helper) — marks a function as a helper for correct failure attribution.
- [`(*testing.T).Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — LIFO teardown registration tied to the test lifecycle.
- [`bytes.Equal`](https://pkg.go.dev/bytes#Equal) — allocation-free byte-slice comparison.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-implement-the-cache.md](01-implement-the-cache.md) | Next: [03-feature-organized-suite.md](03-feature-organized-suite.md)
