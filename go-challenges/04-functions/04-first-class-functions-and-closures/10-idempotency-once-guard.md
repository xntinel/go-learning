# Exercise 10: Exactly-Once Initialization Guard with a Captured sync.Once

Lazy singletons — a DB pool or HTTP client built on first use and shared
thereafter — are the last stateful-closure pattern in this lesson. A `Provider`
captures a `sync.Once` and memoizes the built client and its error, so an
expensive connect runs exactly once even when many goroutines race to be the first
caller. This deliberately makes the *opposite* error-caching choice from the map
memoizer in Exercise 5, and tests the choice explicitly.

This module is fully self-contained.

## What you'll build

```text
onceguard/                 independent module: example.com/onceguard
  go.mod                   go 1.26
  provider.go              Provider, NewProvider, Get (sync.Once memoized)
  cmd/
    demo/
      main.go              5 goroutines race Get; the builder runs once
  provider_test.go         once under concurrency, same pointer, error cached
```

- Files: `provider.go`, `cmd/demo/main.go`, `provider_test.go`.
- Implement: a `Provider` whose `Get() (*Client, error)` captures a `sync.Once` and memoizes `client` + `err`; the injected `build` runs exactly once, and a build error is cached and returned to every caller (not retried).
- Test: many goroutines calling `Get` under `-race` see the builder run once and all receive the same pointer; a builder that errors returns the error to all callers and is not retried.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/onceguard/cmd/demo
cd ~/go-exercises/onceguard
go mod init example.com/onceguard
```

### sync.Once versus the memoize map

`sync.Once` guarantees the function passed to `Do` runs exactly once, no matter
how many goroutines call `Do` concurrently; the others block until the first
finishes, then all return. Capturing a `Once` (plus the fields it populates) is
the idiomatic way to build a lazy singleton: the first `Get` builds the client and
stores it and any error; every later `Get` returns the memoized result without
touching the builder. That is stronger than a plain check-then-build, which two
goroutines can both pass before either stores a result, building twice.

The design contrast with Exercise 5's memoizer is the point of this module. The
map memoizer serves *many* keys and, by policy, does *not* cache errors — a
transient failure retries. This once-guard serves *one* resource and, by policy,
*does* cache its error: if the first build fails, `sync.Once` has fired and will
never run the builder again, so every caller gets that same error forever. Neither
policy is universally right. Caching the error suits a resource whose failure is
structural (bad config, missing credentials) where retrying is pointless; not
caching suits a transient dependency you want to reconnect to. The non-negotiable
is to *decide and document* which you built, and test it — which is why this
module's test asserts the builder is not retried after an error, and Exercise 5's
asserts the opposite.

`Get` wraps a non-nil cached error with `%w` on each call so callers can match a
sentinel with `errors.Is`, while the underlying cause is computed only once.

Create `provider.go`:

```go
package onceguard

import (
	"fmt"
	"sync"
)

// Client is a stand-in for an expensive-to-build resource (a DB pool, an HTTP
// client) that should be created once and shared.
type Client struct {
	DSN string
}

// Provider lazily builds a *Client exactly once, even under concurrent first
// calls, and memoizes the result including any error.
type Provider struct {
	once   sync.Once
	client *Client
	err    error
	build  func() (*Client, error)
}

// NewProvider returns a Provider that will call build on the first Get.
func NewProvider(build func() (*Client, error)) *Provider {
	return &Provider{build: build}
}

// Get returns the memoized client, building it on the first call. The build runs
// exactly once; a build error is cached and returned to every caller (it is NOT
// retried).
func (p *Provider) Get() (*Client, error) {
	p.once.Do(func() {
		p.client, p.err = p.build()
	})
	if p.err != nil {
		return nil, fmt.Errorf("provider: %w", p.err)
	}
	return p.client, nil
}
```

### The runnable demo

The demo launches five goroutines that all race to be the first `Get`, then reads
once more. The builder increments a counter, and the output shows it ran exactly
once despite the race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/onceguard"
)

func main() {
	builds := 0
	p := onceguard.NewProvider(func() (*onceguard.Client, error) {
		builds++
		return &onceguard.Client{DSN: "postgres://localhost/app"}, nil
	})

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Get()
		}()
	}
	wg.Wait()

	c, _ := p.Get()
	fmt.Printf("builds=%d dsn=%s\n", builds, c.DSN)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
builds=1 dsn=postgres://localhost/app
```

### Tests

Create `provider_test.go`:

```go
package onceguard

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetBuildsOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	var builds atomic.Int64
	p := NewProvider(func() (*Client, error) {
		builds.Add(1)
		return &Client{DSN: "dsn"}, nil
	})

	const n = 100
	ptrs := make(chan *Client, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Get()
			if err != nil {
				t.Errorf("Get err = %v", err)
			}
			ptrs <- c
		}()
	}
	wg.Wait()
	close(ptrs)

	if got := builds.Load(); got != 1 {
		t.Fatalf("builder ran %d times, want 1", got)
	}
	first := <-ptrs
	for c := range ptrs {
		if c != first {
			t.Fatal("callers received different *Client pointers; want one shared instance")
		}
	}
}

func TestGetCachesError(t *testing.T) {
	t.Parallel()
	errConnect := errors.New("connect refused")
	var builds atomic.Int64
	p := NewProvider(func() (*Client, error) {
		builds.Add(1)
		return nil, errConnect
	})

	for range 3 {
		c, err := p.Get()
		if c != nil {
			t.Fatalf("client = %v, want nil on error", c)
		}
		if !errors.Is(err, errConnect) {
			t.Fatalf("err = %v, want to wrap errConnect", err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("builder ran %d times, want 1 (error is cached, not retried)", got)
	}
}
```

## Review

The provider is correct when the builder runs exactly once regardless of how many
goroutines race the first `Get`, all callers receive the same `*Client` pointer,
and — by documented policy — a build error is cached so every caller sees it and
the builder is never retried. `sync.Once` is what makes the single-execution
guarantee hold under concurrency, which is why `TestGetBuildsOnceUnderConcurrency`
is clean under `-race` and the pointer-identity check passes. The error-caching
choice is the deliberate contrast with the map memoizer of Exercise 5: same
"call the expensive thing once" idea, opposite error policy, each tested against
the behavior it documents. Run `go test -race`.

## Resources

- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — exactly-once execution of `Do`.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — matching the cached wrapped error.
- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — the captured `build` closure.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-method-value-shutdown-registry.md](09-method-value-shutdown-registry.md) | Next: [11-token-bucket-closure.md](11-token-bucket-closure.md)
