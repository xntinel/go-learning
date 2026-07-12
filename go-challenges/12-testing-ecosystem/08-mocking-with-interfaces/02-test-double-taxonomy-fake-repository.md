# Exercise 2: Stub vs Spy vs Fake vs Mock — Isolating a UserRepository

The five test-double kinds are not synonyms. This module builds one signup service
over one `UserRepository` port and then writes *four* different doubles for that
same port — a dummy, a stub, a spy, and a fake — so the difference between them,
and the difference between state verification and interaction verification, is
concrete rather than a definition you memorize.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
signup/                      independent module: example.com/signup
  go.mod                     go 1.26
  signup.go                  UserRepository port; User; Service; SignUp; ErrNotFound/ErrDuplicate
  cmd/
    demo/
      main.go                runnable demo over the in-memory fake
  signup_test.go             dummy, stub, spy, and fake doubles; state vs interaction tests
```

- Files: `signup.go`, `cmd/demo/main.go`, `signup_test.go`.
- Implement: a `Service.SignUp(ctx, email, password)` that normalizes the email, rejects duplicates via `GetByEmail`, and persists via `Save`; sentinel errors `ErrNotFound`, `ErrDuplicate`.
- Test: a fake (map-backed) driving pure state verification; a spy asserting the normalized email reached `Save` exactly once; a stub returning a fixed `GetByEmail`; a dummy filling an unused slot.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/02-test-double-taxonomy-fake-repository/cmd/demo
cd go-solutions/12-testing-ecosystem/08-mocking-with-interfaces/02-test-double-taxonomy-fake-repository
```

### The service and its port

The signup service enforces one real invariant: an email address is unique. To do
that it needs two capabilities from storage — look a user up by email, and save a
new one — so the `UserRepository` port has exactly two methods. Before it checks or
saves, it *normalizes* the email (trims surrounding whitespace and lowercases it),
because `Alice@Example.com ` and `alice@example.com` are the same account and the
uniqueness check must see them as equal. That normalization is the behavior the
tests must pin: not just "did it save," but "did it save the *normalized* value."

The lookup uses a sentinel-error protocol: `GetByEmail` returns `ErrNotFound` when
the address is free, and `SignUp` treats "not found" as "may proceed." Any other
error from the repository is a real failure and is returned. If the lookup *does*
find a user, `SignUp` returns `ErrDuplicate`. This is the shape of a thousand real
signup handlers.

Create `signup.go`:

```go
package signup

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrNotFound is returned by a repository when no user has the email.
	ErrNotFound = errors.New("user not found")
	// ErrDuplicate is returned by SignUp when the email is already taken.
	ErrDuplicate = errors.New("email already registered")
)

// User is the stored record. Password stands in for a hash in a real system.
type User struct {
	Email    string
	Password string
}

// UserRepository is the two-method port the service depends on. A fat repository
// would force a fat double; this is exactly what SignUp needs and no more.
type UserRepository interface {
	GetByEmail(ctx context.Context, email string) (User, error)
	Save(ctx context.Context, u User) error
}

// Service creates accounts, enforcing email uniqueness.
type Service struct {
	repo UserRepository
}

// New injects the repository through the constructor.
func New(repo UserRepository) *Service {
	return &Service{repo: repo}
}

// normalize makes email comparison case- and whitespace-insensitive.
func normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// SignUp normalizes the email, rejects a duplicate, and persists the user.
func (s *Service) SignUp(ctx context.Context, email, password string) (User, error) {
	email = normalize(email)

	_, err := s.repo.GetByEmail(ctx, email)
	switch {
	case err == nil:
		return User{}, ErrDuplicate
	case errors.Is(err, ErrNotFound):
		// free to proceed
	default:
		return User{}, err
	}

	u := User{Email: email, Password: password}
	if err := s.repo.Save(ctx, u); err != nil {
		return User{}, err
	}
	return u, nil
}
```

### The runnable demo

The demo wires the in-memory *fake* — a real working repository — so you can watch
a first signup succeed and a duplicate (differing only in case and spacing) be
rejected. Because the demo lives in `package main`, it can only touch the exported
API, so the fake here is a small exported type living in the demo itself.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"example.com/signup"
)

// memRepo is a working in-memory UserRepository: a fake, not a stub.
type memRepo struct {
	mu    sync.Mutex
	users map[string]signup.User
}

func newMemRepo() *memRepo { return &memRepo{users: make(map[string]signup.User)} }

func (r *memRepo) GetByEmail(_ context.Context, email string) (signup.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[email]
	if !ok {
		return signup.User{}, signup.ErrNotFound
	}
	return u, nil
}

func (r *memRepo) Save(_ context.Context, u signup.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[u.Email] = u
	return nil
}

func main() {
	svc := signup.New(newMemRepo())
	ctx := context.Background()

	u, err := svc.SignUp(ctx, "  Alice@Example.com ", "s3cret")
	if err != nil {
		fmt.Println("first signup failed:", err)
		return
	}
	fmt.Printf("created: %s\n", u.Email)

	_, err = svc.SignUp(ctx, "alice@example.com", "other")
	if errors.Is(err, signup.ErrDuplicate) {
		fmt.Println("second signup rejected: duplicate")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created: alice@example.com
second signup rejected: duplicate
```

### Four doubles, two verification styles

The test file implements all four doubles for the one port and shows what each is
good for.

- **dummyRepo** exists to fill a slot. `newDummyService` constructs a `Service`
  with it, but no method is ever called — it demonstrates that a dummy is about the
  *signature*, not behavior. (Its methods panic to make "was actually used" a loud
  failure rather than a silent one.)
- **stubRepo** is a stub: `GetByEmail` returns whatever canned answer you seed, and
  `Save` returns a canned error (or nil). It enables a code path — here, the
  duplicate branch — without any recording.
- **spyRepo** is a stub that *also records*: it captures every `User` handed to
  `Save`. That lets a test do interaction verification — assert the *normalized*
  email reached `Save` exactly once.
- **fakeRepo** is a working map-backed implementation with genuine uniqueness. It
  enables pure *state* verification: sign up, then look up, and the user is really
  there; sign up the same address twice and the second really returns
  `ErrDuplicate`. No call assertions at all.

The final test makes the lesson's punchline explicit: the *same* scenario
("signing up a duplicate fails") is written twice — once as robust state
verification against the fake, and once as an over-specified interaction test that
pins `GetByEmail` was called and `Save` was not. The comment marks why the second
style is brittle: it couples the test to the order and identity of internal calls,
so a behavior-preserving refactor would break it. Prefer the fake.

Create `signup_test.go`:

```go
package signup

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// --- dummy: fills a parameter slot, never used ---

type dummyRepo struct{}

func (dummyRepo) GetByEmail(context.Context, string) (User, error) {
	panic("dummy GetByEmail must not be called")
}
func (dummyRepo) Save(context.Context, User) error {
	panic("dummy Save must not be called")
}

// --- stub: canned answers, no recording ---

type stubRepo struct {
	getUser User
	getErr  error
	saveErr error
}

func (s stubRepo) GetByEmail(context.Context, string) (User, error) {
	return s.getUser, s.getErr
}
func (s stubRepo) Save(context.Context, User) error { return s.saveErr }

// --- spy: a stub that records Save arguments ---

type spyRepo struct {
	mu    sync.Mutex
	saved []User
}

func (s *spyRepo) GetByEmail(context.Context, string) (User, error) {
	return User{}, ErrNotFound // always "free to proceed"
}
func (s *spyRepo) Save(_ context.Context, u User) error {
	s.mu.Lock()
	s.saved = append(s.saved, u)
	s.mu.Unlock()
	return nil
}
func (s *spyRepo) Saved() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, len(s.saved))
	copy(out, s.saved)
	return out
}

// --- fake: a working in-memory repository with real uniqueness ---

type fakeRepo struct {
	mu    sync.Mutex
	users map[string]User
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: make(map[string]User)} }

func (f *fakeRepo) GetByEmail(_ context.Context, email string) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[email]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}
func (f *fakeRepo) Save(_ context.Context, u User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.Email] = u
	return nil
}

// The dummy proves the type-checking: a Service can be constructed with a repo
// whose methods are never reached on this path (SignUp is not called here).
func TestDummyFillsTheSlot(t *testing.T) {
	t.Parallel()
	svc := New(dummyRepo{})
	if svc == nil {
		t.Fatal("New returned nil")
	}
}

// State verification against the fake: no assertions about calls at all.
func TestFakeStateVerification(t *testing.T) {
	t.Parallel()
	svc := New(newFakeRepo())
	ctx := context.Background()

	if _, err := svc.SignUp(ctx, "Bob@Example.com", "pw"); err != nil {
		t.Fatalf("first signup: %v", err)
	}
	// The observable outcome: the normalized user is retrievable.
	got, err := svc.repo.GetByEmail(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("lookup after signup: %v", err)
	}
	if got.Email != "bob@example.com" {
		t.Fatalf("stored email = %q, want normalized", got.Email)
	}
}

// The stub drives the duplicate branch by returning a found user.
func TestStubDrivesDuplicatePath(t *testing.T) {
	t.Parallel()
	svc := New(stubRepo{getUser: User{Email: "taken@example.com"}, getErr: nil})
	_, err := svc.SignUp(context.Background(), "taken@example.com", "pw")
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

// The spy enables interaction verification: the NORMALIZED email reached Save
// exactly once.
func TestSpyInteractionVerification(t *testing.T) {
	t.Parallel()
	spy := &spyRepo{}
	svc := New(spy)

	if _, err := svc.SignUp(context.Background(), "  Carol@Example.COM ", "pw"); err != nil {
		t.Fatalf("signup: %v", err)
	}

	saved := spy.Saved()
	if len(saved) != 1 {
		t.Fatalf("Save called %d times, want 1", len(saved))
	}
	if saved[0].Email != "carol@example.com" {
		t.Fatalf("Save received %q, want normalized carol@example.com", saved[0].Email)
	}
}

// The punchline: the same "duplicate fails" scenario, robustly (fake) and then as
// an over-specified interaction test (brittle). Both pass today; only the fake
// survives a refactor of the internal call pattern.
func TestDuplicateRobustVersusBrittle(t *testing.T) {
	t.Parallel()

	// Robust: state verification against the fake.
	fake := newFakeRepo()
	svc := New(fake)
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "dana@example.com", "pw"); err != nil {
		t.Fatalf("seed signup: %v", err)
	}
	if _, err := svc.SignUp(ctx, "dana@example.com", "pw2"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("robust: err = %v, want ErrDuplicate", err)
	}

	// Over-specified: asserts GetByEmail was consulted and Save was NOT reached.
	// It couples the test to the internal call pattern; a refactor that, say,
	// pushed the uniqueness check into Save would break it though behavior is
	// unchanged. Shown as the anti-pattern, not the recommendation.
	spy := &countingRepo{getUser: User{Email: "dana@example.com"}}
	svc2 := New(spy)
	if _, err := svc2.SignUp(ctx, "dana@example.com", "pw"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("brittle: err = %v, want ErrDuplicate", err)
	}
	if spy.getCalls != 1 {
		t.Fatalf("brittle: GetByEmail calls = %d, want 1", spy.getCalls)
	}
	if spy.saveCalls != 0 {
		t.Fatalf("brittle: Save calls = %d, want 0 on duplicate", spy.saveCalls)
	}
}

// countingRepo is the over-specified mock used only to illustrate brittleness.
type countingRepo struct {
	getUser   User
	getCalls  int
	saveCalls int
}

func (c *countingRepo) GetByEmail(context.Context, string) (User, error) {
	c.getCalls++
	return c.getUser, nil // found -> duplicate
}
func (c *countingRepo) Save(context.Context, User) error {
	c.saveCalls++
	return nil
}
```

## Review

The service is correct when signup is "normalize, reject a found duplicate, persist
the normalized user," and the four doubles exist to prove different facets of that
without dragging in a real database. The load-bearing distinction is state versus
interaction verification: `TestFakeStateVerification` and the robust half of the
last test assert only on outcomes (what is retrievable, what error comes back),
which is why they would survive an internal refactor; the spy test reaches for
interaction verification precisely because normalization has no other observable
witness than the argument that reached `Save`. The brittle half is deliberately
shown as the anti-pattern — it passes today but pins the call pattern, and the
comment says so. The mistake to avoid is defaulting to that style: mock the two-
method port only when you must inspect a call, and use the fake for everything
else. Run `go test -race` to confirm the mutex-guarded doubles are sound.

## Resources

- [Martin Fowler: Mocks Aren't Stubs](https://martinfowler.com/articles/mocksArentStubs.html) — the canonical taxonomy of dummy/stub/spy/fake/mock and state vs behavior verification.
- [testing](https://pkg.go.dev/testing) — the standard testing package.
- [errors.Is](https://pkg.go.dev/errors#Is) — the sentinel-error matching the service's `GetByEmail` protocol relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-function-field-stub-payment-gateway.md](03-function-field-stub-payment-gateway.md)
