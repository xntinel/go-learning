# Exercise 31: Context Value Scoping — Restore After Request Boundary

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Sometimes a deep call stack needs to read an ambient value — which tenant
a request is acting on behalf of, say — without every function along the
way threading a `context.Context` parameter by hand. This module builds a
small `Scope` that carries exactly one such value, with a `WithTenant`
boundary that overrides it for the duration of a nested call and restores
the prior value afterward via a deferred closure — so the override never
leaks past the boundary that introduced it, even through nested boundaries
or a panic.

## What you'll build

```text
reqscope/                    independent module: example.com/reqscope
  go.mod
  reqscope/reqscope.go         Scope (mutex-guarded); WithTenant (snapshot + deferred restore)
  cmd/demo/main.go              nested boundaries; watch tenant restore on the way out
  reqscope/reqscope_test.go     single restore; nested restore stacks; restore survives a panic
```

- Files: `reqscope/reqscope.go`, `cmd/demo/main.go`, `reqscope/reqscope_test.go`.
- Implement: a `Scope` wrapping a mutex-guarded `tenantID string`, with `TenantID() string`; and `WithTenant(tenantID string, fn func())`, which snapshots the current tenant ID, overrides it, defers a closure that restores the snapshot, and then calls `fn`.
- Test: a single `WithTenant` call restores the prior tenant after it returns; nested `WithTenant` calls compose like a stack — the inner boundary restores to whatever the outer one had set, and the outer boundary restores to the original; a panic inside `fn` still restores the prior tenant before the panic propagates.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Restoring what a real context.Context restores for free

`context.Context` values are already immutable — calling `WithValue`
creates a brand-new context carrying the extra value, and the original
context, still held by every caller further up the stack, is completely
untouched. Nothing needs to be "restored" there; the old value was never
overwritten in the first place. That immutability is precisely why real
production code should almost always prefer threading a `context.Context`
explicitly over what this exercise builds.

`Scope` exists for the messier, very real case where a value needs to be
read from deep inside a call stack that was not written to accept a
`context.Context` parameter at every level — instrumentation, logging
helpers, or legacy code being incrementally migrated. Because `Scope`
holds its tenant ID in a genuinely *mutable* field, `WithTenant` has to do
by hand what an immutable context gets automatically: snapshot `prev :=
s.tenantID` before overwriting it, and defer a closure that writes `prev`
back. Nested calls compose correctly for the same reason a stack of
defers always does — the inner `WithTenant`'s restore runs first (it was
registered last), putting the tenant back to whatever the outer
`WithTenant` had set, and only then does the outer restore run, putting it
back to the original. A panic inside `fn` does not skip this: Go still
runs a deferred function as a panic unwinds past it, so the restore fires
before the panic keeps propagating outward.

Create `reqscope/reqscope.go`:

```go
package reqscope

import "sync"

// Scope carries request-scoped ambient values -- e.g. the tenant a request
// is acting on behalf of -- that deeply-nested helper calls can read without
// every function threading a context.Context parameter through by hand. A
// single Scope is normally owned by one request at a time; it is
// mutex-guarded so reads and writes are individually race-free.
type Scope struct {
	mu       sync.Mutex
	tenantID string
}

// New returns a Scope seeded with the given tenant ID.
func New(tenantID string) *Scope {
	return &Scope{tenantID: tenantID}
}

// TenantID returns the scope's current tenant ID.
func (s *Scope) TenantID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantID
}

// WithTenant temporarily overrides the scope's tenant ID to tenantID for the
// duration of fn. It snapshots the prior value before overriding, and
// restores it via a deferred closure -- so the override is undone on every
// exit path from fn, including a panic, before WithTenant itself returns.
// Nested calls compose like a stack: entering "b" inside "a" and returning
// restores to "a", and returning from "a" restores to whatever was current
// before it.
func (s *Scope) WithTenant(tenantID string, fn func()) {
	s.mu.Lock()
	prev := s.tenantID
	s.tenantID = tenantID
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.tenantID = prev
		s.mu.Unlock()
	}()

	fn()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqscope/reqscope"
)

func main() {
	scope := reqscope.New("acme")
	fmt.Println("boundary start, tenant:", scope.TenantID())

	scope.WithTenant("beta-impersonation", func() {
		fmt.Println("inside boundary, tenant:", scope.TenantID())

		scope.WithTenant("gamma-nested", func() {
			fmt.Println("inside nested boundary, tenant:", scope.TenantID())
		})

		fmt.Println("after nested boundary, tenant:", scope.TenantID())
	})

	fmt.Println("after boundary, tenant:", scope.TenantID())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
boundary start, tenant: acme
inside boundary, tenant: beta-impersonation
inside nested boundary, tenant: gamma-nested
after nested boundary, tenant: beta-impersonation
after boundary, tenant: acme
```

### Tests

Create `reqscope/reqscope_test.go`:

```go
package reqscope

import "testing"

func TestWithTenantRestoresAfterBoundary(t *testing.T) {
	t.Parallel()

	s := New("acme")
	var seen string
	s.WithTenant("beta", func() { seen = s.TenantID() })

	if seen != "beta" {
		t.Fatalf("seen inside boundary = %q, want %q", seen, "beta")
	}
	if got := s.TenantID(); got != "acme" {
		t.Fatalf("TenantID() after boundary = %q, want %q", got, "acme")
	}
}

func TestWithTenantNestsLikeAStack(t *testing.T) {
	t.Parallel()

	s := New("acme")
	var duringOuter, duringInner, afterInner string

	s.WithTenant("beta", func() {
		duringOuter = s.TenantID()
		s.WithTenant("gamma", func() {
			duringInner = s.TenantID()
		})
		afterInner = s.TenantID()
	})

	cases := []struct {
		name, got, want string
	}{
		{"during outer", duringOuter, "beta"},
		{"during inner", duringInner, "gamma"},
		{"after inner, still in outer", afterInner, "beta"},
		{"after outer", s.TenantID(), "acme"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestWithTenantRestoresEvenOnPanic(t *testing.T) {
	t.Parallel()

	s := New("acme")

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		s.WithTenant("beta", func() {
			panic("boom mid-boundary")
		})
	}()

	if got := s.TenantID(); got != "acme" {
		t.Fatalf("TenantID() after panic = %q, want %q (restored)", got, "acme")
	}
}
```

## Review

`Scope` is correct when every `WithTenant` boundary restores exactly the
value it overrode, in the order nested boundaries actually nested, on every
exit path including a panic. The mistake this pattern exists to prevent is
restoring a *fixed* value (e.g. always resetting to the scope's original
construction-time tenant) instead of the value snapshotted at the top of
that specific `WithTenant` call — which breaks the very first time a
boundary is nested inside another, because the inner boundary would wipe
out the outer one's override instead of returning control to it. Snapshot
what was actually there immediately before overriding it, not what you
expect to have been there, and nesting composes correctly for free.

## Resources

- [context package](https://pkg.go.dev/context) — the immutable, idiomatic alternative this pattern deliberately trades away for ambient convenience.
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-write-both-sinks-rollback-all.md](30-write-both-sinks-rollback-all.md) | Next: [32-queue-item-requeue-on-error.md](32-queue-item-requeue-on-error.md)
