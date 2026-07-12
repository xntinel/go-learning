# Exercise 2: Pointer vs Value Receivers and Error Identity Hazards

The receiver you put on `Error()` silently decides identity, comparability, and
whether `errors.As` finds your type. This module builds the same domain error
twice — once value-receiver, once pointer-receiver — and proves empirically how
they differ, then builds the "one type, many categories" `DomainError` that a real
service uses everywhere.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
erridentity/               independent module: example.com/erridentity
  go.mod                   go 1.24
  erridentity.go           valErr (value recv), ptrErr (ptr recv), DomainError+Kind
  cmd/
    demo/
      main.go              shows As success on *ptrErr and Kind-based matching
  erridentity_test.go      As pointer/value cases, ==, Is-by-Kind, map-key subtest
```

Files: `erridentity.go`, `cmd/demo/main.go`, `erridentity_test.go`.
Implement: `valErr` with a value receiver, `ptrErr` with a pointer receiver, and a `DomainError{Kind, Msg}` whose `Is` matches by `Kind`.
Test: `errors.As` into `*ptrErr` succeeds; two equal-field pointer instances are not `==` but do match via `Is`; a value sentinel vs pointer sentinel as a map key.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Value receiver vs pointer receiver, precisely

Give `Error()` a *value* receiver — `func (e valErr) Error() string` — and both
`valErr` and `*valErr` satisfy `error` (a pointer's method set includes its value
methods). Give it a *pointer* receiver — `func (e *ptrErr) Error() string` — and
only `*ptrErr` satisfies `error`; the bare value `ptrErr` does not. That asymmetry
drives everything downstream.

For `errors.As`, the target's element type must match the concrete type actually
in the chain. If you construct and wrap `&ptrErr{...}`, the chain holds a `*ptrErr`,
and `errors.As(err, &target)` with `target` of type `*ptrErr` matches. This is the
normal, safe case. The trap is on the value side: if you wrap `&valErr{...}` (a
pointer, because that is what you had a reference to) but then ask
`errors.As(err, &v)` with `v` of type `valErr` (the value), the concrete type in
the chain is `*valErr`, the target element type is `valErr`, they differ, and the
match *silently fails* — no panic, no compile error, just `false`. The value
receiver is exactly what lets both spellings compile, which is what makes the miss
silent. Pick the pointer receiver for struct errors so there is only one spelling
that satisfies `error` and only one type to target.

### Identity: two equal instances are not ==

`&DomainError{Kind: KindNotFound}` and a second `&DomainError{Kind: KindNotFound}`
are two distinct allocations, so `==` on the pointers is `false` even though their
fields are equal. This is why you never write `if err == someExpectedError` for a
constructed error — you would be comparing pointer identity, which is almost never
what you mean. The right question is "is this a not-found error?", answered by
`errors.Is` plus a custom `Is` that compares `Kind`.

### One type, many categories: the DomainError pattern

Rather than a separate struct per failure mode, a service commonly uses a single
`DomainError` with a `Kind` enum field. `Is` matches by `Kind`, so package-level
`Kind` sentinels (`ErrNotFound = &DomainError{Kind: KindNotFound}`) give you
`errors.Is(err, ErrNotFound)` category checks across the whole codebase from one
type. This keeps the error surface small and the matching uniform, and it is the
shape modules 3, 7, and 9 reuse.

Create `erridentity.go`:

```go
// Package erridentity demonstrates how the Error() receiver choice affects
// identity, comparability, and errors.As, and shows the one-type-many-categories
// DomainError pattern used across a service.
package erridentity

import "fmt"

// valErr has a VALUE receiver: both valErr and *valErr satisfy error. This is
// what makes a pointer/value errors.As mismatch a SILENT no-match.
type valErr struct{ msg string }

func (e valErr) Error() string { return e.msg }

// ptrErr has a POINTER receiver: only *ptrErr satisfies error. There is exactly
// one spelling to target with errors.As.
type ptrErr struct{ msg string }

func (e *ptrErr) Error() string { return e.msg }

// Kind is a machine-readable error category. One DomainError type carries any of
// these, so matching is uniform across the service.
type Kind int

const (
	KindUnknown Kind = iota
	KindNotFound
	KindConflict
	KindDenied
)

func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindDenied:
		return "denied"
	default:
		return "unknown"
	}
}

// DomainError is the single domain error type. Kind is the category; Msg is the
// human detail. Different failure modes are different Kinds, not different types.
type DomainError struct {
	Kind Kind
	Msg  string
}

func (e *DomainError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

// Is matches by Kind, so errors.Is(err, ErrNotFound) is true for ANY not-found
// DomainError regardless of Msg or pointer identity. It matches on Kind ONLY.
func (e *DomainError) Is(target error) bool {
	t, ok := target.(*DomainError)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

// Package-level category sentinels built from the one type.
var (
	ErrNotFound = &DomainError{Kind: KindNotFound}
	ErrConflict = &DomainError{Kind: KindConflict}
	ErrDenied   = &DomainError{Kind: KindDenied}
)

// NewValErr and NewPtrErr construct the two receiver-style errors as error values
// wrapping a *T, which is the realistic case (you hold a reference).
func NewValErr(msg string) error { return &valErr{msg: msg} }
func NewPtrErr(msg string) error { return &ptrErr{msg: msg} }
```

### The runnable demo

The demo shows the safe pointer-receiver `errors.As`, then the Kind-based match
that treats two distinct allocations as the same category.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/erridentity"
)

func main() {
	// Kind-based identity: a freshly constructed not-found error matches the
	// ErrNotFound sentinel by Kind, though it is a different allocation.
	fresh := &erridentity.DomainError{Kind: erridentity.KindNotFound, Msg: "user 42"}
	fmt.Printf("fresh == sentinel: %v\n", fresh == erridentity.ErrNotFound)
	fmt.Printf("errors.Is not-found: %v\n", errors.Is(fresh, erridentity.ErrNotFound))
	fmt.Printf("errors.Is conflict: %v\n", errors.Is(fresh, erridentity.ErrConflict))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fresh == sentinel: false
errors.Is not-found: true
errors.Is conflict: false
```

### Tests

`TestAsPointerReceiver` proves `errors.As` into `*ptrErr` works. `TestAsValueSilentMiss`
locks in the silent no-match: the chain holds `*valErr`, so asking for the value
type `valErr` returns false while the pointer type matches. `TestIdentityVsIs`
proves two equal-field pointers are not `==` but do match via `Is`.
`TestSentinelMapKeys` shows value sentinels collapse as map keys by value while
pointer sentinels stay distinct by identity.

Create `erridentity_test.go`:

```go
package erridentity

import (
	"errors"
	"fmt"
	"testing"
)

func TestAsPointerReceiver(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrap: %w", NewPtrErr("boom"))

	var pe *ptrErr
	if !errors.As(err, &pe) {
		t.Fatal("errors.As into *ptrErr should match a wrapped *ptrErr")
	}
	if pe.msg != "boom" {
		t.Errorf("pe.msg = %q; want boom", pe.msg)
	}
}

func TestAsValueSilentMiss(t *testing.T) {
	t.Parallel()
	// The chain holds *valErr (NewValErr wraps &valErr).
	err := fmt.Errorf("wrap: %w", NewValErr("boom"))

	var v valErr
	if errors.As(err, &v) {
		t.Error("errors.As into value valErr unexpectedly matched a *valErr chain")
	}
	var pv *valErr
	if !errors.As(err, &pv) {
		t.Error("errors.As into *valErr should match the wrapped pointer")
	}
}

func TestIdentityVsIs(t *testing.T) {
	t.Parallel()
	a := &DomainError{Kind: KindNotFound, Msg: "user 1"}
	b := &DomainError{Kind: KindNotFound, Msg: "user 2"}

	if a == b {
		t.Error("two distinct allocations must not be ==")
	}
	if !errors.Is(a, b) {
		t.Error("same-Kind DomainErrors should match via custom Is")
	}
	if errors.Is(a, ErrConflict) {
		t.Error("not-found must not match the conflict sentinel")
	}
}

func TestSentinelMapKeys(t *testing.T) {
	t.Parallel()

	// Value keys with equal fields collapse to one entry.
	type vkey struct{ code int }
	vm := map[vkey]string{}
	vm[vkey{code: 1}] = "first"
	vm[vkey{code: 1}] = "second"
	if len(vm) != 1 {
		t.Errorf("value keys: len = %d; want 1 (equal values collapse)", len(vm))
	}

	// Pointer keys are distinct by identity even with equal pointee fields.
	pm := map[*DomainError]string{}
	pm[&DomainError{Kind: KindNotFound}] = "first"
	pm[&DomainError{Kind: KindNotFound}] = "second"
	if len(pm) != 2 {
		t.Errorf("pointer keys: len = %d; want 2 (distinct identities)", len(pm))
	}
}

func ExampleDomainError_Is() {
	fresh := &DomainError{Kind: KindNotFound, Msg: "user 42"}
	fmt.Println(errors.Is(fresh, ErrNotFound), fresh == ErrNotFound)
	// Output: true false
}
```

## Review

The lesson is the receiver decision. A pointer receiver on `Error()` means exactly
one type (`*T`) satisfies `error`, so `errors.As` has one unambiguous target and
there is no silent value/pointer miss — `TestAsValueSilentMiss` shows what goes
wrong when you forget this. Constructed errors are never compared with `==`
(`TestIdentityVsIs`); category matching goes through `errors.Is` and a narrow
custom `Is` on `Kind`. The map-key subtest is the comparability corollary: value
errors deduplicate by value, pointer errors by identity, so a pointer sentinel is
a stable unique key and a value error is not. Run `go test -race` to confirm all
four behaviors hold.

## Resources

- [Go Spec: Method sets](https://go.dev/ref/spec#Method_sets) — why a pointer receiver excludes the value from `error`.
- [errors package](https://pkg.go.dev/errors) — `As` semantics and the custom `Is` contract.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — when to use pointer vs value receivers.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-validationerror-structured-type.md](01-validationerror-structured-type.md) | Next: [03-repository-error-translation.md](03-repository-error-translation.md)
