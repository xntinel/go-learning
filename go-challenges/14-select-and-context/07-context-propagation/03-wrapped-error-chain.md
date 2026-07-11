# Exercise 3: Preserve the Cancellation Cause Through Every Layer

A cancellation error that survives a client hang-up is useless at the top of the
stack if the layers between destroyed its identity on the way up. This exercise
builds a three-layer call — repo to service to handler — where each layer wraps
the downstream error, and proves that the top-level error still satisfies
`errors.Is(err, context.DeadlineExceeded)`. It also ships the counter-example: one
layer that wraps with `%v` instead of `%w`, breaking the chain, so you can see
exactly why the verb choice is not cosmetic.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
errchain/                    independent module: example.com/errchain
  go.mod                     go 1.24
  errchain.go                repoGet -> Service.Load -> Handler.Serve, each wrapping with %w;
                             BrokenHandler.Serve wraps with %v
  cmd/
    demo/
      main.go                drives a 10ms deadline through all three layers
  errchain_test.go           %w chain matches errors.Is; %v chain does not
```

Files: `errchain.go`, `cmd/demo/main.go`, `errchain_test.go`.
Implement: three layers that each add a prefix and wrap the downstream error with
`fmt.Errorf("layer: %w", err)`, plus a `BrokenHandler` that uses `%v`.
Test: a 10ms timeout through all three layers still matches
`errors.Is(err, context.DeadlineExceeded)`; the `%v` variant does not; the error
string contains every layer prefix.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errchain/cmd/demo
cd ~/go-exercises/errchain
go mod init example.com/errchain
go mod edit -go=1.24
```

### Why `%w` and `%v` are not interchangeable

`fmt.Errorf` with `%w` produces an error that *wraps* its argument: the argument
stays reachable through `errors.Unwrap`, and `errors.Is`/`errors.As` walk that
chain. `%v` produces an error whose message merely *contains the text* of its
argument; the argument itself is gone, flattened into a string. From the outside
the two error messages can look identical — `handler: service: repo: context
deadline exceeded` either way — but only the `%w` version answers
`errors.Is(err, context.DeadlineExceeded)` with true.

This is the difference between a log line and a decision. At the top of a request
you often want both: a human-readable message that shows the path the error took
(`handler: service: repo: ...`), and a machine-readable classification that lets
you emit the right metric and status code. `%w` gives you both simultaneously —
the prefixes accumulate in the message while the wrapped sentinel stays matchable.
`%v` gives you only the message; the classification silently degrades to false,
and a handler that branches on `errors.Is(err, context.DeadlineExceeded)` to
return a 504 instead of a 500 will now return the wrong status for every timeout.
The bug is invisible in tests that only check the error string, which is why this
module tests the *classification*, not the text.

The chain here is three deep on purpose: a deadline set at the handler flows down
to `repoGet`, which loses the race against `ctx.Done()` and returns
`ctx.Err()` wrapped once; the service wraps it a second time; the handler a third.
`TestWrappedErrorMatchesContext` asserts the sentinel is still matchable after all
three wraps, and `TestBrokenWrapDoesNotMatch` asserts that swapping the handler's
`%w` for `%v` breaks it.

Create `errchain.go`:

```go
package errchain

import (
	"context"
	"fmt"
	"time"
)

// repoGet is the leaf: it races a simulated read against ctx.Done and wraps the
// context error with %w so the cause stays classifiable one layer up.
func repoGet(ctx context.Context, key string, delay time.Duration) (string, error) {
	select {
	case <-time.After(delay):
		return "value-for-" + key, nil
	case <-ctx.Done():
		return "", fmt.Errorf("repo: %w", ctx.Err())
	}
}

// Service is the middle layer.
type Service struct {
	delay time.Duration
}

// NewService returns a Service whose repo reads take delay.
func NewService(delay time.Duration) *Service { return &Service{delay: delay} }

// Load forwards ctx to the repo and wraps any error with %w.
func (s *Service) Load(ctx context.Context, key string) (string, error) {
	v, err := repoGet(ctx, key, s.delay)
	if err != nil {
		return "", fmt.Errorf("service: %w", err)
	}
	return v, nil
}

// Handler is the top layer. It wraps the service error with %w, preserving the
// cause all the way to the caller.
type Handler struct {
	svc *Service
}

// NewHandler wraps svc in a Handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Serve returns the value or an error whose chain still matches the context
// sentinel through errors.Is.
func (h *Handler) Serve(ctx context.Context, key string) (string, error) {
	v, err := h.svc.Load(ctx, key)
	if err != nil {
		return "", fmt.Errorf("handler: %w", err)
	}
	return v, nil
}

// BrokenHandler is identical to Handler except it wraps with %v, flattening the
// cause to a string and breaking errors.Is classification.
type BrokenHandler struct {
	svc *Service
}

// NewBrokenHandler wraps svc in the deliberately-broken handler.
func NewBrokenHandler(svc *Service) *BrokenHandler { return &BrokenHandler{svc: svc} }

// Serve wraps with %v, so the returned error's message reads correctly but no
// longer matches context.DeadlineExceeded via errors.Is.
func (h *BrokenHandler) Serve(ctx context.Context, key string) (string, error) {
	v, err := h.svc.Load(ctx, key)
	if err != nil {
		return "", fmt.Errorf("handler: %v", err)
	}
	return v, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/errchain"
)

func main() {
	svc := errchain.NewService(80 * time.Millisecond)
	good := errchain.NewHandler(svc)
	broken := errchain.NewBrokenHandler(svc)

	ctxGood, cancelGood := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelGood()
	_, gerr := good.Serve(ctxGood, "u1")
	fmt.Printf("good:   msg=%q matches=%v\n", gerr, errors.Is(gerr, context.DeadlineExceeded))

	ctxBad, cancelBad := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelBad()
	_, berr := broken.Serve(ctxBad, "u1")
	fmt.Printf("broken: msg=%q matches=%v\n", berr, errors.Is(berr, context.DeadlineExceeded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good:   msg="handler: service: repo: context deadline exceeded" matches=true
broken: msg="handler: service: repo: context deadline exceeded" matches=false
```

The two messages are byte-for-byte identical; only the `matches` column differs.
That is the entire argument for `%w`.

### Tests

Create `errchain_test.go`:

```go
package errchain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWrappedErrorMatchesContext(t *testing.T) {
	t.Parallel()

	h := NewHandler(NewService(80 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := h.Serve(ctx, "u1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to match DeadlineExceeded after three wraps", err)
	}
	for _, prefix := range []string{"handler:", "service:", "repo:"} {
		if !strings.Contains(err.Error(), prefix) {
			t.Errorf("error %q missing layer prefix %q", err, prefix)
		}
	}
}

func TestBrokenWrapDoesNotMatch(t *testing.T) {
	t.Parallel()

	h := NewBrokenHandler(NewService(80 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := h.Serve(ctx, "u1")
	if err == nil {
		t.Fatal("expected an error from the short deadline")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("flattened error must NOT match DeadlineExceeded; the chain was broken by the string verb")
	}
	// The message still reads correctly even though classification is lost.
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error %q should still contain the flattened message", err)
	}
}

func TestUnwrapReachesSentinel(t *testing.T) {
	t.Parallel()

	h := NewHandler(NewService(80 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := h.Serve(ctx, "u1")
	// Walk the chain manually to confirm the sentinel is genuinely reachable.
	found := false
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e == context.DeadlineExceeded {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("errors.Unwrap chain of %v never reaches context.DeadlineExceeded", err)
	}
}

func ExampleHandler_Serve() {
	h := NewHandler(NewService(50 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, err := h.Serve(ctx, "u1")
	fmt.Println(err)
	fmt.Println(errors.Is(err, context.DeadlineExceeded))
	// Output:
	// handler: service: repo: context deadline exceeded
	// true
}
```

## Review

The chain is correct when the message and the classification agree in the `%w`
case and disagree in the `%v` case. The pair of tests is the point: identical
error strings, opposite `errors.Is` results. If `TestBrokenWrapDoesNotMatch` ever
starts passing `errors.Is`, someone has changed the broken handler back to `%w`
and erased the lesson. In real code the rule is simpler than the demonstration:
any layer that adds context to an error it intends to leave classifiable uses
`%w`; reserve `%v` for the rare case where you deliberately want to *sever* the
chain and expose only a sanitized message (for example, at a trust boundary where
the underlying cause must not leak to a client). Run `go test -race`; the timing
here is single-goroutine per call, so races would only appear if the repo shared
mutable state, which it does not.

## Resources

- [`fmt.Errorf` and `%w`](https://pkg.go.dev/fmt#Errorf) — the wrapping verb.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is`, `errors.As`, `errors.Unwrap`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the wrapping model.

---

Prev: [02-parallel-fanout-shared-context.md](02-parallel-fanout-shared-context.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-cancellation-cause-classification.md](04-cancellation-cause-classification.md)
