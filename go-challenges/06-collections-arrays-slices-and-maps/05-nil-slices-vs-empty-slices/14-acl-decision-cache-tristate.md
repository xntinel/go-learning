# Exercise 14: Authorization Decision Cache: Unknown, Allow, and Deny

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An authorization middleware sitting in front of every request -- the shape of
an Envoy RBAC filter, or a JWT-scope check wrapping a handler -- cannot
afford to run a full policy evaluation on every call. The standard fix is a
cache keyed by principal and resource: evaluate the policy once, remember
the answer, and skip the evaluator on every repeat. The `00-concepts.md`
lesson for this directory makes the point that reading a map is always
safe, even a nil one, and that this makes it easy to forget to think about
what a *missing* entry actually means. Nowhere does that ambiguity matter
more than here: a `map[string]bool` conflates "we have never decided" with
"we decided no," and which of those a cache lookup silently becomes is a
fail-open-or-fail-closed security decision, not a cosmetic one.

`if allowed := cache[key]; allowed { grant() } else { deny() }` reads like
correct code. It is correct for the `true` branch -- a cached `true` really
does mean the policy explicitly granted access. It is wrong for the `false`
branch, because Go maps hand back the zero value for a key that was never
written, and the zero value of `bool` is `false`, the same value an
explicit deny would have stored. An uncached key -- new deployment, cache
that just restarted, a resource nobody has ever requested before -- reads
back exactly like a key that policy actually rejected, and the request is
denied without the real evaluator ever running. That is a fail-closed bug
hiding behind fail-open-looking code, and the direction it fails depends on
which branch of the `if` a future edit happens to touch.

This module builds the cache as a package: a `Decision` type with three
values instead of two, a validated `Key`, and a `Cache` whose `Lookup`
method is built on the comma-ok map form specifically so "never decided"
and "explicitly denied" can never collapse into the same answer again.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
authcache/                module example.com/authcache
  go.mod                  go 1.24
  authcache.go            Decision, Key, Cache; NewKey, NewCache, Lookup, Remember;
                          two sentinel errors
  authcache_test.go       Unknown/Allow/Deny table, zero-value cache, the buggy-lookup
                          contrast, concurrent Remember/Lookup, ExampleCache_Lookup
```

- Files: `authcache.go`, `authcache_test.go`.
- Implement: `NewKey(principal, resource string) (Key, error)` rejecting an empty principal with `ErrEmptyPrincipal` and an empty resource with `ErrEmptyResource`; `Decision` with constants `Unknown`, `Allow`, `Deny` and a `String` method; `NewCache() *Cache`; `(*Cache).Lookup(key Key) Decision` built on the comma-ok map form; `(*Cache).Remember(key Key, allow bool)` storing the explicit outcome.
- Test: the Unknown/Allow/Deny progression for one key plus a never-Remembered key staying Unknown; a zero-value `Cache` is directly usable; the buggy-lookup contrast; `Remember` and `Lookup` exercised concurrently by many goroutines under `-race`; and `ExampleCache_Lookup` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/authcache
cd ~/go-exercises/authcache
go mod init example.com/authcache
go mod edit -go=1.24
```

### The zero value of bool is a decision you never made

A plain map lookup, `allowed := cache[key]`, cannot distinguish three states
that a `map[Key]bool` can only ever hold two of. There is "the policy was
evaluated and said yes," which correctly reads back `true`. There is "the
policy was evaluated and said no," which reads back `false` -- and there is
"the policy has never been evaluated for this key at all," which *also*
reads back `false`, because that is the zero value the map returns for
anything it was never told about. Single-value map access throws away the
one piece of information -- was this key ever written? -- that separates the
second case from the third, and the caller is left treating a gap in the
cache exactly like a denial the policy actually issued.

The comma-ok form is what recovers that information, because it is the only
form of map access that reports whether the key was present at all:

```go
func (c *Cache) Lookup(key Key) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	allow, ok := c.data[key]
	if !ok {
		return Unknown
	}
	if allow {
		return Allow
	}
	return Deny
}
```

`Unknown` is not a fallback error value here -- it is a first-class outcome
the caller is expected to branch on, exactly like `Allow` and `Deny`: "fall
through to the real policy evaluator," never "treat this like a denial." A
`Decision` with three named values makes that branch a compile-time-visible
`switch` instead of a boolean the reader has to remember carries a hidden
third meaning.

Create `authcache.go`:

```go
// Package authcache caches per-request authorization decisions keyed by
// principal and resource, distinguishing "never decided" from "explicitly
// denied" -- a distinction a plain map[Key]bool cannot make.
package authcache

import (
	"errors"
	"sync"
)

// Sentinel errors returned by NewKey. Callers should test for them with
// errors.Is rather than by comparing error strings.
var (
	// ErrEmptyPrincipal means the principal identifying who is acting was empty.
	ErrEmptyPrincipal = errors.New("authcache: principal must not be empty")
	// ErrEmptyResource means the resource being acted on was empty.
	ErrEmptyResource = errors.New("authcache: resource must not be empty")
)

// Decision is the three-valued outcome of a cache lookup: a policy was
// never evaluated for this key, or it was evaluated and explicitly granted
// or denied.
type Decision int

const (
	// Unknown means no policy evaluation has been cached for the key. The
	// caller must fall through to the real policy evaluator.
	Unknown Decision = iota
	// Allow means the key was explicitly evaluated and granted.
	Allow
	// Deny means the key was explicitly evaluated and denied.
	Deny
)

// String implements fmt.Stringer.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "Allow"
	case Deny:
		return "Deny"
	default:
		return "Unknown"
	}
}

// Key identifies one authorization decision: a principal acting on a
// resource.
type Key struct {
	Principal string
	Resource  string
}

// NewKey validates principal and resource and returns the Key built from
// them. It returns ErrEmptyPrincipal or ErrEmptyResource if either is empty.
func NewKey(principal, resource string) (Key, error) {
	if principal == "" {
		return Key{}, ErrEmptyPrincipal
	}
	if resource == "" {
		return Key{}, ErrEmptyResource
	}
	return Key{Principal: principal, Resource: resource}, nil
}

// Cache holds per-key authorization decisions.
//
// Cache is safe for concurrent use by multiple goroutines. The zero value
// is ready to use; NewCache is a convenience for readability at the call
// site.
type Cache struct {
	mu   sync.Mutex
	data map[Key]bool
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{data: make(map[Key]bool)}
}

// Lookup reports the cached decision for key: Unknown if key has never been
// Remembered, Allow or Deny for the explicitly stored outcome. It is built
// on the comma-ok map form specifically so an uncached key never collapses
// to the same zero value as an explicit deny.
func (c *Cache) Lookup(key Key) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	allow, ok := c.data[key]
	if !ok {
		return Unknown
	}
	if allow {
		return Allow
	}
	return Deny
}

// Remember stores the explicit outcome of evaluating key's policy,
// replacing any earlier outcome for the same key. It lazily initializes the
// underlying map, so a zero-value Cache is safe to write to without calling
// NewCache first.
func (c *Cache) Remember(key Key, allow bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[Key]bool)
	}
	c.data[key] = allow
}
```

### Using it

Build a `Key` with `NewKey` at the point where a request's principal and
resource are known, and check `Cache.Lookup` before running the real
policy evaluator: an `Allow` or `Deny` answers the request immediately, an
`Unknown` means run the evaluator and call `Remember` with its answer
before returning. Because `Cache` guards its map with a mutex and lazily
initializes it inside `Remember`, a single `Cache` value -- constructed with
`NewCache` or left as a zero-value struct field -- can be shared by every
request-handling goroutine in the service without any additional
coordination from the caller.

The module has no `main.go`, because a decision cache is a library, not a
tool. Its executable demonstration is `ExampleCache_Lookup`: `go test` runs
it and compares its standard output against the `// Output:` comment, so the
usage shown below cannot drift away from the code.

```go
func ExampleCache_Lookup() {
	c := NewCache()
	key, err := NewKey("user:alice", "doc:42")
	if err != nil {
		panic(err)
	}

	fmt.Println("before any decision:", c.Lookup(key))

	c.Remember(key, false)
	fmt.Println("after an explicit deny:", c.Lookup(key))

	other, err := NewKey("user:bob", "doc:42")
	if err != nil {
		panic(err)
	}
	fmt.Println("a different, never-decided key:", c.Lookup(other))

	// Output:
	// before any decision: Unknown
	// after an explicit deny: Deny
	// a different, never-decided key: Unknown
}
```

The last two lines are the module's entire point printed side by side: an
explicit deny and a key nobody has ever evaluated both existed as `false`
in a naive cache, and here they read back as `Deny` and `Unknown`
respectively -- visibly different outcomes a caller can branch on correctly.

### Tests

`TestNewKeyRejectsEmptyFields` covers the two validated inputs.
`TestLookupDistinguishesUnknownAllowDeny` walks one key through all three
states in order and checks a second, never-touched key stays `Unknown`
throughout. `TestZeroValueCacheIsUsable` confirms the doc comment's claim
that a `var c Cache` needs no constructor call before its first `Remember`.

`TestBuggyLookupCannotDistinguishUnknownFromDeny` is the heart of the
module. `lookupBuggy` is unexported and unreachable from `Cache`; it exists
so the test can state the collapse precisely -- for a key with an explicit
stored deny and a key that was never stored at all, `lookupBuggy` returns
the identical `false` for both, while `Cache.Lookup` returns `Deny` for one
and `Unknown` for the other on the same underlying data.
`TestCacheSafeForConcurrentUse` exercises `Remember` and `Lookup` from
twenty goroutines at once, matching the concurrency contract the type's doc
comment declares.

Create `authcache_test.go`:

```go
package authcache

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// lookupBuggy is the check an authorization middleware is often written
// with the first time: a plain map read with no comma-ok. A key that was
// never Remembered reads as the zero value, false -- exactly the same value
// stored for an explicit deny -- so lookupBuggy cannot tell "not yet
// decided" from "explicitly denied" apart. It is never exported and never
// reachable from Cache; it exists so the tests can pin that collapse.
func lookupBuggy(data map[Key]bool, key Key) bool {
	if allowed := data[key]; allowed {
		return true
	}
	return false
}

func demoKey(t *testing.T, principal, resource string) Key {
	t.Helper()
	key, err := NewKey(principal, resource)
	if err != nil {
		t.Fatalf("NewKey(%q, %q): %v", principal, resource, err)
	}
	return key
}

func TestNewKeyRejectsEmptyFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		principal string
		resource  string
		want      error
	}{
		{name: "empty principal", principal: "", resource: "doc:1", want: ErrEmptyPrincipal},
		{name: "empty resource", principal: "user:alice", resource: "", want: ErrEmptyResource},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewKey(tc.principal, tc.resource); !errors.Is(err, tc.want) {
				t.Fatalf("NewKey(%q, %q) error = %v, want %v", tc.principal, tc.resource, err, tc.want)
			}
		})
	}
}

func TestLookupDistinguishesUnknownAllowDeny(t *testing.T) {
	t.Parallel()

	c := NewCache()
	key := demoKey(t, "user:alice", "doc:42")

	if got := c.Lookup(key); got != Unknown {
		t.Fatalf("Lookup on a fresh cache = %v, want Unknown", got)
	}

	c.Remember(key, false)
	if got := c.Lookup(key); got != Deny {
		t.Fatalf("Lookup after Remember(false) = %v, want Deny", got)
	}

	c.Remember(key, true)
	if got := c.Lookup(key); got != Allow {
		t.Fatalf("Lookup after Remember(true) = %v, want Allow", got)
	}

	other := demoKey(t, "user:bob", "doc:42")
	if got := c.Lookup(other); got != Unknown {
		t.Fatalf("Lookup on a never-Remembered key = %v, want Unknown", got)
	}
}

func TestZeroValueCacheIsUsable(t *testing.T) {
	t.Parallel()

	var c Cache
	key := demoKey(t, "user:alice", "doc:1")

	if got := c.Lookup(key); got != Unknown {
		t.Fatalf("Lookup on a zero-value cache = %v, want Unknown", got)
	}
	c.Remember(key, true)
	if got := c.Lookup(key); got != Allow {
		t.Fatalf("Lookup after Remember on a zero-value cache = %v, want Allow", got)
	}
}

// TestBuggyLookupCannotDistinguishUnknownFromDeny is the heart of the
// module: for an explicit deny and a never-decided key, lookupBuggy returns
// the identical value, while Cache.Lookup tells them apart.
func TestBuggyLookupCannotDistinguishUnknownFromDeny(t *testing.T) {
	t.Parallel()

	denied := demoKey(t, "user:alice", "doc:secret")
	undecided := demoKey(t, "user:bob", "doc:secret")

	data := map[Key]bool{denied: false} // explicit deny stored for "denied"

	if lookupBuggy(data, denied) != lookupBuggy(data, undecided) {
		t.Fatal("lookupBuggy already distinguishes the two cases; the defect did not reproduce")
	}
	if lookupBuggy(data, denied) {
		t.Fatal("lookupBuggy(denied) = true, want false")
	}

	c := NewCache()
	c.Remember(denied, false)
	if got := c.Lookup(denied); got != Deny {
		t.Fatalf("Cache.Lookup(denied) = %v, want Deny", got)
	}
	if got := c.Lookup(undecided); got != Unknown {
		t.Fatalf("Cache.Lookup(undecided) = %v, want Unknown", got)
	}
}

// TestCacheSafeForConcurrentUse exercises Remember and Lookup from many
// goroutines at once, matching the concurrency contract on Cache's doc
// comment. A failure here shows up as a data race report under -race, not
// as a test assertion.
func TestCacheSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c := NewCache()
	const goroutines = 20

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := Key{Principal: fmt.Sprintf("user:%d", g), Resource: "doc:1"}
			c.Remember(key, g%2 == 0)
			want := Deny
			if g%2 == 0 {
				want = Allow
			}
			if got := c.Lookup(key); got != want {
				t.Errorf("goroutine %d: Lookup = %v, want %v", g, got, want)
			}
		}(g)
	}
	wg.Wait()
}

// ExampleCache_Lookup is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleCache_Lookup() {
	c := NewCache()
	key, err := NewKey("user:alice", "doc:42")
	if err != nil {
		panic(err)
	}

	fmt.Println("before any decision:", c.Lookup(key))

	c.Remember(key, false)
	fmt.Println("after an explicit deny:", c.Lookup(key))

	other, err := NewKey("user:bob", "doc:42")
	if err != nil {
		panic(err)
	}
	fmt.Println("a different, never-decided key:", c.Lookup(other))

	// Output:
	// before any decision: Unknown
	// after an explicit deny: Deny
	// a different, never-decided key: Unknown
}
```

## Review

`Lookup` is correct when an explicit deny and an uncached key produce
different `Decision` values -- `TestBuggyLookupCannotDistinguishUnknownFromDeny`
pins exactly that, against `lookupBuggy` producing the identical `false` for
both. The mechanism is the comma-ok map form: `allow, ok := c.data[key]`
recovers the one bit a single-value read throws away, whether the key was
ever written at all, and `Lookup` turns that bit into a real third value
instead of silently reusing `false` to mean two different things. Around
that core, `NewKey` validates the principal and resource up front so a
malformed key can never enter the cache, `Remember` lazily initializes the
map so a zero-value `Cache` needs no constructor call, and every method
holds the mutex only for the map access itself, which is what makes `Cache`
safe to share across every request-handling goroutine in the service.
`ExampleCache_Lookup` is the executable documentation: `go test` verifies
its output. Run `go test -count=1 -race ./...`.

## Resources

- [The Go Programming Language Specification — Index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok form for map reads.
- [Effective Go — Maps](https://go.dev/doc/effective_go#maps) — reading a missing key returns the zero value, and why that is easy to misuse.
- [Envoy RBAC filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rbac_filter) — a real-world authorization filter that caches per-request allow/deny decisions.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding `Cache.data` across `Lookup` and `Remember`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-compacted-log-tombstone-vs-empty.md](13-compacted-log-tombstone-vs-empty.md) | Next: [15-batch-collector-clear-vs-nil-reset.md](15-batch-collector-clear-vs-nil-reset.md)
