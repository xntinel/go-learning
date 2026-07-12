# Exercise 9: Table-Driven Tests With Stubbed Dependency Errors and Error Classification

A service method often branches on *which kind* of error a downstream dependency
returned: a missing resource becomes a 404, a timeout a 503, a validation problem
a 400, anything else a 500. A configurable stub that returns a canned error per
case, driven by a table, exercises the whole classification matrix — including
`errors.Is` through wrapping and `errors.As` for structured errors — with the
original cause still unwrappable.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
errclassify/                 independent module: example.com/errclassify
  go.mod                     go 1.26
  handler.go                 Store; Handler.Fetch; classify to HTTP status
  cmd/
    demo/
      main.go                runnable demo classifying two store errors
  handler_test.go           table over downstream errors -> status; Is/As asserts
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `Handler.Fetch(ctx, id) (int, error)` that maps a `Store` error to an HTTP status: `ErrNotFound` to 404, `context.DeadlineExceeded` to 503, a `*ValidationError` to 400, anything else to 500; success is 200.
- Test: a stub `Store` returning a per-case injected error; a table asserting the mapped status, the classification via `errors.Is`/`errors.As`, and that the original cause remains unwrappable.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/11-mock-interfaces-for-testing/09-table-driven-stubbed-error-classification/cmd/demo
cd go-solutions/08-interfaces/11-mock-interfaces-for-testing/09-table-driven-stubbed-error-classification
```

### Classification is the contract, and a stub steers it

`Handler.Fetch` calls the `Store`, and when the store fails it must translate the
failure into the right HTTP status for the caller. That translation is the unit's
real logic, and to test every branch you need the store to fail in every distinct
way on demand — which the real store will not do reliably. A *stub* supplies each
canned error: the test injects `ErrNotFound`, or `context.DeadlineExceeded`, or a
`*ValidationError`, or an anonymous error, and asserts the status the handler
produced. This is the purest use of a stub — dictate the dependency's output to
drive the SUT down a chosen branch — and a table expresses the whole matrix
compactly, one row per error kind.

### Is versus As, wrapping, and Join

`classify` uses two different classification tools because errors carry
information two different ways. `errors.Is(err, ErrNotFound)` walks the wrap chain
looking for a specific *sentinel value*, so it matches whether the store returned
`ErrNotFound` bare, wrapped with `fmt.Errorf("...: %w", ErrNotFound)`, or joined
with another error via `errors.Join` — the table includes all three to prove the
classification is robust to wrapping. `errors.As(err, &ve)` walks the chain
looking for a value of a specific *type* (`*ValidationError`) and binds it, which
is how you classify a structured error and read its fields (the offending field
name) at the same time. The handler wraps the store error with `%w` before
returning, so the caller gets both the mapped status *and* an error whose original
cause is still unwrappable with `errors.Is`/`errors.As` — the status is a summary,
not a replacement for the cause.

Create `handler.go`:

```go
package errclassify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is the sentinel a Store returns for a missing resource.
var ErrNotFound = errors.New("resource not found")

// ValidationError is a structured error classified with errors.As.
type ValidationError struct {
	Field string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid field %q", e.Field)
}

// Store is the downstream port.
type Store interface {
	Fetch(ctx context.Context, id string) ([]byte, error)
}

// Handler serves a resource, mapping store failures to HTTP statuses.
type Handler struct {
	store Store
}

func NewHandler(s Store) *Handler { return &Handler{store: s} }

// Fetch returns the HTTP status and, on failure, an error wrapping the cause.
func (h *Handler) Fetch(ctx context.Context, id string) (int, error) {
	if _, err := h.store.Fetch(ctx, id); err != nil {
		return classify(err), fmt.Errorf("fetch %s: %w", id, err)
	}
	return http.StatusOK, nil
}

// classify maps a downstream error to an HTTP status.
func classify(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
```

### The runnable demo

The demo classifies a not-found store and a healthy store, printing each status.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/errclassify"
)

// stubStore returns a fixed error (or success when err is nil).
type stubStore struct {
	err error
}

func (s stubStore) Fetch(context.Context, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte("payload"), nil
}

func main() {
	ctx := context.Background()

	missing := errclassify.NewHandler(stubStore{err: errclassify.ErrNotFound})
	status, _ := missing.Fetch(ctx, "res-1")
	fmt.Printf("missing -> %d\n", status)

	ok := errclassify.NewHandler(stubStore{})
	status, _ = ok.Fetch(ctx, "res-2")
	fmt.Printf("ok -> %d\n", status)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
missing -> 404
ok -> 200
```

### Tests

`TestFetchClassifiesErrors` is table-driven over every error kind, each injected
through the stub. Each subtest asserts the mapped status, the classification via
`errors.Is`/`errors.As`, and that the original injected cause is still unwrappable
from the returned error. The `Example` documents `classify` on a wrapped sentinel.

Create `handler_test.go`:

```go
package errclassify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// stubStore returns a per-case injected error, or success when err is nil.
type stubStore struct {
	err error
}

func (s stubStore) Fetch(context.Context, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte("payload"), nil
}

func TestFetchClassifiesErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		injected   error
		wantStatus int
		wantIs     error // errors.Is target for the classification sentinel
		validation bool  // classify via errors.As on *ValidationError
	}{
		{name: "not found", injected: ErrNotFound, wantStatus: http.StatusNotFound, wantIs: ErrNotFound},
		{name: "wrapped not found", injected: fmt.Errorf("db read: %w", ErrNotFound), wantStatus: http.StatusNotFound, wantIs: ErrNotFound},
		{name: "joined not found", injected: errors.Join(errors.New("audit failed"), ErrNotFound), wantStatus: http.StatusNotFound, wantIs: ErrNotFound},
		{name: "timeout", injected: context.DeadlineExceeded, wantStatus: http.StatusServiceUnavailable, wantIs: context.DeadlineExceeded},
		{name: "validation", injected: &ValidationError{Field: "email"}, wantStatus: http.StatusBadRequest, validation: true},
		{name: "unknown", injected: errors.New("boom"), wantStatus: http.StatusInternalServerError},
		{name: "success", injected: nil, wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := NewHandler(stubStore{err: tc.injected})
			status, err := h.Fetch(context.Background(), "res-1")

			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.injected == nil {
				if err != nil {
					t.Fatalf("unexpected error on success: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want a wrapped error, got nil")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("errors.Is(err, %v) = false", tc.wantIs)
			}
			if tc.validation {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("errors.As did not find *ValidationError in %v", err)
				}
				if ve.Field != "email" {
					t.Fatalf("ValidationError.Field = %q, want email", ve.Field)
				}
			}
			// The original injected cause must remain unwrappable.
			if !errors.Is(err, tc.injected) {
				t.Fatalf("original cause not unwrappable from %v", err)
			}
		})
	}
}

func ExampleHandler() {
	h := NewHandler(stubStore{err: fmt.Errorf("db read: %w", ErrNotFound)})
	status, _ := h.Fetch(context.Background(), "res-1")
	fmt.Println(status)
	// Output: 404
}
```

## Review

The stub is the right double here because the contract under test is a pure
mapping from the dependency's failure to an HTTP status — you dictate the failure,
then assert the mapping. The table makes the matrix legible and forces every
branch to be covered, and the wrapped and joined not-found rows prove the
classification uses `errors.Is` (chain-walking) rather than fragile equality or
string matching. `errors.As` handles the structured `*ValidationError`, binding it
so the test also reads its field.

The correctness discipline is that classifying an error must not discard it: the
handler wraps the cause with `%w`, so the returned error still answers
`errors.Is`/`errors.As` for the original — the last assertion in every row. A
handler that returned only a status, or a fresh error that broke the chain, would
lose the cause a caller needs to log or retry on. Assert the minimal true contract
(status plus classification), and keep the cause unwrappable.

## Resources

- [`errors.Is` and `errors.As`](https://pkg.go.dev/errors) — sentinel-value versus type-directed classification through a wrap chain.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining errors while keeping each discoverable by `errors.Is`.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusServiceUnavailable`, and the rest.

---

Back to [08-consumer-defined-interface-spy.md](08-consumer-defined-interface-spy.md) | Next: [../12-interface-pollution-anti-patterns/00-concepts.md](../12-interface-pollution-anti-patterns/00-concepts.md)
