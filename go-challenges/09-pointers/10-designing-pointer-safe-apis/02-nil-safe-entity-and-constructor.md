# Exercise 2: Constructors Return (*T, error); Methods Survive a Nil Receiver

A constructor is an API boundary the same way a repository `Get` is: it decides
what a caller must check before touching the result. This module builds an
`Entity` whose constructor returns `(*Entity, error)` with a non-nil pointer
exactly when the error is nil, initializes the `Data` map so it is never nil, and
gives `Set`/`Get` nil-receiver guards so a possibly-nil handle is safe to read.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
entity/                     independent module: example.com/entity
  go.mod                    go 1.25
  entity.go                 Entity; NewEntity (*Entity, error); Set/Get nil-safe; ErrInvalidName
  cmd/
    demo/
      main.go               construct, set, get, and read through a nil handle
  entity_test.go            constructor validation, roundtrip, nil-receiver no-op tests
```

- Files: `entity.go`, `cmd/demo/main.go`, `entity_test.go`.
- Implement: `NewEntity(id, name) (*Entity, error)` that validates inputs and initializes `Data`; `Set`/`Get` that guard `if e == nil`.
- Test: empty id or name returns `errors.Is(err, ErrInvalidName)`; the success path yields a non-nil pointer with a non-nil `Data` map; `Set` then `Get` roundtrips; a `var e *Entity` then `e.Get(k)` returns `""` and `e.Set(k, v)` does not panic.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The constructor contract: non-nil pointer iff nil error

`NewEntity` validates first, then builds. On invalid input it returns
`(nil, ErrInvalidName)`; on success it returns a non-nil `*Entity`. The property
that makes this pleasant to call is the biconditional: the pointer is non-nil
*if and only if* the error is nil. A caller writes `e, err := NewEntity(...); if
err != nil { return err }` and from that point on knows `e` is safe to
dereference — no second nil-check. Contrast the sloppy alternative that returns
`(nil, nil)` on some failure path: now the caller must check both, and the
redundant check is the one that rots.

The constructor also initializes `Data: make(map[string]string)`. A nil map reads
fine (a lookup on a nil map returns the zero value) but *panics on write*. By
constructing the map here, `Set` can write unconditionally without every caller
worrying whether the map exists. "Initialize your maps in the constructor" is the
same discipline as "never return `(nil, nil)`": it moves a check out of every
call site and into the one place that owns the invariant.

### Nil-receiver methods as a documented no-op

`Set` and `Get` guard `if e == nil` at the top. This is legal because a method
call is sugar for passing the receiver as an argument — `e.Get(k)` is `Get(e, k)`
— so a nil receiver only panics if the body dereferences it. Guarding lets a
caller hold a possibly-nil `*Entity` (say, the result of a lookup that may have
missed) and call `e.Get("k")` to get `""` rather than a panic. The contract is:
on a nil entity, `Get` returns the zero value and `Set` is a no-op. It must be
documented and tested, because a reader cannot tell from the call site alone that
nil is handled.

Create `entity.go`:

```go
package entity

import "errors"

// ErrInvalidName is returned by NewEntity when id or name is empty.
var ErrInvalidName = errors.New("invalid name")

// Entity is a record whose Data map is initialized by NewEntity so it is never
// nil for a properly constructed value.
type Entity struct {
	ID   string
	Name string
	Data map[string]string
}

// NewEntity returns a non-nil *Entity on success or (nil, ErrInvalidName) when
// id or name is empty. The pointer is non-nil if and only if err is nil.
func NewEntity(id, name string) (*Entity, error) {
	if id == "" || name == "" {
		return nil, ErrInvalidName
	}
	return &Entity{ID: id, Name: name, Data: make(map[string]string)}, nil
}

// Set writes key=value. On a nil receiver it is a no-op (does not panic).
func (e *Entity) Set(key, value string) {
	if e == nil {
		return
	}
	e.Data[key] = value
}

// Get returns the value for key, or "" if absent. On a nil receiver it returns
// "" (does not panic).
func (e *Entity) Get(key string) string {
	if e == nil {
		return ""
	}
	return e.Data[key]
}
```

### The runnable demo

The demo constructs an entity, roundtrips a key, then deliberately reads through a
nil handle to show the no-op contract holds without a panic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/entity"
)

func main() {
	e, err := entity.NewEntity("u1", "alice")
	if err != nil {
		fmt.Println("construct failed:", err)
		return
	}
	e.Set("role", "admin")
	fmt.Printf("role=%s\n", e.Get("role"))

	_, err = entity.NewEntity("", "alice")
	fmt.Println("empty id rejected:", errors.Is(err, entity.ErrInvalidName))

	var missing *entity.Entity
	missing.Set("k", "v") // no-op, no panic
	fmt.Printf("nil get=%q, no panic\n", missing.Get("k"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
role=admin
empty id rejected: true
nil get="", no panic
```

### Tests

The tests pin the constructor's biconditional and the nil-receiver contract. A
table drives the validation cases (empty id, empty name, valid). The success case
asserts both a non-nil pointer and a non-nil `Data` map. The nil-receiver test
reads and writes through a `var e *Entity` and asserts `Get` returns `""` and
`Set` does not panic (the test simply completing is the panic assertion).

Create `entity_test.go`:

```go
package entity

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewEntityValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		id      string
		ename   string
		wantErr bool
	}{
		{name: "empty id", id: "", ename: "alice", wantErr: true},
		{name: "empty name", id: "u1", ename: "", wantErr: true},
		{name: "valid", id: "u1", ename: "alice", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := NewEntity(tc.id, tc.ename)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidName) {
					t.Fatalf("err = %v, want ErrInvalidName", err)
				}
				if e != nil {
					t.Fatalf("entity = %+v, want nil on error", e)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if e == nil {
				t.Fatal("entity = nil, want non-nil on success")
			}
			if e.Data == nil {
				t.Fatal("Data map = nil, want initialized")
			}
		})
	}
}

func TestSetGetRoundtrip(t *testing.T) {
	t.Parallel()
	e, _ := NewEntity("u1", "alice")
	e.Set("k", "v")
	if got := e.Get("k"); got != "v" {
		t.Fatalf("Get(k) = %q, want %q", got, "v")
	}
}

func TestNilReceiverIsNoOp(t *testing.T) {
	t.Parallel()
	var e *Entity
	if got := e.Get("k"); got != "" {
		t.Fatalf("nil Get = %q, want empty", got)
	}
	e.Set("k", "v") // must not panic
}

func ExampleNewEntity() {
	e, err := NewEntity("u1", "alice")
	fmt.Println(e.Name, err == nil)

	e.Set("role", "admin")
	fmt.Println(e.Get("role"))
	// Output:
	// alice true
	// admin
}
```

## Review

The constructor is correct when the returned pointer is non-nil exactly when the
error is nil, and when `Data` is always initialized on the success path so callers
never face a nil map. The nil-receiver methods are correct when `Get` returns `""`
and `Set` is a silent no-op on a nil `*Entity`, tested by simply calling them and
not panicking. The mistake to avoid is treating the nil guard as optional
defensive noise: it is a *contract*, and a caller who relies on "reading a missed
entity returns empty" will be broken if a future edit drops the guard, so the test
that pins it is load-bearing. The other mistake is forgetting `make(map...)` in the
constructor, which compiles and even reads fine, then panics on the first `Set`.

## Resources

- [Effective Go: allocation with `new` and composite literals](https://go.dev/doc/effective_go#allocation_new) — why a constructor initializes its maps.
- [Go spec: method sets and calls on nil receivers](https://go.dev/ref/spec#Method_sets) — why `e.Get(k)` on a nil `e` is legal.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching the `ErrInvalidName` sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-pointer-safe-repository.md](01-pointer-safe-repository.md) | Next: [03-defensive-copy-break-aliasing.md](03-defensive-copy-break-aliasing.md)
