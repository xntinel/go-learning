# Exercise 5: Idempotent Registration

A registration endpoint sits on the public edge, so it must do four jobs at once: validate untrusted input, survive a retried request without creating a second account, announce what happened so the rest of the system can react, and translate its internal failures into a stable HTTP contract. This module builds a `Service` whose `Register` runs one-pass validation, deduplicates on an idempotency key, emits a `UserRegistered` domain event exactly once, and pairs with a `ToHTTP` mapper that turns each domain error into a status code and a safe response.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
signup.go            User, RegisterRequest, FieldError, ValidationError (typed),
                     domain + storage sentinels, the three ports, UserRegistered,
                     Service.Register (validate, dedupe, create, emit)
transport.go         ErrorResponse, ToHTTP: domain error -> status + code
memstore.go          MemRepository (counts creates), MemIdempotency,
                     RecordingPublisher
cmd/
  demo/
    main.go          a valid signup, an idempotent retry, a 400, a 409
signup_test.go       validation, idempotent retry, conflict mapping, transport
                     mapping table, the missing-key and nil-dependency guards
```

- Files: `signup.go`, `transport.go`, `memstore.go`, `cmd/demo/main.go`, `signup_test.go`.
- Implement: `(*Service).Register` (key check, idempotency lookup, one-pass validation, create, idempotency store, event emit), `mapRepoError`, and `ToHTTP`.
- Test: `signup_test.go` covers normalization plus one-event emission, multi-field validation with a 400, an idempotent retry that creates no second user and emits no second event, a duplicate email mapped to `ErrEmailTaken`/409 without leaking the storage error, an unknown storage error mapped to `ErrInternal`/500, the missing-key guard, the full transport mapping table, and the nil-dependency guard.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p signup/cmd/demo && cd signup
go mod init example.com/signup
```

### Four responsibilities at the public edge

A registration use case is where untrusted input meets durable state, so it has to police both directions and tolerate the messiness of a real network. Take the four jobs in turn.

Validation is inbound border control. `RegisterRequest.validate` runs every check in one pass and accumulates problems into a typed `*ValidationError` carrying one `FieldError` per fault, so a caller fixing a form sees every error at once instead of one per round-trip. It is a typed error, recovered with `errors.As`, precisely because the caller needs its contents — the transport layer renders the field list into the response body.

Idempotency is the answer to the network's first fact of life: clients retry. A user double-clicks, a proxy resends a request whose response was lost, a mobile app retries on a flaky connection. Without protection, each retry of "register alice@example.com" creates another account. The fix is an idempotency key — a client-supplied token identifying one logical request — and an `IdempotencyStore` that remembers the result for each key. `Register` checks the store first: if the key has been seen, it returns the stored user verbatim and stops, performing no creation and, crucially, emitting no event. Only a genuinely new key proceeds to validation and creation, after which the result is recorded under the key. This is exactly how payment APIs make "charge this card" safe to retry.

Emitting a domain event is how a service stays decoupled from everything that must happen *after* a registration — sending a welcome email, provisioning a workspace, updating analytics. Rather than calling each of those, `Register` publishes one `UserRegistered` event through an `EventPublisher` port and lets subscribers react. The event must fire exactly once per real registration, which is why it is emitted only on the create path and never on an idempotent replay: a retry that emitted a second event would send two welcome emails. The clock is injected (`now func() time.Time`) so the event's timestamp is deterministic under test. Event delivery is treated as non-critical — a publish failure is logged, not returned — because the user already exists and the result is already recorded; failing the whole registration over a transient bus outage would be the wrong trade.

Error mapping is the outbound border, and here it has two layers. First, storage errors become domain errors: `mapRepoError` turns the repository's `ErrConflict` into the domain's `ErrEmailTaken` and hides anything unrecognized behind `ErrInternal`, so callers never branch on a storage-specific value and the database stays swappable. Second, domain errors become a transport contract: `ToHTTP` in `transport.go` is the single place that knows HTTP exists, mapping `*ValidationError` to 400 with its fields, `ErrEmailTaken` to 409, `ErrMissingKey` to 400, and everything else to a bare 500 that carries no internal detail. Keeping `ToHTTP` separate from the service is the point — the service speaks pure domain errors and could be driven by a gRPC or CLI front end that supplies its own mapper.

The ordering inside `Register` encodes these decisions: key check, then idempotency lookup, then validation, then create, then record, then emit. The idempotency lookup precedes validation so a successful request's stored result is replayed even if a malformed retry arrives later under the same key — though in practice a client reuses a key only for the identical request.

Create `signup.go`:

```go
package signup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// User is the record a successful registration produces.
type User struct {
	ID    string
	Email string
	Name  string
}

// RegisterRequest is the raw, untrusted input to the use case.
type RegisterRequest struct {
	Email string
	Name  string
}

// FieldError is a single input problem tied to the field that caused it.
type FieldError struct {
	Field   string
	Message string
}

// ValidationError aggregates every field problem found in one pass. It is a
// typed error: a caller recovers the fields with errors.As, and the transport
// layer turns it into a 400 with the field list.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "signup: validation failed: " + strings.Join(parts, "; ")
}

func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// Storage-layer sentinel a repository returns for a uniqueness clash. The
// service maps it to a domain error and never lets it reach the caller.
var ErrConflict = errors.New("store: unique constraint violated")

// Domain errors: the stable surface the service exposes.
var (
	ErrEmailTaken = errors.New("signup: email already registered")
	ErrInternal   = errors.New("signup: internal error")
	ErrMissingKey = errors.New("signup: an idempotency key is required")
)

// UserRepository persists users and reports a uniqueness clash with ErrConflict.
type UserRepository interface {
	NextID(ctx context.Context) (string, error)
	Create(ctx context.Context, u *User) error
}

// IdempotencyStore remembers the result of a request keyed by an idempotency
// key, so a retry of the same logical request returns the same user instead of
// registering a second one.
type IdempotencyStore interface {
	Get(ctx context.Context, key string) (*User, bool, error)
	Put(ctx context.Context, key string, u *User) error
}

// UserRegistered is the domain event emitted exactly once per successful
// registration. A retry that replays a stored result emits nothing.
type UserRegistered struct {
	UserID string
	Email  string
	At     time.Time
}

// EventPublisher delivers domain events to whatever is downstream.
type EventPublisher interface {
	Publish(ctx context.Context, evt UserRegistered) error
}

// Service registers users with input validation, idempotency, domain-event
// emission, and storage-to-domain error mapping. It owns no data; it guards the
// boundary and sequences its ports.
type Service struct {
	repo   UserRepository
	idem   IdempotencyStore
	events EventPublisher
	now    func() time.Time
}

// NewService wires the dependencies and rejects a nil one at construction. The
// clock is injectable so the event timestamp is deterministic under test.
func NewService(repo UserRepository, idem IdempotencyStore, events EventPublisher, now func() time.Time) (*Service, error) {
	if repo == nil || idem == nil || events == nil {
		return nil, errors.New("signup: all dependencies are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, idem: idem, events: events, now: now}, nil
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

	if len(ve.Fields) > 0 {
		return ve
	}
	return nil
}

// mapRepoError translates a storage error into the domain vocabulary. A clash
// becomes ErrEmailTaken; anything else is hidden behind ErrInternal with the
// original cause kept for logs via %v but not exposed for matching.
func mapRepoError(err error) error {
	switch {
	case errors.Is(err, ErrConflict):
		return ErrEmailTaken
	default:
		return fmt.Errorf("%w: %v", ErrInternal, err)
	}
}

// Register validates the request, deduplicates on the idempotency key, persists
// a normalized user, and emits a UserRegistered event. A retry carrying a key
// already seen returns the stored user without creating a second record or
// emitting a second event.
func (s *Service) Register(ctx context.Context, key string, req RegisterRequest) (*User, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrMissingKey
	}

	if u, ok, err := s.idem.Get(ctx, key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	} else if ok {
		return u, nil
	}

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
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, mapRepoError(err)
	}

	if err := s.idem.Put(ctx, key, u); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}

	evt := UserRegistered{UserID: u.ID, Email: u.Email, At: s.now()}
	if err := s.events.Publish(ctx, evt); err != nil {
		// Event delivery is non-critical: the user is already created and the
		// result is recorded for idempotent replay, so a publish failure is
		// logged, not returned.
		fmt.Printf("warning: publishing UserRegistered for %s failed: %v\n", u.ID, err)
	}

	return u, nil
}
```

`Register` reads as the four responsibilities in sequence; everything before the create is a guard, everything after it is a consequence. Now the transport boundary, kept in its own file so the service core has no idea HTTP exists.

Create `transport.go`:

```go
package signup

import "errors"

// ErrorResponse is the transport-facing shape of a failure: an HTTP status, a
// stable machine-readable code, a safe human message, and, for validation,
// the offending fields. It deliberately carries no internal cause.
type ErrorResponse struct {
	Status  int          `json:"-"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Fields  []FieldError `json:"fields,omitempty"`
}

// ToHTTP maps a domain error to its transport representation. This is the one
// place that knows about HTTP status codes; the service speaks only domain
// errors. An unrecognized error degrades to 500 with no internal detail, so a
// storage or programming failure never leaks to a client.
func ToHTTP(err error) ErrorResponse {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return ErrorResponse{
			Status:  400,
			Code:    "invalid_request",
			Message: "validation failed",
			Fields:  ve.Fields,
		}
	case errors.Is(err, ErrEmailTaken):
		return ErrorResponse{Status: 409, Code: "email_taken", Message: "email already registered"}
	case errors.Is(err, ErrMissingKey):
		return ErrorResponse{Status: 400, Code: "missing_idempotency_key", Message: "an idempotency key is required"}
	default:
		return ErrorResponse{Status: 500, Code: "internal", Message: "internal error"}
	}
}
```

The in-memory adapters back the demo and tests. `MemRepository` enforces email uniqueness and counts its `Create` calls so a test can prove an idempotent retry created nothing new; `RecordingPublisher` captures events so the once-only emission is observable.

Create `memstore.go`:

```go
package signup

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MemRepository is an in-memory UserRepository. It enforces email uniqueness
// and reports a clash with the storage-level ErrConflict, exactly as a real
// driver would surface a unique-constraint violation.
type MemRepository struct {
	mu      sync.Mutex
	byID    map[string]*User
	emails  map[string]bool
	seq     int
	creates int
}

// NewMemRepository returns an empty repository.
func NewMemRepository() *MemRepository {
	return &MemRepository{byID: map[string]*User{}, emails: map[string]bool{}}
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
	r.creates++
	key := strings.ToLower(strings.TrimSpace(u.Email))
	if r.emails[key] {
		return fmt.Errorf("%w: email=%s", ErrConflict, key)
	}
	r.emails[key] = true
	stored := *u
	r.byID[u.ID] = &stored
	return nil
}

// Creates reports how many times Create was invoked, for assertions about
// idempotency (a replayed request must not call Create again).
func (r *MemRepository) Creates() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates
}

// MemIdempotency is an in-memory IdempotencyStore keyed by request key.
type MemIdempotency struct {
	mu   sync.Mutex
	seen map[string]*User
}

// NewMemIdempotency returns an empty store.
func NewMemIdempotency() *MemIdempotency {
	return &MemIdempotency{seen: map[string]*User{}}
}

func (s *MemIdempotency) Get(_ context.Context, key string) (*User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.seen[key]
	return u, ok, nil
}

func (s *MemIdempotency) Put(_ context.Context, key string, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[key] = u
	return nil
}

// RecordingPublisher captures every published event for assertions.
type RecordingPublisher struct {
	mu     sync.Mutex
	Events []UserRegistered
}

func (p *RecordingPublisher) Publish(_ context.Context, evt UserRegistered) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Events = append(p.Events, evt)
	return nil
}

// Count returns how many events were published.
func (p *RecordingPublisher) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Events)
}
```

### The runnable demo

The demo registers a valid user and prints the create-and-event counters, then retries with the *same* idempotency key and shows both counters unchanged and the same id returned — the retry was absorbed. It then submits invalid input and a duplicate email under fresh keys and runs each error through `ToHTTP` to show the 400 and 409 the client would receive, with the storage error contained.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"example.com/signup"
)

func main() {
	ctx := context.Background()
	repo := signup.NewMemRepository()
	idem := signup.NewMemIdempotency()
	pub := &signup.RecordingPublisher{}
	clock := func() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) }
	svc, err := signup.NewService(repo, idem, pub, clock)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== valid registration (emits one event) ===")
	u, err := svc.Register(ctx, "req-1", signup.RegisterRequest{Email: "Alice@Example.com", Name: "Alice"})
	if err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	fmt.Printf("  created %s email=%s\n", u.ID, u.Email)
	fmt.Printf("  creates=%d events=%d\n", repo.Creates(), pub.Count())

	fmt.Println("=== idempotent retry (same key, no new user, no new event) ===")
	again, err := svc.Register(ctx, "req-1", signup.RegisterRequest{Email: "Alice@Example.com", Name: "Alice"})
	if err != nil {
		log.Fatalf("unexpected: %v", err)
	}
	fmt.Printf("  returned %s (same id? %v)\n", again.ID, again.ID == u.ID)
	fmt.Printf("  creates=%d events=%d\n", repo.Creates(), pub.Count())

	fmt.Println("=== invalid input (mapped to a 400) ===")
	_, err = svc.Register(ctx, "req-2", signup.RegisterRequest{Email: "not-an-email", Name: ""})
	resp := signup.ToHTTP(err)
	fmt.Printf("  status=%d code=%s\n", resp.Status, resp.Code)
	for _, f := range resp.Fields {
		fmt.Printf("  field %s: %s\n", f.Field, f.Message)
	}

	fmt.Println("=== duplicate email, new key (mapped to a 409) ===")
	_, err = svc.Register(ctx, "req-3", signup.RegisterRequest{Email: "alice@example.com", Name: "Alice Two"})
	resp = signup.ToHTTP(err)
	fmt.Printf("  email taken? %v\n", errors.Is(err, signup.ErrEmailTaken))
	fmt.Printf("  storage error leaked? %v\n", errors.Is(err, signup.ErrConflict))
	fmt.Printf("  status=%d code=%s\n", resp.Status, resp.Code)
	fmt.Printf("  creates=%d events=%d\n", repo.Creates(), pub.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== valid registration (emits one event) ===
  created usr-001 email=alice@example.com
  creates=1 events=1
=== idempotent retry (same key, no new user, no new event) ===
  returned usr-001 (same id? true)
  creates=1 events=1
=== invalid input (mapped to a 400) ===
  status=400 code=invalid_request
  field email: is not a valid address
  field name: must not be empty
=== duplicate email, new key (mapped to a 409) ===
  email taken? true
  storage error leaked? false
  status=409 code=email_taken
  creates=2 events=1
```

Read the counters across the first two blocks: after the valid signup `creates=1 events=1`, and after the retry under the same key they are still `1` and `1` while the same `usr-001` comes back — the retry created no second account and fired no second event. The last block shows the duplicate under a *new* key did reach `Create` (so `creates=2`) but the clash was mapped to `ErrEmailTaken`, the storage error did not leak, and no event fired for the failed attempt.

### Tests

`TestRegister_HappyPath` checks normalization, the single event, and that the event carries the injected clock's timestamp. `TestRegister_Validation` submits two-field-bad input and asserts a `*ValidationError`, no create, no event, and a 400 from `ToHTTP`. `TestRegister_IdempotentRetry` is the core idempotency proof: the same key twice returns the same id with `Creates()` and the event count both still 1. `TestRegister_DuplicateEmailMapsToTaken` proves a duplicate under a new key becomes `ErrEmailTaken`/409 without leaking `ErrConflict`. The rest pin the missing-key guard, the unknown-storage-error mapping to `ErrInternal`/500, the full `ToHTTP` table, and the nil-dependency guard.

Create `signup_test.go`:

```go
package signup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) }
}

func newSvc(t *testing.T) (*Service, *MemRepository, *RecordingPublisher) {
	t.Helper()
	repo := NewMemRepository()
	pub := &RecordingPublisher{}
	svc, err := NewService(repo, NewMemIdempotency(), pub, fixedClock())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, repo, pub
}

func TestRegister_HappyPath(t *testing.T) {
	t.Parallel()

	svc, repo, pub := newSvc(t)
	u, err := svc.Register(context.Background(), "k1", RegisterRequest{Email: "  Alice@Example.com ", Name: " Alice "})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q, want normalized", u.Email)
	}
	if u.Name != "Alice" {
		t.Errorf("name = %q, want trimmed", u.Name)
	}
	if repo.Creates() != 1 {
		t.Errorf("creates = %d, want 1", repo.Creates())
	}
	if pub.Count() != 1 {
		t.Fatalf("events = %d, want 1", pub.Count())
	}
	evt := pub.Events[0]
	if evt.UserID != u.ID || evt.Email != u.Email {
		t.Errorf("event = %+v, want it to describe %s/%s", evt, u.ID, u.Email)
	}
	if !evt.At.Equal(time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("event time = %v, want the injected clock value", evt.At)
	}
}

func TestRegister_Validation(t *testing.T) {
	t.Parallel()

	svc, repo, pub := newSvc(t)
	_, err := svc.Register(context.Background(), "k1", RegisterRequest{Email: "bad", Name: ""})

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Fields) != 2 {
		t.Fatalf("fields = %d, want 2 (%+v)", len(ve.Fields), ve.Fields)
	}
	if repo.Creates() != 0 {
		t.Errorf("invalid input must not create: creates = %d", repo.Creates())
	}
	if pub.Count() != 0 {
		t.Errorf("invalid input must not emit an event: events = %d", pub.Count())
	}
	if resp := ToHTTP(err); resp.Status != 400 || resp.Code != "invalid_request" {
		t.Errorf("ToHTTP = %+v, want 400/invalid_request", resp)
	}
}

func TestRegister_IdempotentRetry(t *testing.T) {
	t.Parallel()

	svc, repo, pub := newSvc(t)
	req := RegisterRequest{Email: "bob@example.com", Name: "Bob"}

	first, err := svc.Register(context.Background(), "same-key", req)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	second, err := svc.Register(context.Background(), "same-key", req)
	if err != nil {
		t.Fatalf("retry Register: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("retry id = %q, want the original %q", second.ID, first.ID)
	}
	if repo.Creates() != 1 {
		t.Errorf("creates = %d, want 1 (retry must not create a second user)", repo.Creates())
	}
	if pub.Count() != 1 {
		t.Errorf("events = %d, want 1 (retry must not emit a second event)", pub.Count())
	}
}

func TestRegister_DuplicateEmailMapsToTaken(t *testing.T) {
	t.Parallel()

	svc, _, _ := newSvc(t)
	if _, err := svc.Register(context.Background(), "k1", RegisterRequest{Email: "dup@example.com", Name: "First"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, err := svc.Register(context.Background(), "k2", RegisterRequest{Email: "DUP@example.com", Name: "Second"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Error("storage-level ErrConflict leaked through the service boundary")
	}
	if resp := ToHTTP(err); resp.Status != 409 || resp.Code != "email_taken" {
		t.Errorf("ToHTTP = %+v, want 409/email_taken", resp)
	}
}

func TestRegister_RequiresKey(t *testing.T) {
	t.Parallel()

	svc, repo, _ := newSvc(t)
	_, err := svc.Register(context.Background(), "   ", RegisterRequest{Email: "a@b.com", Name: "A"})
	if !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
	if repo.Creates() != 0 {
		t.Errorf("missing key must not create: creates = %d", repo.Creates())
	}
	if resp := ToHTTP(err); resp.Status != 400 {
		t.Errorf("ToHTTP status = %d, want 400", resp.Status)
	}
}

// flakyRepo always fails NextID with a non-conflict storage error.
type flakyRepo struct{}

func (flakyRepo) NextID(context.Context) (string, error) {
	return "", errors.New("store: backend unavailable")
}
func (flakyRepo) Create(context.Context, *User) error { return nil }

func TestRegister_MapsUnknownStorageErrorToInternal(t *testing.T) {
	t.Parallel()

	svc, err := NewService(flakyRepo{}, NewMemIdempotency(), &RecordingPublisher{}, fixedClock())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Register(context.Background(), "k1", RegisterRequest{Email: "x@y.com", Name: "X"})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
	if resp := ToHTTP(err); resp.Status != 500 || resp.Code != "internal" {
		t.Errorf("ToHTTP = %+v, want 500/internal", resp)
	}
}

func TestToHTTP_Mapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", &ValidationError{Fields: []FieldError{{Field: "email", Message: "bad"}}}, 400, "invalid_request"},
		{"taken", ErrEmailTaken, 409, "email_taken"},
		{"missing key", ErrMissingKey, 400, "missing_idempotency_key"},
		{"internal", ErrInternal, 500, "internal"},
		{"unknown", errors.New("boom"), 500, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := ToHTTP(c.err)
			if resp.Status != c.status || resp.Code != c.code {
				t.Errorf("ToHTTP(%v) = %d/%s, want %d/%s", c.err, resp.Status, resp.Code, c.status, c.code)
			}
		})
	}
}

func TestNewService_RejectsNilDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewService(nil, NewMemIdempotency(), &RecordingPublisher{}, nil); err == nil {
		t.Error("expected error for nil repository")
	}
}
```

## Review

The service is correct when each of the four jobs holds independently. Confirm validation runs all checks in one pass into a typed `*ValidationError` — the two-field test fails the moment validation short-circuits on the first problem. Confirm the idempotency check sits before creation and that a replay returns the stored user while `Creates()` and the event count both stay at 1; an off-by-one here is the difference between a safe retry and a duplicate charge. Confirm the event fires exactly once and never on a replay, and that a publish failure is logged rather than returned, since the user already exists. Confirm `mapRepoError` contains the storage error — `errors.Is(err, ErrConflict)` must be false on the service's output — and that `ToHTTP` is the only code mentioning status numbers.

The mistakes to avoid: validating field-by-field with early returns, which forces a resubmit loop; checking idempotency after creating, which defeats the entire purpose; emitting the event on every call including replays, which sends duplicate downstream effects; leaking the repository's error type, which couples callers to the storage technology; and folding HTTP status codes into the service, which welds it to one transport. The shape to internalize: validate in one pass, dedupe on a key before doing work, emit one event on success only, and map outward in two layers — storage error to domain error, domain error to transport — with the transport mapper living entirely outside the service.

## Resources

- [Designing robust and predictable APIs with idempotency (Stripe)](https://docs.stripe.com/api/idempotent_requests) — how a client-supplied idempotency key makes a create-or-charge request safe to retry, the exact contract this service implements.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is` for sentinels, `errors.As` for the typed `ValidationError`, and `%w` wrapping, the tools both error-mapping layers use.
- [Web API design best practices (Microsoft Azure Architecture Center)](https://learn.microsoft.com/en-us/azure/architecture/best-practices/api-design) — the status-code conventions (400 for invalid input, 409 for a conflict) that `ToHTTP` encodes at the transport boundary.
- [Martin Fowler: Service Layer](https://martinfowler.com/eaaCatalog/serviceLayer.html) — the layer that defines an application's operations and guards their boundary, where validation, idempotency, and error mapping belong.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-booking-saga.md](04-booking-saga.md)
