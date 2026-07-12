# Exercise 16: Concurrent Lazy-Compile Route Cache with sync.Map.LoadOrStore

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An HTTP router that matches incoming paths against registered patterns --
`^/users/(\d+)$`, `^/orders/(\d+)/items$` -- compiles each pattern to a
`*regexp.Regexp` the first time a request needs it, then reuses that compiled
value for every later request that matches the same route, from every
request-handling goroutine the server runs. That is a read-mostly,
write-once-per-key workload: a pattern is written exactly once and read an
unbounded number of times afterward, which is precisely the shape the map
internals lesson names as `sync.Map`'s actual fit, in contrast to the
write-heavy or overlapping-key workloads where a plain `RWMutex`-guarded map
wins. `sync.Map.LoadOrStore` gives idempotent lazy initialization keyed by
pattern: whichever goroutine gets there first compiles and installs the
value, and every other goroutine, no matter how many raced to the same
pattern, gets that same installed value back.

The trap is a "check then act" cache that looks identical in a code review
and passes every single-threaded test: `Load` the pattern, and if it is
absent, compile and `Store` it. Two goroutines matching the same brand-new
route on the same instant both see the miss, both compile, and both call
`Store` -- the second `Store` silently overwrites the first, so the two
goroutines (and whichever caller kept a reference from the first compile)
disagree about which `*regexp.Regexp` is now canonical. Nothing crashes and
nothing panics; the defect is a wasted compile plus a torn cache that
`go test -race` will not flag by itself, because `sync.Map`'s own methods
are individually race-free -- it is the *sequence* of two of them that is
not atomic.

This module builds `PatternCache` as the package you drop into a router: a
constructor that validates its capacity, a single `Compiled` method that
never exposes the buggy sequence as an option, and a cached compile error so
a malformed pattern does not pay the compile cost on every request either.
The check-then-act version is not part of that API; it lives in the test
file, isolated, as the thing the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
patterncache/             module example.com/patterncache
  go.mod                  go 1.24
  patterncache.go         entry, PatternCache; New, Len, Compiled; two sentinel errors
  patterncache_test.go    cache table, capacity limit, cached error, race contrast,
                           concurrency, ExamplePatternCache_Compiled
```

- Files: `patterncache.go`, `patterncache_test.go`.
- Implement: `New(capacity int) (*PatternCache, error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*PatternCache).Compiled(pattern string) (*regexp.Regexp, error)` built over `sync.Map.LoadOrStore` plus a per-entry `sync.Once`, returning `ErrCacheFull` when a new pattern arrives after the cache is at capacity; `(*PatternCache).Len() int`.
- Test: the cache table (compile once, reuse on repeat, cached compile error, empty pattern, capacity boundary); a `checkThenActCompile` contrast that pins the check-then-act race deterministically; a fifty-goroutine concurrency test asserting every caller observes the same `*regexp.Regexp` instance; and `ExamplePatternCache_Compiled` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/16-lazy-pattern-compile-cache
cd go-solutions/06-collections-arrays-slices-and-maps/08-map-internals-and-iteration-order/16-lazy-pattern-compile-cache
go mod edit -go=1.24
```

### LoadOrStore is one atomic step; Load-then-Store is two

`sync.Map.LoadOrStore(key, value)` is a single indivisible operation: it
either finds an existing entry and returns it, or installs `value` and
returns that -- and it tells every caller which case happened via its
second return value, `loaded`. Exactly one caller among any number racing on
the same key gets `loaded == false`; every other caller, whether it arrived
a nanosecond before or a full second after, gets `loaded == true` and the
first caller's value, never its own. That is what makes "idempotent lazy
init" a precise phrase rather than a hope: the property holds regardless of
how many goroutines call it concurrently, because the atomicity is
`sync.Map`'s job, not the caller's.

Contrast that with the version almost everyone writes first, because it
reads naturally and passes every test run by a single goroutine:

```go
if v, ok := c.m.Load(pattern); !ok {
    v = compile(pattern)
    c.m.Store(pattern, v)
}
```

`Load` and `Store` are each individually race-free, but nothing links them.
Between the `Load` that returns `!ok` and the `Store` that follows, another
goroutine can run the exact same two lines against the exact same key. Both
compile their own `*regexp.Regexp`; both call `Store`; the one that runs
last wins, and the other's compiled value -- possibly already returned to a
caller -- is now orphaned. `sync.Map`'s own internal bookkeeping never
corrupts, so `-race` finds nothing to report; the bug is purely at the level
of "which value is canonical", and it only shows up as two callers silently
disagreeing about it. The fix is never to write the sequence at all:
`LoadOrStore` a placeholder `*entry` that carries a `sync.Once`, then let
`once.Do` run the actual compile. Every goroutine that raced to that key,
winner and losers alike, shares the one `*entry` `LoadOrStore` settled on,
and `once.Do` guarantees the compile itself runs exactly once regardless of
how many of them call it.

Create `patterncache.go`:

```go
// Package patterncache lazily compiles regular-expression route patterns and
// caches the result, so a pattern that many request-handling goroutines match
// against is compiled at most once regardless of how many goroutines arrive
// for it before the first compile finishes.
//
// It exists to get one detail right that a hand-rolled "check then act" cache
// routinely gets wrong under concurrency: a Load that returns not-found,
// followed by a conditional Store, is two separate map operations with a
// window between them. Two goroutines can both observe not-found and both
// compile, and whichever Store runs last silently wins, discarding the other
// goroutine's compiled *regexp.Regexp. See the package tests for a
// side-by-side demonstration of that race and the property it violates.
package patterncache

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
)

// Sentinel errors returned by New and Compiled. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidCapacity means the configured pattern capacity was not positive.
	ErrInvalidCapacity = errors.New("patterncache: capacity must be positive")
	// ErrCacheFull means a pattern not already cached arrived after the cache
	// reached its configured capacity.
	ErrCacheFull = errors.New("patterncache: capacity exceeded")
)

// entry holds the lazily-compiled result for one pattern. once guarantees
// regexp.Compile runs exactly once per pattern, no matter how many
// goroutines call Compiled for it concurrently; re and err are only read
// after once.Do has returned, which happens-before every reader.
type entry struct {
	once sync.Once
	re   *regexp.Regexp
	err  error
}

// PatternCache maps route path patterns to their compiled *regexp.Regexp,
// compiling each distinct pattern exactly once and reusing the result for
// every later match against the same pattern -- the read-mostly,
// write-once-per-key workload sync.Map is built for.
//
// PatternCache is safe for concurrent use by multiple goroutines. Compiled
// may be called from any number of request-handling goroutines at once; each
// distinct pattern is compiled by exactly one of them, and every caller,
// including the ones that raced to be first, observes the same
// *regexp.Regexp instance.
type PatternCache struct {
	capacity int64
	size     atomic.Int64
	m        sync.Map // string (pattern) -> *entry
}

// New returns a PatternCache that accepts at most capacity distinct
// patterns. It returns ErrInvalidCapacity if capacity is not positive.
//
// capacity exists because the pattern is normally drawn from a finite,
// static set of routes; a positive bound turns an unexpected flood of
// distinct patterns (a bug upstream, or a caller passing raw untrusted path
// segments instead of a registered pattern) into an error instead of
// unbounded regexp compilation and cache growth.
func New(capacity int) (*PatternCache, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &PatternCache{capacity: int64(capacity)}, nil
}

// Len reports the number of distinct patterns currently cached, including
// patterns whose compile failed. It is a live value under concurrent use and
// may be stale by the time the caller reads it.
func (c *PatternCache) Len() int {
	return int(c.size.Load())
}

// Compiled returns the compiled regexp for pattern, compiling it on the
// first call for that pattern and returning the cached result on every
// later call, including a cached compile error for an invalid pattern --
// so a repeatedly-invalid pattern never pays the compile cost twice.
//
// Compiled returns ErrCacheFull if pattern is new and the cache is already
// at its configured capacity. The returned *regexp.Regexp is shared across
// every caller and must not be mutated; regexp.Regexp has no exported
// mutable state, so ordinary use (MatchString, FindStringSubmatch, ...) is
// always safe to share.
func (c *PatternCache) Compiled(pattern string) (*regexp.Regexp, error) {
	if v, ok := c.m.Load(pattern); ok {
		return resolve(v.(*entry), pattern)
	}
	if c.size.Load() >= c.capacity {
		return nil, fmt.Errorf("%w: at capacity %d", ErrCacheFull, c.capacity)
	}

	// LoadOrStore is the atomic check-and-set a Load-then-Store pair cannot
	// be: exactly one goroutine's *entry wins the key, and every goroutine
	// -- winner and losers alike -- gets that same *entry back in actual.
	actual, loaded := c.m.LoadOrStore(pattern, &entry{})
	if !loaded {
		c.size.Add(1)
	}
	return resolve(actual.(*entry), pattern)
}

// resolve compiles pattern into e exactly once and returns the shared
// result. Every goroutine holding e, whether it won or lost the
// LoadOrStore, blocks in once.Do until the first caller's compile finishes,
// then reads the same re/err.
func resolve(e *entry, pattern string) (*regexp.Regexp, error) {
	e.once.Do(func() {
		e.re, e.err = regexp.Compile(pattern)
	})
	if e.err != nil {
		return nil, fmt.Errorf("patterncache: compile %q: %w", pattern, e.err)
	}
	return e.re, nil
}
```

### Using it

Construct one `PatternCache` at router startup with the capacity your route
table can plausibly need, then call `Compiled` from every handler goroutine
that needs to match a path -- there is nothing to lock, because `PatternCache`
declares itself safe for concurrent use and the type backs that with
`sync.Map` plus a per-entry `sync.Once` rather than a mutex the caller has to
remember to take. The contract that crosses the package boundary is on
`Compiled`'s doc comment: the returned `*regexp.Regexp` is shared by every
caller, so treat it as read-only (which is the only way `regexp.Regexp` is
ever meant to be used) and never assume a fresh instance.

`ExamplePatternCache_Compiled` is this module's runnable demonstration: `go
test` executes it and diffs its stdout against the `// Output:` comment
below, so the usage shown here cannot drift from the code that actually
compiled.

```go
func ExamplePatternCache_Compiled() {
	c, err := New(8)
	if err != nil {
		panic(err)
	}

	re, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		panic(err)
	}
	fmt.Println("match /users/42:", re.MatchString("/users/42"))

	again, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		panic(err)
	}
	fmt.Println("same instance on second call:", re == again)

	if _, err := c.Compiled(`(unterminated`); err != nil {
		fmt.Println("invalid pattern rejected:", err)
	}

	fmt.Println("distinct patterns cached:", c.Len())

	// Output:
	// match /users/42: true
	// same instance on second call: true
	// invalid pattern rejected: patterncache: compile "(unterminated": error parsing regexp: missing closing ): `(unterminated`
	// distinct patterns cached: 2
}
```

The last line is worth pausing on: the failed pattern still occupies a slot
in the cache, because `Compiled` caches the *error* too -- otherwise a
client that keeps sending the same malformed pattern would force a
`regexp.Compile` attempt on every single request, turning one bad pattern
into an unbounded compile cost. That is a smaller instance of the same
"unbounded cost from an attacker-influenced key" shape the bounded-cardinality
exercise later in this lesson tackles head-on.

### Tests

`TestCompiledCachesResult` and `TestCompiledCachesCompileError` pin the two
outcomes `resolve` produces and prove both are cached: a successful compile
returns the identical `*regexp.Regexp` pointer on a second call, and a
failing compile returns the identical error text without recompiling.
`TestCompiledEmptyPatternIsValid` covers the edge case where the pattern
itself is the empty string -- a legal regexp that matches everywhere, not an
error. `TestCompiledRejectsWhenCacheFull` and `TestNewRejectsNonPositiveCapacity`
cover the capacity boundary.

`TestCheckThenActRaceLosesAnInstance` is the heart of the module.
`checkThenActCompile` is unexported and unreachable from the package API; it
takes an `afterLoad` hook that runs between the naive `Load` miss and the
`compile`+`Store` that follows, which is what lets the test script the race
deterministically -- caller A's hook runs caller B to completion before A
stores -- instead of hoping two real goroutines happen to interleave badly
on a given run. The test asserts A and B compiled two distinct instances and
that the map now holds only A's, meaning B's instance, already returned to
its caller, is silently stale. `TestCompiledIsSafeForConcurrentUse` then
proves the real `PatternCache` never allows that: fifty goroutines race
`Compiled` on the same new pattern, and every one of them receives the same
pointer.

Create `patterncache_test.go`:

```go
package patterncache

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1, -100} {
		if _, err := New(capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestCompiledCachesResult(t *testing.T) {
	t.Parallel()

	c, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		t.Fatalf("Compiled: %v", err)
	}
	second, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		t.Fatalf("Compiled: %v", err)
	}
	if first != second {
		t.Fatalf("Compiled returned two different instances for the same pattern")
	}
	if !first.MatchString("/users/42") {
		t.Fatalf("compiled pattern does not match the path it was built for")
	}
}

func TestCompiledCachesCompileError(t *testing.T) {
	t.Parallel()

	c, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err1 := c.Compiled(`(unterminated`)
	if err1 == nil {
		t.Fatal("Compiled: want a compile error for an invalid pattern")
	}
	_, err2 := c.Compiled(`(unterminated`)
	if err2 == nil {
		t.Fatal("Compiled: want a compile error on the second call too")
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("cached compile error changed between calls: %q vs %q", err1, err2)
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 (the failed pattern still occupies its slot)", c.Len())
	}
}

func TestCompiledEmptyPatternIsValid(t *testing.T) {
	t.Parallel()

	c, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	re, err := c.Compiled("")
	if err != nil {
		t.Fatalf("Compiled(\"\"): %v", err)
	}
	if !re.MatchString("anything") {
		t.Fatal("empty pattern must match every string at position 0")
	}
}

func TestCompiledRejectsWhenCacheFull(t *testing.T) {
	t.Parallel()

	c, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Compiled("a"); err != nil {
		t.Fatalf("Compiled(a): %v", err)
	}
	if _, err := c.Compiled("b"); err != nil {
		t.Fatalf("Compiled(b): %v", err)
	}
	// A third distinct pattern exceeds the configured capacity.
	if _, err := c.Compiled("c"); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("Compiled(c) error = %v, want ErrCacheFull", err)
	}
	// A pattern already cached still succeeds: the cap applies only to new keys.
	if _, err := c.Compiled("a"); err != nil {
		t.Fatalf("Compiled(a) again: %v", err)
	}
}

// checkThenActCompile is the cache almost everyone writes first: Load, and
// only on a miss, compile and Store. It is never exported and never reached
// from the package API; it exists so the test below can pin exactly what it
// gets wrong. afterLoad runs after the Load miss and before the compile and
// Store, which is what lets the test script a deterministic race instead of
// hoping two real goroutines interleave badly on a given run.
func checkThenActCompile(m *sync.Map, pattern string, afterLoad func()) *regexp.Regexp {
	if v, ok := m.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	if afterLoad != nil {
		afterLoad()
	}
	re := regexp.MustCompile(pattern)
	m.Store(pattern, re)
	return re
}

// TestCheckThenActRaceLosesAnInstance is the whole point of the module. It
// pins the check-then-act race deterministically: caller A's afterLoad hook
// runs caller B to completion between A's Load and A's Store, standing in
// for "B's Store happens to land in that window" without depending on real
// goroutine scheduling. The correct PatternCache, by contrast, guarantees
// exactly one canonical instance no matter how many callers race for the
// same pattern -- TestCompiledIsSafeForConcurrentUse below proves that under
// real concurrency.
func TestCheckThenActRaceLosesAnInstance(t *testing.T) {
	t.Parallel()

	const pattern = `^/orders/(\d+)$`
	var m sync.Map
	var winnerB *regexp.Regexp

	winnerA := checkThenActCompile(&m, pattern, func() {
		winnerB = checkThenActCompile(&m, pattern, nil)
	})

	if winnerA == winnerB {
		t.Fatal("setup: A and B must compile distinct instances for this test to mean anything")
	}
	stored, _ := m.Load(pattern)
	if stored.(*regexp.Regexp) != winnerA {
		t.Fatalf("stored instance = %p, want A's instance %p (the last Store wins)", stored, winnerA)
	}
	// winnerB was fully compiled and handed back to its caller, then
	// immediately overwritten by A's Store: a caller holding winnerB
	// observes a different *regexp.Regexp than every later Load returns --
	// the defect the correct PatternCache makes impossible.
}

func TestCompiledIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const pattern = `^/orders/(\d+)$`

	results := make(chan *regexp.Regexp, 50)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			re, err := c.Compiled(pattern)
			if err != nil {
				t.Errorf("Compiled: %v", err)
				return
			}
			results <- re
		}()
	}
	wg.Wait()
	close(results)

	var first *regexp.Regexp
	for re := range results {
		if first == nil {
			first = re
			continue
		}
		if re != first {
			t.Fatalf("two goroutines observed different compiled instances for %q", pattern)
		}
	}
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1: fifty racing callers share one cache entry", c.Len())
	}
}

// ExamplePatternCache_Compiled is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below.
func ExamplePatternCache_Compiled() {
	c, err := New(8)
	if err != nil {
		panic(err)
	}

	re, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		panic(err)
	}
	fmt.Println("match /users/42:", re.MatchString("/users/42"))

	again, err := c.Compiled(`^/users/(\d+)$`)
	if err != nil {
		panic(err)
	}
	fmt.Println("same instance on second call:", re == again)

	if _, err := c.Compiled(`(unterminated`); err != nil {
		fmt.Println("invalid pattern rejected:", err)
	}

	fmt.Println("distinct patterns cached:", c.Len())

	// Output:
	// match /users/42: true
	// same instance on second call: true
	// invalid pattern rejected: patterncache: compile "(unterminated": error parsing regexp: missing closing ): `(unterminated`
	// distinct patterns cached: 2
}
```

## Review

`PatternCache` is correct when every caller racing on the same new pattern
observes the identical `*regexp.Regexp`, and `LoadOrStore` over a
`sync.Once`-bearing `entry` is what makes that a guarantee rather than a
likelihood: the map operation is atomic, and the `sync.Once` makes the
compile itself run exactly once for whichever entry won. The trap this
avoids is a Load-then-Store pair that looks correct under a single
goroutine and under `-race` -- neither individual method races -- but is not
atomic as a sequence, so two goroutines can each compile their own instance
and silently disagree about which one is canonical afterward.
`ErrInvalidCapacity` guards construction, `ErrCacheFull` bounds how many
distinct patterns the cache will ever hold, and a cached compile error keeps
a single malformed pattern from paying the compile cost on every request
that carries it. Run `go test -count=1 -race ./...` to confirm the cache
table, the capacity boundary, the deterministic race contrast, and the
fifty-goroutine concurrency test.

## Resources

- [`sync.Map.LoadOrStore`](https://pkg.go.dev/sync#Map.LoadOrStore) — the atomic check-and-set this module builds on.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — how the cache guarantees the compile itself runs exactly once per key.
- [`regexp.Compile`](https://pkg.go.dev/regexp#Compile) and [`regexp.Regexp`](https://pkg.go.dev/regexp#Regexp) — the compiled value being cached, and why it is safe to share once built.
- [Go Wiki: sync.Map](https://pkg.go.dev/sync#Map) — the read-mostly, write-once-per-key workload `sync.Map` is tuned for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-weighted-round-robin-pool.md](15-weighted-round-robin-pool.md) | Next: [17-bounded-cardinality-label-counter.md](17-bounded-cardinality-label-counter.md)
