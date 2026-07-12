# Exercise 1: Executable Deprecation of an Internal SDK Helper

You own a shared retry helper in an internal SDK. Its old signature took a fixed
attempt count with no cancellation; the preferred one takes a `context.Context`.
Rather than break every consumer or leave a `Deprecated:` note nobody actions,
you turn the old helper into a thin forwarder and annotate it with
`//go:fix inline`, so each downstream team migrates its own call sites with
`go fix`.

This module is fully self-contained: its own `go mod init`, the library package,
a consumer package with several call sites, a demo, and tests. Nothing here
imports another exercise.

## What you'll build

```text
sdkretry/                      independent module: example.com/sdkretry
  go.mod                       go 1.26
  backoff/
    backoff.go                 Retry (preferred) + RetryN (Deprecated, //go:fix inline forwarder)
    backoff_test.go            behavioral-equivalence tests + Example
    fix_integration_test.go    //go:build gofix_integration: runs `go fix -diff ./...`
  consumer/
    consumer.go                several backoff.RetryN call sites (what go fix rewrites)
  cmd/
    demo/
      main.go                  runnable demo of the deprecated forwarder
```

- Files: `backoff/backoff.go`, `consumer/consumer.go`, `cmd/demo/main.go`, `backoff/backoff_test.go`, `backoff/fix_integration_test.go`.
- Implement: a preferred `Retry(ctx, attempts, fn)` and a `Deprecated`, `//go:fix inline`-annotated forwarder `RetryN(attempts, fn)` that calls `Retry(context.Background(), attempts, fn)`.
- Test: a table-driven test asserting the forwarder and the preferred function behave identically; an `Example`; a build-tag-guarded integration test that shells out to `go fix -diff ./...` and asserts the expected rewrite appears.
- Verify: `go test -count=1 -race ./...`, then `go fix -diff ./...`.

Set up the module. `//go:fix inline` and the revamped `go fix` require Go 1.26:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/01-deprecate-and-inline-a-function-api/backoff go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/01-deprecate-and-inline-a-function-api/consumer go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/01-deprecate-and-inline-a-function-api/cmd/demo
cd go-solutions/48-modern-go-language-and-stdlib/16-go-fix-inline-modernization/01-deprecate-and-inline-a-function-api
go mod edit -go=1.26
```

### The library: a preferred function and a forwarding deprecation

The design goal is a migration that costs consumers one command and zero
thought. The preferred function, `Retry`, takes a context so callers can bound
the total work; it calls `fn` up to `attempts` times, returns `nil` on the first
success, and wraps the final failure with a sentinel so callers can match it with
`errors.Is`.

The old function, `RetryN`, becomes a *thin forwarder*: its entire body is
`return Retry(context.Background(), attempts, fn)`. Two properties make it a valid
inline target. First, the body is trivially inlinable — no `defer`, no unexported
identifiers, nothing the inliner refuses. Second, it references only things a
caller can also reach: the exported `Retry` and the standard `context` package.
That second point is what lets the inliner paste the body into a consumer and swap
in a `context` import; a forwarder that reached into unexported package internals
would inline into code that does not compile.

The forwarder carries both a `Deprecated:` paragraph (for humans and gopls) and,
on the line immediately above the declaration, `//go:fix inline` (for the
tool). Note there is no blank line between the directive and `func RetryN`.

Create `backoff/backoff.go`:

```go
package backoff

import (
	"context"
	"errors"
	"fmt"
)

// ErrExhausted is returned, wrapped, when every attempt failed.
var ErrExhausted = errors.New("backoff: attempts exhausted")

// Retry calls fn up to attempts times, returning nil on the first success. It
// stops early if ctx is cancelled, returning ctx.Err(). If every attempt fails,
// it returns the last error wrapped with ErrExhausted.
func Retry(ctx context.Context, attempts int, fn func() error) error {
	var last error
	for range attempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = fn()
		if last == nil {
			return nil
		}
	}
	return fmt.Errorf("%w: last error: %v", ErrExhausted, last)
}

// RetryN calls fn up to attempts times with no cancellation.
//
// Deprecated: use [Retry], which accepts a context.Context so callers can bound
// the total work. RetryN is a thin forwarder kept for one release to give
// consumers time to run `go fix`.
//
//go:fix inline
func RetryN(attempts int, fn func() error) error {
	return Retry(context.Background(), attempts, fn)
}
```

### The consumer: the call sites go fix will rewrite

A separate package imports the SDK and calls the deprecated `RetryN` in a few
places. This is the code a downstream team owns. When they run `go fix`, the
inliner replaces each `backoff.RetryN(n, fn)` with
`backoff.Retry(context.Background(), n, fn)` and adds the `context` import to this
file. Until then, the code compiles and runs unchanged, because `RetryN` still
exists.

Create `consumer/consumer.go`:

```go
// Package consumer exercises the deprecated backoff.RetryN at several call
// sites. Running `go fix ./...` rewrites each call to backoff.Retry and adds the
// context import to this file.
package consumer

import "example.com/sdkretry/backoff"

// Ping retries a health check a few times.
func Ping(check func() error) error {
	return backoff.RetryN(3, check)
}

// Publish retries a publish up to five times.
func Publish(send func() error) error {
	return backoff.RetryN(5, send)
}
```

### The runnable demo

The demo drives the deprecated forwarder directly so you can watch it succeed
after a transient failure and give up after exhausting its attempts.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/sdkretry/backoff"
)

func main() {
	calls := 0
	err := backoff.RetryN(3, func() error {
		calls++
		if calls < 2 {
			return errors.New("transient")
		}
		return nil
	})
	fmt.Printf("RetryN succeeded after %d calls (err=%v)\n", calls, err)

	fail := 0
	err = backoff.RetryN(2, func() error {
		fail++
		return errors.New("down")
	})
	fmt.Printf("RetryN gave up after %d calls: exhausted=%v\n", fail, errors.Is(err, backoff.ErrExhausted))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
RetryN succeeded after 2 calls (err=<nil>)
RetryN gave up after 2 calls: exhausted=true
```

### Proving the forward is faithful

Before you ask forty teams to inline `RetryN` into `Retry`, you owe them proof
that the two are the same. The test drives each input through both paths — the
deprecated `RetryN` and the exact expression it forwards to,
`Retry(context.Background(), ...)` — and asserts they agree. If they ever diverge,
the forwarder is lying and the `//go:fix inline` directive would spread a
behavior change; the test is the guard that lets you trust the directive.

Create `backoff/backoff_test.go`:

```go
package backoff

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// flaky returns a function that fails its first failFirst calls, then succeeds.
func flaky(failFirst int) func() error {
	n := 0
	return func() error {
		n++
		if n <= failFirst {
			return fmt.Errorf("attempt %d failed", n)
		}
		return nil
	}
}

func TestRetryNMatchesRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		failFirst int
		attempts  int
		wantErr   bool
	}{
		{"succeeds first try", 0, 3, false},
		{"succeeds after failures", 2, 3, false},
		{"never succeeds", 5, 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wErr := RetryN(tc.attempts, flaky(tc.failFirst))
			rErr := Retry(context.Background(), tc.attempts, flaky(tc.failFirst))

			if (wErr != nil) != (rErr != nil) {
				t.Fatalf("divergent outcome: RetryN=%v, Retry=%v", wErr, rErr)
			}
			if tc.wantErr {
				if !errors.Is(wErr, ErrExhausted) {
					t.Fatalf("RetryN err = %v, want wrap of ErrExhausted", wErr)
				}
				if !errors.Is(rErr, ErrExhausted) {
					t.Fatalf("Retry err = %v, want wrap of ErrExhausted", rErr)
				}
			}
		})
	}
}

func TestRetryHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Retry(ctx, 3, func() error { return errors.New("never reached") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retry err = %v, want context.Canceled", err)
	}
}

func Example() {
	err := Retry(context.Background(), 3, func() error { return nil })
	fmt.Println(err)
	// Output: <nil>
}
```

### The migration itself, as a diff

You do not edit the consumer by hand. From the module root you preview the
rewrite:

```bash
go fix -diff ./...
```

`go fix` finds every use of the `//go:fix inline`-annotated `RetryN`, inlines the
forwarder's body, and rewrites the imports. The diff for the consumer is:

```text
--- consumer/consumer.go (old)
+++ consumer/consumer.go (new)
-import "example.com/sdkretry/backoff"
+import (
+	"context"
+
+	"example.com/sdkretry/backoff"
+)
 
 // Ping retries a health check a few times.
 func Ping(check func() error) error {
-	return backoff.RetryN(3, check)
+	return backoff.Retry(context.Background(), 3, check)
 }
 
 // Publish retries a publish up to five times.
 func Publish(send func() error) error {
-	return backoff.RetryN(5, send)
+	return backoff.Retry(context.Background(), 5, send)
 }
```

Two things to notice. The call `backoff.RetryN(3, check)` becomes
`backoff.Retry(context.Background(), 3, check)` — the forwarder's body with the
arguments substituted. And the `context` import appears in the consumer even
though the consumer never named it: the inliner pulled the callee's import into
the caller. Once every consumer's diff is empty, you can delete `RetryN` in a
later release.

### Gating the migration in CI

`go fix -diff ./...` writes nothing and exits, printing the diff to stdout. That
is exactly what a CI check needs: run it, and fail if the output is non-empty,
because a non-empty diff means an un-migrated call site remains. The integration
test below encodes that check. It is guarded by a build tag because it shells out
to the `go` toolchain (which the offline gate does not provide), so it does not
run in the default `go test` path:

```bash
go test -tags gofix_integration ./backoff
```

Create `backoff/fix_integration_test.go`:

```go
//go:build gofix_integration

package backoff_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGoFixRewritesCallSites runs `go fix -diff ./...` over this module and
// asserts the inliner would rewrite the consumer's RetryN calls into Retry with
// an injected context. -diff writes nothing, so the tree is left untouched.
func TestGoFixRewritesCallSites(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "fix", "-diff", "./...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go fix -diff failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "backoff.Retry(context.Background()") {
		t.Fatalf("diff missing inlined call rewrite; got:\n%s", got)
	}
}
```

## Review

The forwarder is correct when it is genuinely thin: `RetryN(attempts, fn)` must be
observably identical to `Retry(context.Background(), attempts, fn)` for every
input, which is exactly what `TestRetryNMatchesRetry` checks by running both paths
and comparing outcomes, and what makes the `//go:fix inline` rewrite safe to fan
out. The wrapped sentinel matters too: `Retry` returns `fmt.Errorf("%w: ...",
ErrExhausted, last)` so consumers match exhaustion with `errors.Is`, which the
test and the demo both rely on.

The mistakes to avoid are the ones that make the directive quietly useless or
actively harmful. Do not put a space after the slashes or a blank line before
`func RetryN` — `// go:fix inline` or a detached directive is just a comment.
Keep the forwarder's body reachable from a consumer: if it referenced an
unexported helper, the inlined result would not compile in the consumer's package.
And do not delete `RetryN` in the same release you annotate it — the directive
enables the migration but does not perform it, so the old symbol must survive
until consumers have run `go fix`. Confirm behavior with `go test -race ./...`,
preview the migration with `go fix -diff ./...`, and, where the toolchain is
available, run the tagged integration test to assert the rewrite in CI.

## Resources

- [Automating your API migrations with go fix inline](https://go.dev/blog/inliner) — the source-level inliner, `//go:fix inline`, and how imports and evaluation order are preserved.
- [Go 1.26 Release Notes: go fix](https://go.dev/doc/go1.26) — the revamped command built on the `go/analysis` framework.
- [Deprecation convention](https://go.dev/wiki/Deprecated) — the `Deprecated:` doc-comment paragraph that `//go:fix inline` complements.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-migrating-consumers-across-a-major-version.md](02-migrating-consumers-across-a-major-version.md)
