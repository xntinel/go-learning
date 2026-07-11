# Exercise 3: Validation And Error Mapping

A service sits on two borders and polices both. On the way in it rejects untrusted input before any work begins; on the way out it translates storage failures into the domain's own vocabulary so callers never depend on the database. This module builds a `RegistrationService` that validates a registration request in one pass — returning every bad field at once — and maps a repository's storage errors to stable domain errors, so a "unique constraint violated" becomes `ErrEmailTaken` and an unknown failure is hidden behind `ErrInternal`.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
registration.go      User, RegisterRequest, FieldError, ValidationError (typed),
                     storage + domain sentinels, RegistrationService.Register,
                     RegisterRequest.validate, mapRepoError
memrepo.go           MemRepository: enforces email uniqueness, returns ErrConflict
cmd/
  demo/
    main.go          a valid registration, a multi-field validation failure,
                     a duplicate email mapped to a domain error
registration_test.go all-fields-at-once validation, conflict mapping, internal
                     mapping, no-leak assertions
```

- Files: `registration.go`, `memrepo.go`, `cmd/demo/main.go`, `registration_test.go`.
- Implement: `(*RegistrationService).Register`, `RegisterRequest.validate` (one pass, all fields), `mapRepoError`, and the `ValidationError` typed error.
- Test: `registration_test.go` covers normalization, collecting all field errors at once, single-field validation, mapping `ErrConflict` to `ErrEmailTaken` without leaking the storage error, mapping an unknown storage error to `ErrInternal`, the nil-repo guard, and the validation message.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p user-registration/cmd/demo && cd user-registration
go mod init example.com/user-registration
```

### Why validation and mapping are border control

Input arrives untrusted. A registration might have a blank name, a malformed email, an absurd age — and a service that does the cheap thing, returning on the first bad field, forces the caller into a miserable loop: fix the email, resubmit, learn the name is empty, fix it, resubmit, learn the age is wrong. Good validation runs *every* check in one pass and accumulates the failures into a structured `ValidationError` that carries one `FieldError` per problem. The caller — a web handler rendering a form, say — gets the complete list in a single round-trip and shows every error at once. `ValidationError` is a *typed* error, not a sentinel, because the caller needs its contents: `errors.As(err, &ve)` recovers the `*ValidationError` and the handler iterates `ve.Fields`.

The outbound border is the mirror image. The repository speaks storage: its `Create` returns `ErrConflict` for a uniqueness clash, just as a real driver surfaces a unique-constraint violation. If that error escaped the service unchanged, every caller would now branch on a storage-specific value, and swapping the database would break all of them. So `mapRepoError` translates at the boundary: `ErrConflict` becomes the domain's `ErrEmailTaken`, and anything unrecognized becomes a generic `ErrInternal` with the original cause attached via `%v` for the logs but *not* exposed as something callers can match. The test `errors.Is(err, ErrConflict)` must return false on the service's output — the storage error is contained, not propagated. This is the difference between an API whose error contract is stable and one that leaks its implementation.

A few smaller decisions reinforce the boundary. The email is normalized — trimmed and lowercased — before it is stored, so equality is canonical and `Alice@Example.com` and `alice@example.com` collide as they should. Validation happens entirely before the repository is touched, so invalid input never reaches storage and the `Count()` after a rejected registration is zero. And the service exposes its domain errors (`ErrEmailTaken`, `ErrInternal`) as the stable surface while keeping the storage sentinels (`ErrConflict`, `ErrUnavailable`) as inputs to the mapping, never as outputs.

Create `registration.go`:

```go
package users

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// User is the record produced by a successful registration.
type User struct {
	ID    string
	Email string
	Name  string
	Age   int
}

// RegisterRequest is the raw, untrusted input to the use case.
type RegisterRequest struct {
	Email string
	Name  string
	Age   int
}

// FieldError is a single input problem tied to the field that caused it.
type FieldError struct {
	Field   string
	Message string
}

// ValidationError aggregates every field problem found in one pass, so the
// caller sees all of them at once instead of fixing inputs one round-trip at a
// time. It is a typed error: callers recover the fields with errors.As.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "users: validation failed: " + strings.Join(parts, "; ")
}

func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// Storage-layer sentinels a repository may return. They describe failure in
// storage terms; the service never lets them reach its callers unchanged.
var (
	ErrConflict    = errors.New("storage: unique constraint violated")
	ErrUnavailable = errors.New("storage: backend unavailable")
)

// Domain errors: the stable, storage-independent surface the service exposes.
var (
	ErrEmailTaken = errors.New("users: email already registered")
	ErrInternal   = errors.New("users: internal error")
)

// UserRepository is the persistence port. Create reports a uniqueness clash by
// returning ErrConflict; the service, not the caller, decides what that means.
type UserRepository interface {
	NextID(ctx context.Context) (string, error)
	Create(ctx context.Context, u *User) error
}

// RegistrationService validates input and maps storage failures to domain
// errors. It owns no data; it guards the boundary between untrusted input and
// the repository, and between storage errors and the caller.
type RegistrationService struct {
	repo UserRepository
}

// NewRegistrationService rejects a nil repository at construction.
func NewRegistrationService(repo UserRepository) (*RegistrationService, error) {
	if repo == nil {
		return nil, errors.New("users: a repository is required")
	}
	return &RegistrationService{repo: repo}, nil
}

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// validate runs every check in one pass and returns a *ValidationError holding
// all field problems, or nil when the input is clean.
func (req RegisterRequest) validate() error {
	ve := &ValidationError{}

	email := strings.TrimSpace(req.Email)
	switch {
	case email == "":
		ve.add("email", "must not be empty")
	case !emailRE.MatchString(email):
		ve.add("email", "is not a valid address")
	}

	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		ve.add("name", "must not be empty")
	case len(name) > 64:
		ve.add("name", "must be at most 64 characters")
	}

	if req.Age < 13 || req.Age > 130 {
		ve.add("age", "must be between 13 and 130")
	}

	if len(ve.Fields) > 0 {
		return ve
	}
	return nil
}

// mapRepoError translates a storage error into the service's domain vocabulary.
// A uniqueness clash becomes ErrEmailTaken; anything else is hidden behind
// ErrInternal so storage internals never leak to the caller while the original
// cause stays attached for logs via %v.
func mapRepoError(err error) error {
	switch {
	case errors.Is(err, ErrConflict):
		return ErrEmailTaken
	default:
		return fmt.Errorf("%w: %v", ErrInternal, err)
	}
}

// Register validates the request, then persists a normalized user. Validation
// failures surface as *ValidationError; storage failures are mapped to domain
// errors. The email is lowercased and trimmed so equality is canonical.
func (s *RegistrationService) Register(ctx context.Context, req RegisterRequest) (*User, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}

	id, err := s.repo.NextID(ctx)
	if err != nil {
		return nil, mapRepoError(err)
	}

	u := &User{
		ID:    id,
		Email: strings.ToLower(strings.TrimSpace(req.Email)),
		Name:  strings.TrimSpace(req.Name),
		Age:   req.Age,
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, mapRepoError(err)
	}
	return u, nil
}
```

`validate` runs three independent checks and only returns after all of them have had a chance to record a problem; that single-pass structure is the whole reason a caller sees every error at once. `mapRepoError` is the entire outbound boundary: two cases, one for the clash and one for everything else.

Now the repository. It enforces uniqueness on the normalized email and reports a clash with the storage-level `ErrConflict`, exactly as a database driver would.

Create `memrepo.go`:

```go
package users

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MemRepository is an in-memory UserRepository. It enforces email uniqueness
// and reports a clash with the storage-level ErrConflict, exactly as a real
// database driver would surface a unique-constraint violation.
type MemRepository struct {
	mu     sync.Mutex
	byID   map[string]*User
	emails map[string]bool
	seq    int
}

// NewMemRepository returns an empty repository.
func NewMemRepository() *MemRepository {
	return &MemRepository{
		byID:   map[string]*User{},
		emails: map[string]bool{},
	}
}

func (r *MemRepository) NextID(_ context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return fmt.Sprintf("usr-%03d", r.seq), nil
}

func (r *MemRepository) Create(_ context.Context, u *User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(u.Email))
	if r.emails[key] {
		return fmt.Errorf("%w: email=%s", ErrConflict, key)
	}
	r.emails[key] = true
	stored := *u
	r.byID[u.ID] = &stored
	return nil
}

// Count returns how many users are stored, for assertions.
func (r *MemRepository) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}
```

### The runnable demo

The demo registers a valid user, then submits input that is wrong in three ways at once and prints every field error the service collected, then attempts a duplicate email and shows the result is the domain `ErrEmailTaken` — with `errors.Is(err, ErrConflict)` returning false, proving the storage error did not leak. The duplicate uses different casing to confirm normalization makes the collision happen.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"example.com/user-registration"
)

func main() {
	ctx := context.Background()
	repo := users.NewMemRepository()
	svc, err := users.NewRegistrationService(repo)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== valid registration ===")
	u, err := svc.Register(ctx, users.RegisterRequest{Email: "Alice@Example.com", Name: "Alice", Age: 30})
	if err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	fmt.Printf("  created %s email=%s name=%s\n", u.ID, u.Email, u.Name)

	fmt.Println("=== invalid input (collects every field error) ===")
	_, err = svc.Register(ctx, users.RegisterRequest{Email: "not-an-email", Name: "", Age: 9})
	var ve *users.ValidationError
	if errors.As(err, &ve) {
		for _, f := range ve.Fields {
			fmt.Printf("  %s: %s\n", f.Field, f.Message)
		}
	}

	fmt.Println("=== duplicate email (storage error mapped to domain error) ===")
	_, err = svc.Register(ctx, users.RegisterRequest{Email: "alice@example.com", Name: "Alice Two", Age: 41})
	fmt.Printf("  error: %v\n", err)
	fmt.Printf("  email taken? %v\n", errors.Is(err, users.ErrEmailTaken))
	fmt.Printf("  storage error leaked? %v\n", errors.Is(err, users.ErrConflict))
	fmt.Printf("  users stored: %d\n", repo.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== valid registration ===
  created usr-001 email=alice@example.com name=Alice
=== invalid input (collects every field error) ===
  email: is not a valid address
  name: must not be empty
  age: must be between 13 and 130
=== duplicate email (storage error mapped to domain error) ===
  error: users: email already registered
  email taken? true
  storage error leaked? false
  users stored: 1
```

The last block is the boundary working: the caller sees `email already registered`, the storage clash is contained, and the repository still holds exactly one user.

### Tests

`TestRegister_HappyPath` checks the email is normalized and the user is stored. `TestRegister_CollectsAllFieldErrors` submits three-way-bad input and asserts the `*ValidationError` carries all three fields and that nothing was persisted. `TestRegister_RejectsEmptyEmail` pins single-field validation. The two mapping tests are the core: `TestRegister_MapsConflictToEmailTaken` proves a duplicate becomes `ErrEmailTaken` and that `ErrConflict` does *not* leak, and `TestRegister_MapsUnknownStorageErrorToInternal` proves an unrecognized storage failure becomes `ErrInternal` without exposing the raw cause. The rest pin the nil-repo guard and the validation message.

Create `registration_test.go`:

```go
package users

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newSvc(t *testing.T) (*RegistrationService, *MemRepository) {
	t.Helper()
	repo := NewMemRepository()
	svc, err := NewRegistrationService(repo)
	if err != nil {
		t.Fatalf("NewRegistrationService: %v", err)
	}
	return svc, repo
}

func TestRegister_HappyPath(t *testing.T) {
	t.Parallel()

	svc, repo := newSvc(t)
	u, err := svc.Register(context.Background(), RegisterRequest{Email: "  Bob@Example.com ", Name: " Bob ", Age: 25})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if u.Email != "bob@example.com" {
		t.Errorf("email = %q, want canonical lowercase trimmed", u.Email)
	}
	if u.Name != "Bob" {
		t.Errorf("name = %q, want trimmed", u.Name)
	}
	if repo.Count() != 1 {
		t.Errorf("stored = %d, want 1", repo.Count())
	}
}

func TestRegister_CollectsAllFieldErrors(t *testing.T) {
	t.Parallel()

	svc, repo := newSvc(t)
	_, err := svc.Register(context.Background(), RegisterRequest{Email: "bad", Name: "", Age: 200})

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Fields) != 3 {
		t.Fatalf("got %d field errors, want 3: %+v", len(ve.Fields), ve.Fields)
	}
	got := map[string]bool{}
	for _, f := range ve.Fields {
		got[f.Field] = true
	}
	for _, want := range []string{"email", "name", "age"} {
		if !got[want] {
			t.Errorf("missing field error for %q", want)
		}
	}
	if repo.Count() != 0 {
		t.Errorf("invalid input must not persist: stored %d", repo.Count())
	}
}

func TestRegister_RejectsEmptyEmail(t *testing.T) {
	t.Parallel()

	svc, _ := newSvc(t)
	_, err := svc.Register(context.Background(), RegisterRequest{Email: "   ", Name: "Bob", Age: 25})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Fields) != 1 || ve.Fields[0].Field != "email" {
		t.Errorf("fields = %+v, want one email error", ve.Fields)
	}
}

func TestRegister_MapsConflictToEmailTaken(t *testing.T) {
	t.Parallel()

	svc, _ := newSvc(t)
	first := RegisterRequest{Email: "dup@example.com", Name: "First", Age: 30}
	if _, err := svc.Register(context.Background(), first); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, err := svc.Register(context.Background(), RegisterRequest{Email: "DUP@example.com", Name: "Second", Age: 31})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Error("storage-level ErrConflict leaked through the service boundary")
	}
}

// flakyRepo always fails NextID with a non-conflict storage error.
type flakyRepo struct{}

func (flakyRepo) NextID(context.Context) (string, error) { return "", ErrUnavailable }
func (flakyRepo) Create(context.Context, *User) error    { return nil }

func TestRegister_MapsUnknownStorageErrorToInternal(t *testing.T) {
	t.Parallel()

	svc, err := NewRegistrationService(flakyRepo{})
	if err != nil {
		t.Fatalf("NewRegistrationService: %v", err)
	}
	_, err = svc.Register(context.Background(), RegisterRequest{Email: "x@y.com", Name: "X", Age: 20})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("raw storage error leaked through; only ErrInternal should surface to callers")
	}
}

func TestNewRegistrationService_RejectsNilRepo(t *testing.T) {
	t.Parallel()

	if _, err := NewRegistrationService(nil); err == nil {
		t.Error("expected error for nil repository")
	}
}

func TestValidationError_MessageListsFields(t *testing.T) {
	t.Parallel()

	svc, _ := newSvc(t)
	_, err := svc.Register(context.Background(), RegisterRequest{Email: "bad", Name: "", Age: 5})
	msg := err.Error()
	for _, want := range []string{"email", "name", "age"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
```

## Review

The boundary is correct when invalid input never reaches storage and storage errors never reach the caller unchanged. Confirm `validate` runs all three checks before returning, so the caller gets every problem in one pass — the all-fields test fails the moment validation short-circuits on the first error. Confirm `mapRepoError` returns the domain error and that `errors.Is` on the storage sentinel is false against the service's output; the no-leak assertions are what guarantee the database can be swapped without breaking callers. Confirm the email is normalized before the uniqueness check, or two differently-cased duplicates slip through.

The mistakes to avoid: validating field-by-field with early returns, which forces the caller into a resubmit loop; leaking the repository's error type, which couples every caller to the storage technology; and using a sentinel where a typed error is needed — `ValidationError` must carry its fields, so it is a struct recovered with `errors.As`, not a bare value matched with `errors.Is`. The shape to internalize: validate in one pass into a typed error on the way in, map to stable domain errors on the way out.

## Resources

- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is` for sentinels, `errors.As` for typed errors like `ValidationError`, and `%w` wrapping, the exact tools this boundary uses.
- [`errors` package](https://pkg.go.dev/errors) — the standard-library reference for `Is`, `As`, `Join`, and the `Unwrap` contract behind error mapping.
- [Martin Fowler: Service Layer](https://martinfowler.com/eaaCatalog/serviceLayer.html) — the layer whose job includes guarding the application boundary, where this validation and mapping belongs.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-transactional-unit-of-work.md](02-transactional-unit-of-work.md) | Next: [04-booking-saga.md](04-booking-saga.md)
