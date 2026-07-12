# Exercise 7: Reusable Generic Options Helper with Collect-All Validation

The option machinery — a `func(*T) error`, a loop that applies them, a place to
collect errors — is identical for every type. This module writes it once with
generics and reuses it to configure two unrelated config structs, then contrasts
the two validation strategies: fail-fast (return on the first error) and
collect-all (`errors.Join` every problem so a config surface can report them all).

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
optkit/                          independent module: example.com/optkit
  go.mod                         go 1.26
  optkit.go                      Option[T], Apply[T], ApplyAll[T], WebhookConfig, WorkerConfig,
                                 WithURL, WithSecret, WithConcurrency, WithQueue, sentinel errors
  cmd/
    demo/
      main.go                    configures a worker (fail-fast) and a webhook (collect-all)
  optkit_test.go                 asserts collect-all finds both sentinels, fail-fast finds only the first
```

- Files: `optkit.go`, `cmd/demo/main.go`, `optkit_test.go`.
- Implement: `type Option[T any] func(*T) error`, `Apply[T any](*T, ...Option[T]) error` (fail-fast), `ApplyAll[T any](*T, ...Option[T]) error` (collect-all via `errors.Join`), and two config types reusing the same `Option[T]`.
- Test: apply three options where two are invalid; in collect-all mode assert `errors.Is` finds both sentinels; in fail-fast mode assert only the first; reuse `Apply` on both config types to prove genericity.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/07-generic-options-errors-join/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/07-generic-options-errors-join
```

### One options engine, many types

`type Option[T any] func(*T) error` is the whole idea: an option is a function
that configures a `*T`, and `T` can be anything. `Apply` and `ApplyAll` are the
two reusable drivers. Because they are generic, `Apply[WebhookConfig]` and
`Apply[WorkerConfig]` are the same code — you write the loop once and never again.
The two config types here (`WebhookConfig` and `WorkerConfig`) are deliberately
unrelated: they share no fields and no meaning, and yet they share the entire
options mechanism. That is the payoff generics buy over a hand-written option loop
per type.

### Fail-fast versus collect-all

`Apply` returns on the first option that fails. This is the right default for a
programmatic caller who fixes one problem, recompiles, and moves on. `ApplyAll`
runs every option, collects the failures, and returns `errors.Join(errs...)`. The
joined error is not a flattened string — it carries the original errors in a
hidden `Unwrap() []error`, so a caller can still `errors.Is` each sentinel out of
the aggregate, and a config UI can list every problem at once instead of making the
user fix them one at a time. The sentinel errors (`ErrEmptyURL`, `ErrWeakSecret`,
and so on) are package-level values wrapped with `%w`, which is what makes
`errors.Is` work through both the wrapping and the join. Choosing between the two
drivers is a deliberate API decision, not an accident of which loop you wrote
first.

Create `optkit.go`:

```go
package optkit

import (
	"errors"
	"fmt"
	"strings"
)

// Option configures a *T and may reject invalid input.
type Option[T any] func(*T) error

// Apply runs opts against target, returning on the first error (fail-fast).
func Apply[T any](target *T, opts ...Option[T]) error {
	for _, opt := range opts {
		if err := opt(target); err != nil {
			return err
		}
	}
	return nil
}

// ApplyAll runs every option and returns the joined errors (collect-all).
func ApplyAll[T any](target *T, opts ...Option[T]) error {
	var errs []error
	for _, opt := range opts {
		if err := opt(target); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Sentinel errors, wrapped with %w so errors.Is finds them through a join.
var (
	ErrEmptyURL       = errors.New("webhook url is empty")
	ErrInsecureURL    = errors.New("webhook url must be https")
	ErrWeakSecret     = errors.New("webhook secret too short")
	ErrBadConcurrency = errors.New("worker concurrency must be positive")
	ErrEmptyQueue     = errors.New("worker queue name is empty")
)

// WebhookConfig configures an outbound webhook.
type WebhookConfig struct {
	URL    string
	Secret string
}

// WithURL sets a required https URL.
func WithURL(u string) Option[WebhookConfig] {
	return func(c *WebhookConfig) error {
		u = strings.TrimSpace(u)
		if u == "" {
			return ErrEmptyURL
		}
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("%q: %w", u, ErrInsecureURL)
		}
		c.URL = u
		return nil
	}
}

// WithSecret sets a signing secret of at least 16 bytes.
func WithSecret(s string) Option[WebhookConfig] {
	return func(c *WebhookConfig) error {
		if len(s) < 16 {
			return fmt.Errorf("length %d: %w", len(s), ErrWeakSecret)
		}
		c.Secret = s
		return nil
	}
}

// WorkerConfig configures a background worker.
type WorkerConfig struct {
	Concurrency int
	Queue       string
}

// WithConcurrency sets a positive worker count.
func WithConcurrency(n int) Option[WorkerConfig] {
	return func(c *WorkerConfig) error {
		if n < 1 {
			return fmt.Errorf("got %d: %w", n, ErrBadConcurrency)
		}
		c.Concurrency = n
		return nil
	}
}

// WithQueue sets a non-empty queue name.
func WithQueue(name string) Option[WorkerConfig] {
	return func(c *WorkerConfig) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return ErrEmptyQueue
		}
		c.Queue = name
		return nil
	}
}
```

### The runnable demo

The demo configures a worker with fail-fast `Apply` (all valid) and a webhook with
collect-all `ApplyAll` (two invalid options), then walks the joined error's
`Unwrap() []error` to print every problem.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/optkit"
)

func main() {
	var worker optkit.WorkerConfig
	if err := optkit.Apply(&worker,
		optkit.WithConcurrency(4),
		optkit.WithQueue("jobs"),
	); err != nil {
		panic(err)
	}
	fmt.Printf("worker: concurrency=%d queue=%s\n", worker.Concurrency, worker.Queue)

	var hook optkit.WebhookConfig
	err := optkit.ApplyAll(&hook,
		optkit.WithURL("http://insecure"),
		optkit.WithSecret("short"),
	)
	fmt.Println("webhook config errors:")
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			fmt.Printf("  - %s\n", e)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker: concurrency=4 queue=jobs
webhook config errors:
  - "http://insecure": webhook url must be https
  - length 5: webhook secret too short
```

### Tests

`TestCollectAllFindsEverySentinel` applies two invalid options and asserts
`errors.Is` finds *both* sentinels inside the joined error.
`TestFailFastStopsAtFirst` applies the same options through `Apply` and asserts
only the first sentinel is present. `TestGenericReuse` applies options to both
config types in one test, proving the generic `Apply` compiles and validates each
type independently.

Create `optkit_test.go`:

```go
package optkit

import (
	"errors"
	"testing"
)

func TestCollectAllFindsEverySentinel(t *testing.T) {
	t.Parallel()

	var hook WebhookConfig
	err := ApplyAll(&hook,
		WithURL("http://insecure"), // ErrInsecureURL
		WithSecret("short"),        // ErrWeakSecret
	)
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	if !errors.Is(err, ErrInsecureURL) {
		t.Error("joined error missing ErrInsecureURL")
	}
	if !errors.Is(err, ErrWeakSecret) {
		t.Error("joined error missing ErrWeakSecret")
	}
}

func TestFailFastStopsAtFirst(t *testing.T) {
	t.Parallel()

	var hook WebhookConfig
	err := Apply(&hook,
		WithURL("http://insecure"), // fails first
		WithSecret("short"),        // never reached
	)
	if !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("error = %v, want ErrInsecureURL", err)
	}
	if errors.Is(err, ErrWeakSecret) {
		t.Fatal("fail-fast should not have reached WithSecret")
	}
}

func TestGenericReuse(t *testing.T) {
	t.Parallel()

	var hook WebhookConfig
	if err := Apply(&hook,
		WithURL("https://example.com/hook"),
		WithSecret("0123456789abcdef"),
	); err != nil {
		t.Fatalf("webhook apply: %v", err)
	}
	if hook.URL != "https://example.com/hook" {
		t.Errorf("URL = %q", hook.URL)
	}

	var worker WorkerConfig
	if err := Apply(&worker,
		WithConcurrency(8),
		WithQueue("emails"),
	); err != nil {
		t.Fatalf("worker apply: %v", err)
	}
	if worker.Concurrency != 8 || worker.Queue != "emails" {
		t.Errorf("worker = %+v", worker)
	}
}
```

## Review

The helper is correct when one generic `Apply`/`ApplyAll` pair configures any
number of unrelated types with no per-type boilerplate — `TestGenericReuse` proves
the same driver validates a `WebhookConfig` and a `WorkerConfig` independently. The
fail-fast versus collect-all distinction is the real lesson: `TestCollectAllFindsEverySentinel`
only passes because `errors.Join` preserves each underlying error for `errors.Is`,
while `TestFailFastStopsAtFirst` proves `Apply` never runs the second option. Wrap
sentinels with `%w` so `errors.Is` sees through both the wrapping and the join;
formatting them into a plain string would break every assertion here.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors)
- [Go generics tutorial](https://go.dev/doc/tutorial/generics)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-clock-injection-cache-options.md](06-clock-injection-cache-options.md) | Next: [08-config-precedence-options.md](08-config-precedence-options.md)
