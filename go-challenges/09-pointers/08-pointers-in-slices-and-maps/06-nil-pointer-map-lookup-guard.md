# Exercise 6: Comma-Ok or Panic — Guarding a Nil *T From a Missing Map Key

A repository lookup on a `map[string]*Record` returns a nil `*Record` for a
missing key, and the next line that touches a field panics. This module builds the
safe read path — comma-ok returning `ErrNotFound` — plus a nil-receiver-safe method
so the zero case is a defined answer, and proves the hazard with a recovered panic.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
recordrepo/                   independent module: example.com/recordrepo
  go.mod                      go 1.24
  repo.go                     Repo{byID map[string]*Record}; safe Get (comma-ok), nil-safe IsActive
  repo_test.go                missing key -> ErrNotFound + nil ptr; deref-missing panics (recovered); nil receiver safe
  cmd/demo/main.go            runnable demo of a hit, a miss, and a nil-safe method call
```

Files: `repo.go`, `repo_test.go`, `cmd/demo/main.go`.
Implement: `Get(id) (*Record, error)` using comma-ok and `ErrNotFound`; an
`IsActive()` method with a nil-receiver guard; a demonstration function that
dereferences a missing key and recovers the panic.
Test: missing key returns `ErrNotFound` and a nil pointer with no panic; the unsafe
deref panics (recovered); `IsActive` on a nil `*Record` returns false.
Verify: `go test -count=1 -race ./...`

### Why the cold path panics, and how to define the zero case

A map lookup always succeeds in the type system: `r := m[id]` yields a value of the
map's value type whether or not the key exists. For `map[string]*Record`, that
value type is `*Record`, whose zero value is `nil`. So on a cache miss `r` is a nil
`*Record`, and `r.Active` (a field access) or `r.IsActive()` (a method that reads a
field) dereferences nil and panics with a runtime error. It compiles cleanly and
works for every present key, so it passes tests that only cover hits and blows up
the first time production takes a miss. This is the archetypal cold-path bug.

Two defenses, and you want both. On the read path, use comma-ok:
`r, ok := m[id]; if !ok { return nil, ErrNotFound }`. This turns "missing" into an
error the caller must handle, instead of a nil that detonates later. Wrap or return
a package sentinel (`ErrNotFound`) so callers can `errors.Is` against it. Second,
where a nil `*Record` is a legitimate value to pass around (say, "no record yet"),
make its methods nil-receiver-safe: `func (r *Record) IsActive() bool { if r == nil
{ return false }; return r.Status == StatusActive }`. A method with a pointer
receiver can be called on a nil pointer — Go dispatches on the static type, not the
value — so the guard runs and returns a defined answer instead of panicking. That
turns the zero case from a landmine into a documented behavior.

Create `repo.go`:

```go
package recordrepo

import "errors"

// ErrNotFound is returned by Get when the id is absent.
var ErrNotFound = errors.New("record not found")

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type Record struct {
	ID     string
	Status Status
}

// IsActive is nil-receiver-safe: a nil *Record is a defined "not active".
func (r *Record) IsActive() bool {
	if r == nil {
		return false
	}
	return r.Status == StatusActive
}

type Repo struct {
	byID map[string]*Record
}

func NewRepo() *Repo { return &Repo{byID: make(map[string]*Record)} }

func (repo *Repo) Put(r *Record) { repo.byID[r.ID] = r }

// Get is the safe read path: comma-ok turns a miss into ErrNotFound and a nil
// pointer, never a later panic.
func (repo *Repo) Get(id string) (*Record, error) {
	r, ok := repo.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

// unsafeDerefMissing shows the hazard: r is nil for a missing key and r.Status
// dereferences nil. Recovered by the caller in a test.
func unsafeDerefMissing(m map[string]*Record, id string) (s Status) {
	r := m[id] // nil for a missing key
	return r.Status
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/recordrepo"
)

func main() {
	repo := recordrepo.NewRepo()
	repo.Put(&recordrepo.Record{ID: "r1", Status: recordrepo.StatusActive})

	r, err := repo.Get("r1")
	fmt.Printf("hit:  active=%v err=%v\n", r.IsActive(), err)

	missing, err := repo.Get("nope")
	// missing is a nil *Record; IsActive is still safe to call.
	fmt.Printf("miss: active=%v notfound=%v\n", missing.IsActive(), errors.Is(err, recordrepo.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit:  active=true err=<nil>
miss: active=false notfound=true
```

### Tests

`TestMissingKeyReturnsNotFound` asserts `Get` on an empty repo returns
`ErrNotFound` and a nil pointer without panicking. `TestDerefMissingPanics`
recovers the panic from the unsafe dereference, documenting the hazard.
`TestNilReceiverMethodSafe` calls `IsActive` on a nil `*Record` and asserts false.

Create `repo_test.go`:

```go
package recordrepo

import (
	"errors"
	"testing"
)

func TestMissingKeyReturnsNotFound(t *testing.T) {
	t.Parallel()

	repo := NewRepo()
	r, err := repo.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if r != nil {
		t.Fatalf("r = %v, want nil pointer on miss", r)
	}
}

func TestDerefMissingPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected a nil-pointer dereference panic on the unsafe path")
		}
	}()

	m := map[string]*Record{}
	_ = unsafeDerefMissing(m, "missing") // r is nil; r.Status panics
	t.Fatal("unreachable: the deref should have panicked")
}

func TestNilReceiverMethodSafe(t *testing.T) {
	t.Parallel()

	var r *Record // nil
	if r.IsActive() {
		t.Fatal("nil *Record should report not active")
	}
}
```

## Review

The type system never warns you here — every path compiles — so the discipline is
yours to enforce. `Get` reads with comma-ok and returns `ErrNotFound` plus a nil
pointer, which `TestMissingKeyReturnsNotFound` confirms causes no panic;
`TestDerefMissingPanics` shows exactly what you avoided by recovering the crash from
the unsafe deref. The nil-receiver guard on `IsActive` is the second layer: a nil
`*Record` becomes a defined "not active" instead of a panic, so even code that
forgets to check the error (as the demo's `missing.IsActive()` does) stays safe.
Wrap misses in a package sentinel so callers can branch on `errors.Is`, and add
nil guards to any method that a legitimately-nil pointer might reach.

## Resources

- [Go spec: Index expressions (maps)](https://go.dev/ref/spec#Index_expressions) — a map index yields the zero value and the comma-ok boolean for a missing key.
- [Go spec: Method sets and calls on nil](https://go.dev/ref/spec#Calls) — a method with a pointer receiver may be called on a nil pointer.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel on the read path.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-pointer-map-eviction-gc-leak.md](07-pointer-map-eviction-gc-leak.md)
