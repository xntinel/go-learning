# Exercise 8: Drive A Behavior Migration With A Workspace godebug

When a Go release changes a runtime default, you sometimes need every module in a
platform to opt into the old (or new) behavior at once during a migration. The
`go.work` `godebug` directive pins a GODEBUG setting for the whole workspace, and
while the workspace is active it *overrides* any `godebug` in an individual
`go.mod`. This exercise makes the effect observable with `panicnil`, whose value
you can read directly through `panic(nil)` and `recover`.

## What you'll build

```text
migration/                     module: example.com/migration
  go.mod                       go 1.26
  panicnil.go                  RecoverNilPanic: panic(nil) then recover, returns what was recovered
  panicnil_test.go             asserts the go 1.21+ default: *runtime.PanicNilError
  cmd/
    demo/
      main.go                  prints what recover() observed for panic(nil)
```

- Files: `panicnil.go`, `panicnil_test.go`, `cmd/demo/main.go`.
- Implement: `RecoverNilPanic() any` that runs `panic(nil)` and returns the recovered value.
- Test: under the go 1.21+ default (`panicnil=0`), the recovered value is a `*runtime.PanicNilError`, asserted via `errors.As`.
- Verify: adding `godebug panicnil=1` to `go.work` flips the recovered value to a literal `nil`; a `godebug` in `go.mod` is ignored while the workspace is active.

Set up the module:

```bash
mkdir -p ~/migration/cmd/demo
cd ~/migration
go mod init example.com/migration
go mod edit -go=1.26
```

### The migration, and why go.work wins

Before Go 1.21, `panic(nil)` recovered as a literal `nil`, so `recover() == nil`
could not distinguish "a goroutine panicked with nil" from "nothing panicked".
Go 1.21 changed the default: `panic(nil)` now recovers as a non-nil
`*runtime.PanicNilError`. The `panicnil` GODEBUG setting selects between the two —
`panicnil=1` restores the old literal-nil behavior, `panicnil=0` (the default at
`go 1.21` and above) yields the error.

During a fleet-wide migration you may need every module to hold the old behavior
temporarily while you fix code that relied on it. Rather than edit each module's
`go.mod`, pin it once in `go.work`:

```text
go 1.26

use (
	./greeter
	./billing
	./worker
)

godebug (
	panicnil=1
)
```

With the workspace active, every module builds with `panicnil=1`, and — this is
the load-bearing rule — a `godebug panicnil=0` line in any individual `go.mod` is
*ignored*; `go.work`'s value wins. That is what makes `go.work` the right lever
for a coordinated migration and also the trap: a developer who sets the directive
in one `go.mod` and expects it to apply sees `go.work`'s value instead. Remove the
`go.work` godebug and each module falls back to its own `go.mod` default again.

The gated artifact runs under the workspace's effective default with no `godebug`
override, which at `go 1.26` is `panicnil=0`: `panic(nil)` recovers as a
`*runtime.PanicNilError`. Flipping the `go.work` directive to `panicnil=1` would
make the same code recover a literal `nil` — the observable proof that the
workspace-level GODEBUG is compiled in.

Create `panicnil.go`:

```go
// panicnil.go
package migration

// RecoverNilPanic panics with a nil value and returns whatever recover observes.
// Under panicnil=0 (the go 1.21+ default) that is a *runtime.PanicNilError; under
// panicnil=1 (set via go.work's godebug directive) it is a literal nil.
func RecoverNilPanic() (recovered any) {
	defer func() {
		recovered = recover()
	}()
	var nilValue any
	panic(nilValue)
}
```

### The demo

The demo prints what `recover` observed. Under the default the value is the
error's message; the `%T` shows the concrete type so the migration state is
unambiguous.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/migration"
)

func main() {
	got := migration.RecoverNilPanic()
	fmt.Printf("recovered value: %v\n", got)
	fmt.Printf("recovered type:  %T\n", got)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered value: panic called with nil argument
recovered type:  *runtime.PanicNilError
```

### Tests

The test asserts the go 1.21+ default: `panic(nil)` recovers as a non-nil
`*runtime.PanicNilError`. If a `go.work` `godebug panicnil=1` were compiled in, the
recovered value would be `nil` and the assertion would flip — which is exactly how
you would prove the workspace directive took effect.

Create `panicnil_test.go`:

```go
// panicnil_test.go
package migration

import (
	"errors"
	"runtime"
	"testing"
)

func TestRecoverNilPanicIsError(t *testing.T) {
	t.Parallel()

	got := RecoverNilPanic()
	if got == nil {
		t.Fatal("recovered nil; expected *runtime.PanicNilError under the go 1.21+ default")
	}
	err, ok := got.(error)
	if !ok {
		t.Fatalf("recovered %T, want an error", got)
	}
	var target *runtime.PanicNilError
	if !errors.As(err, &target) {
		t.Fatalf("recovered %T, want *runtime.PanicNilError", err)
	}
}

func TestRecoverNilPanicMessage(t *testing.T) {
	t.Parallel()

	got := RecoverNilPanic()
	err, ok := got.(error)
	if !ok {
		t.Fatalf("recovered %T, want an error", got)
	}
	if msg := err.Error(); msg != "panic called with nil argument" {
		t.Fatalf("message = %q, want %q", msg, "panic called with nil argument")
	}
}
```

## Review

The migration lever is the `go.work` `godebug` directive: it pins a GODEBUG value
for every workspace module at once and overrides any `godebug` in an individual
`go.mod`, which is precisely why it is the tool for a coordinated, fleet-wide
behavior change — and why setting the directive per-`go.mod` under an active
workspace silently does nothing. The `panicnil` setting makes the effect legible:
at the `go 1.26` default (`panicnil=0`) the code recovers a
`*runtime.PanicNilError`, which the test pins with `errors.As`; add
`godebug panicnil=1` to `go.work` and the same `panic(nil)` recovers a literal
`nil`. When the migration completes, drop the `go.work` directive and let each
module carry its own default again.

## Resources

- [Go, Backwards Compatibility, and GODEBUG](https://go.dev/doc/godebug) — the `godebug` directive in `go.mod`/`go.work` and how defaults are chosen.
- [Go Modules Reference — Workspaces](https://go.dev/ref/mod#workspaces) — the `godebug` directive in `go.work` and its precedence over `go.mod`.
- [`runtime.PanicNilError`](https://pkg.go.dev/runtime#PanicNilError) — the value `panic(nil)` recovers as under the go 1.21+ default.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-go-work-edit-automation.md](09-go-work-edit-automation.md)
