# Exercise 12: Tenant Rate-Limit Override Resolver With Explicit-Zero Semantics

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An API gateway enforces one request-rate limit per tenant, most of them at a
shared global default, with the occasional operator override: raise a large
customer's ceiling, or -- when a tenant is abusing the API or an incident is
in progress -- slam it to zero as an emergency kill switch. That override
table is a small `map[string]int`, and resolving a tenant's effective limit
looks, at first glance, like the simplest possible map read: look the tenant
up, fall back to the default if it isn't there. The trap is that "isn't
there" and "is there, and set to zero" are indistinguishable through a bare
map read, because both produce the zero value.

A bare read, `if v := overrides[tenant]; v != 0 { return v }; return def`,
treats a stored zero exactly like an absent key: it falls through to the
default. That is precisely backwards for a kill switch. An operator sets a
malicious or runaway tenant's limit to zero, believing the gateway will now
reject every one of its requests, and instead the tenant keeps sailing
through at the global default -- because the map read that was supposed to
enforce the block cannot tell "explicitly blocked" from "never configured".
The bug is invisible in code review, because the line reads as a reasonable
default-fallback idiom, and it is invisible in the common test suite too,
because most tests exercise a *non-zero* override and never think to check
what happens when the override itself is the interesting value.

The two-result map read, `v, ok := overrides[tenant]`, is the only way to
recover the distinction: `ok` is false exactly when the tenant has never been
configured, regardless of what value would otherwise be stored. This module
builds that as a package: a `Resolver` that lets an operator set, clear, and
query per-tenant overrides, rejects a negative limit outright (it has no
meaning), and treats a stored zero as the deliberate block it is.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
limitconfig/             module example.com/limitconfig
  go.mod                 go 1.24
  limitconfig.go         Resolver; New, DefaultLimit, SetOverride, ClearOverride, Limit
  limitconfig_test.go    resolution table, the naive-fallback contrast, negative rejection,
                         clear-and-restore, concurrency, ExampleResolver_Limit
```

- Files: `limitconfig.go`, `limitconfig_test.go`.
- Implement: `New(defaultLimit int) *Resolver`, clamping a negative `defaultLimit` to zero; `(*Resolver) SetOverride(tenant string, limit int) error` returning `ErrNegativeLimit` for a negative `limit` and otherwise storing it, zero included; `(*Resolver) ClearOverride(tenant string)`, a no-op for an unconfigured tenant; `(*Resolver) Limit(tenant string) int`, resolving via comma-ok so a stored zero and an absent key are never confused.
- Test: an unconfigured tenant resolves to the default; a raised override wins; the unexported `limitNaive` contrast shows the bare-read version letting an explicitly blocked tenant through at the default, while `Limit` correctly returns zero; `SetOverride` rejects a negative limit with `errors.Is`, and the rejected value is never stored; `ClearOverride` restores the default, including after a kill switch, and is a no-op on an unconfigured tenant; `New` with a negative default clamps to zero; `Resolver` is safe for concurrent use; and `ExampleResolver_Limit` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/12-tenant-override-resolver
cd go-solutions/06-collections-arrays-slices-and-maps/14-custom-map-based-data-structure/12-tenant-override-resolver
go mod edit -go=1.24
```

### comma-ok is the only way a stored zero survives

`v := m[k]` returns the zero value both when `k` is absent and when `k` maps
to the zero value on purpose. For an `int` map that difference matters
exactly when zero is a value someone would legitimately store -- and a rate
limit of zero, meaning "block this tenant entirely", is exactly that case.
The version that looks reasonable and is wrong:

```go
// limitNaive — reads like a normal default-fallback, is backwards for a kill switch.
func limitNaive(overrides map[string]int, tenant string, def int) int {
    if v := overrides[tenant]; v != 0 {
        return v
    }
    return def       // a tenant explicitly set to 0 lands here too
}
```

An operator who calls `SetOverride("bad-actor", 0)` expects every subsequent
request from `bad-actor` to be rejected. `limitNaive` cannot deliver that: it
sees the stored `0`, treats it the same as "nothing stored", and returns
`def` -- the tenant keeps its full default quota during exactly the incident
the override was meant to stop. Nothing about the call site looks wrong; the
bug lives entirely in the fact that `int`'s zero value collides with a value
the domain treats as meaningful.

`v, ok := overrides[tenant]` breaks that collision apart: `ok` reports
whether the key exists, independent of what it maps to. `Limit` uses exactly
that form, so a stored `0` and an absent key take different branches even
though `overrides[tenant]` alone would return the identical `0` for both.

Create `limitconfig.go`:

```go
// Package limitconfig resolves the effective request-rate limit for a
// tenant of an API gateway: an operator-configured override if one exists,
// including an explicit zero as a kill switch, or the gateway's global
// default otherwise.
package limitconfig

import (
	"errors"
	"fmt"
	"sync"
)

// ErrNegativeLimit is returned by SetOverride when the requested limit is
// negative. A rate limit of zero is valid -- it blocks the tenant entirely
// -- but a negative one has no meaning and is refused rather than stored.
var ErrNegativeLimit = errors.New("limitconfig: limit must not be negative")

// Resolver resolves a tenant's effective rate limit against a global
// default and a set of per-tenant overrides.
//
// Resolver is safe for concurrent use by multiple goroutines: every method
// takes the internal lock for the duration of its map access, so gateway
// request handlers may call Limit concurrently with an operator calling
// SetOverride or ClearOverride.
type Resolver struct {
	mu        sync.RWMutex
	def       int
	overrides map[string]int
}

// New returns a Resolver whose global default is defaultLimit. A negative
// defaultLimit has no meaning for a rate limit and is treated as zero
// (fully blocked) rather than propagated as a negative limit that every
// caller of Limit would then have to guard against.
func New(defaultLimit int) *Resolver {
	if defaultLimit < 0 {
		defaultLimit = 0
	}
	return &Resolver{
		def:       defaultLimit,
		overrides: make(map[string]int),
	}
}

// DefaultLimit reports the global default this Resolver falls back to for
// a tenant with no override.
func (r *Resolver) DefaultLimit() int {
	return r.def
}

// SetOverride configures tenant's rate limit explicitly, replacing any
// previous override. A limit of zero is a valid, deliberate kill switch: it
// blocks the tenant while leaving every other tenant at the default.
// SetOverride returns ErrNegativeLimit, wrapped with the tenant and the
// rejected value, for a negative limit.
func (r *Resolver) SetOverride(tenant string, limit int) error {
	if limit < 0 {
		return fmt.Errorf("%w: tenant %q got %d", ErrNegativeLimit, tenant, limit)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[tenant] = limit
	return nil
}

// ClearOverride removes any configured override for tenant, so that Limit
// falls back to the global default for it again. Clearing a tenant with no
// override is a no-op, not an error.
func (r *Resolver) ClearOverride(tenant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.overrides, tenant)
}

// Limit returns tenant's effective rate limit: its configured override if
// one exists -- including an override explicitly set to zero -- or the
// global default if it does not. Limit distinguishes those two cases with
// a comma-ok map read; a bare map[tenant] read cannot, because it returns
// zero for both an override of zero and no override at all.
func (r *Resolver) Limit(tenant string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if v, ok := r.overrides[tenant]; ok {
		return v
	}
	return r.def
}
```

### Using it

Construct one `Resolver` per gateway process with `New(defaultLimit)` at
startup, then call `Limit(tenant)` on the hot path of every request and
`SetOverride`/`ClearOverride` from whatever admin surface an operator uses.
`Resolver` guards its override map with an internal `sync.RWMutex`, so
request-handling goroutines calling `Limit` concurrently with an operator's
`SetOverride` or `ClearOverride` never race -- that is the concurrency
contract the type's doc comment states and `TestResolverIsSafeForConcurrentUse`
holds it to. `SetOverride` never stores a rejected value: a negative limit
returns `ErrNegativeLimit` and leaves any prior override, or the absence of
one, untouched.

The `Example` below is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift from the code that actually
runs.

```go
func ExampleResolver_Limit() {
	r := New(100)

	fmt.Println("unconfigured tenant:", r.Limit("startup-inc"))

	if err := r.SetOverride("acme", 500); err != nil {
		panic(err)
	}
	fmt.Println("acme with a raised override:", r.Limit("acme"))

	if err := r.SetOverride("bad-actor", 0); err != nil {
		panic(err)
	}
	fmt.Println("bad-actor with the kill switch:", r.Limit("bad-actor"))

	r.ClearOverride("bad-actor")
	fmt.Println("bad-actor after clearing the kill switch:", r.Limit("bad-actor"))

	if err := r.SetOverride("acme", -10); err != nil {
		fmt.Println("rejected override:", err)
	}

	// Output:
	// unconfigured tenant: 100
	// acme with a raised override: 500
	// bad-actor with the kill switch: 0
	// bad-actor after clearing the kill switch: 100
	// rejected override: limitconfig: limit must not be negative: tenant "acme" got -10
}
```

### Tests

`TestLimitUnconfiguredTenantUsesDefault` and `TestLimitOverrideWins` pin the
ordinary paths. `TestExplicitZeroBlocksTenant` is the module's core test:
`limitNaive` is unexported and unreachable from the package API, and exists
only so the test can show, numerically, what it gets wrong -- given the
identical stored override of zero, `limitNaive` returns the default while
`Limit` correctly returns zero, and the test asserts the two disagree.
`TestSetOverrideRejectsNegative` checks the sentinel with `errors.Is` and
that a rejected call never mutates the stored state.
`TestClearOverrideRestoresDefault` and `TestClearOverrideAfterKillSwitch`
check that clearing an override -- including a kill switch -- restores the
default, and that clearing an unconfigured tenant is harmless.
`TestNewClampsNegativeDefault` pins the constructor's handling of a
nonsensical negative default. `TestResolverIsSafeForConcurrentUse` runs
twenty goroutines each setting, reading, and clearing their own tenant's
override under `-race`.

Create `limitconfig_test.go`:

```go
package limitconfig

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// limitNaive is the resolver as it is usually written the first time: a
// bare map read, falling back to the default whenever the stored value is
// the zero value. It is never exported and never reachable from the
// package API; it exists only so the tests can pin what it gets wrong --
// it cannot tell "tenant explicitly blocked" from "tenant never
// configured", because both read back as 0.
func limitNaive(overrides map[string]int, tenant string, def int) int {
	if v := overrides[tenant]; v != 0 {
		return v
	}
	return def
}

func TestLimitUnconfiguredTenantUsesDefault(t *testing.T) {
	t.Parallel()

	r := New(100)
	if got := r.Limit("unknown-tenant"); got != 100 {
		t.Fatalf("Limit(unconfigured) = %d, want 100 (the default)", got)
	}
}

func TestLimitOverrideWins(t *testing.T) {
	t.Parallel()

	r := New(100)
	if err := r.SetOverride("acme", 500); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got := r.Limit("acme"); got != 500 {
		t.Fatalf("Limit(acme) = %d, want 500", got)
	}
	if got := r.Limit("other-tenant"); got != 100 {
		t.Fatalf("Limit(other-tenant) = %d, want 100 (unaffected by acme's override)", got)
	}
}

// TestExplicitZeroBlocksTenant is the heart of the module. A tenant that an
// operator has explicitly set to zero -- the kill switch -- must resolve to
// zero, not to the default. limitNaive, the map-read-with-a-nonzero-check
// version, gets this exactly backwards: it treats the stored zero as "no
// override" and lets the blocked tenant through at the default limit.
func TestExplicitZeroBlocksTenant(t *testing.T) {
	t.Parallel()

	r := New(100)
	if err := r.SetOverride("blocked-tenant", 0); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	got := r.Limit("blocked-tenant")
	if got != 0 {
		t.Fatalf("Limit(blocked-tenant) = %d, want 0 (the kill switch)", got)
	}

	// The naive version, given the identical stored override, lets the
	// blocked tenant through at the default -- the incident this module
	// exists to prevent.
	naiveOverrides := map[string]int{"blocked-tenant": 0}
	naive := limitNaive(naiveOverrides, "blocked-tenant", 100)
	if naive != 100 {
		t.Fatalf("limitNaive(blocked-tenant) = %d, want 100 (demonstrating the bug)", naive)
	}
	if naive == got {
		t.Fatal("limitNaive agreed with Limit; the antipattern contrast did not reproduce the bug")
	}
}

func TestSetOverrideRejectsNegative(t *testing.T) {
	t.Parallel()

	r := New(100)
	err := r.SetOverride("acme", -1)
	if !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("SetOverride(-1) error = %v, want ErrNegativeLimit", err)
	}
	// A rejected override must not be stored; the tenant still resolves to
	// the default.
	if got := r.Limit("acme"); got != 100 {
		t.Fatalf("Limit(acme) after rejected override = %d, want 100", got)
	}
}

func TestClearOverrideRestoresDefault(t *testing.T) {
	t.Parallel()

	r := New(100)
	if err := r.SetOverride("acme", 500); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	r.ClearOverride("acme")
	if got := r.Limit("acme"); got != 100 {
		t.Fatalf("Limit(acme) after ClearOverride = %d, want 100", got)
	}

	// Clearing a tenant that was never configured is a harmless no-op.
	r.ClearOverride("never-configured")
	if got := r.Limit("never-configured"); got != 100 {
		t.Fatalf("Limit(never-configured) = %d, want 100", got)
	}
}

func TestClearOverrideAfterKillSwitch(t *testing.T) {
	t.Parallel()

	r := New(100)
	if err := r.SetOverride("acme", 0); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got := r.Limit("acme"); got != 0 {
		t.Fatalf("Limit(acme) = %d, want 0 before clearing", got)
	}
	r.ClearOverride("acme")
	if got := r.Limit("acme"); got != 100 {
		t.Fatalf("Limit(acme) = %d, want 100 after clearing the kill switch", got)
	}
}

func TestNewClampsNegativeDefault(t *testing.T) {
	t.Parallel()

	r := New(-5)
	if got := r.DefaultLimit(); got != 0 {
		t.Fatalf("DefaultLimit() = %d, want 0 for a negative constructor argument", got)
	}
	if got := r.Limit("any-tenant"); got != 0 {
		t.Fatalf("Limit(any-tenant) = %d, want 0", got)
	}
}

func TestResolverIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r := New(100)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", i)
			if err := r.SetOverride(tenant, i*10); err != nil {
				t.Errorf("SetOverride: %v", err)
				return
			}
			if got := r.Limit(tenant); got != i*10 {
				t.Errorf("Limit(%s) = %d, want %d", tenant, got, i*10)
			}
			r.ClearOverride(tenant)
		}(i)
	}
	wg.Wait()
}

// ExampleResolver_Limit is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment
// below.
func ExampleResolver_Limit() {
	r := New(100)

	fmt.Println("unconfigured tenant:", r.Limit("startup-inc"))

	if err := r.SetOverride("acme", 500); err != nil {
		panic(err)
	}
	fmt.Println("acme with a raised override:", r.Limit("acme"))

	if err := r.SetOverride("bad-actor", 0); err != nil {
		panic(err)
	}
	fmt.Println("bad-actor with the kill switch:", r.Limit("bad-actor"))

	r.ClearOverride("bad-actor")
	fmt.Println("bad-actor after clearing the kill switch:", r.Limit("bad-actor"))

	if err := r.SetOverride("acme", -10); err != nil {
		fmt.Println("rejected override:", err)
	}

	// Output:
	// unconfigured tenant: 100
	// acme with a raised override: 500
	// bad-actor with the kill switch: 0
	// bad-actor after clearing the kill switch: 100
	// rejected override: limitconfig: limit must not be negative: tenant "acme" got -10
}
```

## Review

`Limit` is correct when a tenant explicitly set to zero resolves to zero,
never to the default -- that is the entire reason the kill switch exists.
The trap is that a bare `overrides[tenant]` read cannot tell that case apart
from "never configured", because both return `int`'s zero value; `limitNaive`
demonstrates exactly that confusion, and `Limit`'s comma-ok read,
`v, ok := overrides[tenant]`, is what resolves it. Around that core,
`SetOverride` rejects a negative limit with `ErrNegativeLimit`, checkable
with `errors.Is`, and never stores the rejected value; `ClearOverride` is a
harmless no-op on a tenant with no override; and `New` treats a nonsensical
negative default as zero rather than propagating it. `Resolver` guards its
map with an internal `RWMutex`, so gateway request handlers and an
operator's admin calls can run concurrently without corrupting the override
table. Run `go test -count=1 -race ./...` to confirm the resolution table,
the kill-switch contrast, the rejection and clearing paths, and the
concurrent-use test.

## Resources

- [Go Spec: Index expressions](https://go.dev/ref/spec#Index_expressions) — the comma-ok form for map reads and why the second result is the only reliable presence check.
- [Effective Go: Maps](https://go.dev/doc/effective_go#maps) — the language's own explanation of the zero-value-vs-absent ambiguity.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — how callers should test `SetOverride`'s returned error against `ErrNegativeLimit`.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the lock `Resolver` uses to stay safe under concurrent reads and writes.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-deterministic-cache-key-signature.md](11-deterministic-cache-key-signature.md) | Next: [13-conn-registry-map-compaction.md](13-conn-registry-map-compaction.md)
