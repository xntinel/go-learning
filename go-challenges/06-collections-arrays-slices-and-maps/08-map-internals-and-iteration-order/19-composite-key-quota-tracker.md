# Exercise 19: Composite Struct Keys for a Multi-Tenant Quota Tracker

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A multi-tenant rate limiter sitting in front of a shared resource pool --
CPU quota, connection slots, API call budget -- tracks a usage counter per
`(tenant, resource)` pair. Every tenant's usage of every resource is
logically its own independent bucket, and the tracker's entire correctness
rests on never letting two distinct buckets share a counter. That single
property is also the easiest one to break by accident, because Go map keys
only need to be comparable, and both a struct of comparable fields and a
plain string satisfy that requirement equally well from the compiler's point
of view. Only one of them is actually safe to use here.

The instinct that leads to the unsafe one is not laziness -- it looks like
the more portable choice, the one that will slot into a log line or a
metrics label without a second type. Concatenate `tenant` and `resource`
with a delimiter, `tenant + ":" + resource`, and use that string as the map
key. It compiles, it reads naturally, and it works in every test where the
tenant and resource names happen not to contain the delimiter. It silently
breaks the instant one of them does: a tenant literally named `"acme:prod"`
requesting resource `"cpu"` produces the exact same string as tenant
`"acme"` requesting resource `"prod:cpu"`. Two unrelated logical buckets
now share one counter, and one tenant's usage corrupts another's -- exactly
the kind of cross-tenant data leak that turns a rate-limiter bug into a
security incident, not just a metrics glitch.

This module builds `QuotaTracker` as the package you drop into a limiter:
`BucketKey`, a small struct of the two fields that actually identify a
bucket, used directly as a map key with no encoding step in between. The
concatenated-string version never appears in the type's API; it lives in
the test file, isolated, as the key strategy the tests prove corrupts data.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
quotatracker/              module example.com/quotatracker
  go.mod                   go 1.24
  quotatracker.go          BucketKey, QuotaTracker; New, Increment, Usage, Snapshot
  quotatracker_test.go     bucket table, limit boundary, snapshot aliasing,
                           concatenated-key collision, concurrency, ExampleQuotaTracker_Increment
```

- Files: `quotatracker.go`, `quotatracker_test.go`.
- Implement: `New(limit int64) (*QuotaTracker, error)` rejecting a non-positive limit with `ErrInvalidLimit`; `(*QuotaTracker).Increment(tenant, resource string) (int64, error)` returning `ErrQuotaExceeded` (with usage unchanged) once a bucket is at the limit; `Usage(tenant, resource string) (int64, bool)`; `Snapshot() map[BucketKey]int64` returning an independent copy; `BucketKey struct{ Tenant, Resource string }`.
- Test: independent bucket tracking; the exact limit boundary (allowed at the limit, rejected past it, usage left unchanged on rejection); an absent bucket reading `ok == false`; `Snapshot` never aliasing the internal map; a `concatKey` contrast proving a delimiter-joined string key collides across two different tenant/resource pairs while `BucketKey` does not; concurrent `Increment` calls from many goroutines; and `ExampleQuotaTracker_Increment` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/quotatracker
cd ~/go-exercises/quotatracker
go mod init example.com/quotatracker
go mod edit -go=1.24
```

### A map key needs only be comparable -- it never needs to be a string

The Go specification's requirement for a map key type is exactly one word:
comparable. `==` and `!=` must be defined for it. Strings satisfy that.
Integers satisfy that. And so does any struct whose fields are themselves
comparable -- struct equality compares every field, in order, and two
struct values are equal only when every field matches. `BucketKey{Tenant:
"a", Resource: "b"}` and `BucketKey{Tenant: "a", Resource: "c"}` are
unequal because `Resource` differs, exactly as `map[BucketKey]int64`
requires, with no encoding step and no delimiter anywhere in the picture.

The concatenated-string key exists only because building a string feels
like "the normal way to make a key" when you have not stopped to ask
whether the key type needs to be a string at all. Here is the version that
looks completely reasonable in a review:

```go
// BUGGY: two different (tenant, resource) pairs can produce the same key.
key := tenant + ":" + resource
usage[key]++
```

The collision is not a theoretical edge case reserved for adversarial
input. Tenant slugs, resource names, and namespaced identifiers routinely
contain colons, slashes, or whatever delimiter a team picks, especially once
a platform lets tenants choose their own names or resources are named
hierarchically (`"prod:cpu"`, `"team-a/queue-1"`). The moment any field's
value can contain the delimiter, the string built from `"acme:prod"` +
`"cpu"` is byte-for-byte identical to the string built from `"acme"` +
`"prod:cpu"`, and the map cannot tell them apart -- it never saw two
fields, only the one string that resulted from joining them. A struct key
never faces this problem because it never joins anything: `Tenant` and
`Resource` remain two separate fields all the way into the comparison the
map performs internally.

Create `quotatracker.go`:

```go
// Package quotatracker tracks a per-(tenant, resource) usage counter against
// a shared limit, the shape of a multi-tenant rate limiter sitting in front
// of a pooled resource.
//
// It exists to get one detail right that a hand-rolled tracker routinely
// gets wrong: the bucket key is a struct of the two comparable fields that
// actually identify it, not a string built by concatenating them. A map key
// needs only be comparable -- it never needs to be a string -- and a struct
// of comparable fields is a natural, collision-free key. See the package
// tests for a demonstration of the collision a concatenated string key
// introduces.
package quotatracker

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

// Sentinel errors returned by New and Increment. Callers should test for
// them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidLimit means the configured per-bucket limit was not positive.
	ErrInvalidLimit = errors.New("quotatracker: limit must be positive")
	// ErrQuotaExceeded means an Increment would have taken a bucket's usage
	// above the configured limit; the bucket's usage is left unchanged.
	ErrQuotaExceeded = errors.New("quotatracker: quota exceeded")
)

// BucketKey identifies one (tenant, resource) usage bucket. Both fields
// participate in equality, so two buckets are the same bucket only when
// both Tenant and Resource match exactly -- there is no delimiter for a
// value to accidentally contain and no ambiguity a naive "tenant:resource"
// string key would introduce.
type BucketKey struct {
	Tenant   string
	Resource string
}

// QuotaTracker counts usage per (tenant, resource) bucket against a single
// shared limit, refusing any increment that would push a bucket's usage
// above it.
//
// QuotaTracker is safe for concurrent use by multiple goroutines.
type QuotaTracker struct {
	mu    sync.RWMutex
	limit int64
	usage map[BucketKey]int64
}

// New returns a QuotaTracker where every bucket is capped at limit. It
// returns ErrInvalidLimit if limit is not positive.
func New(limit int64) (*QuotaTracker, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidLimit, limit)
	}
	return &QuotaTracker{
		limit: limit,
		usage: make(map[BucketKey]int64),
	}, nil
}

// Increment adds one to the usage of the (tenant, resource) bucket and
// returns the new count. If the bucket is already at the configured limit,
// Increment returns ErrQuotaExceeded and leaves the bucket's usage
// unchanged; the returned count in that case is the usage before the
// rejected increment.
func (t *QuotaTracker) Increment(tenant, resource string) (int64, error) {
	key := BucketKey{Tenant: tenant, Resource: resource}

	t.mu.Lock()
	defer t.mu.Unlock()

	current := t.usage[key]
	if current >= t.limit {
		return current, fmt.Errorf("%w: tenant=%q resource=%q limit=%d", ErrQuotaExceeded, tenant, resource, t.limit)
	}
	current++
	t.usage[key] = current
	return current, nil
}

// Usage reports the current count for the (tenant, resource) bucket. ok is
// false if that exact bucket has never been incremented, distinguishing "no
// usage recorded yet" from "usage recorded and happens to be zero" --
// though in practice Increment never leaves a bucket at zero once touched,
// this is the comma-ok form a caller should always use to check presence.
func (t *QuotaTracker) Usage(tenant, resource string) (int64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.usage[BucketKey{Tenant: tenant, Resource: resource}]
	return n, ok
}

// Snapshot returns an independent copy of every bucket's current usage,
// safe to read or mutate without affecting the tracker or racing against
// concurrent Increment calls. It never returns the tracker's internal map.
func (t *QuotaTracker) Snapshot() map[BucketKey]int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return maps.Clone(t.usage)
}
```

### Using it

Construct one `QuotaTracker` per limiter with the shared per-bucket limit,
then call `Increment` from every request-handling goroutine as it admits or
rejects a request for a given tenant and resource -- `QuotaTracker` takes
its own lock, so nothing further needs to be coordinated by the caller. The
error `Increment` returns is meant to be checked with `errors.Is(err,
ErrQuotaExceeded)` and used directly as the signal to reject the request;
the returned count on that path is the bucket's usage *before* the rejected
attempt, not a sentinel value, so a caller can still report "you are at
N of your limit" without a second lookup. `Snapshot` hands back an
independent copy for reporting or export, so mutating it can never corrupt
the tracker's live state.

`ExampleQuotaTracker_Increment` is this module's runnable demonstration: `go
test` executes it and diffs its stdout against the `// Output:` comment
below.

```go
func ExampleQuotaTracker_Increment() {
	q, err := New(2)
	if err != nil {
		panic(err)
	}

	for range 3 {
		n, err := q.Increment("acme", "cpu")
		if err != nil {
			fmt.Println("rejected:", err)
			continue
		}
		fmt.Println("usage:", n)
	}

	usage, ok := q.Usage("acme", "cpu")
	fmt.Println("final usage:", usage, "ok:", ok)

	// Output:
	// usage: 1
	// usage: 2
	// rejected: quotatracker: quota exceeded: tenant="acme" resource="cpu" limit=2
	// final usage: 2 ok: true
}
```

### Tests

`TestIncrementTracksBucketsIndependently` is the ordinary path: two
resources for one tenant and a second tenant on the same resource all get
their own count. `TestUsageAbsentBucketIsNotOK` covers the comma-ok edge
case: a bucket nobody has incremented yet reads `ok == false`, not a
misleading zero. `TestIncrementRejectsPastLimit` is the boundary this module
cares about most on the quota side: it fills a bucket exactly to the limit,
confirms the next `Increment` is refused with `ErrQuotaExceeded` and that
usage is left at the limit rather than clamped or wrapped, and confirms a
sibling bucket for the same tenant is unaffected. `TestSnapshotDoesNotAliasInternalMap`
mutates the returned map and checks the tracker's own state.

`TestConcatenatedKeyCollidesAcrossTenants` is the heart of the module.
`concatKey` is unexported and unreachable from the package API; the test
first proves the collision in isolation -- `concatKey("acme:prod", "cpu")`
and `concatKey("acme", "prod:cpu")` produce the identical string -- and then
shows the consequence on a plain `map[string]int64`: incrementing the first
pair twice and the second pair once reports a merged count of three under
the shared key, meaning one tenant's usage silently absorbed another's.
The same three increments against the real `QuotaTracker`, keyed by
`BucketKey`, report the two pairs' usage correctly separated.
`TestQuotaTrackerIsSafeForConcurrentUse` then drives `Increment` from twenty
goroutines across four resources and checks every increment is accounted
for exactly once in the final snapshot.

Create `quotatracker_test.go`:

```go
package quotatracker

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestNewRejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()

	for _, limit := range []int64{0, -1, -100} {
		if _, err := New(limit); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("New(%d) error = %v, want ErrInvalidLimit", limit, err)
		}
	}
}

func TestIncrementTracksBucketsIndependently(t *testing.T) {
	t.Parallel()

	q, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := q.Increment("acme", "cpu"); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := q.Increment("acme", "cpu"); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := q.Increment("acme", "memory"); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := q.Increment("globex", "cpu"); err != nil {
		t.Fatalf("Increment: %v", err)
	}

	cases := []struct {
		tenant, resource string
		want             int64
	}{
		{"acme", "cpu", 2},
		{"acme", "memory", 1},
		{"globex", "cpu", 1},
	}
	for _, tc := range cases {
		got, ok := q.Usage(tc.tenant, tc.resource)
		if !ok || got != tc.want {
			t.Errorf("Usage(%q,%q) = (%d,%v), want (%d,true)", tc.tenant, tc.resource, got, ok, tc.want)
		}
	}
}

func TestUsageAbsentBucketIsNotOK(t *testing.T) {
	t.Parallel()

	q, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := q.Increment("acme", "cpu"); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if n, ok := q.Usage("acme", "memory"); ok || n != 0 {
		t.Fatalf("Usage(untouched bucket) = (%d,%v), want (0,false)", n, ok)
	}
}

func TestIncrementRejectsPastLimit(t *testing.T) {
	t.Parallel()

	q, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n, err := q.Increment("acme", "cpu"); err != nil || n != 1 {
		t.Fatalf("Increment #1 = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.Increment("acme", "cpu"); err != nil || n != 2 {
		t.Fatalf("Increment #2 = (%d,%v), want (2,nil)", n, err)
	}
	// The bucket is now exactly at the limit; the third increment must be
	// refused and usage must stay at 2, not silently clamp or wrap.
	n, err := q.Increment("acme", "cpu")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Increment #3 error = %v, want ErrQuotaExceeded", err)
	}
	if n != 2 {
		t.Fatalf("Increment #3 returned usage %d, want 2 (unchanged)", n)
	}
	if got, _ := q.Usage("acme", "cpu"); got != 2 {
		t.Fatalf("Usage after rejected increment = %d, want 2", got)
	}
	// A different resource for the same tenant is a different bucket and is
	// unaffected by the first bucket being at its limit.
	if _, err := q.Increment("acme", "memory"); err != nil {
		t.Fatalf("Increment(acme, memory): %v", err)
	}
}

func TestSnapshotDoesNotAliasInternalMap(t *testing.T) {
	t.Parallel()

	q, err := New(5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := q.Increment("acme", "cpu"); err != nil {
		t.Fatalf("Increment: %v", err)
	}

	snap := q.Snapshot()
	snap[BucketKey{Tenant: "acme", Resource: "cpu"}] = 999
	snap[BucketKey{Tenant: "injected", Resource: "x"}] = 1

	if got, _ := q.Usage("acme", "cpu"); got != 1 {
		t.Fatalf("mutating a snapshot changed the tracker's internal state: got %d, want 1", got)
	}
	if _, ok := q.Usage("injected", "x"); ok {
		t.Fatal("mutating a snapshot injected a bucket into the tracker")
	}
}

// concatKey is the bucket key almost everyone reaches for first: join the
// two fields with a delimiter into one string. It is never exported and
// never reached from the package API; it exists so the test below can pin
// exactly what it gets wrong.
func concatKey(tenant, resource string) string {
	return tenant + ":" + resource
}

// TestConcatenatedKeyCollidesAcrossTenants is the whole point of the
// module. Two entirely different (tenant, resource) pairs produce the
// identical concatenated string the moment a value contains the delimiter,
// so a map keyed by that string silently merges their usage counters --
// tenant "acme:prod" resource "cpu" collides with tenant "acme" resource
// "prod:cpu". BucketKey, a struct of the two raw fields, cannot collide
// this way: struct equality compares Tenant and Resource independently, so
// no value of either field can be mistaken for a delimiter.
func TestConcatenatedKeyCollidesAcrossTenants(t *testing.T) {
	t.Parallel()

	keyA := concatKey("acme:prod", "cpu")
	keyB := concatKey("acme", "prod:cpu")
	if keyA != keyB {
		t.Fatalf("setup: concatKey(%q) = %q, concatKey(%q) = %q; want them equal to demonstrate the collision", "acme:prod/cpu", keyA, "acme/prod:cpu", keyB)
	}

	// A tracker built on the naive string key merges the two tenants' usage:
	// incrementing one pair corrupts the other's reported count.
	naive := make(map[string]int64)
	naive[concatKey("acme:prod", "cpu")]++
	naive[concatKey("acme:prod", "cpu")]++
	naive[concatKey("acme", "prod:cpu")]++ // meant to be a disjoint bucket
	if got := naive[concatKey("acme:prod", "cpu")]; got != 3 {
		t.Fatalf("naive merged count = %d, want 3: acme's cpu usage absorbed acme:prod's usage", got)
	}

	// The real QuotaTracker keeps the two pairs fully separate.
	q, err := New(10)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustIncrement(t, q, "acme:prod", "cpu")
	mustIncrement(t, q, "acme:prod", "cpu")
	mustIncrement(t, q, "acme", "prod:cpu")

	gotA, _ := q.Usage("acme:prod", "cpu")
	gotB, _ := q.Usage("acme", "prod:cpu")
	if gotA != 2 {
		t.Errorf("Usage(acme:prod, cpu) = %d, want 2", gotA)
	}
	if gotB != 1 {
		t.Errorf("Usage(acme, prod:cpu) = %d, want 1: it must not have absorbed the other bucket's usage", gotB)
	}
}

func mustIncrement(t *testing.T, q *QuotaTracker, tenant, resource string) {
	t.Helper()
	if _, err := q.Increment(tenant, resource); err != nil {
		t.Fatalf("Increment(%q,%q): %v", tenant, resource, err)
	}
}

func TestQuotaTrackerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	q, err := New(1000)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			resource := "res-" + strconv.Itoa(g%4)
			for range 25 {
				if _, err := q.Increment("shared-tenant", resource); err != nil {
					t.Errorf("Increment: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	var total int64
	for _, n := range q.Snapshot() {
		total += n
	}
	if total != 500 { // 20 goroutines * 25 increments each
		t.Fatalf("total usage = %d, want 500", total)
	}
}

// ExampleQuotaTracker_Increment is the runnable demonstration of this
// module: go test executes it and compares its stdout against the Output
// comment below.
func ExampleQuotaTracker_Increment() {
	q, err := New(2)
	if err != nil {
		panic(err)
	}

	for range 3 {
		n, err := q.Increment("acme", "cpu")
		if err != nil {
			fmt.Println("rejected:", err)
			continue
		}
		fmt.Println("usage:", n)
	}

	usage, ok := q.Usage("acme", "cpu")
	fmt.Println("final usage:", usage, "ok:", ok)

	// Output:
	// usage: 1
	// usage: 2
	// rejected: quotatracker: quota exceeded: tenant="acme" resource="cpu" limit=2
	// final usage: 2 ok: true
}
```

## Review

`QuotaTracker` is correct when two different `(tenant, resource)` pairs
never share a counter, no matter what characters either value contains, and
`BucketKey` is what makes that a guarantee rather than a convention to
remember: struct equality compares `Tenant` and `Resource` as two
independent fields, so there is no delimiter, no encoding, and no string a
value could contain that would make two distinct pairs compare equal. The
trap this design avoids looks completely idiomatic -- concatenating fields
into a string key is a pattern that appears throughout real codebases -- and
fails silently rather than loudly: `TestConcatenatedKeyCollidesAcrossTenants`
shows it merging two tenants' usage under one counter with no panic, no
error, and no test failure until someone specifically checks for the
collision. Around that core, `ErrInvalidLimit` guards construction,
`ErrQuotaExceeded` enforces the per-bucket cap while leaving a rejected
bucket's usage untouched, `Usage`'s comma-ok result distinguishes an absent
bucket from a zero one, and `Snapshot` returns an independent copy so a
caller can export usage without risking the tracker's live state. Run
`go test -count=1 -race ./...` to confirm the bucket table, the limit
boundary, the aliasing contract, the collision contrast, and the
concurrency test.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — the exact rule for which types, including structs, are valid map keys.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — struct equality comparing corresponding fields.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the independent copy `Snapshot` returns.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — checking a wrapped sentinel error like `ErrQuotaExceeded`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-blocklist-membership-scanner.md](18-blocklist-membership-scanner.md) | Next: [20-config-fingerprint-etag.md](20-config-fingerprint-etag.md)
