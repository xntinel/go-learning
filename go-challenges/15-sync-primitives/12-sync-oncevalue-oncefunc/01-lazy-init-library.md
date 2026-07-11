# Exercise 1: A Lazy-Init Library: Typed Wrappers over OnceValue and OnceValues

Platform teams publish small internal packages so service teams stop
hand-rolling the same `var once sync.Once` + `var cfg *Config` pairs; this
exercise builds that package — thin, documented, typed wrappers over the Go
1.21 once helpers — and pins its contract with a race-enabled test suite.

## What you'll build

```text
lazyinit/                  independent module: example.com/lazyinit
  go.mod                   go mod init example.com/lazyinit
  lazy/
    lazy.go                Int, String, Strings: typed once-initialized values
    lazy_test.go           run-once, 100-goroutine concurrency, value/error caching, Example
  cmd/
    demo/
      main.go              runnable demo: cached int, cached region string, cached secret error
```

- Files: `lazy/lazy.go`, `lazy/lazy_test.go`, `cmd/demo/main.go`.
- Implement: `lazy.Int(fn func() int) func() int`, `lazy.String(fn func() string) func() string`, and `lazy.Strings(fn func() (string, error)) func() (string, error)` over `sync.OnceValue`/`sync.OnceValues`.
- Test: init runs exactly once across 100 sequential and 100 concurrent calls; both the value and the error of a `(string, error)` init are cached; sentinel errors survive wrapping and are asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/lazyinit/lazy ~/go-exercises/lazyinit/cmd/demo
cd ~/go-exercises/lazyinit
go mod init example.com/lazyinit
```

### Why a wrapper package at all

`sync.OnceValue(fn)` is already a one-liner, so why wrap it? Two reasons that
show up in real platform code. First, discoverability and documentation: a
service engineer reaching for "lazy config value" finds `lazy.String` with a
doc comment stating the contract (first call runs `fn`, every later call
returns the cached value, concurrent callers block until the first completes)
without having to know that the primitive lives in `sync` and is generic.
Second, a wrapper is a seam: if the platform team later wants to add metrics
(count of lazy inits per process), a warmup registry, or a deadline around
`fn`, every call site is already routed through one package. The wrappers are
deliberately thin — they add a name and a contract, not behavior.

The `Strings` variant is the important one for production: it is the
`(value, error)` shape, the right tool for "load a resource that may fail" —
a secret file, an environment-derived credential, a parsed config. Note what
its contract implies: the **error is cached too**. The first failure is the
permanent answer for this closure. That is correct for deterministic failures
(the secret is simply not mounted in this environment) and wrong for transient
ones; exercises 04 and 05 dig into exactly that boundary. Here the tests pin
the mechanism so nobody on the team is surprised by it.

Create `lazy/lazy.go`:

```go
// Package lazy provides typed, once-initialized values over the Go 1.21
// sync.OnceValue and sync.OnceValues helpers. Each constructor returns a
// function: the first call runs fn and caches its result; every later call
// returns the cached result. Concurrent callers during the first call block
// until fn completes, so fn runs exactly once per returned function.
package lazy

import "sync"

// Int constructs a once-initialized int. The first call to the returned
// function invokes fn; every later call returns the cached value.
func Int(fn func() int) func() int {
	return sync.OnceValue(fn)
}

// String returns a once-initialized string.
func String(fn func() string) func() string {
	return sync.OnceValue(fn)
}

// Strings is the (string, error) variant. Both the value and the error of
// the first call are cached: a failing fn fails every caller, forever.
// Use it for deterministic loads (a mounted secret, an embedded file), not
// for connections that may be transiently down.
func Strings(fn func() (string, error)) func() (string, error) {
	return sync.OnceValues(fn)
}
```

### The demo

The demo shows the three wrappers doing the jobs they exist for: a cached
computation (note `int inits: 1` after two calls), a region string derived
from the environment with a default, and a secret load whose error is cached —
the second call returns the same error without re-reading the environment.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"example.com/lazyinit/lazy"
)

func main() {
	calls := 0
	getNumber := lazy.Int(func() int {
		calls++
		return 42
	})

	fmt.Println(getNumber())
	fmt.Println(getNumber())
	fmt.Println("int inits:", calls)

	getRegion := lazy.String(func() string {
		if r := os.Getenv("APP_REGION"); r != "" {
			return r
		}
		return "us-east-1"
	})
	fmt.Println("region:", getRegion())
	fmt.Println("region:", getRegion())

	loadSecret := lazy.Strings(func() (string, error) {
		v := os.Getenv("APP_SECRET")
		if v == "" {
			return "", errors.New("APP_SECRET not set")
		}
		return v, nil
	})

	if _, err := loadSecret(); err != nil {
		fmt.Println("first:", err)
	}
	if _, err := loadSecret(); err != nil {
		fmt.Println("cached:", err)
	}
}
```

Run it (with `APP_REGION` and `APP_SECRET` unset):

```bash
go run ./cmd/demo
```

Expected output:

```
42
42
int inits: 1
region: us-east-1
region: us-east-1
first: APP_SECRET not set
cached: APP_SECRET not set
```

### Tests that pin the contract

The hard tests are the concurrent ones: 100 goroutines hammer a cold wrapper
and the init function must run exactly once, with every caller seeing the
cached result — that is the single-flight property, and `-race` proves the
publication is safe. `TestStringsCachesError` uses a package-level sentinel
wrapped with `%w` and asserts identity through the wrap with `errors.Is`,
because that is how real callers will match "the secret is missing" against
the cached error. The `Example` function locks the basic behavior into the
documented output.

Create `lazy/lazy_test.go`:

```go
package lazy

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var errSecretMissing = errors.New("secret missing")

func TestIntRunsOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := Int(func() int {
		calls.Add(1)
		return 42
	})

	for i := range 100 {
		if got := get(); got != 42 {
			t.Fatalf("get() #%d = %d, want 42", i, got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestIntConcurrentGet(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := Int(func() int {
		calls.Add(1)
		return 1
	})

	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if got := get(); got != 1 {
				t.Errorf("get = %d, want 1", got)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times under concurrency, want 1", got)
	}
}

func TestStringRunsOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := String(func() string {
		calls.Add(1)
		return "us-east-1"
	})

	for range 100 {
		if got := get(); got != "us-east-1" {
			t.Fatalf("get() = %q, want us-east-1", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestStringsCachesError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := Strings(func() (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("load secret: %w", errSecretMissing)
	})

	for i := range 10 {
		_, err := get()
		if !errors.Is(err, errSecretMissing) {
			t.Fatalf("err #%d = %v, want errSecretMissing", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestStringsCachesValue(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := Strings(func() (string, error) {
		calls.Add(1)
		return "v", nil
	})

	v1, err1 := get()
	v2, err2 := get()
	if v1 != "v" || v2 != "v" || err1 != nil || err2 != nil {
		t.Fatalf("get: %q/%v then %q/%v", v1, err1, v2, err2)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times, want 1", got)
	}
}

func TestStringsConcurrentSeeSameError(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	get := Strings(func() (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("load secret: %w", errSecretMissing)
	})

	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := get(); !errors.Is(err, errSecretMissing) {
				t.Errorf("err = %v, want errSecretMissing", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("init called %d times under concurrency, want 1", got)
	}
}

func ExampleInt() {
	get := Int(func() int { return 42 })
	fmt.Println(get())
	fmt.Println(get())
	// Output:
	// 42
	// 42
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The package is correct when every wrapper it returns runs its init function
exactly once no matter how it is called: 100 sequential calls, 100 concurrent
goroutines, value-returning or error-returning — the atomic counters in the
tests must all land on 1 under `-race`. The mistake this module exists to
prevent is scattering `var once sync.Once; var cfg *Config` pairs through a
codebase, where nothing but convention couples the flag to the value and a
copy or a second `once` silently breaks the invariant. The mistake it can
still permit is misclassifying a failure: `Strings` caches its error forever,
so wire it only to loads whose failure is deterministic. If your init can fail
transiently, this is the wrong package — see the retryable `Lazy[T]` in
exercise 05. Finally, remember that each call to `lazy.Int(fn)` mints an
independent once; store the returned function once and share it, or you have
built an expensive identity function.

## Resources

- [sync.OnceValue and sync.OnceValues](https://pkg.go.dev/sync#OnceValue) — the exact signatures and the caching contract.
- [Go 1.21 release notes, sync section](https://go.dev/doc/go1.21#sync) — where the helpers landed and why.
- [Proposal: sync: add OnceFunc, OnceValue, OnceValues](https://github.com/golang/go/issues/56102) — the design discussion, including the panic semantics decision.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-lazy-counter-oncefunc.md](02-lazy-counter-oncefunc.md)
