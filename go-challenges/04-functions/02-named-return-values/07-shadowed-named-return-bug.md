# Exercise 7: Debug a Deferred Wrapper That Never Fires

Every idiom in this lesson keys on the named `err`. They all break the same silent
way: an inner `:=` re-declares `err` in a nested scope, so the outer named result
stays at its zero value and the deferred wrapper or rollback inspects nothing. This
exercise reproduces that bug, proves it with a test, and fixes it — and the fix is
one character.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
lookupfix/                  independent module: example.com/lookupfix
  go.mod
  lookup.go                 Repo.LookupName (fixed: = assigns the named err)
  cmd/demo/
    main.go                 runnable demo: a not-found returns the wrapped error
  lookup_test.go            asserts the wrapper prefix + errors.Is chain are present
```

- Files: `lookup.go`, `cmd/demo/main.go`, `lookup_test.go`.
- Implement: `LookupName(id) (name string, err error)` with a deferred wrapper, assigning the store result to the named `err` with `=` (not a shadowing `:=`).
- Test: on not-found, the returned error carries the `LookupName "id"` prefix AND `errors.Is` matches the sentinel; the shadowed form (shown illustratively) would return a nil error and fail this test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/07-shadowed-named-return-bug/cmd/demo
cd go-solutions/04-functions/02-named-return-values/07-shadowed-named-return-bug
```

### The bug: a `:=` in an inner block

Here is the broken version. It compiles, `go vet` says nothing, and it silently
swallows every error. Do not assemble it — study it:

```go
// BROKEN — do not use. Shadows the named err in the inner block.
func (r *Repo) LookupName(id string) (name string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("LookupName %q: %w", id, err)
		}
	}()

	{
		row, err := r.store.Get(id) // := declares a NEW err in this block
		if err != nil {
			return // naked return: hands back the OUTER err, still nil
		}
		name = row.Name
	}
	return
}
```

Trace it on a not-found. `r.store.Get(id)` returns `ErrNotFound`. But `row, err :=`
declared a fresh `err` local to the inner `{ }` block, so that inner `err` holds
`ErrNotFound` while the outer named `err` is still `nil`. The `if err != nil`
guard sees the inner one and executes `return` — a *naked* return, which hands back
the outer named results: `name == ""` and `err == nil`. The deferred wrapper then
inspects the outer `err`, sees `nil`, and does nothing. The function reports success
with an empty name while the store actually returned a not-found. The error is gone.

The trap has three colluding parts: the `:=` shadows in a nested scope, the naked
return propagates the *outer* (unset) result, and `go vet`'s default passes do not
flag shadowing (the shadow analyzer ships separately in `golang.org/x/tools` and
must be run with `go vet -vettool=$(go env GOPATH)/bin/shadow`). None of the three
alone is wrong; together they silently eat errors. The dependable gate is
behavioral: assert the wrapper prefix is present on the returned error.

### The fix: `=` assigns the named result

Pre-declare the other variable and assign with `=` so the store result lands on the
named `err`:

```go
func (r *Repo) LookupName(id string) (name string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("LookupName %q: %w", id, err)
		}
	}()

	var row Row
	row, err = r.store.Get(id) // = assigns the NAMED err
	if err != nil {
		return
	}
	name = row.Name
	return
}
```

Now `row, err = r.store.Get(id)` writes to the named `err`. On a not-found the named
`err` is `ErrNotFound`, the naked `return` hands it back, the deferred wrapper sees
it and prepends `LookupName "id": `, and `errors.Is(err, ErrNotFound)` still matches
through `%w`. One character — `:=` becomes `=` plus a `var row Row` declaration — is
the whole fix.

Create `lookup.go`:

```go
package lookupfix

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) when no row has the given id.
var ErrNotFound = errors.New("not found")

// Row is the stored record.
type Row struct {
	ID   string
	Name string
}

// Store is the backing data source.
type Store interface {
	Get(id string) (Row, error)
}

// Repo looks up names, wrapping failures uniformly.
type Repo struct {
	store Store
}

// New builds a Repo over a store.
func New(store Store) *Repo {
	return &Repo{store: store}
}

// LookupName returns the name for an id. The deferred closure wraps any non-nil
// named err. The store result is assigned with = (not :=), so it lands on the
// named err; a shadowing := in an inner block would leave the named err nil and
// the wrapper would never fire.
func (r *Repo) LookupName(id string) (name string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("LookupName %q: %w", id, err)
		}
	}()

	var row Row
	row, err = r.store.Get(id)
	if err != nil {
		return
	}
	name = row.Name
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/lookupfix"
)

type memStore map[string]lookupfix.Row

func (m memStore) Get(id string) (lookupfix.Row, error) {
	row, ok := m[id]
	if !ok {
		return lookupfix.Row{}, lookupfix.ErrNotFound
	}
	return row, nil
}

func main() {
	repo := lookupfix.New(memStore{"42": {ID: "42", Name: "Ada"}})

	name, err := repo.LookupName("42")
	fmt.Printf("found: %q err=%v\n", name, err)

	name, err = repo.LookupName("99")
	fmt.Printf("missing: %q err=%v\n", name, err)
	fmt.Println("is not-found:", errors.Is(err, lookupfix.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
found: "Ada" err=<nil>
missing: "" err=LookupName "99": not found
is not-found: true
```

### Tests

The test asserts exactly what the shadowed version would fail: the returned error
on a not-found must be non-nil, carry the wrapper prefix, and still match the
sentinel. With the buggy `:=` form the returned error would be nil and every one of
these assertions would fail — which is why the behavioral test, not `go vet`, is the
gate.

Create `lookup_test.go`:

```go
package lookupfix

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeStore struct {
	row Row
	err error
}

func (f fakeStore) Get(string) (Row, error) { return f.row, f.err }

func TestLookupNameWrapsNotFound(t *testing.T) {
	t.Parallel()

	repo := New(fakeStore{err: ErrNotFound})
	name, err := repo.LookupName("99")

	if err == nil {
		t.Fatal("LookupName swallowed the error (shadowed named return?)")
	}
	if name != "" {
		t.Fatalf("name = %q, want empty on failure", name)
	}
	if !strings.Contains(err.Error(), `LookupName "99"`) {
		t.Fatalf("error %q missing wrapper prefix", err.Error())
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error %q lost the ErrNotFound chain", err.Error())
	}
}

func TestLookupNameSuccess(t *testing.T) {
	t.Parallel()

	repo := New(fakeStore{row: Row{ID: "42", Name: "Ada"}})
	name, err := repo.LookupName("42")
	if err != nil {
		t.Fatalf("LookupName: unexpected error: %v", err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q, want Ada", name)
	}
}

func ExampleRepo_LookupName() {
	repo := New(fakeStore{err: ErrNotFound})
	_, err := repo.LookupName("7")
	fmt.Println(err)
	// Output: LookupName "7": not found
}
```

## Review

The function is correct when a store failure reaches the caller wrapped and still
`errors.Is`-matchable, and a not-found never masquerades as an empty-name success.
The whole exercise is the one-character difference between `:=` and `=`: the former,
inside a nested block, declares a new `err` that shadows the named result, so a
naked-return path returns the still-nil outer `err` and the deferred wrapper fires
on nothing. Because default `go vet` does not catch this, the behavioral test is the
real gate — it asserts the prefix and the sentinel chain, both of which the shadowed
form would fail to produce. When you see a deferred wrapper or rollback that "never
fires," look for a `:=` on `err` in an inner scope first. Run `go test -race`.

## Resources

- [Go Spec: Short variable declarations](https://go.dev/ref/spec#Short_variable_declarations)
- [Go Spec: Declarations and scope](https://go.dev/ref/spec#Declarations_and_scope)
- [`golang.org/x/tools/go/analysis/passes/shadow`](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/shadow)
- [Go Code Review Comments: Named Result Parameters](https://go.dev/wiki/CodeReviewComments#named-result-parameters)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-latency-and-outcome-metrics.md](06-latency-and-outcome-metrics.md) | Next: [08-error-returning-handler-adapter.md](08-error-returning-handler-adapter.md)
