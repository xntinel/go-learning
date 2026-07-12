# Exercise 4: Build an RBAC Permission Set with Bitmask Operations

Authorization middleware needs to answer "does this principal hold all of these
permissions?" fast and allocation-free. A bitmask makes each permission a single
bit, so a whole permission set is one `uint16` and every check is a bitwise op.
This module builds the set operations on top of a `1 << iota` permission enum:
`Grant`, `Revoke`, `HasAll`, `HasAny`, `Count`, and compile-time composites.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
rbac/                           module: example.com/rbac
  go.mod                        go 1.26
  rbac.go                       Permission bitmask + set operations, composites
  cmd/
    demo/
      main.go                   grant, revoke, and inspect a permission set
  rbac_test.go                  grant/revoke transitions, Count, distinct-power-of-two
```

Files: `rbac.go`, `cmd/demo/main.go`, `rbac_test.go`.
Implement: `Grant`, `Revoke`, `HasAll`, `HasAny`, `Count`, and composite constants `PermissionReadWrite` and `PermissionAll`.
Test: grant/revoke transitions; `Revoke` uses `&^` and leaves unrelated bits; `Count` equals `bits.OnesCount16`; every single permission is a distinct power of two.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/04-constants-and-iota/04-rbac-bitmask-permission-set/cmd/demo
cd go-solutions/02-variables-types-and-constants/04-constants-and-iota/04-rbac-bitmask-permission-set
```

## Why a set is powers of two, and what the operations are

A permission set is fundamentally different from a state enum. A state is one
value at a time; a permission set is *many at once* — a principal may hold
`Read` and `Write` but not `Admin`. To represent an arbitrary combination in a
single integer, each permission must own a distinct bit, which is exactly what
`1 << iota` produces: `Read = 1` (bit 0), `Write = 2` (bit 1), `Admin = 4`
(bit 2). Because the bits do not overlap, any subset is a unique OR of the
members, and the empty set is `0`.

The set operations are the bitwise ones, and each has a precise meaning:

- Union (grant): `set | p` turns on the bits in `p`, idempotently.
- Membership of all: `set & required == required` is true only when every
  required bit is present — this is the authorization check.
- Membership of any: `set & required != 0` is true when at least one is present.
- Difference (revoke): `set &^ p` (AND-NOT) clears the bits in `p` and touches
  nothing else. This is the idiom to prefer over `set & ^p` (whose complement's
  width matters on an unsigned type) and over subtraction (which underflows if
  the bit was already clear).
- Cardinality: `math/bits.OnesCount16` counts the set bits — the number of
  granted permissions — with a single hardware popcount where available.

Composite masks are built at compile time from the members:
`PermissionReadWrite = PermissionRead | PermissionWrite` and `PermissionAll =
PermissionRead | PermissionWrite | PermissionAdmin`. These are ordinary const
expressions; they cost nothing at runtime and read as intent at the call site.

Contrast this with a state enum, where combining `iota` values with `|` is
meaningless: `StateQueued(1) | StateRunning(2)` is `3`, which is no state at all.
The bitmask is legitimate *only* because every value is an independent power of
two — a property the tests assert directly.

Create `rbac.go`:

```go
package rbac

import "math/bits"

// Permission is a single RBAC capability. Each is a distinct power of two, so a
// permission set is any bitwise-OR combination.
type Permission uint16

const (
	PermissionRead Permission = 1 << iota
	PermissionWrite
	PermissionAdmin
)

// Composite masks built at compile time from the individual permissions.
const (
	PermissionReadWrite = PermissionRead | PermissionWrite
	PermissionAll       = PermissionRead | PermissionWrite | PermissionAdmin
)

// Grant returns set with p's bits turned on (union).
func Grant(set, p Permission) Permission { return set | p }

// Revoke returns set with p's bits turned off, leaving unrelated bits intact.
func Revoke(set, p Permission) Permission { return set &^ p }

// HasAll reports whether every bit in required is present in set.
func HasAll(set, required Permission) bool { return set&required == required }

// HasAny reports whether at least one bit in required is present in set.
func HasAny(set, required Permission) bool { return set&required != 0 }

// Count returns the number of permissions granted in the set.
func Count(set Permission) int { return bits.OnesCount16(uint16(set)) }
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rbac"
)

func main() {
	set := rbac.PermissionReadWrite
	fmt.Printf("start: count=%d admin=%v\n", rbac.Count(set), rbac.HasAll(set, rbac.PermissionAdmin))

	set = rbac.Grant(set, rbac.PermissionAdmin)
	fmt.Printf("after grant admin: count=%d all=%v\n", rbac.Count(set), rbac.HasAll(set, rbac.PermissionAll))

	set = rbac.Revoke(set, rbac.PermissionWrite)
	fmt.Printf("after revoke write: read=%v write=%v admin=%v\n",
		rbac.HasAll(set, rbac.PermissionRead),
		rbac.HasAll(set, rbac.PermissionWrite),
		rbac.HasAll(set, rbac.PermissionAdmin))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: count=2 admin=false
after grant admin: count=3 all=true
after revoke write: read=true write=false admin=true
```

## Tests

`TestGrantRevoke` walks the transitions and checks `Revoke` clears only the named
bit. `TestHasAllHasAny` covers the empty set and partial matches. `TestCount`
ties `Count` to `bits.OnesCount16`. `TestDistinctPowersOfTwo` loops the members
and asserts each has exactly one bit set and none overlap — the invariant that
makes the whole scheme valid.

Create `rbac_test.go`:

```go
package rbac

import (
	"math/bits"
	"testing"
)

func TestGrantRevoke(t *testing.T) {
	t.Parallel()

	var set Permission // empty
	if HasAny(set, PermissionAll) {
		t.Fatal("empty set should hold no permissions")
	}

	set = Grant(set, PermissionRead)
	set = Grant(set, PermissionAdmin)
	if !HasAll(set, PermissionRead) || !HasAll(set, PermissionAdmin) {
		t.Fatal("granted read+admin not both present")
	}
	if HasAll(set, PermissionWrite) {
		t.Fatal("write was never granted")
	}

	set = Revoke(set, PermissionAdmin)
	if HasAll(set, PermissionAdmin) {
		t.Fatal("admin should be revoked")
	}
	if !HasAll(set, PermissionRead) {
		t.Fatal("revoking admin must not clear read")
	}
}

func TestHasAllHasAny(t *testing.T) {
	t.Parallel()

	set := PermissionReadWrite
	if !HasAll(set, PermissionReadWrite) {
		t.Fatal("read+write set should HasAll(read+write)")
	}
	if HasAll(set, PermissionAll) {
		t.Fatal("read+write set should not HasAll(all)")
	}
	if !HasAny(set, PermissionAll) {
		t.Fatal("read+write set should HasAny(all)")
	}
	if !HasAll(PermissionAll, PermissionRead) {
		t.Fatal("PermissionAll should include read")
	}
}

func TestCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		set  Permission
		want int
	}{
		{0, 0},
		{PermissionRead, 1},
		{PermissionReadWrite, 2},
		{PermissionAll, 3},
	}
	for _, tt := range tests {
		if got := Count(tt.set); got != tt.want {
			t.Fatalf("Count(%b) = %d, want %d", tt.set, got, tt.want)
		}
		if got := Count(tt.set); got != bits.OnesCount16(uint16(tt.set)) {
			t.Fatalf("Count disagrees with OnesCount16 for %b", tt.set)
		}
	}
}

func TestDistinctPowersOfTwo(t *testing.T) {
	t.Parallel()

	perms := []Permission{PermissionRead, PermissionWrite, PermissionAdmin}
	var seen Permission
	for _, p := range perms {
		if bits.OnesCount16(uint16(p)) != 1 {
			t.Fatalf("permission %b is not a single power of two", p)
		}
		if seen&p != 0 {
			t.Fatalf("permission %b overlaps a previous one", p)
		}
		seen |= p
	}
}
```

## Review

The set is correct when every permission is a distinct power of two, so any
combination round-trips through `Grant`/`Revoke` without disturbing unrelated
bits. The one operation to get right is `Revoke` with `&^`: subtraction or a
mis-sized complement would corrupt the set. `Count` should always agree with
`bits.OnesCount16` — if you ever find yourself counting bits by hand, use the
stdlib. And remember why this is a bitmask and a state is not: only independent,
non-overlapping bits may be combined; ORing enum ordinals is a category error.

## Resources

- [math/bits: OnesCount family](https://pkg.go.dev/math/bits#OnesCount16)
- [Go Specification: Arithmetic operators](https://go.dev/ref/spec#Arithmetic_operators)
- [Effective Go: Constants and iota](https://go.dev/doc/effective_go#constants)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-sql-enum-valuer-scanner-persistence.md](03-sql-enum-valuer-scanner-persistence.md) | Next: [05-byte-size-limit-constants.md](05-byte-size-limit-constants.md)
