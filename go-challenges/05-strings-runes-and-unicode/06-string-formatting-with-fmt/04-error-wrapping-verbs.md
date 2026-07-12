# Exercise 4: Error Wrapping — %w vs %v vs %+v in a Repository Layer

A repository annotates a low-level failure (`sql.ErrNoRows`, a network error) with
operation context and hands it up. Whether the caller two layers away can still
match that error with `errors.Is`/`errors.As` depends entirely on one character in
a format string: `%w` versus `%v`. This exercise builds that contract and proves
it.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
repoerr/                   independent module: example.com/repoerr
  go.mod                   go 1.24
  repoerr.go               sentinel ErrNotFound; NotFoundError; GetUser; wrapping helpers
  cmd/
    demo/
      main.go              runnable demo: wrap, match, and flatten
  repoerr_test.go          Is-through-layers/As-typed/join/flatten/log-line tests
```

- Files: `repoerr.go`, `cmd/demo/main.go`, `repoerr_test.go`.
- Implement: a sentinel `ErrNotFound`, a typed `*NotFoundError`, a `GetUser` that wraps a store error with `%w` through two layers, a `LogLine` compact single-line formatter (`%v`), and a `flatten` helper that wraps with `%v` to show the broken case.
- Test: `errors.Is` finds the sentinel through multiple layers; a `%w`-built error unwraps while a `%v`-built one does not; `errors.As` extracts `*NotFoundError`; `errors.Join` of two errors is matchable by both; the log line is stable and single-line.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### %w records a link; %v records only text

`fmt.Errorf` with `%w` does something no other verb does: it stores the wrapped
error and gives the result an `Unwrap()` method that returns it. That is what makes
the error *matchable*. `errors.Is(err, ErrNotFound)` walks the `Unwrap` chain
comparing each link to the sentinel; `errors.As(err, &target)` walks it looking for
one that assigns to `target`. Wrap with `%w` at every layer and the sentinel or the
typed error is reachable from the top no matter how deep it is.

`%v` produces an error with the *same human-readable text* but no `Unwrap` method.
The chain is flattened into a string. `errors.Is` has nothing to walk, so it
returns false; `errors.Unwrap` returns nil. This is the silent bug: the log line
looks identical, the tests that only check the message pass, and then a caller's
`if errors.Is(err, ErrNotFound) { return 404 }` quietly stops firing and every
missing row becomes a 500. The rule: wrap with `%w` when any caller might need to
match; use `%v` only when you *deliberately* want to seal the chain (e.g. to avoid
leaking an internal error type across an API boundary).

Two more tools complete the contract. A *typed* error (`*NotFoundError` carrying
the key) is extracted with `errors.As`, so the handler can read `e.Key` for the
response. And `errors.Join(e1, e2)` builds a multi-error matchable by `errors.Is`
against *both* — the same capability a single `fmt.Errorf` with two `%w` verbs
provides (Go 1.20+). Use `Join` when an operation genuinely failed for more than
one reason (a batch, a cleanup that also failed).

Create `repoerr.go`:

```go
package repoerr

import (
	"errors"
	"fmt"
)

// ErrNotFound is the sentinel a caller matches with errors.Is to map a missing
// row to a 404. It is the store-agnostic contract of the repository.
var ErrNotFound = errors.New("not found")

// NotFoundError is the typed error carrying which key was missing, extracted with
// errors.As so a handler can report the key.
type NotFoundError struct {
	Key string
}

func (e *NotFoundError) Error() string { return "not found: " + e.Key }

// Unwrap makes a NotFoundError also match the ErrNotFound sentinel, so callers can
// use either the sentinel or the type.
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// store is the low-level layer. It returns a typed NotFoundError for a missing id.
func store(id string) error {
	if id == "missing" {
		return &NotFoundError{Key: id}
	}
	return nil
}

// GetUser is the repository method. It annotates the store error with operation
// context using %w at each layer, preserving the matchable chain.
func GetUser(id string) error {
	if err := store(id); err != nil {
		// two layers of context, both with %w
		inner := fmt.Errorf("query users id=%s: %w", id, err)
		return fmt.Errorf("GetUser: %w", inner)
	}
	return nil
}

// GetUserFlattened wraps with %v instead of %w. The message is identical but the
// chain is severed: errors.Is/As can no longer match. Shown to contrast, not to use.
func GetUserFlattened(id string) error {
	if err := store(id); err != nil {
		return fmt.Errorf("GetUser: %v", err)
	}
	return nil
}

// LogLine renders an error as one compact, stable line for logs using %v.
func LogLine(op string, err error) string {
	if err == nil {
		return fmt.Sprintf("op=%s status=ok", op)
	}
	return fmt.Sprintf("op=%s status=error err=%q", op, err.Error())
}
```

### The runnable demo

The demo shows the two rendering choices side by side: `GetUser` (wrapped with
`%w`) is matchable by both the sentinel and the type, while `GetUserFlattened`
(`%v`) produces the same text but matches nothing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repoerr"
)

func main() {
	err := repoerr.GetUser("missing")
	fmt.Println(repoerr.LogLine("GetUser", err))
	fmt.Println("Is ErrNotFound:", errors.Is(err, repoerr.ErrNotFound))

	var nfe *repoerr.NotFoundError
	if errors.As(err, &nfe) {
		fmt.Println("As NotFoundError, key:", nfe.Key)
	}

	flat := repoerr.GetUserFlattened("missing")
	fmt.Println("flattened Is ErrNotFound:", errors.Is(flat, repoerr.ErrNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
op=GetUser status=error err="GetUser: query users id=missing: not found: missing"
Is ErrNotFound: true
As NotFoundError, key: missing
flattened Is ErrNotFound: false
```

### Tests

`TestIsThroughLayers` proves the sentinel is matchable through both wrapping
layers. `TestFlattenBreaksMatching` is the contrast: the `%v` version has the same
message but `errors.Is` returns false and `errors.Unwrap` returns nil.
`TestAsExtractsTyped` pulls the `*NotFoundError` out and reads its key.
`TestJoinMatchesBoth` proves `errors.Join` is matchable by each joined error.
`TestLogLineStable` pins the single-line format.

Create `repoerr_test.go`:

```go
package repoerr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestIsThroughLayers(t *testing.T) {
	t.Parallel()

	err := GetUser("missing")
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is could not find ErrNotFound in %v", err)
	}
}

func TestFlattenBreaksMatching(t *testing.T) {
	t.Parallel()

	err := GetUserFlattened("missing")
	if errors.Is(err, ErrNotFound) {
		t.Fatal("flattened error should NOT match ErrNotFound")
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("flattened error should have no unwrap link")
	}
	// The %w version, by contrast, does unwrap.
	if errors.Unwrap(GetUser("missing")) == nil {
		t.Fatal("wrapped error should have an unwrap link")
	}
}

func TestAsExtractsTyped(t *testing.T) {
	t.Parallel()

	err := GetUser("missing")
	var nfe *NotFoundError
	if !errors.As(err, &nfe) {
		t.Fatalf("errors.As could not extract *NotFoundError from %v", err)
	}
	if nfe.Key != "missing" {
		t.Fatalf("key = %q, want missing", nfe.Key)
	}
}

func TestJoinMatchesBoth(t *testing.T) {
	t.Parallel()

	e1 := errors.New("primary failed")
	e2 := errors.New("cleanup failed")
	joined := errors.Join(e1, e2)
	if !errors.Is(joined, e1) || !errors.Is(joined, e2) {
		t.Fatalf("joined error should match both, got %v", joined)
	}
}

func TestLogLineStable(t *testing.T) {
	t.Parallel()

	line := LogLine("GetUser", GetUser("missing"))
	if strings.Contains(line, "\n") {
		t.Fatalf("log line must be single-line, got %q", line)
	}
	want := `op=GetUser status=error err="GetUser: query users id=missing: not found: missing"`
	if line != want {
		t.Fatalf("got %q, want %q", line, want)
	}
	if LogLine("Ping", nil) != "op=Ping status=ok" {
		t.Fatalf("nil error line = %q", LogLine("Ping", nil))
	}
}

func Example() {
	err := GetUser("missing")
	fmt.Println(errors.Is(err, ErrNotFound))
	// Output: true
}
```

## Review

The layer is correct when a low-level failure stays matchable at the top: `%w` at
every wrapping layer keeps `errors.Is(err, ErrNotFound)` and `errors.As(err,
&nfe)` working, and `TestIsThroughLayers`/`TestAsExtractsTyped` prove it through
two layers. `TestFlattenBreaksMatching` is the whole point of the lesson: `%v`
produces the same log text but severs the chain, so the sentinel becomes
unreachable and the 404 logic silently dies. The typed `*NotFoundError` with an
`Unwrap` returning the sentinel lets callers use either the type (to read the key)
or the sentinel (to branch), and `errors.Join` gives a multi-error matchable by
each cause. Reserve `%v`-wrapping for the deliberate case of sealing a chain at an
API boundary; everywhere else, `%w`.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `Is`, `As`, `Join`, `Unwrap`.
- [`fmt.Errorf`](https://pkg.go.dev/fmt#Errorf) — the `%w` verb and multiple-wrap semantics.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the design of `%w` and `errors.Is`/`As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-sprintf-hotpath-alloc.md](05-sprintf-hotpath-alloc.md)
