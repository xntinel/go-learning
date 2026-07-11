# Exercise 9: Observability: Log the Full Chain, Return a Sanitized Client Message

A failure has two audiences with opposite needs. The operator wants *everything* —
the full wrapped chain, ids, the SQL, the reason a charge was declined — to debug at
3am. The client must see *nothing* internal — no table names, no file paths, no
secrets. The production pattern that serves both is one failure, two views: log the
whole chain with `log/slog`, and return a sanitized `*PublicError` extracted with
`errors.As`. This module builds that boundary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
boundary/                   independent module: example.com/boundary
  go.mod                    go 1.24
  boundary.go               *PublicError; Handle logs full chain, returns sanitized view
  cmd/
    demo/
      main.go               runnable demo: client message vs captured log line
  boundary_test.go          secret in log, not in client value; internal-only -> generic
```

Files: `boundary.go`, `cmd/demo/main.go`, `boundary_test.go`.
Implement: a `*PublicError` (safe code + message), and `Handle(log, op, err)` that logs the full chain with `slog` and returns the `*PublicError` extracted via `errors.As` (or a generic one when none is present).
Test: an internal error wrapping a `*PublicError` plus a secret — assert the returned value carries only the public message (no secret substring) and that a `slog` handler writing to a buffer captured the full chain including the secret; a negative internal-only case yields a generic message, never the raw cause.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/boundary/cmd/demo
cd ~/go-exercises/boundary
go mod init example.com/boundary
go mod edit -go=1.24
```

### One failure, two views

The chain that reaches the service boundary carries both kinds of information at
once: the safe, user-facing part (a `*PublicError` saying "card was declined") and
the sensitive part (the wrapped cause containing a connection string or a raw
decline reason). `Handle` splits them. It logs the *entire* `err` with `slog.Any` —
`err.Error()` renders the whole chain, secret and all, into the log record, which is
correct: the log is an internal, access-controlled surface. Then it uses
`errors.As(err, &pe)` to pull out the `*PublicError` and returns *that* — never the
raw `err` — to the caller.

The sanitization is structural, not a string-scrubbing afterthought. Because
`*PublicError` is a distinct type whose fields are deliberately safe, returning it
*is* the redaction: there is no code path by which the wrapped secret reaches the
client, since the client only ever receives the `*PublicError` value. If no
`*PublicError` is present in the chain — an unexpected internal failure the code did
not classify — `Handle` returns a generic `*PublicError` ("an internal error
occurred") rather than leaking the raw cause. The full chain is still logged, so the
operator can diagnose it; the client just sees the generic message.

This is the concepts file's "error text is a debugging surface *and* a security
surface" made into code. The two surfaces are different values: `slog` gets the
chain, the caller gets the `*PublicError`.

Create `boundary.go`:

```go
package boundary

import (
	"errors"
	"log/slog"
)

// PublicError is the sanitized, client-safe view of a failure: a stable machine
// code and a user-facing message. It carries no internal detail by construction.
type PublicError struct {
	Code    string
	Message string
}

func (e *PublicError) Error() string { return e.Code + ": " + e.Message }

// Handle is the service boundary. It logs the FULL wrapped chain (internal surface)
// and returns only the sanitized *PublicError (public surface). When the chain has
// no *PublicError, it returns a generic one and never leaks the raw cause.
func Handle(log *slog.Logger, op string, err error) *PublicError {
	if err == nil {
		return nil
	}
	// Log everything: err.Error() renders the whole chain, including sensitive causes.
	log.Error("operation failed", slog.String("op", op), slog.Any("error", err))

	var pe *PublicError
	if errors.As(err, &pe) {
		return pe
	}
	return &PublicError{Code: "internal_error", Message: "an internal error occurred"}
}
```

### The runnable demo

The demo builds an internal error that wraps a `*PublicError` and a secret, sends
the log to a buffer (with timestamps stripped for reproducibility), and prints three
facts: what the client sees, whether the client value leaks the secret, and whether
the log captured it. That contrast is the whole lesson.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"

	"example.com/boundary"
)

func main() {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	pub := &boundary.PublicError{Code: "payment_declined", Message: "card was declined"}
	secret := fmt.Errorf("stripe: insufficient_funds (customer_token=sk_live_9x8y7z)")
	internal := fmt.Errorf("charge user 42: %w (cause: %v)", pub, secret)

	client := boundary.Handle(log, "ChargeCard", internal)

	fmt.Printf("client sees:         %s\n", client)
	fmt.Printf("client leaks secret: %v\n", strings.Contains(client.Error(), "sk_live"))
	fmt.Printf("log captured secret: %v\n", strings.Contains(buf.String(), "sk_live"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
client sees:         payment_declined: card was declined
client leaks secret: false
log captured secret: true
```

### Tests

The test drives `Handle` with a `slog.NewTextHandler` writing to a
`bytes.Buffer`, so it can assert on the actual log output. The primary case wraps a
`*PublicError` and a secret: it asserts the returned client value carries the public
message and *not* the secret, and that the buffer (the log) *does* contain the
secret — proving the two views diverge exactly as intended. The negative case passes
an internal-only error and asserts the client gets the generic message and never the
raw cause, while the log still captured the cause.

Create `boundary_test.go`:

```go
package boundary

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func newBufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	return log, &buf
}

func TestHandleSplitsViews(t *testing.T) {
	t.Parallel()
	log, buf := newBufLogger()

	pub := &PublicError{Code: "payment_declined", Message: "card was declined"}
	secret := fmt.Errorf("db dsn=postgres://admin:hunter2@10.0.0.1/prod")
	internal := fmt.Errorf("charge user 42: %w (cause: %v)", pub, secret)

	client := Handle(log, "ChargeCard", internal)

	if client.Code != "payment_declined" || client.Message != "card was declined" {
		t.Errorf("client = %+v, want the public error", client)
	}
	if strings.Contains(client.Error(), "hunter2") {
		t.Errorf("client value leaked the secret: %q", client.Error())
	}
	if !strings.Contains(buf.String(), "hunter2") {
		t.Errorf("log should have captured the full chain including the secret; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "ChargeCard") {
		t.Errorf("log should include the op name; got %q", buf.String())
	}
}

func TestHandleUnclassifiedIsGeneric(t *testing.T) {
	t.Parallel()
	log, buf := newBufLogger()

	internal := fmt.Errorf("panic recovered: nil map write at /srv/app/store.go:88")
	client := Handle(log, "SaveOrder", internal)

	if client.Code != "internal_error" {
		t.Errorf("client.Code = %q, want internal_error", client.Code)
	}
	if strings.Contains(client.Error(), "store.go") {
		t.Errorf("generic client message leaked internal detail: %q", client.Error())
	}
	if !strings.Contains(buf.String(), "store.go") {
		t.Errorf("log should still capture the raw cause; got %q", buf.String())
	}
}

func TestHandleNil(t *testing.T) {
	t.Parallel()
	log, _ := newBufLogger()
	if got := Handle(log, "op", nil); got != nil {
		t.Errorf("Handle(nil) = %v, want nil", got)
	}
}

func ExamplePublicError() {
	pe := &PublicError{Code: "not_found", Message: "user does not exist"}
	fmt.Println(pe)
	// Output: not_found: user does not exist
}
```

## Review

The boundary is correct when the two views provably diverge: the returned
`*PublicError` carries only the safe code and message, and the `slog` buffer carries
the full chain including the secret. `TestHandleSplitsViews` asserts both directions
at once — secret absent from the client value, present in the log — which is the
property that lets you debug without leaking. The generic-fallback case is the
safety net: an error the code did not classify still yields a bland client message
and never the raw cause, because the client only ever receives a `*PublicError`,
never `err` itself. The mistake to avoid is "sanitizing" by string-scrubbing the raw
error; returning a distinct safe type makes leakage structurally impossible rather
than dependent on catching every sensitive substring.

## Resources

- [log/slog](https://pkg.go.dev/log/slog) — structured logging; `Logger.Error`, `slog.Any`, `NewTextHandler`.
- [errors.As](https://pkg.go.dev/errors#As) — extracting the typed `*PublicError` from the chain.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — wrapping internal context with `%w` while keeping a public type reachable.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../07-multiple-error-returns/00-concepts.md](../07-multiple-error-returns/00-concepts.md)
