# Exercise 1: Forward Context Through a Repo/Service Layer Stack

The propagation contract is easiest to see — and to break — in a three-layer
stack: a repository that respects cancellation, a service that forwards its
caller's context, and a broken twin of that service that swaps in
`context.Background()`. This exercise builds all three and proves, with
side-by-side tests, that the well-behaved service honors a short deadline while
the broken one silently ignores it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
propagation/                 independent module: example.com/propagation
  go.mod                     go 1.24
  stack.go                   Repo.Get (select on ctx.Done); Service.GetUser (forwards ctx);
                             BrokenService.GetUser (context.Background anti-pattern)
  cmd/
    demo/
      main.go                runs good vs broken side by side under a 20ms budget
  stack_test.go              propagation contract + anti-pattern contract, table-driven
```

Files: `stack.go`, `cmd/demo/main.go`, `stack_test.go`.
Implement: a `Repo` key-value layer that respects `ctx` via `select` on
`time.After(delay)` vs `ctx.Done()`, a `Service` that forwards `ctx` to
`Repo.Get`, and a `BrokenService` that discards it for `context.Background()`.
Test: the well-behaved service errors within the timeout budget with
`errors.Is(err, context.DeadlineExceeded)`; the broken service returns nil error
after the full simulated latency; the happy path and the not-found branch.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/propagation/cmd/demo
cd ~/go-exercises/propagation
go mod init example.com/propagation
go mod edit -go=1.24
```

### Why the broken twin earns its place in the code

It would be enough to *describe* the `context.Background()` bug in prose, but a
description does not fail when someone reintroduces it. Shipping the anti-pattern
as a real type with a real test does two things a paragraph cannot. First, the
test `TestBrokenServiceIgnoresContext` asserts that the broken service actually
takes the *full* simulated latency even under a short timeout — if that assertion
ever flips, the anti-pattern has stopped being an anti-pattern and the lesson is
lying. Second, placing the good and broken services immediately beside each other,
identical except for the one line where `ctx` becomes `context.Background()`, puts
the entire bug on a single diff. That one line is the whole lesson.

The repository models real I/O with a `select` between `time.After(r.delay)` — a
stand-in for a slow disk read or network round-trip — and `ctx.Done()`. This is
the shape every cancellation-aware leaf has: race the work against the context,
and if the context wins, return `ctx.Err()` wrapped so the cause survives. The
service forwards its `ctx` unchanged. The broken service takes the same `ctx`,
explicitly discards it with `_ = ctx` (so the intent is unmistakable), and calls
the repo with a fresh root context that has no deadline. Under a 10ms timeout
against a 50ms repo delay, the good service returns a `DeadlineExceeded` error in
about 10ms; the broken service blocks the full 50ms and returns a value, because
the timeout it was handed never reached the layer that could act on it.

Create `stack.go`:

```go
package propagation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is returned when a key is absent from the repository.
var ErrNotFound = errors.New("propagation: key not found")

// Repo is the lowest layer: a key-value store that respects context by racing
// its simulated I/O latency against ctx.Done.
type Repo struct {
	mu    sync.RWMutex
	data  map[string]string
	delay time.Duration
}

// NewRepo returns a Repo that takes delay to serve each read, seeded with a
// defensive copy of kvs.
func NewRepo(delay time.Duration, kvs map[string]string) *Repo {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	return &Repo{data: cp, delay: delay}
}

// Get returns the value for key, honoring ctx: if the context is cancelled or
// its deadline fires before the simulated latency elapses, Get returns the
// context error wrapped so errors.Is still classifies it.
func (r *Repo) Get(ctx context.Context, key string) (string, error) {
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return "", fmt.Errorf("repo.Get %q: %w", key, ctx.Err())
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.data[key]
	if !ok {
		return "", fmt.Errorf("repo.Get %q: %w", key, ErrNotFound)
	}
	return v, nil
}

// Service is the middle layer. It forwards the caller's context to the repo,
// which is the entire point of the exercise.
type Service struct {
	repo *Repo
}

// NewService wraps repo in a well-behaved Service.
func NewService(repo *Repo) *Service { return &Service{repo: repo} }

// GetUser forwards ctx to the repository so the caller's deadline reaches the
// leaf I/O.
func (s *Service) GetUser(ctx context.Context, id string) (string, error) {
	v, err := s.repo.Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("service.GetUser: %w", err)
	}
	return v, nil
}

// BrokenService is the anti-pattern: it accepts ctx and then throws it away,
// calling the repo with context.Background(). The caller's deadline never
// reaches the repo, so a client hang-up or SLO timeout cannot stop the query.
type BrokenService struct {
	repo *Repo
}

// NewBrokenService wraps repo in the deliberately-broken Service.
func NewBrokenService(repo *Repo) *BrokenService { return &BrokenService{repo: repo} }

// GetUser demonstrates the bug: ctx is discarded and a fresh root context is
// used, detaching the repo call from the request's lifetime.
func (b *BrokenService) GetUser(ctx context.Context, id string) (string, error) {
	_ = ctx // the bug, made explicit: the caller's context is ignored.
	v, err := b.repo.Get(context.Background(), id)
	if err != nil {
		return "", fmt.Errorf("brokenService.GetUser: %w", err)
	}
	return v, nil
}
```

### The runnable demo

The demo runs the good and broken services side by side under the same 20ms
budget against a repo whose reads take 80ms, so the contrast is visible in the
elapsed column: the good service aborts at ~20ms with an error, the broken one
plods through the full ~80ms and returns a value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/propagation"
)

type userGetter interface {
	GetUser(context.Context, string) (string, error)
}

func main() {
	repo := propagation.NewRepo(80*time.Millisecond, map[string]string{"u1": "alice"})
	services := []struct {
		name string
		svc  userGetter
	}{
		{"good", propagation.NewService(repo)},
		{"broken", propagation.NewBrokenService(repo)},
	}

	for _, s := range services {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		start := time.Now()
		v, err := s.svc.GetUser(ctx, "u1")
		elapsed := time.Since(start).Truncate(10 * time.Millisecond)
		cancel()
		fmt.Printf("%s: v=%q err=%v elapsed>=%v\n", s.name, v, err != nil, elapsed)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: v="" err=true elapsed>=20ms
broken: v="alice" err=false elapsed>=80ms
```

### Tests

`TestServicePropagatesContext` and `TestBrokenServiceIgnoresContext` are the two
side-by-side contracts: identical inputs, opposite outcomes. The good service must
error within a ceiling well below the 50ms repo delay and match
`DeadlineExceeded`; the broken service must return nil at or beyond the full
delay, so the anti-pattern actually fires rather than passing by luck.
`TestServiceReturnsValueWhenFastEnough` covers the happy path, and
`TestRepoReturnsNotFound` covers the missing-key branch via a wrapped
`ErrNotFound`.

Create `stack_test.go`:

```go
package propagation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestServicePropagatesContext(t *testing.T) {
	t.Parallel()

	repo := NewRepo(50*time.Millisecond, map[string]string{"u1": "alice"})
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := svc.GetUser(ctx, "u1")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed > 40*time.Millisecond {
		t.Fatalf("service ignored parent context: took %v", elapsed)
	}
}

func TestBrokenServiceIgnoresContext(t *testing.T) {
	t.Parallel()

	repo := NewRepo(50*time.Millisecond, map[string]string{"u1": "alice"})
	svc := NewBrokenService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	v, err := svc.GetUser(ctx, "u1")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("BrokenService.GetUser: err = %v, want nil (delay must complete)", err)
	}
	if v != "alice" {
		t.Fatalf("BrokenService.GetUser: v = %q, want %q", v, "alice")
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("BrokenService should ignore the short timeout but returned in %v", elapsed)
	}
}

func TestServiceReturnsValueWhenFastEnough(t *testing.T) {
	t.Parallel()

	repo := NewRepo(5*time.Millisecond, map[string]string{"u1": "alice"})
	svc := NewService(repo)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	v, err := svc.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUser: err = %v, want nil", err)
	}
	if v != "alice" {
		t.Fatalf("GetUser: v = %q, want %q", v, "alice")
	}
}

func TestRepoReturnsNotFound(t *testing.T) {
	t.Parallel()

	repo := NewRepo(time.Millisecond, nil)
	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func ExampleService_GetUser() {
	repo := NewRepo(time.Millisecond, map[string]string{"u1": "alice"})
	svc := NewService(repo)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, _ := svc.GetUser(ctx, "u1")
	fmt.Println(v)
	// Output: alice
}
```

## Review

The stack is correct when the good and broken services differ in exactly one
observable way: the good one lets the caller's deadline reach the repo, the broken
one does not. If `TestServicePropagatesContext` ever takes near 50ms, the service
has stopped forwarding `ctx`; if `TestBrokenServiceIgnoresContext` ever returns
faster than the delay, the anti-pattern has quietly been fixed and the test should
be updated to match reality rather than deleted. The wrapped errors matter for
more than aesthetics: because `Repo.Get` returns `ctx.Err()` under `%w`, the
`errors.Is` assertion at the service boundary holds even though the error crossed
a layer. Run `go test -race` — the repo's `RWMutex` guards the map, and the demo's
concurrency-free path should stay clean.

## Resources

- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context)
- [Go Blog: Contexts and structs](https://go.dev/blog/context-and-structs)
- [`context` package](https://pkg.go.dev/context) — the first-parameter convention and `Context.Done`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-parallel-fanout-shared-context.md](02-parallel-fanout-shared-context.md)
