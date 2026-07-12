# 4. Deadlock Detection Strategies

Go's runtime detects total deadlocks — situations where every goroutine is blocked — and prints `fatal error: all goroutines are asleep - deadlock!` to stderr before exiting. Partial deadlocks, where some goroutines are live but others are permanently blocked, are silent and appear as hangs, timeouts, or resource exhaustion. This lesson builds concrete examples of each deadlock pattern, demonstrates why Go's detection works in some cases and misses others, and implements prevention strategies that make deadlock impossible by construction.

```text
deadlocks/
  go.mod
  locker/
    locker.go
    locker_test.go
  cmd/demo/
    main.go
```

## Concepts

### The Four Necessary Conditions

A deadlock requires all four conditions simultaneously (Coffman 1971): mutual exclusion (a resource is held by at most one goroutine), hold-and-wait (a goroutine holds one resource and waits for another), no preemption (a held resource cannot be taken away), and circular wait (goroutines form a cycle in which each waits for a resource held by the next). Eliminating any one condition prevents the deadlock.

Go's most practical approach to prevention is eliminating circular wait by enforcing a consistent lock ordering.

### Total vs Partial Deadlock

Go's runtime detects total deadlocks using a simple invariant: if `runtime.NumGoroutine() > 0` and no goroutine is runnable, the scheduler cannot make progress. It fires `throw("all goroutines are asleep - deadlock!")`.

A partial deadlock bypasses this check because at least one goroutine remains alive. A typical case: a goroutine runs `time.Sleep` while two other goroutines are deadlocked on mutexes. The sleeping goroutine keeps the process alive; the deadlocked pair is invisible to the runtime.

### Lock Ordering

The canonical prevention strategy is a global lock ordering: assign a unique integer rank to every mutex and require that goroutines always acquire mutexes in ascending rank order. A goroutine that wants locks A (rank 1) and B (rank 2) must acquire A first. No two goroutines can form a cycle because the cycle would require one goroutine to hold a higher-rank lock and wait for a lower-rank one, which the ordering forbids.

### Go Mutexes Are Not Reentrant

`sync.Mutex` and `sync.RWMutex` are not reentrant. If a goroutine holds a mutex and calls a function that also tries to acquire the same mutex, the goroutine deadlocks with itself. The fix is to refactor into a "locked" internal variant that assumes the lock is held by the caller.

### Context Timeouts as a Detection Fallback

For lock acquisitions that may fail in partial-deadlock scenarios, a `context.WithTimeout` wrapped around the operation provides a detection-and-recovery path: if the lock cannot be acquired within the deadline, the operation returns an error instead of blocking forever. This is a detection mechanism, not a prevention one — the root cause must still be fixed.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/32-concurrency-debugging-and-testing/04-deadlock-detection-strategies/04-deadlock-detection-strategies/locker
mkdir -p go-solutions/32-concurrency-debugging-and-testing/04-deadlock-detection-strategies/04-deadlock-detection-strategies/cmd/demo
cd go-solutions/32-concurrency-debugging-and-testing/04-deadlock-detection-strategies/04-deadlock-detection-strategies
```

### Exercise 1: Safe Ordered Locker

Create `locker/locker.go`:

```go
package locker

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrLockTimeout is returned when a timed lock acquisition exceeds its deadline.
var ErrLockTimeout = errors.New("lock acquisition timed out")

// Resource represents a resource that requires a mutex to access.
// Rank is used for deadlock-free multi-lock acquisition.
type Resource struct {
	mu   sync.Mutex
	Rank int
	Name string
	val  int
}

// NewResource creates a named resource with the given rank.
func NewResource(name string, rank int) *Resource {
	return &Resource{Name: name, Rank: rank}
}

// WithLock calls f while holding r's mutex.
func (r *Resource) WithLock(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// Set stores v under the resource's lock.
func (r *Resource) Set(v int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.val = v
}

// Value returns the stored value under the resource's lock.
func (r *Resource) Value() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.val
}

// AcquireInOrder acquires a set of resources in ascending rank order.
// This is deadlock-free because no cycle can form in a strict total order.
// Returns a release function that must be deferred by the caller.
func AcquireInOrder(resources ...*Resource) (release func()) {
	// Sort by rank to enforce consistent ordering.
	sorted := make([]*Resource, len(resources))
	copy(sorted, resources)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Rank < sorted[j-1].Rank; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for _, res := range sorted {
		res.mu.Lock()
	}
	return func() {
		// Release in reverse order (good practice; required by some platforms).
		for i := len(sorted) - 1; i >= 0; i-- {
			sorted[i].mu.Unlock()
		}
	}
}

// TransferWithTimeout transfers value from src to dst within the deadline.
// Uses AcquireInOrder to prevent deadlock and ctx to prevent indefinite blocking.
func TransferWithTimeout(ctx context.Context, src, dst *Resource, val int) error {
	done := make(chan struct{}, 1)
	var transferErr error

	go func() {
		release := AcquireInOrder(src, dst)
		defer release()
		if src.val < val {
			transferErr = fmt.Errorf("insufficient: have %d, want %d", src.val, val)
		} else {
			src.val -= val
			dst.val += val
		}
		done <- struct{}{}
	}()

	select {
	case <-done:
		return transferErr
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrLockTimeout, ctx.Err())
	}
}

// InnerLocked is a function that must be called with r's lock held.
// It is the canonical pattern for non-reentrant mutexes.
func (r *Resource) innerLocked(delta int) {
	r.val += delta // called only when mu is held by the caller
}

// IncrementTwice increments the resource twice in a single lock acquisition.
// This is the fix for the self-deadlock pattern.
func (r *Resource) IncrementTwice(a, b int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.innerLocked(a)
	r.innerLocked(b)
}
```

### Exercise 2: Tests for Lock Ordering and Deadlock Prevention

Create `locker/locker_test.go`:

```go
package locker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestAcquireInOrderNoCycleIsPossible proves deadlock cannot occur with ordered acquisition.
// Two goroutines each acquire both resources; neither will deadlock because both
// always take rank-1 first then rank-2.
func TestAcquireInOrderNoCycleIsPossible(t *testing.T) {
	t.Parallel()

	a := NewResource("account-A", 1)
	b := NewResource("account-B", 2)
	a.Set(100)
	b.Set(200)

	const goroutines = 20
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release := AcquireInOrder(a, b)
			defer release()
			// Swap values atomically under both locks.
			a.val, b.val = b.val, a.val
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// No deadlock: all goroutines completed.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: goroutines did not complete within 5s")
	}
}

// TestAcquireInOrderReverseInputOrder verifies the sort works regardless of argument order.
func TestAcquireInOrderReverseInputOrder(t *testing.T) {
	t.Parallel()

	a := NewResource("a", 1)
	b := NewResource("b", 2)
	c := NewResource("c", 3)

	// Pass in reverse order: should still acquire a < b < c.
	release := AcquireInOrder(c, b, a)
	defer release()
	// If we got here without deadlock, the sort worked.
}

// TestIncrementTwiceNoSelfDeadlock verifies IncrementTwice does not self-deadlock.
func TestIncrementTwiceNoSelfDeadlock(t *testing.T) {
	t.Parallel()

	r := NewResource("counter", 1)
	r.Set(10)

	done := make(chan struct{})
	go func() {
		r.IncrementTwice(5, 3)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("IncrementTwice deadlocked")
	}

	if got := r.Value(); got != 18 {
		t.Fatalf("Value() = %d, want 18", got)
	}
}

// TestTransferWithTimeoutSuccess transfers a value between resources.
func TestTransferWithTimeoutSuccess(t *testing.T) {
	t.Parallel()

	src := NewResource("src", 1)
	dst := NewResource("dst", 2)
	src.Set(100)
	dst.Set(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := TransferWithTimeout(ctx, src, dst, 40); err != nil {
		t.Fatalf("TransferWithTimeout: %v", err)
	}

	if got := src.Value(); got != 60 {
		t.Fatalf("src = %d, want 60", got)
	}
	if got := dst.Value(); got != 40 {
		t.Fatalf("dst = %d, want 40", got)
	}
}

// TestTransferWithTimeoutInsufficientFunds verifies the error path.
func TestTransferWithTimeoutInsufficientFunds(t *testing.T) {
	t.Parallel()

	src := NewResource("src", 1)
	dst := NewResource("dst", 2)
	src.Set(10)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := TransferWithTimeout(ctx, src, dst, 100)
	if err == nil {
		t.Fatal("expected error for insufficient funds, got nil")
	}
}

// TestTransferContextCancellation verifies the timeout path returns ErrLockTimeout.
func TestTransferContextCancellation(t *testing.T) {
	t.Parallel()

	src := NewResource("src", 1)
	dst := NewResource("dst", 2)
	src.Set(50)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := TransferWithTimeout(ctx, src, dst, 10)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
}

// TestValueUnderLoad verifies Value and Set are race-free under concurrent access.
func TestValueUnderLoad(t *testing.T) {
	t.Parallel()

	r := NewResource("r", 1)
	var wg sync.WaitGroup
	const n = 100
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(v int) {
			defer wg.Done()
			r.Set(v)
		}(i)
		go func() {
			defer wg.Done()
			_ = r.Value()
		}()
	}
	wg.Wait()
}

// ExampleAcquireInOrder shows the ordered acquisition pattern.
func ExampleAcquireInOrder() {
	a := NewResource("a", 1)
	b := NewResource("b", 2)
	a.Set(10)
	b.Set(20)

	release := AcquireInOrder(b, a) // input order does not matter; rank decides
	defer release()
	// a and b are both locked here; modify them safely.
	// Output:
}
```

Your turn: add `TestConcurrentTransfers` that launches 10 goroutines, each calling `TransferWithTimeout` to transfer 1 unit from resource A to resource B, then 10 goroutines transferring in the opposite direction, and asserts that after all goroutines complete the sum `A.Value() + B.Value()` equals the initial sum. Use `t.Parallel()`.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/deadlocks/locker"
)

func main() {
	a := locker.NewResource("account-A", 1)
	b := locker.NewResource("account-B", 2)
	a.Set(1000)
	b.Set(500)

	fmt.Printf("initial: A=%d B=%d\n", a.Value(), b.Value())

	const transfers = 20
	var wg sync.WaitGroup
	for i := 0; i < transfers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			src, dst := a, b
			if n%2 == 0 {
				src, dst = b, a
			}
			if err := locker.TransferWithTimeout(ctx, src, dst, 10); err != nil {
				fmt.Printf("transfer %d: %v\n", n, err)
			}
		}(i)
	}
	wg.Wait()

	fmt.Printf("final:   A=%d B=%d sum=%d\n", a.Value(), b.Value(), a.Value()+b.Value())
}
```

## Common Mistakes

### Acquiring Locks in Different Orders in Different Code Paths

Wrong: function `deposit` acquires `mu_account` then `mu_ledger`; function `reconcile` acquires `mu_ledger` then `mu_account`. Under concurrent calls the two goroutines deadlock.

What happens: the partial deadlock is silent unless both goroutines happen to be the only live goroutines.

Fix: enforce a global rank order enforced at every call site. `AcquireInOrder` above does this by sorting before locking regardless of argument order.

### Locking in a defer Inside a Loop

Wrong:

```go
for _, r := range resources {
	r.mu.Lock()
	defer r.mu.Unlock() // defers are collected, not released at loop end
	process(r)
}
```

What happens: all locks are held until the enclosing function returns, not until the end of each loop iteration. This widens the critical section and increases contention.

Fix: extract the loop body into a function, or use an anonymous function:

```go
for _, r := range resources {
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		process(r)
	}()
}
```

### Using tryLock With a Goroutine to Add a Timeout

Wrong:

```go
func tryLock(mu *sync.Mutex, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false // goroutine is now leaked: it will block on mu forever
	}
}
```

What happens: when the timeout fires, the goroutine that called `mu.Lock()` is still blocked and leaks.

Fix: design code so that partial deadlock cannot occur in the first place — use lock ordering. If a timeout is genuinely needed, use a channel to transfer ownership rather than a mutex.

## Verification

From `~/go-exercises/deadlocks`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race -timeout 30s ./...
go run ./cmd/demo
```

The `-timeout 30s` flag ensures the test suite fails fast if a deadlock test does hang. All tests must complete; the demo must print matching initial and final sums.

## Summary

- Go detects total deadlocks (all goroutines asleep) but not partial ones (some goroutines live, others permanently blocked).
- Circular wait is the preventable condition: enforce a global lock rank and always acquire in ascending order.
- `sync.Mutex` is not reentrant; a goroutine that calls Lock twice deadlocks with itself. Split into public (locks) and internal (assumes locked) variants.
- Use `context.WithTimeout` as a detection fallback for operations that may block indefinitely, but fix the root cause rather than relying on the timeout.
- `defer` in a loop delays release until the function returns, not until loop iteration end.

## What's Next

Next: [Contention Analysis](../05-contention-analysis/05-contention-analysis.md).

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [The Go Memory Model](https://go.dev/ref/mem)
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)
- [Coffman conditions (Wikipedia)](https://en.wikipedia.org/wiki/Deadlock#Coffman_conditions)
