# Exercise 11: Order Finalization: Defer Reading a Shadowed Named Return

**Nivel: Intermedio** — validacion rapida (un test corto).

An order-finalization function uses a named return `err` so a single
deferred closure can log a consistent audit message on every exit path. This
is a legitimate, common pattern — until an `if err := validate(id); err !=
nil` inside the function shadows the named return with a local of the same
name, and the code that follows never notices the failure.

## What you'll build

```text
finalize/                    independent module: example.com/finalize
  go.mod                     go 1.24
  finalize.go                 Notifier, FinalizeFixed, FinalizeBuggy
  finalize_test.go            table test: fixed vs. shadowed named return
```

- Files: `finalize.go`, `finalize_test.go`.
- Implement: `FinalizeFixed` assigning to the named return `err` (no shadow); `FinalizeBuggy` shadowing it with `:=`, to see what a deferred audit closure logs when it does.
- Test: one table test asserting returned status, returned error, and the audit `Notifier.Sent` messages for both variants.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A shadow the compiler cannot always catch for you

`FinalizeBuggy` shadows the named return with `if err := validate(id); err !=
nil { ... }`. Go refuses a bare `return` written INSIDE that shadowed block —
the compiler stops you with "result parameter err not in scope at return."
That protection only covers code literally inside the shadow's own scope.
Once the block ends, nothing stops you falling through to the rest of the
function believing you handled the failure, while the named `err` was never
touched. `FinalizeFixed` removes the shadow by assigning (`err =
validate(id)`) instead of declaring (`err := validate(id)`).

Create `finalize.go`:

```go
package finalize

// Notifier records the audit messages emitted by order finalization.
type Notifier struct {
	Sent []string
}

func (n *Notifier) Notify(msg string) {
	n.Sent = append(n.Sent, msg)
}

// FinalizeBuggy validates and commits an order. Its deferred closure reads
// the NAMED return err to decide which audit message to send. The bug: the
// `if err := validate(id)` below uses `:=`, which declares a new local err
// that SHADOWS the named return for the rest of the if-block. The author
// intended the "rejected" status to mark failure, but forgot to actually
// return out of the block, so execution falls through to the unconditional
// `status = "committed"` below, and the named return err is NEVER touched by
// the shadowed local.
func FinalizeBuggy(id string, validate func(string) error, n *Notifier) (status string, err error) {
	defer func() {
		if err != nil {
			n.Notify("failed:" + id)
		} else {
			n.Notify("ok:" + id)
		}
	}()

	if err := validate(id); err != nil { // BUG: := shadows the named return err
		status = "rejected" // looks like it records the failure...
		// ...but there is no return here, so execution falls through below.
	}
	status = "committed" // BUG: unconditionally overwrites status either way
	return
}

// FinalizeFixed is the same flow with the shadow removed: it assigns to the
// named return err instead of declaring a new local, so both the returned
// error and the deferred audit closure see the real outcome.
func FinalizeFixed(id string, validate func(string) error, n *Notifier) (status string, err error) {
	defer func() {
		if err != nil {
			n.Notify("failed:" + id)
		} else {
			n.Notify("ok:" + id)
		}
	}()

	if err = validate(id); err != nil { // correct: assigns the named return, no shadow
		return
	}
	status = "committed"
	return
}
```

### Test

One table test drives both `FinalizeFixed` and `FinalizeBuggy` with the same
failing validator and checks the returned values plus the audit trail.

Create `finalize_test.go`:

```go
package finalize

import (
	"errors"
	"reflect"
	"testing"
)

var errValidation = errors.New("validation failed")

func failingValidate(string) error { return errValidation }
func okValidate(string) error      { return nil }

func TestFinalize(t *testing.T) {
	tests := []struct {
		name       string
		finalize   func(string, func(string) error, *Notifier) (string, error)
		validate   func(string) error
		wantStatus string
		wantErr    error
		wantSent   []string
	}{
		{
			name:       "fixed: failure is reported and logged as failed",
			finalize:   FinalizeFixed,
			validate:   failingValidate,
			wantStatus: "",
			wantErr:    errValidation,
			wantSent:   []string{"failed:order"},
		},
		{
			name:       "fixed: success is committed and logged as ok",
			finalize:   FinalizeFixed,
			validate:   okValidate,
			wantStatus: "committed",
			wantErr:    nil,
			wantSent:   []string{"ok:order"},
		},
		{
			name:       "buggy: shadowed err falls through as a false success",
			finalize:   FinalizeBuggy,
			validate:   failingValidate,
			wantStatus: "committed",
			wantErr:    nil,
			wantSent:   []string{"ok:order"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := &Notifier{}
			status, err := tt.finalize("order", tt.validate, n)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", status, tt.wantStatus)
			}
			if !reflect.DeepEqual(n.Sent, tt.wantSent) {
				t.Fatalf("Sent = %v, want %v", n.Sent, tt.wantSent)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The buggy case is the sharpest lesson here: `FinalizeBuggy` returns `status
== "committed"` and `err == nil` for an order whose validation FAILED — the
caller cannot tell anything went wrong, and the audit log backs up the lie
with "ok:order". `FinalizeFixed` fixes it purely by removing the `:=` shadow;
nothing else about the control flow changes. Named returns plus `defer` are
a legitimate pattern for consistent cleanup and logging, but every branch
must assign to the named identifiers directly — the moment a nested `:=`
reintroduces a variable of the same name, the deferred closure and the final
return silently stop tracking what actually happened.

## Resources

- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — how deferred functions interact with named results.
- [Go spec: Return statements](https://go.dev/ref/spec#Return_statements) — how a bare `return` reads the current named result values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-reused-decode-buffer-pointer-capture.md](10-reused-decode-buffer-pointer-capture.md) | Next: [12-per-tenant-billing-shared-accumulator.md](12-per-tenant-billing-shared-accumulator.md)
