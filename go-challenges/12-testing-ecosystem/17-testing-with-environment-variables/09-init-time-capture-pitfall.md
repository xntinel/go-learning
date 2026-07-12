# Exercise 9: Fix The Bug Where t.Setenv Cannot Change Config Read At Init

"My env test does nothing" is a real, recurring bug. Its usual cause is config
read at package-initialization time: a package-level variable is frozen before any
test runs, so `t.Setenv` has no effect and the test silently asserts stale config.
This exercise reproduces the trap and fixes it with lazy reads.

## What you'll build

```text
initcapture/               independent module: example.com/initcapture
  go.mod                   go directive supplied by the gate
  timeout.go               FrozenTimeout (init-captured); Timeout() (lazy); NewCachedTimeout (OnceValue)
  cmd/
    demo/
      main.go              runnable demo: frozen vs lazy read
  timeout_test.go          prove FrozenTimeout ignores t.Setenv; lazy read honors it; cache caches
```

Files: `timeout.go`, `cmd/demo/main.go`, `timeout_test.go`.
Implement: `FrozenTimeout` (a package var read at init), `Timeout()` (lazy per-call read), and `NewCachedTimeout()` (a fresh `sync.OnceValue` reader).
Test: a test showing `t.Setenv` does NOT change `FrozenTimeout`; one showing `Timeout()` honors it; one showing the `OnceValue` reader caches its first call.
Verify: `go test -count=1 -race ./...`

## Why the env test does nothing

Package-level variable initializers and `init()` functions run once, when the
package is loaded — which happens *before* any test function executes. So a
declaration like `var FrozenTimeout = parseTimeout(os.Getenv("HTTP_TIMEOUT"))`
reads the environment exactly once, at load time, capturing whatever value existed
then (in a test binary, typically unset, so the default). A test that later calls
`t.Setenv("HTTP_TIMEOUT", "5s")` and reads `FrozenTimeout` sees the frozen
init-time value, not `5s`. The test either fails confusingly or — the dangerous
case — passes against the wrong data because the assertion happened to match the
default.

The fix is to defer the read until it is actually needed. `Timeout()` reads
`os.Getenv` on every call, so whatever `t.Setenv` set is observed immediately;
this is the simplest and most testable shape. When you want to avoid re-parsing on
a hot path, `sync.OnceValue(f)` returns a function that runs `f` once and caches
the result for all later calls. That cache is the catch: a *package-level*
`sync.OnceValue` freezes on its first call, which in a test suite is again
whatever ran first — so a value-caching loader must be constructed *per test*
(`NewCachedTimeout` returns a fresh one) to stay observable. The lesson is to know
which shape you have: lazy-every-call is always testable; cached is testable only
if you can make a new cache.

Note the init-time read must not panic on an unset variable, so `parseTimeout`
falls back to a 30-second default when parsing fails — the same default a real
service would compile in.

Create `timeout.go`:

```go
package initcapture

import (
	"os"
	"sync"
	"time"
)

// FrozenTimeout is captured at package initialization, BEFORE any test runs.
// t.Setenv in a test cannot change it — this is the trap.
var FrozenTimeout = parseTimeout(os.Getenv("HTTP_TIMEOUT"))

// parseTimeout returns the parsed duration, or a 30s default when unset/invalid.
func parseTimeout(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// Timeout reads the environment lazily on every call, so a test's t.Setenv is
// observed. This is the fix: no value is frozen at init.
func Timeout() time.Duration {
	return parseTimeout(os.Getenv("HTTP_TIMEOUT"))
}

// cachedTimeout is a package-level OnceValue: it reads once, on its first call,
// then caches forever. In a test suite that first call is whatever ran first, so
// it is NOT reliably testable — prefer NewCachedTimeout for tests.
var cachedTimeout = sync.OnceValue(func() time.Duration {
	return parseTimeout(os.Getenv("HTTP_TIMEOUT"))
})

// CachedTimeout returns the process-wide cached timeout.
func CachedTimeout() time.Duration {
	return cachedTimeout()
}

// NewCachedTimeout returns a FRESH OnceValue-backed reader. A test can build its
// own, set the environment first, and observe the value at first call — while
// still exercising the caching behavior.
func NewCachedTimeout() func() time.Duration {
	return sync.OnceValue(func() time.Duration {
		return parseTimeout(os.Getenv("HTTP_TIMEOUT"))
	})
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/initcapture"
)

func main() {
	// FrozenTimeout was read at package init, before main set anything.
	fmt.Println("frozen (init-time):", initcapture.FrozenTimeout)

	os.Setenv("HTTP_TIMEOUT", "5s")
	fmt.Println("lazy (reads now):  ", initcapture.Timeout())

	// The package-level cache locks in on first use.
	fmt.Println("cached (first use):", initcapture.CachedTimeout())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frozen (init-time): 30s
lazy (reads now):   5s
cached (first use): 5s
```

## Tests

`TestFrozenIgnoresSetenv` is the diagnostic: it sets `HTTP_TIMEOUT` and asserts
`FrozenTimeout` is *still* the 30s default, proving the init-time capture is
immune to `t.Setenv`. `TestLazyHonorsSetenv` sets the variable and asserts
`Timeout()` returns the new value — the fix working. `TestFreshCacheReadsAtFirstCall`
builds a fresh `NewCachedTimeout`, sets the environment first, and shows the reader
observes it; a follow-up assertion after changing the environment shows the value
is cached (the second read does not change), demonstrating the trade-off.

Create `timeout_test.go`:

```go
package initcapture

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestFrozenIgnoresSetenv(t *testing.T) {
	// The package was loaded with HTTP_TIMEOUT unset, so FrozenTimeout is 30s.
	// Setting it now cannot change a value captured at init.
	t.Setenv("HTTP_TIMEOUT", "5s")
	if FrozenTimeout != 30*time.Second {
		t.Fatalf("FrozenTimeout = %v; t.Setenv should NOT have changed it (init-captured)", FrozenTimeout)
	}
}

func TestLazyHonorsSetenv(t *testing.T) {
	t.Setenv("HTTP_TIMEOUT", "5s")
	if got := Timeout(); got != 5*time.Second {
		t.Fatalf("Timeout() = %v, want 5s (lazy read honors t.Setenv)", got)
	}

	t.Setenv("HTTP_TIMEOUT", "250ms")
	if got := Timeout(); got != 250*time.Millisecond {
		t.Fatalf("Timeout() = %v, want 250ms (re-reads on every call)", got)
	}
}

func TestFreshCacheReadsAtFirstCall(t *testing.T) {
	t.Setenv("HTTP_TIMEOUT", "7s")
	read := NewCachedTimeout()

	if got := read(); got != 7*time.Second {
		t.Fatalf("first read = %v, want 7s", got)
	}

	// Change the environment: the cached reader must NOT observe it.
	t.Setenv("HTTP_TIMEOUT", "9s")
	if got := read(); got != 7*time.Second {
		t.Fatalf("second read = %v, want 7s (OnceValue caches the first call)", got)
	}
}

func ExampleTimeout() {
	os.Setenv("HTTP_TIMEOUT", "250ms")
	fmt.Println(Timeout())

	os.Unsetenv("HTTP_TIMEOUT")
	fmt.Println(Timeout())
	// Output:
	// 250ms
	// 30s
}
```

## Review

The trap is real and the test that exposes it is counterintuitive: a *passing*
assertion that `FrozenTimeout` did *not* change is what documents the bug. The fix
is to read lazily — `Timeout()` re-reads on every call, so `t.Setenv` is always
honored, which the second assertion of `TestLazyHonorsSetenv` confirms by changing
the value twice. The `sync.OnceValue` caveat is the senior nuance: caching is fine
for production but makes the value observable only at first call, so a cached
loader must be constructible per test — `NewCachedTimeout` is that escape hatch,
and `TestFreshCacheReadsAtFirstCall` shows both the read and the cache. When an env
test "does nothing", look first for a read that happened at `init`.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — run a function once and cache its result.
- [testing.T.Setenv](https://pkg.go.dev/testing#T.Setenv) — effective only for reads that happen after it runs.
- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — when package-level vars and `init` run.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-config-precedence-layering.md](08-config-precedence-layering.md) | Next: [10-secret-redaction-logvaluer.md](10-secret-redaction-logvaluer.md)
