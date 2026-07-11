# Exercise 4: Refactor Get Cases Into A Table-Driven Test

When cases differ only in data — not in logic — a table collapses them into rows
of a named struct iterated by one loop. This module turns the `Get` behavior into
a table-driven `TestGet`, showing when a table beats hand-written subtests and how
to compare an expected error with `errors.Is`, including the nil-wanted branch
that a naive comparison gets wrong.

## What you'll build

```text
tablecache/                 independent module: example.com/tablecache
  go.mod
  cache.go                  the cache under test
  cmd/
    demo/
      main.go               runnable demo
  cache_test.go             one table, N rows, each a parallel subtest
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: reuse the cache.
Test: a `[]struct` table with `name`, seed key/value/ttl, a clock `advance`, a lookup key, and `wantVal`/`wantErr`; iterate as parallel subtests.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/tablecache/cmd/demo
cd ~/go-exercises/tablecache
go mod init example.com/tablecache
```

### When a table beats subtests, and the nil-error trap

The `Get` behavior is a set of homogeneous cases: seed a key, maybe advance the
clock, look a key up, and check the returned value and error. They differ only in
their inputs and expected outputs, never in their shape — the textbook case for a
table. Each row is a named struct, the loop runs `t.Run(tt.name, ...)`, and adding
a case is one line rather than a new function. Contrast with the previous module's
hand-written subtests: those were justified because `expired` needed its own cache
and clock manipulation, i.e. *different logic*. The rule is to let the shape
decide — a table for uniform cases, subtests for cases that each need bespoke
setup.

The sharp edge is the error comparison. `wantErr` is either a sentinel
(`ErrNotFound`, `ErrExpired`) or `nil` for the success case, and the comparison
must handle both. `errors.Is(got, want)` does exactly this: `errors.Is(nil, nil)`
is `true`, so the success row (`wantErr: nil`, and `Get` returns `nil`) passes,
while a row expecting a sentinel fails if `Get` returns `nil` or the wrong
sentinel. Writing `if got != tt.wantErr` instead would appear to work for
sentinels but silently misbehave the moment an error is wrapped with `%w`, because
`==` compares the wrapper, not the chain. Always compare expected errors with
`errors.Is`.

Each row gives the clock a fixed base and an `advance` duration; the row's own
cache is frozen at `base`, seeded, then the clock is moved to `base+advance`
before the lookup. Because each row builds its own cache, the subtests are safe to
run with `t.Parallel()` — no shared mutable state. In Go 1.22+ the loop variable
`tt` is per-iteration, so no `tt := tt` copy is needed before the parallel
closure.

Create `cache.go`:

```go
package tablecache

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
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/tablecache"
)

func main() {
	c := tablecache.New()
	c.Set("k", []byte("v"), 0)
	if v, err := c.Get("k"); err == nil {
		fmt.Printf("hit: %s\n", v)
	}
	if _, err := c.Get("missing"); errors.Is(err, tablecache.ErrNotFound) {
		fmt.Println("miss: not found")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: v
miss: not found
```

### The table-driven test

Create `cache_test.go`:

```go
package tablecache

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestGet(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		seedKey   string
		seedVal   string
		seedTTL   time.Duration
		advance   time.Duration
		lookupKey string
		wantVal   string
		wantErr   error
	}{
		{
			name:    "hit_no_ttl",
			seedKey: "k", seedVal: "v", seedTTL: 0,
			lookupKey: "k", wantVal: "v", wantErr: nil,
		},
		{
			name:    "hit_within_ttl",
			seedKey: "k", seedVal: "v", seedTTL: time.Minute,
			advance:   30 * time.Second,
			lookupKey: "k", wantVal: "v", wantErr: nil,
		},
		{
			name:    "empty_value_is_not_an_error",
			seedKey: "k", seedVal: "", seedTTL: 0,
			lookupKey: "k", wantVal: "", wantErr: nil,
		},
		{
			name:    "missing_key",
			seedKey: "k", seedVal: "v", seedTTL: 0,
			lookupKey: "other", wantErr: ErrNotFound,
		},
		{
			name:    "expired_after_ttl",
			seedKey: "k", seedVal: "v", seedTTL: time.Minute,
			advance:   2 * time.Minute,
			lookupKey: "k", wantErr: ErrExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := New()
			c.now = func() time.Time { return base }
			c.Set(tt.seedKey, []byte(tt.seedVal), tt.seedTTL)
			c.now = func() time.Time { return base.Add(tt.advance) }

			got, err := c.Get(tt.lookupKey)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Get(%q) err = %v; want %v", tt.lookupKey, err, tt.wantErr)
			}
			if tt.wantErr == nil && !bytes.Equal(got, []byte(tt.wantVal)) {
				t.Fatalf("Get(%q) = %q; want %q", tt.lookupKey, got, tt.wantVal)
			}
		})
	}
}
```

## Review

The table is correct when every row differs only in data and the loop body is the
single source of truth for the logic: build a frozen cache, seed, advance the
clock, look up, and compare. The `errors.Is(err, tt.wantErr)` comparison is what
makes the nil-wanted success rows and the sentinel-wanted failure rows share one
assertion — `errors.Is(nil, nil)` is `true`, so no special-casing is needed, and
it keeps working if `Get` later wraps its sentinels with `%w`. Because each row
constructs its own cache, the parallel subtests share nothing and stay race-clean.
Reserve this shape for homogeneous cases; when a case needs bespoke setup, a named
subtest is clearer than a row full of half-used fields.

## Resources

- [`errors.Is`](https://pkg.go.dev/errors#Is) — chain-aware comparison, including the nil/nil case.
- [Go Blog: Using Subtests and Sub-benchmarks](https://go.dev/blog/subtests) — table-driven rows as subtests.
- [Go Wiki: TableDrivenTests](https://go.dev/wiki/TableDrivenTests) — the idiom and its conventions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-feature-organized-suite.md](03-feature-organized-suite.md) | Next: [05-golden-file-stats-report.md](05-golden-file-stats-report.md)
