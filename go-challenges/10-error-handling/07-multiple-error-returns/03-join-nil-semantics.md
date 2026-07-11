# Exercise 3: Nil-Skipping And All-Nil Semantics of errors.Join

The two nil rules of `errors.Join` are what let a call site accumulate errors
unconditionally, with no nil guards, and still return a clean nil on the happy
path. This exercise pins both rules with thin wrappers and asserts the sharp
detail that all-nil returns *untyped* nil, not a non-nil interface wrapping nil.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
joinnil/                   independent module: example.com/joinnil
  go.mod                   go 1.26
  joinnil.go               Collect2, Collect3 thin wrappers over errors.Join
  cmd/
    demo/
      main.go              prints the recovered error and the all-nil result
  joinnil_test.go          asserts nil-skipping, all-nil-is-untyped-nil
```

- Files: `joinnil.go`, `cmd/demo/main.go`, `joinnil_test.go`.
- Implement: `Collect2(a, b error) error` and `Collect3(a, b, c error) error`, each returning `errors.Join` of their inputs.
- Test: `Collect2(nil, errA)` is `errors.Is` errA; `Collect2(nil, nil)` is nil via `err != nil`; `Collect3(nil, nil, nil)` is nil; and assert the returned value is *untyped* nil, not a non-nil interface.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/joinnil/cmd/demo
cd ~/go-exercises/joinnil
go mod init example.com/joinnil
```

### Why the nil rules matter for real code

Aggregation loops want to write `errs = append(errs, step())` and then
`errors.Join(errs...)` once, without checking each `step()` for nil first. That is
only clean because `errors.Join` discards nil inputs internally: `Join(nil, err, nil)`
is an error from which `err` is recoverable, and `Join(nil, nil, nil)` is nil. If
`Join` instead produced a non-nil aggregate whenever it received *any* argument, the
happy path would return a truthy error and every caller would need a guard.

The subtle part is the *kind* of nil. In Go, an interface value is nil only when
both its type and its value are nil. A function that returns a `*MultiError` typed
as `error` can return a non-nil `error` interface that wraps a nil pointer — the
classic "typed nil" bug, where `err != nil` is true even though there is no error.
`errors.Join` avoids this: when every input is nil it returns the untyped nil
`error`, so `err != nil` is false and `err == nil` is true. This exercise asserts
that directly with `err != nil`, which is the check real code uses.

Create `joinnil.go`:

```go
package joinnil

import "errors"

// Collect2 joins two errors. errors.Join discards nil inputs, so Collect2(nil, b)
// is recoverable as b, and Collect2(nil, nil) is untyped nil.
func Collect2(a, b error) error {
	return errors.Join(a, b)
}

// Collect3 joins three errors with the same nil semantics.
func Collect3(a, b, c error) error {
	return errors.Join(a, b, c)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/joinnil"
)

var errDisk = errors.New("disk full")

func main() {
	// A nil interspersed with a real error is discarded; the error survives.
	recovered := joinnil.Collect2(nil, errDisk)
	fmt.Println("recovered:", recovered)
	fmt.Println("is errDisk:", errors.Is(recovered, errDisk))

	// Every input nil yields untyped nil.
	clean := joinnil.Collect3(nil, nil, nil)
	fmt.Println("all-nil == nil:", clean == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered: disk full
is errDisk: true
all-nil == nil: true
```

### Tests

The tests pin both rules and the untyped-nil detail. `TestNilSkipped` proves a nil
before a real error is discarded and the error is recoverable by `errors.Is`.
`TestPairAllNil` and `TestTripleAllNil` prove the all-nil result satisfies
`err != nil` being false — the exact check that would break under a typed nil.

Create `joinnil_test.go`:

```go
package joinnil

import (
	"errors"
	"testing"
)

var errA = errors.New("a")

func TestNilSkipped(t *testing.T) {
	t.Parallel()

	err := Collect2(nil, errA)
	if err == nil {
		t.Fatal("Collect2(nil, errA) = nil, want non-nil")
	}
	if !errors.Is(err, errA) {
		t.Fatalf("errors.Is(Collect2(nil, errA), errA) = false, want true")
	}
}

func TestPairAllNil(t *testing.T) {
	t.Parallel()

	if err := Collect2(nil, nil); err != nil {
		t.Fatalf("Collect2(nil, nil) = %v (%T), want untyped nil", err, err)
	}
}

func TestTripleAllNil(t *testing.T) {
	t.Parallel()

	if err := Collect3(nil, nil, nil); err != nil {
		t.Fatalf("Collect3(nil, nil, nil) = %v (%T), want untyped nil", err, err)
	}
}
```

## Review

The wrappers are correct when interspersed nils vanish and all-nil produces untyped
nil. The `err != nil` assertions are the point: they would fail if `errors.Join`
(or a hand-rolled aggregate) returned a non-nil interface wrapping a nil value, the
typed-nil bug. This is why, when you build your own aggregate type later
(Exercise 7), you must return literal `nil` on the zero-failure path rather than a
`*MultiError` with an empty slice — otherwise callers see a truthy error when
nothing failed. Run with `-race` for habit.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — "Join returns nil if every value in errs is nil."
- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the typed-nil interface trap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-joined-error-message-shape.md](02-joined-error-message-shape.md) | Next: [04-validate-all-fields.md](04-validate-all-fields.md)
