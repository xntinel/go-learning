# Exercise 19: Composite-Key Token-Bucket Rate Limiter

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An API gateway rate-limits per `(tenant, route)` pair: tenant `acme` gets its
own burst allowance on `/checkout`, independent of tenant `acme`'s allowance
on `/status` and independent of tenant `globex`'s allowance on `/checkout`.
That is a composite dimension — two fields that together identify one
bucket — and the reflex when a Go map only comes to mind as `map[string]T`
is to squash the two fields into one string key: `tenant + ":" + route`.
The moment either field can itself contain a colon, that reflex produces a
silent collision: `tenant="a:b", route="c"` and `tenant="a", route="b:c"`
both flatten to the identical string `"a:b:c"`, and the gateway ends up
sharing one rate-limit bucket between two tenants that have nothing to do
with each other — one tenant's traffic can exhaust the other's quota.

Go does not require flattening a composite key at all. Any struct whose
fields are all comparable is itself a legal, natural map key — `map[Key]T`
where `Key` is `struct{ Tenant, Route string }` compares two keys field by
field, with `==`, the same equality the language already gives you for free.
There is no serialization step, no delimiter to escape, and therefore no
way for two genuinely different `(tenant, route)` pairs to collide unless
their fields are actually equal. This is not a workaround for the
delimiter problem; it is the idiomatic way to key a map on more than one
value, and reaching for string concatenation instead is the trap.

This module builds `ratelimit`, a per-`(tenant, route)` burst quota limiter
keyed by exactly this kind of struct. Its test isolates the
delimiter-collision mistake as an unexported helper, `stringKeyedLimiter`,
keyed by `tenant + ":" + route`, and pins the exact pair of inputs that
makes it silently share a bucket — the same pair the struct-keyed `Limiter`
keeps correctly separate.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ratelimit/                module example.com/ratelimit
  go.mod                  go 1.24
  ratelimit.go            Key, Limiter; NewLimiter, Allow, Reset, Burst
  ratelimit_test.go       allow/deny table, per-key isolation, Reset, the
                          delimiter-collision contrast, concurrency, ExampleLimiter
```

- Files: `ratelimit.go`, `ratelimit_test.go`.
- Implement: `Key struct{ Tenant, Route string }`; `Limiter` backed by `map[Key]*bucket`; `NewLimiter(burst int) *Limiter` clamping a non-positive burst to 1; `(*Limiter).Allow(tenant, route string) bool` consuming one unit of that pair's remaining allowance; `(*Limiter).Reset(tenant, route string)` restoring a pair's allowance to a full burst; `(*Limiter).Burst() int`.
- Test: consuming a burst down to denial, per-key isolation across tenant and route independently, `Reset` restoring an exhausted key and being harmless on an untouched one, burst clamping, a `stringKeyedLimiter` contrast proving a delimiter collision only affects the naive string key, a concurrency test asserting successful `Allow` calls never exceed the configured burst, and `ExampleLimiter` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/19-composite-key-token-bucket
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/19-composite-key-token-bucket
go mod edit -go=1.24
```

### A comparable struct is already a map key; nothing needs flattening

Go's rule for map keys is narrow but exact: a type is a legal key if values
of that type support `==`. Structs qualify whenever every field does, and
`Key{Tenant, Route string}` qualifies trivially — two strings compare with
`==` the same way they always have. `map[Key]*bucket` therefore needs no
extra machinery at all; `Key{"acme", "/checkout"}` and
`Key{"acme", "/status"}` are two ordinary, distinct map entries the moment
you write them, exactly as `map[string]*bucket` treats two different
strings as distinct entries.

The mistake this module targets is not using a struct key wrong — it is
not using one at all, in favor of a string built by concatenation:

```go
key := tenant + ":" + route   // looks harmless
buckets[key]                  // but "a:b" + ":" + "c" == "a" + ":" + "b:c"
```

Both sides of that comment produce the string `"a:b:c"`. If tenant IDs or
route paths are ever attacker-influenced, or simply drawn from a namespace
that was never designed to exclude colons, this is not a hypothetical: it
is a routine collision waiting for the first tenant whose name or route
happens to contain the delimiter. A struct key sidesteps the entire class
of bug, because there is no string representation for two field values to
collide inside — the equality check compares `Tenant` to `Tenant` and
`Route` to `Route` independently, never a joined blob.

The rest of `Limiter` is a plain, mutex-guarded counting bucket: each
`(tenant, route)` pair starts with a full `burst` allowance the first time
`Allow` sees it, `Allow` consumes one unit per call and returns `false` once
exhausted, and `Reset` restores the allowance. There is deliberately no
timer-driven refill inside the type — that would require an injected clock
this module does not need, and a caller that wants a fixed window (say, one
minute) drives that by calling `Reset` on its own schedule, keeping
`Limiter` itself simple and its tests free of any simulated time.

Create `ratelimit.go`:

```go
// Package ratelimit provides a per-(tenant, route) burst quota limiter keyed
// by a composite struct, so the two dimensions can never be confused with
// one another regardless of what characters either one contains.
package ratelimit

import "sync"

// Key identifies one rate-limit bucket by the composite (tenant, route)
// dimension an API gateway limits on.
//
// Key is a plain comparable struct and is a legal map key on its own: two
// Keys are equal exactly when both fields are equal. Key{"a:b", "c"} and
// Key{"a", "b:c"} are always distinct map entries, never a chance collision
// the way a delimiter-joined string key (tenant + ":" + route) would be if
// either field happened to contain the delimiter -- a composite key never
// needs flattening into a string to be used as a map key.
type Key struct {
	Tenant string
	Route  string
}

// bucket is the per-Key mutable state: how much of the burst allowance
// remains before Allow starts returning false.
type bucket struct {
	remaining int
}

// Limiter enforces a fixed per-(tenant, route) burst quota. Each Key starts
// with a full Burst allowance; Allow consumes one unit of it, and Reset
// restores a Key to a full Burst allowance.
//
// Limiter does not refill automatically on a timer. A caller that wants
// windowed rate limiting calls Reset at the start of each window, driven by
// whatever clock or scheduler it already has -- keeping this type free of
// injected-clock machinery a fixed-quota bucket does not need.
//
// Limiter is safe for concurrent use by multiple goroutines.
type Limiter struct {
	burst int

	mu      sync.Mutex
	buckets map[Key]*bucket
}

// NewLimiter returns a Limiter granting burst requests per (tenant, route)
// pair before Allow starts returning false for that pair. A non-positive
// burst is clamped to 1 rather than rejected, so a Limiter is always usable
// once constructed; Burst reports the value actually in effect.
func NewLimiter(burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{burst: burst, buckets: make(map[Key]*bucket)}
}

// Burst reports the per-Key allowance this Limiter was constructed with.
func (l *Limiter) Burst() int { return l.burst }

// Allow reports whether a request for (tenant, route) may proceed,
// consuming one unit of that pair's remaining burst allowance if so. The
// first call for a given (tenant, route) lazily creates its bucket with a
// full Burst allowance. Allow returns false once that pair's allowance is
// exhausted, until Reset restores it.
func (l *Limiter) Allow(tenant, route string) bool {
	key := Key{Tenant: tenant, Route: route}

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{remaining: l.burst}
		l.buckets[key] = b
	}
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// Reset restores (tenant, route)'s allowance to a full Burst, whether or
// not a bucket already existed for that pair.
func (l *Limiter) Reset(tenant, route string) {
	key := Key{Tenant: tenant, Route: route}

	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[key]; ok {
		b.remaining = l.burst
		return
	}
	l.buckets[key] = &bucket{remaining: l.burst}
}
```

### Using it

Construct one `Limiter` per gateway process at startup with the burst your
deployment allows, then call `Allow(tenant, route)` on the request path and
reject the request when it returns `false`. `Limiter` carries its own
mutex, so a single value is shared by every handler goroutine without extra
synchronization at the call site — that is the concurrency contract the
type's doc comment states, and `TestConcurrentAllowNeverExceedsBurst` holds
it to. There is no aliasing to worry about: every method takes and returns
plain values, never a slice or map the caller could mutate out from under
the limiter.

`ExampleLimiter` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleLimiter() {
	l := NewLimiter(2)

	fmt.Println(l.Allow("acme", "/checkout"))
	fmt.Println(l.Allow("acme", "/checkout"))
	fmt.Println(l.Allow("acme", "/checkout")) // burst exhausted

	fmt.Println(l.Allow("acme", "/status")) // different route, own bucket

	l.Reset("acme", "/checkout")
	fmt.Println(l.Allow("acme", "/checkout")) // burst restored

	// Output:
	// true
	// true
	// false
	// true
	// true
}
```

### Tests

`TestAllowConsumesBurstThenDenies` and `TestAllowIsPerKey` cover the
ordinary contract: a burst counts down to denial, and tenant and route each
independently determine which bucket a call touches. `TestReset` and
`TestResetOnUntouchedKeyIsHarmless` cover both ends of `Reset` — restoring
an exhausted key, and calling it on a key `Allow` never saw.
`TestNewLimiterClampsNonPositiveBurst` pins the zero-and-negative edge case.

`stringKeyedLimiter` is the antipattern from the concepts section,
reproduced as an unexported test type keyed by `tenant + ":" + route`.
`TestDelimiterCollisionOnlyAffectsStringKey` is the module's central test:
it feeds `stringKeyedLimiter` the pair `("a:b", "c")` and `("a", "b:c")` —
both of which flatten to `"a:b:c"` — and shows the second call is denied
because it collided with the first, then feeds the identical pair to the
real, struct-keyed `Limiter` and shows both calls succeed independently.
`TestConcurrentAllowNeverExceedsBurst` runs two hundred goroutines against
one key with a burst of fifty and asserts the success count is exactly
fifty — no lost decrement inflating it, no missed lock letting it run over
— which `-race` also checks for an unsynchronized bucket access.

Create `ratelimit_test.go`:

```go
package ratelimit

import (
	"fmt"
	"sync"
	"testing"
)

func TestAllowConsumesBurstThenDenies(t *testing.T) {
	t.Parallel()

	l := NewLimiter(3)
	for i := range 3 {
		if !l.Allow("tenant-a", "/orders") {
			t.Fatalf("Allow call %d: want true (burst not yet exhausted)", i+1)
		}
	}
	if l.Allow("tenant-a", "/orders") {
		t.Fatal("Allow after burst exhausted: want false")
	}
}

func TestAllowIsPerKey(t *testing.T) {
	t.Parallel()

	l := NewLimiter(1)
	if !l.Allow("tenant-a", "/orders") {
		t.Fatal("first Allow(tenant-a, /orders): want true")
	}
	if !l.Allow("tenant-b", "/orders") {
		t.Fatal("Allow(tenant-b, /orders): want true; different tenant, independent bucket")
	}
	if !l.Allow("tenant-a", "/users") {
		t.Fatal("Allow(tenant-a, /users): want true; different route, independent bucket")
	}
	if l.Allow("tenant-a", "/orders") {
		t.Fatal("second Allow(tenant-a, /orders): want false; that bucket's burst is spent")
	}
}

func TestReset(t *testing.T) {
	t.Parallel()

	l := NewLimiter(1)
	if !l.Allow("tenant-a", "/orders") {
		t.Fatal("first Allow: want true")
	}
	if l.Allow("tenant-a", "/orders") {
		t.Fatal("second Allow before Reset: want false")
	}
	l.Reset("tenant-a", "/orders")
	if !l.Allow("tenant-a", "/orders") {
		t.Fatal("Allow after Reset: want true")
	}
}

func TestResetOnUntouchedKeyIsHarmless(t *testing.T) {
	t.Parallel()

	l := NewLimiter(2)
	l.Reset("tenant-a", "/orders") // no prior Allow for this key
	if !l.Allow("tenant-a", "/orders") {
		t.Fatal("Allow after Reset on an untouched key: want true")
	}
}

func TestNewLimiterClampsNonPositiveBurst(t *testing.T) {
	t.Parallel()

	for _, burst := range []int{0, -5} {
		l := NewLimiter(burst)
		if l.Burst() != 1 {
			t.Fatalf("NewLimiter(%d).Burst() = %d, want 1", burst, l.Burst())
		}
	}
}

// stringKeyedLimiter reproduces this module's antipattern: a composite
// (tenant, route) key flattened into a single delimited string before it
// ever reaches the map. It is unexported, unreachable from Limiter's own
// API, and exists only so TestDelimiterCollisionOnlyAffectsStringKey can
// pin the collision numerically.
type stringKeyedLimiter struct {
	burst   int
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newStringKeyedLimiter(burst int) *stringKeyedLimiter {
	return &stringKeyedLimiter{burst: burst, buckets: make(map[string]*bucket)}
}

func (l *stringKeyedLimiter) allow(tenant, route string) bool {
	key := tenant + ":" + route // the antipattern: a flattened composite key
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{remaining: l.burst}
		l.buckets[key] = b
	}
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

// TestDelimiterCollisionOnlyAffectsStringKey is the heart of this module.
// tenant="a:b",route="c" and tenant="a",route="b:c" both flatten to the
// identical string "a:b:c" whenever the delimiter used to join them also
// appears inside one of the fields, so stringKeyedLimiter silently shares
// one bucket between two distinct tenants. Limiter's struct key cannot
// collide unless the fields are actually equal, so it grants both pairs
// their own full burst.
func TestDelimiterCollisionOnlyAffectsStringKey(t *testing.T) {
	t.Parallel()

	naive := newStringKeyedLimiter(1)
	if !naive.allow("a:b", "c") {
		t.Fatal(`naive.allow("a:b", "c"): want true`)
	}
	if naive.allow("a", "b:c") {
		t.Fatal(`naive.allow("a", "b:c"): want false -- collided with tenant "a:b" route "c" via the flattened key "a:b:c"`)
	}

	real := NewLimiter(1)
	if !real.Allow("a:b", "c") {
		t.Fatal(`real.Allow("a:b", "c"): want true`)
	}
	if !real.Allow("a", "b:c") {
		t.Fatal(`real.Allow("a", "b:c"): want true -- Key{"a","b:c"} is a distinct struct from Key{"a:b","c"}`)
	}
}

// TestConcurrentAllowNeverExceedsBurst runs many goroutines calling Allow on
// the same (tenant, route) pair at once and counts the successes. Under
// -race this also proves the mutex actually serializes bucket access; a
// missing lock would let two goroutines both observe remaining > 0 and both
// decrement, letting the success count exceed burst.
func TestConcurrentAllowNeverExceedsBurst(t *testing.T) {
	t.Parallel()

	const burst = 50
	const goroutines = 200
	l := NewLimiter(burst)

	var successes int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("tenant-a", "/orders") {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes != burst {
		t.Fatalf("successes = %d, want exactly %d (no lost decrements, no over-grants)", successes, burst)
	}
}

// ExampleLimiter is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleLimiter() {
	l := NewLimiter(2)

	fmt.Println(l.Allow("acme", "/checkout"))
	fmt.Println(l.Allow("acme", "/checkout"))
	fmt.Println(l.Allow("acme", "/checkout")) // burst exhausted

	fmt.Println(l.Allow("acme", "/status")) // different route, own bucket

	l.Reset("acme", "/checkout")
	fmt.Println(l.Allow("acme", "/checkout")) // burst restored

	// Output:
	// true
	// true
	// false
	// true
	// true
}
```

## Review

`Limiter` is correct when two distinct `(tenant, route)` pairs never share a
bucket unless both fields genuinely match, and `Key`'s struct equality
guarantees that by construction — there is no string representation in the
middle for two different pairs to collide inside. The mistake this module
targets is not a logic error in the counting itself; `stringKeyedLimiter`
counts correctly, it just counts the *wrong pair together* whenever a
delimiter-joined key happens to alias two different `(tenant, route)`
inputs, which `TestDelimiterCollisionOnlyAffectsStringKey` demonstrates
with the exact pair that triggers it. Around that core, `NewLimiter` clamps
an invalid burst to 1 rather than leaving a `Limiter` unusable, `Reset`
restores a pair's allowance unconditionally, and the whole type is
protected by a single mutex, verified under `-race` by a concurrency test
that would fail if any bucket read-modify-write happened outside the lock.
Run `go test -count=1 -race ./...` to confirm the table, the collision
contrast, and the concurrent-allowance property.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the rule that a struct is comparable, and therefore a legal map key, when every field is.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding the shared bucket map against concurrent Allow calls.
- [Go Wiki: CodeReviewComments](https://go.dev/wiki/CodeReviewComments) — general guidance on map key selection and avoiding string-concatenation keys.
- [Stripe API rate limiters](https://stripe.com/blog/rate-limiters) — a real-world discussion of per-dimension rate limiting design.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-content-hash-dedup-tool.md](18-content-hash-dedup-tool.md) | Next: [20-kv-snapshot-diff-tool.md](20-kv-snapshot-diff-tool.md)
