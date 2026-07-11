# Exercise 4: RBAC Permission Bitmask With Typed Flags

A permission set is the classic bitmask: read, write, delete, admin, packed into
one integer. This module builds it with `iota` shift constants folded at compile
time, but wraps them in a defined `Perm` type so the type is part of the API
contract — callers can combine flags, but cannot pass an arbitrary integer where a
`Perm` is required.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
authzperm/                    independent module: example.com/authzperm
  go.mod                      go 1.26
  perm.go                     type Perm uint32; PermRead..PermAdmin via 1<<iota; Has, Grant,
                              Revoke, String; PermAll
  cmd/
    demo/
      main.go                 grants, revokes, prints a permission set
  perm_test.go                grant/has/revoke behavior; String order; PermAll identity
```

Files: `perm.go`, `cmd/demo/main.go`, `perm_test.go`.
Implement: `type Perm uint32`, flags via `1 << iota`, `Has`, `Grant`, `Revoke`, a
`fmt.Stringer` rendering `"read|write"`, and `PermAll`.
Test: `Grant` then `Has` is true; `Revoke` clears only the target bit; `String`
lists set flags in a stable order; `PermAll` equals the OR of all flags.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/authzperm/cmd/demo
cd ~/go-exercises/authzperm
go mod init example.com/authzperm
```

### iota shift constants, typed by a defined type

`PermRead Perm = 1 << iota` sets the first flag to `1 << 0 = 1` with type `Perm`.
The following lines in the block repeat that expression implicitly with `iota`
advancing, so `PermWrite` is `1 << 1 = 2`, `PermDelete` is `4`, `PermAdmin` is `8`
— all of type `Perm`. The shift arithmetic is untyped integer math folded entirely
at compile time; the `Perm` wrapper is what turns those numbers into a contract.
Because `Perm` is a defined type, a function `func Grant(p, add Perm) Perm` will not
accept a bare `uint32` or `int` — the caller must pass a `Perm`. That is the whole
point of the defined type: it resists the "pass any integer as a permission" bug
that a bare `uint32` alias would permit. This constraint is documented here, not
executed, because its value is that the offending call never compiles.

`PermAll` is the OR of every flag, itself a compile-time constant. `Has` tests
`p&flag == flag` (so a multi-bit query like `Has(PermRead|PermWrite)` requires
*both*), `Grant` ORs bits in, and `Revoke` uses AND-NOT (`&^`) to clear exactly the
requested bits and nothing else. `String` walks the flags in a fixed declaration
order and joins the set names with `|`, giving stable, testable output.

Create `perm.go`:

```go
package authzperm

import "strings"

// Perm is a set of RBAC permission flags. The defined type is the contract: a
// bare uint32 cannot be passed where a Perm is expected.
type Perm uint32

const (
	PermRead Perm = 1 << iota
	PermWrite
	PermDelete
	PermAdmin
)

// PermAll is every flag OR'd together, folded at compile time.
const PermAll = PermRead | PermWrite | PermDelete | PermAdmin

// flagNames lists flags in stable rendering order.
var flagNames = []struct {
	bit  Perm
	name string
}{
	{PermRead, "read"},
	{PermWrite, "write"},
	{PermDelete, "delete"},
	{PermAdmin, "admin"},
}

// Has reports whether p contains every bit in flag.
func (p Perm) Has(flag Perm) bool {
	return p&flag == flag
}

// Grant returns p with the bits in add set.
func (p Perm) Grant(add Perm) Perm {
	return p | add
}

// Revoke returns p with the bits in remove cleared, leaving all others intact.
func (p Perm) Revoke(remove Perm) Perm {
	return p &^ remove
}

// String renders the set flags joined by "|", or "none" when empty.
func (p Perm) String() string {
	if p == 0 {
		return "none"
	}
	var parts []string
	for _, f := range flagNames {
		if p&f.bit != 0 {
			parts = append(parts, f.name)
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "|")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/authzperm"
)

func main() {
	var p authzperm.Perm
	p = p.Grant(authzperm.PermRead | authzperm.PermWrite)
	fmt.Printf("after grant: %s\n", p)

	fmt.Printf("has write: %v\n", p.Has(authzperm.PermWrite))
	fmt.Printf("has admin: %v\n", p.Has(authzperm.PermAdmin))

	p = p.Revoke(authzperm.PermWrite)
	fmt.Printf("after revoke write: %s\n", p)

	fmt.Printf("all: %s\n", authzperm.PermAll)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after grant: read|write
has write: true
has admin: false
after revoke write: read
all: read|write|delete|admin
```

### Tests

The tests drive behavior: `TestGrantHasRevoke` grants two bits, asserts `Has`, then
revokes one and asserts only that bit cleared. `TestString` checks the stable order
and the empty case. `TestPermAllIdentity` proves `PermAll` equals the OR of the
individual flags and that `Has(PermAll)` on a full set is true.

Create `perm_test.go`:

```go
package authzperm

import "testing"

func TestGrantHasRevoke(t *testing.T) {
	t.Parallel()

	var p Perm
	p = p.Grant(PermRead | PermWrite)

	if !p.Has(PermRead) || !p.Has(PermWrite) {
		t.Fatalf("after grant, Has(read)=%v Has(write)=%v", p.Has(PermRead), p.Has(PermWrite))
	}
	if p.Has(PermDelete) {
		t.Fatal("delete was never granted")
	}
	if !p.Has(PermRead | PermWrite) {
		t.Fatal("multi-bit Has should require all bits")
	}

	p = p.Revoke(PermWrite)
	if p.Has(PermWrite) {
		t.Fatal("write should be cleared after Revoke")
	}
	if !p.Has(PermRead) {
		t.Fatal("Revoke(write) must not clear read")
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		p    Perm
		want string
	}{
		{0, "none"},
		{PermRead, "read"},
		{PermRead | PermWrite, "read|write"},
		{PermAll, "read|write|delete|admin"},
		{PermWrite | PermAdmin, "write|admin"},
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("Perm(%d).String() = %q, want %q", uint32(tt.p), got, tt.want)
		}
	}
}

func TestPermAllIdentity(t *testing.T) {
	t.Parallel()

	want := PermRead | PermWrite | PermDelete | PermAdmin
	if PermAll != want {
		t.Fatalf("PermAll = %d, want %d", uint32(PermAll), uint32(want))
	}
	if !PermAll.Has(PermRead | PermAdmin) {
		t.Fatal("PermAll must contain every flag")
	}
}
```

## Review

The bitmask is correct when `Has` requires all queried bits, `Grant` is idempotent
OR, and `Revoke` uses AND-NOT so it clears only the targeted bits — a plain
subtraction or XOR would misbehave on bits that were not set. The defined `Perm`
type is the lesson: the flag values are untyped `1 << iota` arithmetic folded at
compile time, but the type wrapper is what stops a caller from smuggling an
arbitrary integer in as a permission. `String`'s fixed `flagNames` order is what
makes the output testable; relying on map iteration there would produce
nondeterministic strings.

## Resources

- [Go Language Specification: Iota](https://go.dev/ref/spec#Iota) — how iota numbers const blocks.
- [Effective Go: Constants](https://go.dev/doc/effective_go#constants) — iota and the ByteSize example this pattern descends from.
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer) — the interface `String` satisfies.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-byte-unit-ladder-si-vs-binary.md](03-byte-unit-ladder-si-vs-binary.md) | Next: [05-overflow-guardrails-math-consts.md](05-overflow-guardrails-math-consts.md)
