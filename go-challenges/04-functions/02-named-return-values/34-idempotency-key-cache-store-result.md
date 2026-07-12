# Exercise 34: Idempotency Result Cache Storage on Success

An idempotency key exists so that a client retrying a request — because the
response was lost, not because the operation failed — gets back the same
result instead of triggering the operation a second time. That guarantee
only holds if the cache is populated exclusively from successful operations:
caching a failure would make every retry replay that failure forever instead
of trying again. This exercise builds an `Execute` that stores its result
into a cache from a deferred closure keyed on the named `err` result, so a
failed attempt is never cached and a successful one always is.

**Nivel: Intermedio** — validacion rapida (dos pruebas: replay tras exito, reintento tras fallo).

## What you'll build

```text
idemcache/                  independent module: example.com/idemcache
  go.mod
  idemcache.go                Cache; Execute (named result+err, deferred cache-on-success)
  cmd/demo/
    main.go                  runnable demo: first call, cached replay, failure, retry after failure
  idemcache_test.go           caches and replays on success; never caches a failure
```

- Files: `idemcache.go`, `cmd/demo/main.go`, `idemcache_test.go`.
- Implement: `Execute(c *Cache, key string, op func() (string, error)) (result string, err error)` that checks the cache first, then runs `op` and caches `result` from a deferred closure only when the named `err` is nil.
- Test: a successful `op` is cached and a second call with the same key does not call `op` again; a failing `op` is not cached, so a retry with the same key calls `op` again.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Replay first, cache only what succeeded

```go
if cached, ok := c.Get(key); ok {
    return cached, nil
}

defer func() {
    if err == nil {
        c.set(key, result)
    }
}()

result, err = op()
return
```

`Execute` checks the cache before doing anything else — a cache hit returns
immediately and never re-registers the deferred closure, so a replayed
result is never re-cached (harmlessly redundant, but unnecessary work
avoided all the same). On a cache miss, the deferred closure is registered
before `op` runs, and because `result` and `err` are both named results, it
sees exactly what `op` produced after the assignment `result, err = op()`
copies those values in. The `if err == nil` guard is the whole guarantee:
whatever `op` returns when it fails is deliberately left out of the cache,
so the next call with the same key treats it as a fresh attempt rather than
replaying the failure that a transient retry was supposed to overcome.

Create `idemcache.go`:

```go
package idemcache

import "sync"

// Cache stores idempotent operation results keyed by idempotency key.
type Cache struct {
	mu   sync.Mutex
	data map[string]string
}

// NewCache returns an empty Cache.
func NewCache() *Cache {
	return &Cache{data: make(map[string]string)}
}

// Get returns a previously cached result for key, if any.
func (c *Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *Cache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = value
}

// Execute replays a cached result for key if one exists; otherwise it runs
// op and, only when op succeeds, caches the result under key so a retry with
// the same key returns the same answer without running op again.
//
// result and err are named results: a single deferred closure checks err
// once Execute is about to return from the "run op" branch and stores result
// in the cache exactly when err is nil. A failed op is deliberately left
// uncached, so the next attempt with the same key retries op rather than
// replaying a failure.
func Execute(c *Cache, key string, op func() (string, error)) (result string, err error) {
	if cached, ok := c.Get(key); ok {
		return cached, nil
	}

	defer func() {
		if err == nil {
			c.set(key, result)
		}
	}()

	result, err = op()
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/idemcache"
)

func main() {
	c := idemcache.NewCache()
	calls := 0

	chargeCard := func() (string, error) {
		calls++
		return "charge-confirmation-1", nil
	}

	result, err := idemcache.Execute(c, "req-1", chargeCard)
	fmt.Printf("first call: result=%q err=%v calls=%d\n", result, err, calls)

	result, err = idemcache.Execute(c, "req-1", chargeCard)
	fmt.Printf("retry same key: result=%q err=%v calls=%d\n", result, err, calls)

	failingOp := func() (string, error) {
		calls++
		return "", errors.New("card declined")
	}
	result, err = idemcache.Execute(c, "req-2", failingOp)
	fmt.Printf("failing call: result=%q err=%v calls=%d\n", result, err, calls)

	result, err = idemcache.Execute(c, "req-2", chargeCard)
	fmt.Printf("retry after failure: result=%q err=%v calls=%d\n", result, err, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first call: result="charge-confirmation-1" err=<nil> calls=1
retry same key: result="charge-confirmation-1" err=<nil> calls=1
failing call: result="" err=card declined calls=2
retry after failure: result="charge-confirmation-1" err=<nil> calls=3
```

### Tests

Create `idemcache_test.go`:

```go
package idemcache

import (
	"errors"
	"testing"
)

func TestExecuteCachesSuccessAndSkipsSecondCall(t *testing.T) {
	t.Parallel()

	c := NewCache()
	calls := 0
	op := func() (string, error) {
		calls++
		return "value-1", nil
	}

	result, err := Execute(c, "key-1", op)
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if result != "value-1" {
		t.Fatalf("result = %q, want value-1", result)
	}

	result, err = Execute(c, "key-1", op)
	if err != nil {
		t.Fatalf("Execute (replay): unexpected error: %v", err)
	}
	if result != "value-1" {
		t.Fatalf("replayed result = %q, want value-1", result)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (second call should replay the cache)", calls)
	}
}

func TestExecuteDoesNotCacheFailure(t *testing.T) {
	t.Parallel()

	c := NewCache()
	wantErr := errors.New("boom")
	calls := 0
	failing := func() (string, error) {
		calls++
		return "", wantErr
	}

	_, err := Execute(c, "key-2", failing)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if _, ok := c.Get("key-2"); ok {
		t.Fatal("Get(key-2) found a cached value after a failed op, want none")
	}

	succeeding := func() (string, error) {
		calls++
		return "value-2", nil
	}
	result, err := Execute(c, "key-2", succeeding)
	if err != nil {
		t.Fatalf("Execute retry: unexpected error: %v", err)
	}
	if result != "value-2" {
		t.Fatalf("retry result = %q, want value-2", result)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2 (failure must not have been cached)", calls)
	}
}
```

## Review

`Execute` is correct when a successful `op` is cached and replayed without
running again, and a failed one leaves no trace in the cache — the exact
two properties the tests assert by counting calls to `op` rather than
trusting the cache's internal state. The deferred closure is what keeps
"cache only on success" a single rule instead of a convention every caller
of `op` has to remember: it reads the named `err` after `op` has run, so it
never caches speculatively. The mistake to avoid is caching unconditionally
right after `result, err = op()` — that looks identical on the success path,
but on failure it stores an empty `result` under `key` forever, and every
future retry with that key gets back the empty string instead of a fresh
attempt.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Stripe API: Idempotent requests](https://stripe.com/docs/api/idempotent_requests)
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-environment-variable-temporary-set-restore.md](33-environment-variable-temporary-set-restore.md) | Next: [35-memory-buffer-pool-checkout-return.md](35-memory-buffer-pool-checkout-return.md)
