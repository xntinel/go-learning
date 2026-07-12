# Exercise 22: Panic Recovery — Deferred Cleanup Survives Panic Unwinding

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A handler that recovers from a panic but does so in a defer registered
*after* its resource-cleanup defers has a subtle ordering bug: LIFO means
the resource cleanups registered later would run before the recover, which
happens to work, but a recover registered in the wrong position relative to
cleanup can just as easily let a panic escape past resources that never got
a chance to close. This module builds the pattern the right way around —
recover deferred first, so it runs last — and proves with a table of panic
and non-panic outcomes that every resource is always closed by the time the
panic is converted into an ordinary error. The module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
handler/                    independent module: example.com/panic-recovery-deferred-cleanup
  go.mod                     go 1.24
  handler.go                 Resource, PanicErr, Handle(newResource, op) (string, error)
  cmd/
    demo/
      main.go                runnable demo: op succeeds, then op panics
  handler_test.go            table over success/error/panic(string)/panic(error)/mid-op panic
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `Resource` (`Close`), `PanicErr` (`Error`, `Unwrap`), `Handle(newResource func(string) *Resource, op func(a, b *Resource) (string, error)) (result string, err error)`.
- Test: a table over a clean success, a normal returned error, a string panic, an error-value panic, and a panic partway through using both resources — asserting the error shape and that both resources always end up closed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/22-panic-recovery-deferred-cleanup/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/22-panic-recovery-deferred-cleanup
go mod edit -go=1.24
```

### Why the recover defer goes first, not last

`Handle` registers its recover closure as the very first defer, before
either resource is even acquired. LIFO makes that choice matter: with the
recover defer registered first, it is the *last* one to run during an
unwind, after `b.Close()` and `a.Close()` — both registered later, in
acquisition order — have already fired. So by the time the recover closure
actually calls `recover()` and decides to convert the panic into `err`,
every resource `op` might have panicked while using is already closed. If
the recover closure were instead registered *last*, after both resource
defers, LIFO would flip that: recover would run first, converting the panic
into a normal return, and only *then* would `b.Close()` and `a.Close()` run —
which still works for this exact function, since all defers in a frame run
regardless of whether an earlier one recovered, but it inverts the mental
model in a way that gets dangerous the moment a later defer is added that
assumes the panic has already been dealt with. Registering recover first is
the idiomatic shape specifically because it makes "cleanup happens, *then*
the panic is silenced" the ordering the code reads as, not an ordering that
happens to also be true.

Create `handler.go`:

```go
package handler

import "fmt"

// Resource is a stand-in for anything that must be released on every exit
// path -- a file, a lock, a pooled connection.
type Resource struct {
	Name   string
	Closed bool
}

// Close marks the resource released.
func (r *Resource) Close() { r.Closed = true }

// PanicErr wraps a recovered panic value as a regular error, so a caller
// can handle "the operation panicked" the same way it handles any other
// failure, using errors.As/errors.Is instead of a bespoke recover call of
// its own.
type PanicErr struct {
	Value any
}

func (e *PanicErr) Error() string {
	return fmt.Sprintf("recovered from panic: %v", e.Value)
}

// Unwrap lets errors.Is/errors.As see through to the panic value when it
// was itself an error -- the common case of `panic(fmt.Errorf(...))`.
func (e *PanicErr) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Handle acquires two resources via newResource, runs op against them, and
// converts any panic op raises into a returned *PanicErr -- without
// skipping either resource's cleanup.
//
// The recover closure is deferred first, before either resource is
// acquired, so LIFO order runs it *last*: b.Close() first, then a.Close(),
// and only after both have already run does the recover closure decide
// whether to convert a panic into err. That ordering means every resource
// is released by the time the panic is silenced, regardless of what op
// does or which resource it panics while using.
func Handle(newResource func(name string) *Resource, op func(a, b *Resource) (string, error)) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicErr{Value: r}
		}
	}()

	a := newResource("A")
	defer a.Close()

	b := newResource("B")
	defer b.Close()

	return op(a, b)
}
```

### The runnable demo

The demo runs `Handle` once with an `op` that succeeds and once with an
`op` that panics, printing the result, error, and both resources' closed
state after the panic case.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/panic-recovery-deferred-cleanup"
)

func main() {
	var created []*handler.Resource
	newResource := func(name string) *handler.Resource {
		r := &handler.Resource{Name: name}
		created = append(created, r)
		return r
	}

	fmt.Println("-- op succeeds --")
	result, err := handler.Handle(newResource, func(a, b *handler.Resource) (string, error) {
		return "ok:" + a.Name + b.Name, nil
	})
	fmt.Println("result:", result, "error:", err)

	fmt.Println("-- op panics --")
	created = nil
	result, err = handler.Handle(newResource, func(a, b *handler.Resource) (string, error) {
		panic("boom")
	})
	fmt.Println("result:", result, "error:", err)
	fmt.Println("resource A closed:", created[0].Closed, "resource B closed:", created[1].Closed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
-- op succeeds --
result: ok:AB error: <nil>
-- op panics --
result:  error: recovered from panic: boom
resource A closed: true resource B closed: true
```

### Tests

`TestHandleConvertsPanicWithoutSkippingCleanup` runs a table of five cases —
success, a normal returned error, a string panic, an error-value panic, and
a panic that happens after `op` has already mutated both resources — using
`errors.As` to confirm panics surface as `*PanicErr` (and non-panics do
not), `errors.Is` to confirm an error-value panic is still reachable through
`Unwrap`, and a direct check that both resources are closed in every single
case, panic or not.

Create `handler_test.go`:

```go
package handler

import (
	"errors"
	"testing"
)

func TestHandleConvertsPanicWithoutSkippingCleanup(t *testing.T) {
	errDownstream := errors.New("downstream failure")

	tests := []struct {
		name       string
		op         func(a, b *Resource) (string, error)
		wantResult string
		wantErrIs  error // checked with errors.Is when set
		wantPanic  bool  // wantErr should be a *PanicErr
	}{
		{
			name:       "op succeeds",
			op:         func(a, b *Resource) (string, error) { return "ok:" + a.Name + b.Name, nil },
			wantResult: "ok:AB",
		},
		{
			name:      "op returns a normal error",
			op:        func(a, b *Resource) (string, error) { return "", errDownstream },
			wantErrIs: errDownstream,
		},
		{
			name:      "op panics with a string",
			op:        func(a, b *Resource) (string, error) { panic("boom") },
			wantPanic: true,
		},
		{
			name:      "op panics with an error value",
			op:        func(a, b *Resource) (string, error) { panic(errDownstream) },
			wantPanic: true,
			wantErrIs: errDownstream,
		},
		{
			name: "op panics after partially using both resources",
			op: func(a, b *Resource) (string, error) {
				a.Name = "used-" + a.Name
				b.Name = "used-" + b.Name
				panic("boom mid-operation")
			},
			wantPanic: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var created []*Resource
			newResource := func(name string) *Resource {
				r := &Resource{Name: name}
				created = append(created, r)
				return r
			}

			result, err := Handle(newResource, tc.op)

			if result != tc.wantResult {
				t.Errorf("result = %q, want %q", result, tc.wantResult)
			}

			var panicErr *PanicErr
			if tc.wantPanic {
				if !errors.As(err, &panicErr) {
					t.Fatalf("err = %v (%T), want a *PanicErr", err, err)
				}
			} else if panicErr != nil {
				t.Fatalf("did not expect a panic, got %v", err)
			}

			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("err = %v, want it to wrap/equal %v", err, tc.wantErrIs)
			}

			if len(created) != 2 {
				t.Fatalf("expected 2 resources to be created, got %d", len(created))
			}
			if !created[0].Closed || !created[1].Closed {
				t.Fatalf("expected both resources closed, got a=%v b=%v", created[0].Closed, created[1].Closed)
			}
		})
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

`Handle` is correct when both resources are closed in every single row of
the table, whether `op` returns cleanly, returns an ordinary error, or
panics with any kind of value — and when a panic never escapes past
`Handle` to crash the caller. Both properties come from the same
decision: registering the recover closure before either resource is
acquired, so LIFO guarantees every resource-cleanup defer runs before the
panic is silenced. The mistake this design avoids is a recover placed after
the resource defers (which happens to still work here, by coincidence of
this function's simple shape) or, worse, no recover at all around code that
acquires cleanup-requiring resources — where a panic still runs `defer`
cleanup on its way up (that part is automatic), but nothing ever converts
it into a value the caller's normal error-handling code can see, so it
crashes the whole goroutine instead.

## Resources

- [Go Specification: Handling panics](https://go.dev/ref/spec#Handling_panics) — how `recover` interacts with deferred function execution during a panic.
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — the canonical recover-to-error pattern this handler implements.
- [errors.As and errors.Unwrap](https://pkg.go.dev/errors#As) — letting a wrapped panic value stay reachable through the standard error-inspection functions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-worker-pool-deferred-cleanup-signal.md](21-worker-pool-deferred-cleanup-signal.md) | Next: [23-cascading-timeout-nested-defers.md](23-cascading-timeout-nested-defers.md)
