# Exercise 4: Parallel Validation That Collects Every Error

A signup form with three bad fields should tell the user all three at once, not reject the username, then — after a fix and resubmit — reject the email, then the password. This is the case where `errgroup`'s first-error-wins model is exactly wrong, and the senior move is to use `errgroup` as a goroutine manager while deliberately declining its error collection. This exercise builds `ValidateAll`, which runs every validation rule concurrently, never lets one failure abort the others, and folds every failure into one `errors.Join` value the caller can take apart with `errors.Is`.

This module is fully self-contained: its own `go mod init`, a direct `errgroup` import, its own demo and tests. The gate fetches `errgroup`; locally run `go mod tidy` once.

## What you'll build

```text
validate.go            Rule{Name, Check}, ValidateAll(ctx, rules...) collecting all via errors.Join
cmd/
  demo/
    main.go            validate a signup form with three bad fields and print every problem
validate_test.go       all-pass returns nil, all failures are collected, errors.Is finds each, race-safe at scale
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `type Rule struct { Name string; Check func(ctx context.Context) error }` and `ValidateAll(ctx context.Context, rules ...Rule) error` that returns the `errors.Join` of every failure, or nil.
- Test: all-pass returns nil; multiple failures are all present (`errors.Is` finds each, a passing field is absent); the joined error unwraps to exactly the failing count; a 50-rule run is race-clean.
- Verify: `go test -race ./...`

Set up the module:

```bash
go get golang.org/x/sync/errgroup
```

### Why errgroup, and why we refuse its error

The instinct after the previous exercises is to hand each rule to `g.Go` and return its error. That gives you the wrong thing: `g.Wait` returns only the first failure, the rest are silently dropped, and worse, the first failure cancels the context, so rules still in flight may abort before they ever check their field. For validation that is a regression — the whole point is to report every problem in one pass.

The fix keeps `errgroup` but changes the contract each task honors. Every rule's goroutine returns `nil` to the group, no matter what its check found, so the group never aborts and every rule runs to completion. The actual outcome of each check is written into the rule's own slot of a pre-sized `results []error` slice — `results[i]` for the rule launched at index `i` — exactly the indexed-write discipline from the dashboard exercise, and race-free for the same reason: distinct indices are distinct memory, written before the goroutine returns and read only after `Wait`. After `Wait`, `errors.Join(results...)` collapses the slice into one value: it skips the `nil` entries, returns `nil` when every entry is `nil` (so a clean form yields a clean result), and otherwise returns a combined error whose `Unwrap() []error` lets `errors.Is` locate any individual failure and lets the caller count or iterate them.

You might ask why use `errgroup` at all here rather than a bare `sync.WaitGroup`, since no task ever returns an error. Two reasons. First, `errgroup.WithContext` hands each rule a context, so a rule that does I/O — a uniqueness check against the database, a call to an email-deliverability service — can honor an upstream deadline; the manager still propagates cancellation downward even though it never originates one. Second, it keeps the whole chapter's mental model uniform: you reach for the same primitive whether you want first-error or all-errors, and the only thing that changes is whether your tasks return their error or stash it. Each rule wraps its failure with the field name (`fmt.Errorf("%s: %w", r.Name, err)`) so the joined message reads as a list of named problems and `errors.Is` still threads through to the underlying sentinel.

Create `validate.go`:

```go
package validation

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Rule validates one independent aspect of an input. Check returns a non-nil
// error describing the problem, or nil when the aspect is valid. Name labels
// the field so a collected failure says which one broke.
type Rule struct {
	Name  string
	Check func(ctx context.Context) error
}

// ValidateAll runs every rule concurrently and returns a single error joining
// EVERY rule that failed, or nil if all pass. Unlike errgroup's first-error
// model, no rule aborts the others: each runs to completion so the caller sees
// all problems at once. The returned error supports errors.Is against each
// underlying failure and exposes Unwrap() []error via errors.Join.
func ValidateAll(ctx context.Context, rules ...Rule) error {
	results := make([]error, len(rules))
	g, ctx := errgroup.WithContext(ctx)
	for i, r := range rules {
		g.Go(func() error {
			if err := r.Check(ctx); err != nil {
				results[i] = fmt.Errorf("%s: %w", r.Name, err)
			}
			// Always return nil: the group must not abort, so every field is
			// checked. The per-rule outcome lives in results[i] instead.
			return nil
		})
	}
	// Wait never yields an error here, since no rule returns one; we wait only
	// to know every results[i] has been written.
	_ = g.Wait()
	return errors.Join(results...)
}
```

### The runnable demo

The demo validates a signup form whose three fields are all bad and prints the joined error, which `errors.Join` renders one failure per line. It then shows that `errors.Is` still finds a specific problem inside the combined value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"example.com/validation"
)

var (
	errTooShort     = errors.New("too short")
	errInvalidEmail = errors.New("invalid email")
	errWeakPassword = errors.New("weak password")
)

func signupRules(user, email, pass string) []validation.Rule {
	return []validation.Rule{
		{Name: "username", Check: func(ctx context.Context) error {
			if len(user) < 3 {
				return errTooShort
			}
			return nil
		}},
		{Name: "email", Check: func(ctx context.Context) error {
			if !strings.Contains(email, "@") {
				return errInvalidEmail
			}
			return nil
		}},
		{Name: "password", Check: func(ctx context.Context) error {
			if len(pass) < 8 {
				return errWeakPassword
			}
			return nil
		}},
	}
}

func main() {
	err := validation.ValidateAll(context.Background(), signupRules("ab", "not-an-email", "short")...)
	if err == nil {
		fmt.Println("all fields valid")
		return
	}
	fmt.Println("validation failed:")
	fmt.Println(err)
	fmt.Println("contains weak-password problem:", errors.Is(err, errWeakPassword))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
validation failed:
username: too short
email: invalid email
password: weak password
contains weak-password problem: true
```

All three problems appear together in rule order — the order is deterministic because each result lands in its own index and `errors.Join` walks the slice in order — and `errors.Is` reaches through the joined value and the per-field wrap to match the original sentinel.

### Tests

`TestValidateAllPass` runs three passing rules and asserts the result is nil, the contract that a clean form yields no error. `TestValidateAllCollectsEvery` runs three rules where the first and third fail and the middle passes, then asserts both failures are found by `errors.Is`, the passing field's sentinel is absent, and the joined error unwraps to exactly two leaves — proving none were dropped and none invented. `TestValidateAllParallelSafe` runs fifty rules, half of them failing, and asserts the joined count is exactly twenty-five; its real job is to drive the indexed writes hard under `-race`.

Create `validate_test.go`:

```go
package validation

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

var (
	errA = errors.New("bad a")
	errB = errors.New("bad b")
	errC = errors.New("bad c")
)

func ruleReturning(name string, err error) Rule {
	return Rule{Name: name, Check: func(ctx context.Context) error { return err }}
}

func TestValidateAllPass(t *testing.T) {
	t.Parallel()

	err := ValidateAll(context.Background(),
		ruleReturning("a", nil),
		ruleReturning("b", nil),
		ruleReturning("c", nil),
	)
	if err != nil {
		t.Fatalf("ValidateAll = %v, want nil", err)
	}
}

func TestValidateAllCollectsEvery(t *testing.T) {
	t.Parallel()

	err := ValidateAll(context.Background(),
		ruleReturning("a", errA),
		ruleReturning("b", nil),
		ruleReturning("c", errC),
	)
	if err == nil {
		t.Fatal("ValidateAll = nil, want a combined error")
	}
	if !errors.Is(err, errA) {
		t.Errorf("errA missing from %v", err)
	}
	if !errors.Is(err, errC) {
		t.Errorf("errC missing from %v", err)
	}
	if errors.Is(err, errB) {
		t.Errorf("errB (a passing rule) should not be present in %v", err)
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error %T does not implement Unwrap() []error", err)
	}
	if n := len(joined.Unwrap()); n != 2 {
		t.Fatalf("joined %d errors, want 2", n)
	}
}

func TestValidateAllParallelSafe(t *testing.T) {
	t.Parallel()

	const n = 50
	rules := make([]Rule, n)
	for i := range rules {
		rules[i] = Rule{Name: fmt.Sprintf("f%d", i), Check: func(ctx context.Context) error {
			if i%2 == 0 {
				return fmt.Errorf("field %d invalid", i)
			}
			return nil
		}}
	}

	err := ValidateAll(context.Background(), rules...)
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("want a joined error, got %T", err)
	}
	if got := len(joined.Unwrap()); got != n/2 {
		t.Fatalf("joined %d errors, want %d", got, n/2)
	}
}
```

## Review

`ValidateAll` is correct when it collects every failure and exactly the failures, which the tests pin under `-race`. The defining mistake it avoids is the one the concepts file warns about: using `errgroup`'s first-error model to gather all errors, which returns one problem and may cancel the rest before they run. Returning `nil` from every `g.Go` is what makes the group a pure manager so each rule runs to completion, and the indexed `results[i]` write keeps the gather race-free without a mutex. `TestValidateAllCollectsEvery` is the load-bearing test because it checks all three failure modes at once — a present failure found, an absent rule not invented, and the exact count — so an implementation that dropped errors, double-counted, or smeared writes across one index would fail it. The 50-rule test exists to make `-race` exercise the per-index writes at scale; if two goroutines ever shared a slot the detector would fire. The contrast with the dashboard exercise is the lesson: the same `errgroup`, the same indexed-slot discipline, and the only thing that flips between fail-fast and collect-all is whether your task returns its error or stashes it.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library combiner: skips nils, returns nil when all nil, and exposes `Unwrap() []error` for `errors.Is`.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping and `errors.Is`, which let a per-field wrap still match its underlying sentinel.
- [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) — the manager used here as a bounded, context-aware way to run the rules, with its first-error collection deliberately declined.

---

Back to [03-scatter-gather-dashboard.md](03-scatter-gather-dashboard.md) | Next: [../07-errgroup-with-context/07-errgroup-with-context.md](../07-errgroup-with-context/07-errgroup-with-context.md)
