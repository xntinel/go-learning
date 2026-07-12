# Exercise 33: Environment Variable Temporary Override Restore

Tests and one-off operations sometimes need to flip an environment variable
for the duration of a single call — a feature flag, a config override — and
put it back exactly as it was afterward, whether that call succeeds, fails,
or panics. This exercise builds a `WithEnv` helper that captures the
variable's original state before changing it and registers the restore as a
`defer` in the same breath, so every exit path — not just the one the author
remembered to write a cleanup line for — restores it.

**Nivel: Intermedio** — validacion rapida (cuatro casos cortos: variable inexistente, variable previa, error, panic).

## What you'll build

```text
envtemp/                    independent module: example.com/envtemp
  go.mod
  envtemp.go                  WithEnv (capture original, defer restore)
  cmd/demo/
    main.go                  runnable demo: a successful call and a failing one
  envtemp_test.go             restores unset var, restores previous value, restores on error and on panic
```

- Files: `envtemp.go`, `cmd/demo/main.go`, `envtemp_test.go`.
- Implement: `WithEnv(key, value string, fn func() error) (err error)` that captures `os.LookupEnv(key)` before calling `os.Setenv`, and defers a restore (`os.Setenv` back to the original, or `os.Unsetenv` if it didn't exist) immediately after.
- Test: a variable that didn't exist is unset again afterward; one that had a value gets that value back; the restore happens whether `fn` returns an error or panics.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/33-environment-variable-temporary-set-restore/cmd/demo
cd go-solutions/04-functions/02-named-return-values/33-environment-variable-temporary-set-restore
go mod edit -go=1.24
```

### Capture first, defer immediately, restore unconditionally

```go
original, existed := os.LookupEnv(key)
if setErr := os.Setenv(key, value); setErr != nil {
    return fmt.Errorf("setenv %s: %w", key, setErr)
}
defer func() {
    if existed {
        os.Setenv(key, original)
    } else {
        os.Unsetenv(key)
    }
}()

return fn()
```

The restore closure is registered right after `os.Setenv` succeeds, before
`fn` ever runs — so however `fn` exits, the deferred call still fires: on a
normal return, on an early error return, or by unwinding through a panic
`fn` triggers. `existed` decides which restore is correct: a variable that
was genuinely absent before must go back to being absent (`os.Unsetenv`),
not merely present with an empty string, which is a different observable
state (`os.LookupEnv` would report `ok = true` for an empty string but
`ok = false` for unset). Capturing `original, existed` up front — before any
mutation — is what makes the restore correct in the first place; the
alternative of skipping the capture and unconditionally unsetting would
silently destroy a value that was there for a legitimate reason before this
call ever started.

Create `envtemp.go`:

```go
package envtemp

import (
	"fmt"
	"os"
)

// WithEnv sets key to value for the duration of fn, then restores the
// environment to exactly how it looked before: back to the original value if
// key already existed, or unset entirely if it did not.
//
// err is a named result only in the sense that the deferred restore runs
// regardless of how fn exits (return, error, or panic); the restore itself
// does not depend on err, but registering it as a defer right after the
// Setenv call — before fn ever runs — is what guarantees it fires on every
// exit path, including a panic inside fn.
func WithEnv(key, value string, fn func() error) (err error) {
	original, existed := os.LookupEnv(key)
	if setErr := os.Setenv(key, value); setErr != nil {
		return fmt.Errorf("setenv %s: %w", key, setErr)
	}
	defer func() {
		if existed {
			os.Setenv(key, original)
		} else {
			os.Unsetenv(key)
		}
	}()

	return fn()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/envtemp"
)

func main() {
	os.Unsetenv("DEMO_FEATURE_FLAG")

	err := envtemp.WithEnv("DEMO_FEATURE_FLAG", "on", func() error {
		fmt.Println("inside fn: DEMO_FEATURE_FLAG =", os.Getenv("DEMO_FEATURE_FLAG"))
		return nil
	})
	_, existsAfter := os.LookupEnv("DEMO_FEATURE_FLAG")
	fmt.Printf("after success: err=%v existed-before=false exists-after=%v\n", err, existsAfter)

	err = envtemp.WithEnv("DEMO_FEATURE_FLAG", "on", func() error {
		return errors.New("operation failed")
	})
	_, existsAfter = os.LookupEnv("DEMO_FEATURE_FLAG")
	fmt.Printf("after failure: err=%v exists-after=%v\n", err, existsAfter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inside fn: DEMO_FEATURE_FLAG = on
after success: err=<nil> existed-before=false exists-after=false
after failure: err=operation failed exists-after=false
```

### Tests

Create `envtemp_test.go`:

```go
package envtemp

import (
	"errors"
	"os"
	"testing"
)

func TestWithEnvRestoresUnsetVariable(t *testing.T) {
	os.Unsetenv("ENVTEMP_TEST_UNSET")

	var sawInside string
	err := WithEnv("ENVTEMP_TEST_UNSET", "temp-value", func() error {
		sawInside = os.Getenv("ENVTEMP_TEST_UNSET")
		return nil
	})
	if err != nil {
		t.Fatalf("WithEnv: unexpected error: %v", err)
	}
	if sawInside != "temp-value" {
		t.Fatalf("value inside fn = %q, want temp-value", sawInside)
	}
	if _, ok := os.LookupEnv("ENVTEMP_TEST_UNSET"); ok {
		t.Fatal("ENVTEMP_TEST_UNSET still set after WithEnv, want unset (it did not exist before)")
	}
}

func TestWithEnvRestoresPreviousValue(t *testing.T) {
	os.Setenv("ENVTEMP_TEST_PREV", "original")
	defer os.Unsetenv("ENVTEMP_TEST_PREV")

	err := WithEnv("ENVTEMP_TEST_PREV", "temporary", func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("WithEnv: unexpected error: %v", err)
	}
	if got := os.Getenv("ENVTEMP_TEST_PREV"); got != "original" {
		t.Fatalf("ENVTEMP_TEST_PREV = %q after WithEnv, want original", got)
	}
}

func TestWithEnvRestoresOnFnError(t *testing.T) {
	os.Unsetenv("ENVTEMP_TEST_ERR")

	wantErr := errors.New("boom")
	err := WithEnv("ENVTEMP_TEST_ERR", "temp", func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if _, ok := os.LookupEnv("ENVTEMP_TEST_ERR"); ok {
		t.Fatal("ENVTEMP_TEST_ERR still set after fn returned an error, want restored")
	}
}

func TestWithEnvRestoresOnPanic(t *testing.T) {
	os.Unsetenv("ENVTEMP_TEST_PANIC")

	func() {
		defer func() { _ = recover() }()
		_ = WithEnv("ENVTEMP_TEST_PANIC", "temp", func() error {
			panic("fn blew up")
		})
	}()

	if _, ok := os.LookupEnv("ENVTEMP_TEST_PANIC"); ok {
		t.Fatal("ENVTEMP_TEST_PANIC still set after fn panicked, want restored")
	}
}
```

## Review

`WithEnv` is correct when the environment looks, after the call, exactly as
it did before — not "unset" as a default assumption, but whatever `existed`
actually captured. The panic test is the one that matters most: it proves
the restore is a property of `defer` running during stack unwinding, not of
`fn` returning normally, which is precisely why the restore is registered as
a `defer` immediately after `Setenv` succeeds rather than as a plain
statement at the end of the function. The mistake to avoid is capturing
`original` after calling `Setenv` instead of before — by then the "original"
value is already gone, overwritten by the very call meant to be temporary.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`os.LookupEnv`](https://pkg.go.dev/os#LookupEnv)
- [`os.Setenv` / `os.Unsetenv`](https://pkg.go.dev/os#Setenv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-event-handler-post-process-on-success.md](32-event-handler-post-process-on-success.md) | Next: [34-idempotency-key-cache-store-result.md](34-idempotency-key-cache-store-result.md)
