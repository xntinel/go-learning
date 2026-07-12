# Exercise 1: Structured error logging in a service layer with slog

The first job of error observability is to make a failed operation leave a
machine-readable trail. This module builds a repository-style service whose
`Get`/`Put` wrap a sentinel with `%w`, log the failure through `slog` with
request-scoped fields, and return the wrapped error unchanged to the caller — so
the log line is rich *and* the `errors.Is` chain survives.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
errlog/                      independent module: example.com/errlog
  go.mod                     go 1.25
  service.go                 ErrNotFound, ErrInvalid; Service.Get/Put log via slog and wrap with %w
  cmd/
    demo/
      main.go                runnable demo: a hit, a miss, an invalid put
  service_test.go            buffer-capture log assertions + errors.Is chain assertions
```

- Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
- Implement: a `Service` with `Get(ctx, id)` and `Put(ctx, id, value)` that wrap `ErrNotFound`/`ErrInvalid` with `fmt.Errorf("%w")`, log via `slog` with `op`, `id`, and `err` fields using `ErrorContext`, and return the wrapped error.
- Test: capture logs into a `bytes.Buffer` with a text handler; assert the buffer carries `op=Get`, `id=missing`, an `err=` field; assert the returned error still satisfies `errors.Is(err, ErrNotFound)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why the error is an attribute, not a message

The whole exercise turns on one line. When `Get` misses, it builds
`err := fmt.Errorf("get %q: %w", id, ErrNotFound)` and then logs it as
`logger.ErrorContext(ctx, "operation failed", "err", err)`. Two properties fall
out of that shape and both matter.

First, because the error is wrapped with `%w`, the sentinel is still reachable:
the caller can write `errors.Is(err, ErrNotFound)` and get `true`, and can branch
on it (return 404, retry, whatever). Logging did not consume the error; `Get`
still returns it. Observing an error and handling it are independent, and neither
should destroy the other.

Second, because the error is passed as the *value* of an `"err"` attribute rather
than pre-formatted into the message string, the structured handler renders it as
its own field. A JSON handler would emit `"err":"get \"missing\": not found"` as
a queryable key; a downstream enrichment (`LogValuer`, Exercise 4) would expand
it into a nested object. The anti-pattern `logger.Error(err.Error())` throws all
of that away — it produces a message with no `err` field and no chain to match on.

The `logger.With("op", "Get", "id", id)` call binds the request-scoped fields
once so every line this operation emits carries them; `ErrorContext` is used
(rather than `Error`) so a context-aware handler — the one you build in
Exercise 3 — can later stamp correlation IDs onto the same record without any
change here.

Create `service.go`:

```go
package errlog

import (
	"context"
	"fmt"
	"log/slog"
)

// Sentinels the caller can match with errors.Is. They are wrapped, never
// returned bare, so the log line and the match both work.
var (
	ErrNotFound = fmt.Errorf("not found")
	ErrInvalid  = fmt.Errorf("invalid")
)

// Service is a repository-style layer that observes its own failures: it logs
// each error with structured fields and returns the wrapped error unchanged.
type Service struct {
	logger *slog.Logger
	byID   map[string]string
}

// New returns a Service that logs through the given logger.
func New(logger *slog.Logger) *Service {
	return &Service{logger: logger, byID: make(map[string]string)}
}

// Get returns the value for id. On a miss it wraps ErrNotFound with %w, logs the
// failure with op/id/err, and returns the wrapped error so errors.Is still works.
func (s *Service) Get(ctx context.Context, id string) (string, error) {
	logger := s.logger.With("op", "Get", "id", id)
	v, ok := s.byID[id]
	if !ok {
		err := fmt.Errorf("get %q: %w", id, ErrNotFound)
		logger.ErrorContext(ctx, "operation failed", "err", err)
		return "", err
	}
	return v, nil
}

// Put stores value under id. An empty id wraps ErrInvalid, is logged, and
// returned; a valid put stores and returns nil without logging.
func (s *Service) Put(ctx context.Context, id, value string) error {
	logger := s.logger.With("op", "Put", "id", id)
	if id == "" {
		err := fmt.Errorf("put: %w", ErrInvalid)
		logger.ErrorContext(ctx, "operation failed", "err", err)
		return err
	}
	s.byID[id] = value
	return nil
}
```

### The runnable demo

The demo writes text logs to stderr and the operation results to stdout, so the
Expected-output block below is only the stdout lines. It stores a user, reads it
back (a hit), reads a missing id (a miss that logs and is matched with
`errors.Is`), and rejects an empty-id put.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"example.com/errlog"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := errlog.New(logger)
	ctx := context.Background()

	if err := s.Put(ctx, "u1", "alice"); err != nil {
		fmt.Println("put u1:", err)
	} else {
		fmt.Println("put u1: ok")
	}

	if v, err := s.Get(ctx, "u1"); err == nil {
		fmt.Println("get u1:", v)
	}

	if _, err := s.Get(ctx, "missing"); errors.Is(err, errlog.ErrNotFound) {
		fmt.Println("get missing: not found (chain intact)")
	}

	if err := s.Put(ctx, "", "x"); errors.Is(err, errlog.ErrInvalid) {
		fmt.Println("put empty: invalid (chain intact)")
	}
}
```

Run it:

```bash
go run ./cmd/demo 2>/dev/null
```

Expected output:

```
put u1: ok
get u1: alice
get missing: not found (chain intact)
put empty: invalid (chain intact)
```

### Tests

The tests capture the log stream into a `bytes.Buffer` through a text handler so
they can assert on the exact structured fields, and they assert the returned
error still satisfies `errors.Is` to prove logging did not swallow the chain.
`newService` is a helper returning the service and its buffer.

Create `service_test.go`:

```go
package errlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func newService(t *testing.T) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	return New(logger), &buf
}

func TestGetHit(t *testing.T) {
	t.Parallel()
	s, buf := newService(t)
	if err := s.Put(context.Background(), "u1", "alice"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Get(u1) err = %v, want nil", err)
	}
	if v != "alice" {
		t.Fatalf("Get(u1) = %q, want alice", v)
	}
	if buf.Len() != 0 {
		t.Fatalf("a successful path logged nothing to log, got %q", buf.String())
	}
}

func TestGetMissLogsStructuredFields(t *testing.T) {
	t.Parallel()
	s, buf := newService(t)
	_, err := s.Get(context.Background(), "missing")

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("returned err = %v, want errors.Is ErrNotFound (chain must survive logging)", err)
	}

	logs := buf.String()
	for _, want := range []string{"op=Get", "id=missing", "err="} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log missing %q; got %q", want, logs)
		}
	}
}

func TestPutRejectsEmptyID(t *testing.T) {
	t.Parallel()
	s, buf := newService(t)
	err := s.Put(context.Background(), "", "x")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is ErrInvalid", err)
	}
	if !strings.Contains(buf.String(), "op=Put") {
		t.Fatalf("log missing op=Put; got %q", buf.String())
	}
}

func TestWrappedErrorMessageIncludesSentinel(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	_, err := s.Get(context.Background(), "missing")
	if got := err.Error(); !strings.Contains(got, "not found") {
		t.Fatalf("err message = %q, want it to contain %q", got, "not found")
	}
}

func Example() {
	// Send logs to a discard buffer; the example only asserts the return values.
	s := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	_ = s.Put(context.Background(), "answer", "42")
	v, err := s.Get(context.Background(), "answer")
	fmt.Println(v, err)
	// Output: 42 <nil>
}
```

## Review

The service is correct when observing and handling stay independent: `Get` logs
the miss and still returns an error that satisfies `errors.Is(err, ErrNotFound)`,
so the caller keeps every option it had. The proof is
`TestGetMissLogsStructuredFields` asserting both the log fields *and* the chain in
one test — if either half fails, the shape is wrong. The success-path assertion
in `TestGetHit` (nothing logged) pins the other half of the contract: you observe
failures, not every call.

The mistake this exercise exists to prevent is `logger.Error(err.Error())`.
Formatting the error into the message drops the `err` attribute and the chain;
the text handler would show a message with no queryable `err=` field and
`errors.Is` upstream would fail. Keep the error as the attribute value. Also note
`With("op", ..., "id", ...)` binds the request fields once so every line the
operation emits carries them, and `ErrorContext` (not `Error`) leaves room for a
context-aware handler to enrich the same record later.

## Resources

- [`log/slog`](https://pkg.go.dev/log/slog) — `New`, `NewTextHandler`, `Logger.With`, `Logger.ErrorContext`.
- [Structured Logging with slog](https://go.dev/blog/slog) — the official introduction to attributes, handlers, and context methods.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` and the `%w` wrapping contract the log must not break.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-error-metrics-by-category.md](02-error-metrics-by-category.md)
