# Exercise 9: Idempotency Guard — When A Sentinel Is Not Enough

A sentinel answers "is this a duplicate?" — but a 409 response usually needs to
say *which* resource already exists so the client can find it. This exercise
builds an idempotency guard that returns a bare `ErrDuplicate` for the identity
check *and* a typed `*ConflictError{ExistingID, RequestKey}` for the data, with
the type implementing `Is` so a single value satisfies both `errors.Is` and
`errors.As`/`errors.AsType`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
idem/                         independent module: example.com/idem
  go.mod                      go 1.26
  idem.go                     ErrDuplicate/ErrValidation; *ConflictError with Is; Guard.Create
  cmd/
    demo/
      main.go                 first create, replay (409 with id), blank key (validation)
  idem_test.go                replay->Is+As+AsType, validation->not conflict, first->ok
```

- Files: `idem.go`, `cmd/demo/main.go`, `idem_test.go`.
- Implement: `Guard.Create(key, resourceID)` returning `ErrValidation` (bare sentinel) for a blank key and `*ConflictError` (typed, `Is`-linked to `ErrDuplicate`) for a replayed key.
- Test: a replay where `errors.Is(err, ErrDuplicate)` is true AND `errors.As`/`errors.AsType` yields the existing id; a validation error where `errors.As` returns false; a first create succeeding.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/idem/cmd/demo
cd ~/go-exercises/idem
go mod init example.com/idem
```

### The decision rule: identity vs data

An idempotency key lets a client safely retry a create: the second request with
the same key must not create a second resource. Detecting the replay is an
*identity* question — "have I seen this key?" — and a bare sentinel
`ErrDuplicate` answers it perfectly for a caller that only needs to know "this
was a duplicate, return the cached result".

But the HTTP layer usually needs more: to build a proper `409 Conflict` with a
`Location: /orders/{id}` header, it must know *which* resource the key already
created. A bare sentinel cannot carry that id; forcing the handler to parse it
out of the error string re-introduces exactly the string-fragility sentinels
exist to avoid. That is the signal to return a *typed* error —
`*ConflictError{ExistingID, RequestKey}` — matched with `errors.As` (or the Go
1.26 generic `errors.AsType[*ConflictError]`), which hands the handler the id
directly.

The two patterns coexist on one value. `*ConflictError` implements
`Is(target error) bool` returning `target == ErrDuplicate`, so the *same*
returned error satisfies `errors.Is(err, ErrDuplicate)` (for the identity-only
caller) and `errors.As(err, &conflict)` (for the data-needing caller). Neither
caller is forced to know about the other's needs. A different failure — a blank
key — is a plain `ErrValidation` with no data, so `errors.As(err, &conflict)`
correctly returns `false` and a validation error is never mistaken for a
conflict.

Create `idem.go`:

```go
package idem

import (
	"errors"
	"fmt"
)

var (
	// ErrDuplicate is the identity signal: this idempotency key was already used.
	ErrDuplicate = errors.New("duplicate request")
	// ErrValidation is a bare sentinel for a bad request (no data to carry).
	ErrValidation = errors.New("validation failed")
)

// ConflictError carries the data a 409 needs: the id of the resource the key
// already created, and the replayed key. It implements Is so errors.Is(err,
// ErrDuplicate) matches too, letting the sentinel and the typed error coexist.
type ConflictError struct {
	ExistingID string
	RequestKey string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("idempotency key %q already created resource %s", e.RequestKey, e.ExistingID)
}

// Is links this typed error to the ErrDuplicate sentinel. It is a shallow
// comparison and does not call Unwrap.
func (e *ConflictError) Is(target error) bool {
	return target == ErrDuplicate
}

// Guard enforces idempotency: one resource per key.
type Guard struct {
	seen map[string]string // idempotency key -> created resource id
}

func NewGuard() *Guard { return &Guard{seen: make(map[string]string)} }

// Create records the key or reports a conflict. A blank key is a validation
// error (bare sentinel: the caller needs only identity). A replayed key returns
// a *ConflictError (typed: the caller needs the existing id).
func (g *Guard) Create(key, resourceID string) error {
	if key == "" {
		return fmt.Errorf("create: %w", ErrValidation)
	}
	if existing, ok := g.seen[key]; ok {
		return fmt.Errorf("create: %w", &ConflictError{ExistingID: existing, RequestKey: key})
	}
	g.seen[key] = resourceID
	return nil
}
```

### The runnable demo

The demo creates a resource, replays the same key (recovering the existing id via
`errors.AsType` to build a `Location`), then sends a blank key (a validation
error that is *not* a conflict).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/idem"
)

func main() {
	g := idem.NewGuard()

	fmt.Println("first:", g.Create("order-abc", "order-1001"))

	err := g.Create("order-abc", "order-9999")
	fmt.Println("replay is duplicate:", errors.Is(err, idem.ErrDuplicate))
	if ce, ok := errors.AsType[*idem.ConflictError](err); ok {
		fmt.Printf("existing resource: %s (Location: /orders/%s)\n", ce.ExistingID, ce.ExistingID)
	}

	bad := g.Create("", "order-2")
	fmt.Println("blank key is validation:", errors.Is(bad, idem.ErrValidation))
	_, isConflict := errors.AsType[*idem.ConflictError](bad)
	fmt.Println("blank key is conflict:", isConflict)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first: <nil>
replay is duplicate: true
existing resource: order-1001 (Location: /orders/order-1001)
blank key is validation: true
blank key is conflict: false
```

### Tests

`TestReplayReturnsConflictWithData` proves one value satisfies both patterns:
`errors.Is(err, ErrDuplicate)` for identity, and `errors.As` /
`errors.AsType[*ConflictError]` for the existing id.
`TestValidationIsNotConflict` proves a validation error is neither a duplicate
nor extractable as a conflict.

Create `idem_test.go`:

```go
package idem

import (
	"errors"
	"fmt"
	"testing"
)

func TestReplayReturnsConflictWithData(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	if err := g.Create("key-1", "res-100"); err != nil {
		t.Fatalf("first create: %v", err)
	}

	err := g.Create("key-1", "res-200")

	// Identity: the sentinel matches.
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("errors.Is(err, ErrDuplicate) = false; err = %v", err)
	}

	// Data: errors.As yields the existing id.
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("errors.As(*ConflictError) = false; err = %v", err)
	}
	if ce.ExistingID != "res-100" {
		t.Fatalf("ExistingID = %q, want res-100", ce.ExistingID)
	}

	// Same via errors.AsType (Go 1.26).
	got, ok := errors.AsType[*ConflictError](err)
	if !ok || got.ExistingID != "res-100" {
		t.Fatalf("AsType = %+v, %v; want ExistingID res-100", got, ok)
	}
}

func TestValidationIsNotConflict(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	err := g.Create("", "res-1")

	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatal("a validation error must not be extractable as *ConflictError")
	}
	if errors.Is(err, ErrDuplicate) {
		t.Fatal("a validation error must not match ErrDuplicate")
	}
}

func TestFirstCreateSucceeds(t *testing.T) {
	t.Parallel()

	g := NewGuard()
	if err := g.Create("k", "r"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A different key is independent.
	if err := g.Create("k2", "r2"); err != nil {
		t.Fatalf("Create k2: %v", err)
	}
}

func ExampleGuard_Create() {
	g := NewGuard()
	_ = g.Create("k", "res-1")
	err := g.Create("k", "res-2")
	ce, ok := errors.AsType[*ConflictError](err)
	fmt.Println(errors.Is(err, ErrDuplicate), ok, ce.ExistingID)
	// Output: true true res-1
}
```

## Review

The guard is correct when a replay returns a single value that answers *both*
questions — `errors.Is(err, ErrDuplicate)` for the caller that needs only
identity, and `errors.As`/`errors.AsType[*ConflictError]` for the caller that
needs the existing id — because `*ConflictError.Is` links the type to the
sentinel. A validation error, carrying no data, is a bare `ErrValidation` that
`errors.As` correctly refuses to extract as a conflict. The decision rule
generalizes: reach for a sentinel when identity is all the caller needs, and a
typed error (with `errors.As`/`AsType`) the moment they need data off the failure
— and give the type an `Is` method when you want both to work on one value.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) and [`errors.AsType`](https://pkg.go.dev/errors#AsType) — extracting a typed error's data (`AsType` added in Go 1.26).
- [`errors.Is`](https://pkg.go.dev/errors#Is) — identity matching, including via a custom `Is` method.
- [Stripe: Idempotent Requests](https://docs.stripe.com/api/idempotent_requests) — the idempotency-key pattern this guard models.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-fs-config-loader.md](08-fs-config-loader.md) | Next: [../06-error-wrapping-chains/00-concepts.md](../06-error-wrapping-chains/00-concepts.md)
