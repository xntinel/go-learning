# Exercise 1: The Instrumented Repository

This exercise builds the interface incarnation of the decorator pattern: a domain `UserRepository`, a real in-memory implementation, and four wrappers — logging, metrics, caching, and retry — that each add one cross-cutting concern and forward the rest. You assemble them into a single stack and prove that the stack behaves as one repository.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
repository.go        UserRepository, MemoryRepository, and four decorators:
                     LoggingRepository, MetricsRepository,
                     CachingRepository, RetryRepository
cmd/
  demo/
    main.go          stack the four decorators and watch each layer act
repository_test.go   delegation, per-call metrics, cache miss/hit, retry on
                     transient only, exhaustion wrapping, the full stack
example_test.go      a go-doc Example that proves logging is transparent
```

- Files: `repository.go`, `cmd/demo/main.go`, `repository_test.go`, `example_test.go`.
- Implement: `UserRepository` plus `MemoryRepository` and the `Logging`, `Metrics`, `Caching`, and `Retry` decorators, each with a `New…` constructor that stores the interface.
- Test: delegation is transparent, metrics count every call, the cache serves the second read and never caches errors, retry covers only transient failures and wraps on exhaustion, and the four-layer stack composes correctly.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/08-middleware-decorator-pattern/01-instrumented-repository/cmd/demo && cd go-solutions/24-design-patterns-in-go/08-middleware-decorator-pattern/01-instrumented-repository
```

### The interface is the contract every layer shares

The pattern starts and ends with one decision: every decorator stores the interface it decorates, not a concrete type. `MemoryRepository` is the real work — an in-memory map guarded by a mutex — but no decorator mentions it. Each decorator holds a `UserRepository` field, implements `UserRepository` itself, and adds its concern around the delegating call. That is what lets `LoggingRepository` wrap a `CachingRepository` wrap a `RetryRepository` wrap the real store: each layer is the same type as the one below it, so the wrapping never bottoms out until it reaches the implementation you choose to put at the bottom.

`MemoryRepository` carries one piece of test scaffolding worth understanding before you read the retry decorator. `FailNextN(id, n)` arms the next `n` calls to `GetByID(id)` to return `ErrTransient` before succeeding, and `GetByID` decrements that counter under the lock on each failing call. This is the seam that makes retries testable and demonstrable without a flaky network: a test can say "fail twice, then succeed" and assert the retry decorator drove the operation to completion in exactly three attempts. The counter is decremented under `sync.Mutex` rather than `sync.RWMutex` because the read path now mutates state, so it needs the exclusive lock.

The three error sentinels divide the world the way the retry decorator needs. `ErrTransient` is the only retryable error; `ErrNotFound` and `ErrDuplicate` are permanent and must propagate on the first attempt. The retry predicate `isTransient` is a single `errors.Is` against the transient sentinel, and it is the seam that keeps the decorator from amplifying load on problems a retry cannot fix.

Create `repository.go`:

```go
package decorator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// User is the domain entity the repository stores.
type User struct {
	ID    string
	Name  string
	Email string
}

var (
	// ErrNotFound is a permanent error: the user does not exist. Retrying it is pointless.
	ErrNotFound = errors.New("decorator: user not found")
	// ErrTransient is a retryable error: a temporary failure that a later attempt may survive.
	ErrTransient = errors.New("decorator: transient failure")
	// ErrDuplicate is a permanent error: the user already exists.
	ErrDuplicate = errors.New("decorator: duplicate user")
)

// UserRepository is the abstraction every decorator wraps and implements.
type UserRepository interface {
	GetByID(ctx context.Context, id string) (*User, error)
	Create(ctx context.Context, user *User) error
	Count(ctx context.Context) (int, error)
}

// MemoryRepository is the real, in-memory implementation at the bottom of any stack.
type MemoryRepository struct {
	mu         sync.Mutex
	users      map[string]*User
	failNTimes map[string]int
}

// NewMemoryRepository returns an empty in-memory repository.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		users:      make(map[string]*User),
		failNTimes: make(map[string]int),
	}
}

// FailNextN arms the next n GetByID calls for id to return ErrTransient before
// succeeding, so a test or demo can exercise the retry decorator deterministically.
func (r *MemoryRepository) FailNextN(id string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNTimes[id] = n
}

func (r *MemoryRepository) GetByID(_ context.Context, id string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNTimes[id] > 0 {
		r.failNTimes[id]--
		return nil, ErrTransient
	}
	u, ok := r.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (r *MemoryRepository) Create(_ context.Context, user *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[user.ID]; ok {
		return ErrDuplicate
	}
	r.users[user.ID] = user
	return nil
}

func (r *MemoryRepository) Count(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.users), nil
}

// isTransient is the retry predicate: only transient failures are worth retrying.
func isTransient(err error) bool { return errors.Is(err, ErrTransient) }

// LoggingRepository logs every call and forwards it unchanged.
type LoggingRepository struct {
	next   UserRepository
	logger *slog.Logger
}

// NewLoggingRepository wraps next. A nil logger defaults to slog.Default.
func NewLoggingRepository(next UserRepository, logger *slog.Logger) *LoggingRepository {
	if next == nil {
		panic("decorator: LoggingRepository requires a next repository")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &LoggingRepository{next: next, logger: logger}
}

func (r *LoggingRepository) GetByID(ctx context.Context, id string) (*User, error) {
	start := time.Now()
	user, err := r.next.GetByID(ctx, id)
	r.logger.Info("GetByID", "id", id, "duration", time.Since(start), "error", err)
	return user, err
}

func (r *LoggingRepository) Create(ctx context.Context, user *User) error {
	start := time.Now()
	err := r.next.Create(ctx, user)
	r.logger.Info("Create", "id", user.ID, "duration", time.Since(start), "error", err)
	return err
}

func (r *LoggingRepository) Count(ctx context.Context) (int, error) {
	start := time.Now()
	count, err := r.next.Count(ctx)
	r.logger.Info("Count", "duration", time.Since(start), "error", err)
	return count, err
}

// MetricsRepository records a call count and total latency per method, on every
// call regardless of outcome, and forwards unchanged.
type MetricsRepository struct {
	mu     sync.Mutex
	next   UserRepository
	counts map[string]int
	total  map[string]time.Duration
}

// NewMetricsRepository wraps next.
func NewMetricsRepository(next UserRepository) *MetricsRepository {
	if next == nil {
		panic("decorator: MetricsRepository requires a next repository")
	}
	return &MetricsRepository{
		next:   next,
		counts: make(map[string]int),
		total:  make(map[string]time.Duration),
	}
}

func (r *MetricsRepository) record(name string, start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[name]++
	r.total[name] += time.Since(start)
}

func (r *MetricsRepository) GetByID(ctx context.Context, id string) (*User, error) {
	start := time.Now()
	user, err := r.next.GetByID(ctx, id)
	r.record("GetByID", start)
	return user, err
}

func (r *MetricsRepository) Create(ctx context.Context, user *User) error {
	start := time.Now()
	err := r.next.Create(ctx, user)
	r.record("Create", start)
	return err
}

func (r *MetricsRepository) Count(ctx context.Context) (int, error) {
	start := time.Now()
	count, err := r.next.Count(ctx)
	r.record("Count", start)
	return count, err
}

// Counts returns a copy of the per-method call counts.
func (r *MetricsRepository) Counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// TotalDuration returns a copy of the per-method total latency.
func (r *MetricsRepository) TotalDuration() map[string]time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]time.Duration, len(r.total))
	for k, v := range r.total {
		out[k] = v
	}
	return out
}

// CachingRepository serves GetByID from an in-memory map when present, and
// populates it on a successful GetByID or Create. Errors are never cached.
type CachingRepository struct {
	mu    sync.RWMutex
	next  UserRepository
	cache map[string]*User
	hits  int
	miss  int
}

// NewCachingRepository wraps next with an empty cache.
func NewCachingRepository(next UserRepository) *CachingRepository {
	if next == nil {
		panic("decorator: CachingRepository requires a next repository")
	}
	return &CachingRepository{next: next, cache: make(map[string]*User)}
}

func (r *CachingRepository) GetByID(ctx context.Context, id string) (*User, error) {
	r.mu.RLock()
	if user, ok := r.cache[id]; ok {
		r.mu.RUnlock()
		r.mu.Lock()
		r.hits++
		r.mu.Unlock()
		return user, nil
	}
	r.mu.RUnlock()

	user, err := r.next.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[id] = user
	r.miss++
	r.mu.Unlock()
	return user, nil
}

func (r *CachingRepository) Create(ctx context.Context, user *User) error {
	err := r.next.Create(ctx, user)
	if err == nil {
		r.mu.Lock()
		r.cache[user.ID] = user
		r.mu.Unlock()
	}
	return err
}

func (r *CachingRepository) Count(ctx context.Context) (int, error) {
	return r.next.Count(ctx)
}

// Len returns the number of cached users.
func (r *CachingRepository) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// Hits returns the number of GetByID calls served from the cache.
func (r *CachingRepository) Hits() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hits
}

// Misses returns the number of GetByID calls that fell through to next and were cached.
func (r *CachingRepository) Misses() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.miss
}

// RetryRepository retries transient failures up to max attempts and propagates
// permanent errors immediately.
type RetryRepository struct {
	mu       sync.Mutex
	next     UserRepository
	max      int
	attempts map[string]int
}

// NewRetryRepository wraps next, retrying up to max attempts (at least one).
func NewRetryRepository(next UserRepository, max int) *RetryRepository {
	if next == nil {
		panic("decorator: RetryRepository requires a next repository")
	}
	if max < 1 {
		max = 1
	}
	return &RetryRepository{next: next, max: max, attempts: make(map[string]int)}
}

func (r *RetryRepository) GetByID(ctx context.Context, id string) (*User, error) {
	var lastErr error
	for attempt := 1; attempt <= r.max; attempt++ {
		user, err := r.next.GetByID(ctx, id)
		r.recordAttempt("GetByID", id, attempt)
		if err == nil {
			return user, nil
		}
		if !isTransient(err) {
			return nil, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("decorator: GetByID exhausted %d attempts: %w", r.max, lastErr)
}

func (r *RetryRepository) Create(ctx context.Context, user *User) error {
	var lastErr error
	for attempt := 1; attempt <= r.max; attempt++ {
		err := r.next.Create(ctx, user)
		r.recordAttempt("Create", user.ID, attempt)
		if err == nil {
			return nil
		}
		if !isTransient(err) {
			return err
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("decorator: Create exhausted %d attempts: %w", r.max, lastErr)
}

func (r *RetryRepository) Count(ctx context.Context) (int, error) {
	return r.next.Count(ctx)
}

func (r *RetryRepository) recordAttempt(op, id string, attempt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts[op+":"+id] = attempt
}

// Attempts returns how many attempts the last op:id call took.
func (r *RetryRepository) Attempts(op, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[op+":"+id]
}
```

Read the four decorators as variations on one shape. `LoggingRepository` and `MetricsRepository` are pure pass-throughs that observe: they record `time.Since(start)` around the delegation and forward the result untouched, and crucially they record on every call, not only successful ones, so a failing `GetByID("missing")` still increments the counter. `CachingRepository` short-circuits: a cache hit returns without ever touching `next`, which is why the metrics layer must sit outside the cache if you want it to count the logical call rather than only the misses. The cache reads under `RLock` so concurrent hits proceed in parallel, takes the exclusive `Lock` only to mutate the counter or insert, and — the property a test pins — only inserts when `next` returned a nil error, so a transient failure is never cached. `RetryRepository` is the only layer that calls `next` more than once: it loops up to `max` attempts, returns immediately on success or on any non-transient error, records the attempt count, checks `ctx.Err()` between attempts so a cancelled context stops the loop, and wraps the last transient error with `%w` when the budget is spent.

### The runnable demo

The demo seeds the real repository directly with one user, arms two transient failures, then builds the stack `Logging(Metrics(Caching(Retry(Real))))` and drives it. The first `GetByID` misses the cache, descends to retry, fails twice and succeeds on the third attempt; the second `GetByID` is served from the warm cache without touching retry at all; and a final lookup of a missing id propagates `ErrNotFound` through every layer unretried. The logger writes to `io.Discard` so the output is exactly the lines the demo prints.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"example.com/instrumented-repository"
)

func main() {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Seed the real repository directly, then arm two transient failures so the
	// first GetByID exercises the retry layer before the cache is warm.
	mem := decorator.NewMemoryRepository()
	if err := mem.Create(ctx, &decorator.User{ID: "1", Name: "Alice"}); err != nil {
		fmt.Println("seed:", err)
		os.Exit(1)
	}
	mem.FailNextN("1", 2)

	// Stack: Logging(Metrics(Caching(Retry(Real)))). Logging is outermost.
	retry := decorator.NewRetryRepository(mem, 3)
	caching := decorator.NewCachingRepository(retry)
	metrics := decorator.NewMetricsRepository(caching)
	var repo decorator.UserRepository = decorator.NewLoggingRepository(metrics, quiet)

	got, err := repo.GetByID(ctx, "1")
	if err != nil {
		fmt.Println("first GetByID:", err)
		os.Exit(1)
	}
	fmt.Printf("first call: %s (retry attempts=%d)\n", got.Name, retry.Attempts("GetByID", "1"))

	got, err = repo.GetByID(ctx, "1")
	if err != nil {
		fmt.Println("second GetByID:", err)
		os.Exit(1)
	}
	fmt.Printf("second call: %s\n", got.Name)
	fmt.Printf("cache hits=%d misses=%d\n", caching.Hits(), caching.Misses())

	count, _ := repo.Count(ctx)
	fmt.Printf("count=%d\n", count)

	_, err = repo.GetByID(ctx, "missing")
	fmt.Printf("missing handled as not-found: %v\n", errors.Is(err, decorator.ErrNotFound))

	c := metrics.Counts()
	fmt.Printf("metrics: GetByID=%d Count=%d\n", c["GetByID"], c["Count"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first call: Alice (retry attempts=3)
second call: Alice
cache hits=1 misses=1
count=1
missing handled as not-found: true
metrics: GetByID=3 Count=1
```

The metrics layer reports `GetByID=3` because it sits outside the cache and counts all three logical reads (first, cached second, and the missing lookup), even though only one of them descended past the cache.

### Tests

The tests pin each decorator's contract independently and then the stack as a whole. Delegation must be transparent; metrics must count failures as well as successes; the cache must serve the second read, expose a hit, and never cache an error; retry must drive a transient failure to success, refuse to retry a permanent error, and wrap on exhaustion; and the four-layer stack must compose so that the cache absorbs the second read while metrics still counts both and retry records its attempts.

Create `repository_test.go`:

```go
package decorator

import (
	"errors"
	"io"
	"log/slog"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seeded(t *testing.T, users ...*User) *MemoryRepository {
	t.Helper()
	mem := NewMemoryRepository()
	for _, u := range users {
		if err := mem.Create(t.Context(), u); err != nil {
			t.Fatalf("seed Create(%s): %v", u.ID, err)
		}
	}
	return mem
}

func TestLoggingRepository_DelegatesTransparently(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	logged := NewLoggingRepository(mem, quietLogger())

	got, err := logged.GetByID(t.Context(), "1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Name = %q, want Alice", got.Name)
	}

	if _, err := logged.GetByID(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMetricsRepository_CountsEveryCall(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	m := NewMetricsRepository(mem)

	_, _ = m.GetByID(t.Context(), "1")
	_, _ = m.GetByID(t.Context(), "1")
	_, _ = m.GetByID(t.Context(), "missing") // failure must still be counted
	_ = m.Create(t.Context(), &User{ID: "2", Name: "Bob"})

	counts := m.Counts()
	if counts["GetByID"] != 3 {
		t.Errorf("GetByID count = %d, want 3", counts["GetByID"])
	}
	if counts["Create"] != 1 {
		t.Errorf("Create count = %d, want 1", counts["Create"])
	}
	if m.TotalDuration()["GetByID"] < 0 {
		t.Errorf("TotalDuration GetByID = %v, want >= 0", m.TotalDuration()["GetByID"])
	}
}

func TestCachingRepository_MissThenHit(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	c := NewCachingRepository(mem)

	got, err := c.GetByID(t.Context(), "1")
	if err != nil || got.Name != "Alice" {
		t.Fatalf("first call: got=%+v err=%v", got, err)
	}
	if c.Hits() != 0 || c.Misses() != 1 {
		t.Errorf("after miss: hits=%d misses=%d, want 0/1", c.Hits(), c.Misses())
	}

	got, err = c.GetByID(t.Context(), "1")
	if err != nil || got.Name != "Alice" {
		t.Fatalf("second call: got=%+v err=%v", got, err)
	}
	if c.Hits() != 1 {
		t.Errorf("after hit: hits=%d, want 1", c.Hits())
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}

func TestCachingRepository_DoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	mem := NewMemoryRepository()
	c := NewCachingRepository(mem)

	if _, err := c.GetByID(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first: err = %v, want ErrNotFound", err)
	}
	if _, err := c.GetByID(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second: err = %v, want ErrNotFound", err)
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d, want 0 (errors must not be cached)", c.Len())
	}
}

func TestRetryRepository_RetriesTransient(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	mem.FailNextN("1", 2) // two transient failures, then success
	r := NewRetryRepository(mem, 3)

	got, err := r.GetByID(t.Context(), "1")
	if err != nil {
		t.Fatalf("GetByID after retries: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Name = %q, want Alice", got.Name)
	}
	if r.Attempts("GetByID", "1") != 3 {
		t.Errorf("Attempts = %d, want 3", r.Attempts("GetByID", "1"))
	}
}

func TestRetryRepository_DoesNotRetryPermanent(t *testing.T) {
	t.Parallel()

	mem := NewMemoryRepository()
	r := NewRetryRepository(mem, 3)

	if _, err := r.GetByID(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if r.Attempts("GetByID", "missing") != 1 {
		t.Errorf("Attempts = %d, want 1 (no retry on permanent error)", r.Attempts("GetByID", "missing"))
	}
}

func TestRetryRepository_ExhaustsAndWraps(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	mem.FailNextN("1", 99) // always transient
	r := NewRetryRepository(mem, 2)

	_, err := r.GetByID(t.Context(), "1")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !errors.Is(err, ErrTransient) {
		t.Errorf("err = %v, want wrap of ErrTransient", err)
	}
	if r.Attempts("GetByID", "1") != 2 {
		t.Errorf("Attempts = %d, want 2", r.Attempts("GetByID", "1"))
	}
}

func TestStackedDecorators_ComposeAsOne(t *testing.T) {
	t.Parallel()

	mem := seeded(t, &User{ID: "1", Name: "Alice"})
	mem.FailNextN("1", 1) // one transient failure on the first GetByID

	retry := NewRetryRepository(mem, 3)
	caching := NewCachingRepository(retry)
	metrics := NewMetricsRepository(caching)
	var repo UserRepository = NewLoggingRepository(metrics, quietLogger())

	got, err := repo.GetByID(t.Context(), "1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Name = %q, want Alice", got.Name)
	}

	got, err = repo.GetByID(t.Context(), "1") // served from cache
	if err != nil {
		t.Fatalf("cached GetByID: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("cached Name = %q, want Alice", got.Name)
	}

	if metrics.Counts()["GetByID"] != 2 {
		t.Errorf("metrics GetByID = %d, want 2", metrics.Counts()["GetByID"])
	}
	if caching.Hits() != 1 {
		t.Errorf("cache hits = %d, want 1", caching.Hits())
	}
	if retry.Attempts("GetByID", "1") != 2 {
		t.Errorf("retry attempts = %d, want 2", retry.Attempts("GetByID", "1"))
	}
}
```

The go-doc Example doubles as a regression test: `go test` runs it and compares its printed output to the `// Output:` comment, so if `LoggingRepository` ever stops delegating, the example fails.

Create `example_test.go`:

```go
package decorator_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"example.com/instrumented-repository"
)

func ExampleLoggingRepository() {
	repo := decorator.NewMemoryRepository()
	logged := decorator.NewLoggingRepository(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_ = logged.Create(context.Background(), &decorator.User{ID: "1", Name: "Alice"})
	got, _ := logged.GetByID(context.Background(), "1")
	fmt.Printf("got=%s\n", got.Name)
	// Output: got=Alice
}
```

## Review

The stack is correct when each layer adds exactly its concern and is invisible otherwise. Confirm that every decorator stores `UserRepository` and not `*MemoryRepository`, because the concrete-type version compiles but can only ever wrap one implementation and cannot itself be wrapped. Confirm the metrics layer increments on the path that handles failures, not only inside the `err == nil` branch — the `GetByID=3` in the demo includes a miss and a not-found, and the test pins the failing `missing` call as a counted call. Confirm the cache inserts only after a nil error: the `DoesNotCacheErrors` test would catch a version that cached the `ErrNotFound`, because a second lookup would then come back from the cache and `Len()` would be one instead of zero. Confirm retry distinguishes transient from permanent by predicate, not by "any error": the demo's final lookup returns `ErrNotFound` in a single attempt, and `TestRetryRepository_DoesNotRetryPermanent` pins the attempt count at one.

The placement of the cache relative to metrics is the subtle design point worth re-reading. Because the cache short-circuits on a hit, anything inside it (here, retry and the real store) never runs on a cached read, while anything outside it (metrics, logging) still does. That is why the stack puts metrics outside the cache: you want the dashboard to count the logical read your callers made, not only the subset that paid for a database round-trip. Reorder the two and the same code reports a different, also-defensible number; there is no universally right order, only an order that matches what you intend to measure, which is exactly why composition order is a behavioral decision the test makes explicit.

## Resources

- [Decorator pattern](https://refactoring.guru/design-patterns/decorator) — the pattern's intent and structure, language-independent.
- [`log/slog`](https://pkg.go.dev/log/slog) — the structured logging package the logging decorator uses, including `slog.NewTextHandler` and `io.Discard`.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is`, sentinel errors, and the `%w` wrapping verb the retry decorator relies on.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock that lets the cache serve concurrent reads while serializing writes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-http-middleware-chain.md](02-http-middleware-chain.md)
