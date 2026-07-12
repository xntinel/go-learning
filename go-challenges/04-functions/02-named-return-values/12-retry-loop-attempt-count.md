# Exercise 12: Report the Final Attempt Count from a Retry Loop

A retry loop that gives up after `maxAttempts` needs to log how many attempts it
actually took, whether it succeeded or not — one line, one place, regardless of
which iteration exited the loop. The named `attempts` result is updated on every
pass and stays reachable from a deferred closure after the loop has already
returned, so the log line never has to be duplicated at each exit.

**Nivel: Intermedio** — validacion rapida (un test corto).

## What you'll build

```text
retryreport/                 independent module: example.com/retryreport
  go.mod
  retryreport.go             Op; Logger; Retry (named attempts read by defer)
  retryreport_test.go        succeeds after failures, succeeds first try, exhausts
```

- Files: `retryreport.go`, `retryreport_test.go`.
- Implement: `Retry(maxAttempts int, op Op, log Logger) (result string, attempts int, err error)` that retries `op` up to `maxAttempts` times and logs the final attempt count and outcome from one deferred closure.
- Test: succeeds after two failures, succeeds on the first try, exhausts all attempts and returns a wrapped error — checking both the return values and what was logged.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One counter, read after the loop is gone

`attempts` is assigned inside the loop body on every iteration, before either exit
path — success or exhaustion — is taken:

```go
defer func() {
    log(attempts, err)
}()

for a := 1; a <= maxAttempts; a++ {
    attempts = a
    result, err = op(a)
    if err == nil {
        return
    }
}
```

By the time the deferred closure runs, the loop variable `a` is long gone — only the
named `attempts` survives to be read. That is the whole reason this needs a named
result: a local loop counter cannot be inspected from a defer once the function has
returned.

Create `retryreport.go`:

```go
package retryreport

import "fmt"

// Op is one attempt at the underlying operation, given its 1-based attempt
// number. Deterministic test doubles use the attempt number to decide when
// to succeed, so tests need no sleeps or randomness.
type Op func(attempt int) (string, error)

// Logger records the final attempt count and outcome of a Retry call.
type Logger func(attempts int, err error)

// Retry calls op up to maxAttempts times, stopping at the first success. The
// named attempts result is updated on every iteration, so whichever exit path
// is taken — success, exhaustion — the deferred closure can log the true
// final attempt count alongside the outcome. Only a named result stays
// visible to a defer after the loop has already returned.
func Retry(maxAttempts int, op Op, log Logger) (result string, attempts int, err error) {
	defer func() {
		log(attempts, err)
	}()

	for a := 1; a <= maxAttempts; a++ {
		attempts = a
		result, err = op(a)
		if err == nil {
			return
		}
	}
	err = fmt.Errorf("after %d attempts: %w", attempts, err)
	return
}
```

### Tests

Create `retryreport_test.go`:

```go
package retryreport

import (
	"errors"
	"strings"
	"testing"
)

func flakyThenSucceed(failures int) Op {
	return func(attempt int) (string, error) {
		if attempt <= failures {
			return "", errors.New("temporarily unavailable")
		}
		return "ok", nil
	}
}

func alwaysFail() Op {
	return func(attempt int) (string, error) {
		return "", errors.New("permanently unavailable")
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		maxAttempts  int
		op           Op
		wantResult   string
		wantAttempts int
		wantErr      bool
	}{
		{
			name: "succeeds after two failures", maxAttempts: 5, op: flakyThenSucceed(2),
			wantResult: "ok", wantAttempts: 3, wantErr: false,
		},
		{
			name: "succeeds on first try", maxAttempts: 5, op: flakyThenSucceed(0),
			wantResult: "ok", wantAttempts: 1, wantErr: false,
		},
		{
			name: "exhausts all attempts", maxAttempts: 3, op: alwaysFail(),
			wantResult: "", wantAttempts: 3, wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var loggedAttempts int
			var loggedErr error
			log := func(attempts int, err error) {
				loggedAttempts = attempts
				loggedErr = err
			}

			result, attempts, err := Retry(tt.maxAttempts, tt.op, log)

			if result != tt.wantResult {
				t.Errorf("result = %q, want %q", result, tt.wantResult)
			}
			if attempts != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tt.wantAttempts)
			}
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "permanently unavailable") {
				t.Errorf("error %q missing underlying cause", err.Error())
			}
			if loggedAttempts != tt.wantAttempts {
				t.Errorf("logged attempts = %d, want %d", loggedAttempts, tt.wantAttempts)
			}
			if (loggedErr != nil) != tt.wantErr {
				t.Errorf("logged err = %v, wantErr = %v", loggedErr, tt.wantErr)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The named `attempts` result plays a role neither `err` nor a plain return value
could: it is mutated across loop iterations, not just at the exits, and it must
survive past the point where the `for` loop's own variable goes out of scope. The
deferred closure logs the same way on both the success and exhaustion paths because
it never has to know which one happened — it just reads whatever `attempts` and
`err` hold when it runs. A common mistake is logging inside the loop itself on
every failed attempt; that produces noisy, partial logs instead of one summary line
per call.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Spec: For statements](https://go.dev/ref/spec#For_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-multi-field-validation-joined-errors.md](11-multi-field-validation-joined-errors.md) | Next: [13-cache-fallback-consistent-results.md](13-cache-fallback-consistent-results.md)
