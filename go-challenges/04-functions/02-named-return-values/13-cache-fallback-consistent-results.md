# Exercise 13: Keep Two Named Results Consistent with err

A cache-then-fallback lookup returns three things: the value, whether it was a
cache hit, and an error. When the fallback fails, `value` and `hit` must not lie —
no stale-looking value, no false hit — next to a non-nil `err`. A deferred closure
enforces that invariant across two non-error named results at once, not just the
error itself.

**Nivel: Intermedio** — validacion rapida (un test corto).

## What you'll build

```text
cachefallback/                independent module: example.com/cachefallback
  go.mod
  cachefallback.go             Store; Get (defer keeps value/hit consistent with err)
  cachefallback_test.go        cache hit, miss+success, miss+failure resets results
```

- Files: `cachefallback.go`, `cachefallback_test.go`.
- Implement: `(*Store).Get(key string) (value string, hit bool, err error)` that checks an in-memory cache, falls back to a loader on a miss, and uses a deferred closure to reset `value` and `hit` whenever `err` is non-nil.
- Test: a cache hit returns the cached value with `hit == true`; a miss with a successful loader populates the cache and reports `hit == false`; a miss with a failing loader leaves `value` empty and `hit` false.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cachefallback
cd ~/go-exercises/cachefallback
go mod init example.com/cachefallback
go mod edit -go=1.24
```

### One guard, two results

The body sets `value` and `hit` independently on each path, but the deferred
closure is the single place that guarantees they never disagree with `err`:

```go
defer func() {
    if err != nil {
        value = ""
        hit = false
    }
}()
```

Every earlier branch can be written without worrying about the error case — the
guard at the top cleans up after all of them uniformly. This is a different job
than wrapping an error: it derives the correct values of two other named results
from the final state of `err`.

Create `cachefallback.go`:

```go
package cachefallback

import "fmt"

// Loader fetches a value on a cache miss.
type Loader func(key string) (string, error)

// Store is a small in-memory cache with a fallback loader.
type Store struct {
	cache  map[string]string
	loader Loader
}

// New returns a Store backed by loader, with an empty cache.
func New(loader Loader) *Store {
	return &Store{cache: make(map[string]string), loader: loader}
}

// Get returns the cached value for key, or falls back to the loader on a
// miss, populating the cache on success. The deferred closure keeps value and
// hit consistent with err: if the call ends in error, both non-error named
// results are reset, so a caller can never see a stale value or a false hit
// paired with a non-nil error. Only named results stay reachable from a
// defer after the body has already set them.
func (s *Store) Get(key string) (value string, hit bool, err error) {
	defer func() {
		if err != nil {
			value = ""
			hit = false
		}
	}()

	if v, ok := s.cache[key]; ok {
		return v, true, nil
	}

	v, lerr := s.loader(key)
	if lerr != nil {
		err = fmt.Errorf("load %q: %w", key, lerr)
		return
	}
	s.cache[key] = v
	return v, false, nil
}
```

### Tests

Create `cachefallback_test.go`:

```go
package cachefallback

import (
	"errors"
	"testing"
)

func TestGetCacheHit(t *testing.T) {
	t.Parallel()

	s := New(func(key string) (string, error) {
		t.Fatal("loader should not be called on a cache hit")
		return "", nil
	})
	s.cache["k"] = "cached-value"

	value, hit, err := s.Get("k")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hit {
		t.Error("hit = false, want true")
	}
	if value != "cached-value" {
		t.Errorf("value = %q, want %q", value, "cached-value")
	}
}

func TestGetMissFallbackSuccess(t *testing.T) {
	t.Parallel()

	calls := 0
	s := New(func(key string) (string, error) {
		calls++
		return "loaded-" + key, nil
	})

	value, hit, err := s.Get("k")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hit {
		t.Error("hit = true on first lookup, want false")
	}
	if value != "loaded-k" {
		t.Errorf("value = %q, want %q", value, "loaded-k")
	}

	value2, hit2, err2 := s.Get("k")
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if !hit2 {
		t.Error("hit = false on second lookup, want true")
	}
	if value2 != "loaded-k" {
		t.Errorf("value = %q, want %q", value2, "loaded-k")
	}
	if calls != 1 {
		t.Errorf("loader called %d times, want 1", calls)
	}
}

func TestGetMissFallbackFailureResetsResults(t *testing.T) {
	t.Parallel()

	s := New(func(key string) (string, error) {
		return "should-not-be-seen", errors.New("backend down")
	})

	value, hit, err := s.Get("k")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if value != "" {
		t.Errorf("value = %q, want empty string on error", value)
	}
	if hit {
		t.Error("hit = true, want false on error")
	}
	if _, cached := s.cache["k"]; cached {
		t.Error("failed load must not populate the cache")
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The interesting mechanic here is not error wrapping — it is a defer deriving one
named result's correctness from another. `value` and `hit` are set optimistically
in each branch, and the guard at the top is the only code that has to reason about
what happens when the fallback fails. Without it, a caller could receive a
leftover-looking `value` alongside a non-nil `err` if a future branch forgot to
clear it explicitly. The mistake to avoid is scattering that reset logic into every
failure branch instead of the one deferred guard.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Effective Go: Deferred functions](https://go.dev/doc/effective_go#defer)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-retry-loop-attempt-count.md](12-retry-loop-attempt-count.md) | Next: [14-audit-reason-backfill-guard.md](14-audit-reason-backfill-guard.md)
