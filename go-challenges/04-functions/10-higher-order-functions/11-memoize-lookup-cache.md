# Exercise 11: Memoize a Lookup — Cache Successes, Never Cache Failures

**Nivel: Intermedio** — validacion rapida (un test corto).

A tax-rate lookup keyed by region code is a slow, rate-limited call in
production, and the same region gets asked for over and over within a
request batch. `Memoize` wraps any `func(string) (float64, error)` with a
plain cache so a repeated key is served without a second call — and a
failed call is never cached, so a transient outage does not poison the key
forever.

## What you'll build

```text
taxrate/                    independent module: example.com/taxrate
  go.mod                    go 1.24
  taxrate.go                type Lookup; func Memoize
  taxrate_test.go           repeat-key hit, distinct keys, failure not cached
```

- Files: `taxrate.go`, `taxrate_test.go`.
- Implement: `Lookup func(key string) (float64, error)` and `Memoize(fn Lookup) Lookup`, backed by a plain `map[string]float64` with no locking (single-goroutine use).
- Test: a repeated key calls the wrapped function once and returns the cached value after; distinct keys each call through independently; a failing key is retried on every call rather than being cached.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/taxrate
cd ~/go-exercises/taxrate
go mod init example.com/taxrate
go mod edit -go=1.24
```

### Cache the value, never the error

`Memoize` has one job and one trap. The job: the first call for a key runs
`fn` and stores the result, so every later call for that key is a map
lookup with no call at all. The trap: if `fn` fails, storing that failure
in the cache would pin the error to the key — every future caller gets the
stale failure even after the underlying dependency recovers. The fix is to
check `err != nil` before writing to the cache and return early without
storing anything, so a failing key simply calls through again next time.

This version deliberately skips concurrency (no `sync.RWMutex`, no
`singleflight`) — it targets a single-goroutine batch job, like a report
generator that resolves tax rates for a list of orders sequentially. A
concurrent, stampede-safe version of the same idea is a distinct, heavier
problem (see the earlier memoize-with-singleflight exercise in this
lesson); this one isolates the cache-only-on-success rule on its own.

Create `taxrate.go`:

```go
package taxrate

// Lookup fetches a value for key, e.g. a tax rate for a region code, from a
// dependency that is slow or rate-limited and must not be called more than
// once per distinct key.
type Lookup func(key string) (float64, error)

// Memoize decorates fn with a plain, single-threaded cache: the first call
// for a given key invokes fn and stores the result only on success; every
// later call for that key returns the cached value without calling fn
// again. A failure is never cached, so a transient error on a cold key
// lets the next call retry against fn instead of replaying the same error
// forever.
func Memoize(fn Lookup) Lookup {
	cache := make(map[string]float64)
	return func(key string) (float64, error) {
		if v, ok := cache[key]; ok {
			return v, nil
		}
		v, err := fn(key)
		if err != nil {
			return 0, err
		}
		cache[key] = v
		return v, nil
	}
}
```

### Tests

The table drives three scenarios through the same harness: a repeated key
must call the wrapped function exactly once, distinct keys must each call
through independently, and a failing key must call through on every
invocation because nothing was ever cached for it.

Create `taxrate_test.go`:

```go
package taxrate

import (
	"errors"
	"testing"
)

func TestMemoize(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("rate service unavailable")

	tests := []struct {
		name  string
		calls []string // keys looked up in order
		fail  map[string]bool
		rate  map[string]float64
	}{
		{
			name:  "repeat key hits cache after first call",
			calls: []string{"US-CA", "US-CA", "US-CA"},
			rate:  map[string]float64{"US-CA": 0.0725},
		},
		{
			name:  "distinct keys each call through once",
			calls: []string{"US-CA", "US-NY", "US-CA", "US-NY"},
			rate:  map[string]float64{"US-CA": 0.0725, "US-NY": 0.08},
		},
		{
			name:  "a failure is not cached and the next call retries",
			calls: []string{"US-TX", "US-TX"},
			fail:  map[string]bool{"US-TX": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := make(map[string]int)
			m := Memoize(func(key string) (float64, error) {
				calls[key]++
				if tc.fail[key] {
					return 0, errBoom
				}
				return tc.rate[key], nil
			})

			for _, key := range tc.calls {
				rate, err := m(key)
				if tc.fail[key] {
					if !errors.Is(err, errBoom) {
						t.Fatalf("m(%q) err = %v, want errBoom", key, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("m(%q) unexpected err: %v", key, err)
				}
				if rate != tc.rate[key] {
					t.Fatalf("m(%q) = %v, want %v", key, rate, tc.rate[key])
				}
			}

			for key, n := range calls {
				if tc.fail[key] {
					if n != len(tc.calls) {
						t.Errorf("failing key %q: fn called %d times, want %d (never cached)", key, n, len(tc.calls))
					}
					continue
				}
				if n != 1 {
					t.Errorf("key %q: fn called %d times, want 1 (cached after first)", key, n)
				}
			}
		})
	}
}
```

## Review

The fix is correct when the cache write happens only on the success path —
that single `if err != nil { return 0, err }` before the `cache[key] = v`
line is the entire contract. The failure-not-cached case in the test is the
one that matters most: it is easy to write a memoize decorator that looks
right on the happy path and only breaks in production, hours later, when a
dependency has a five-second blip that gets remembered forever. Note this
version is not safe for concurrent use — the map has no lock — which is a
deliberate scope cut, not an oversight; see the Resources link for the
concurrent, stampede-safe version.

## Resources

- [Go maps in action](https://go.dev/blog/maps) — map semantics and zero-value behavior used by the cache.
- [errors package](https://pkg.go.dev/errors) — `errors.Is` used to assert on the injected sentinel error.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-middleware-chain-composer.md](10-middleware-chain-composer.md) | Next: [12-predicate-combinators-access-rules.md](12-predicate-combinators-access-rules.md)
