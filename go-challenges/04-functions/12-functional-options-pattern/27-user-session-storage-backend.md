# Exercise 27: Session Manager With Pluggable Storage Backend and TTL Options

**Nivel: Intermedio** — validacion rapida (un test corto).

A session manager that can be pointed at memory, Redis, or a database
behind the same interface needs to make sure the TTL a caller asks for is
one the chosen backend can actually honor: a Redis deployment might cap key
lifetime at an hour, a database might reap sessions after a day regardless
of activity, and memory can hold a session forever. This module builds that
manager with functional options for backend and TTL.

## What you'll build

```text
session/                         independent module: example.com/user-session-storage-backend
  go.mod                         go 1.24
  session.go                     Backend, MemoryBackend, RedisBackend, DatabaseBackend,
                                  Manager, Option, New, WithBackend, WithTTL, WithClock,
                                  Create, Get, Delete, BackendName
  cmd/
    demo/
      main.go                    creates a session on redis, expires it, then a rejected ttl
  session_test.go                 option-validation table, expiry, unlimited ttl, delete tests
```

- Files: `session.go`, `cmd/demo/main.go`, `session_test.go`.
- Implement: a `Manager` built by `New(opts ...Option) (*Manager, error)` whose `WithBackend` injects a pluggable `Backend`, whose `WithTTL` sets the session lifetime, and whose `New` validates the TTL is non-negative and compatible with the chosen backend's `MaxTTL`.
- Test: every option-validation case including the exact `MaxTTL` boundary, a session expiring against an injected clock, a zero TTL never expiring, and deletion.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/session/cmd/demo
cd ~/go-exercises/session
go mod init example.com/user-session-storage-backend
go mod edit -go=1.24
```

### Why the backend decides what TTL is legal

`WithBackend` and `WithTTL` are independent options, set in whatever order a
caller likes. `Backend` is a two-method interface — `Name` and `MaxTTL` —
deliberately small enough that a memory, Redis, or database backend is
trivial to implement, but `MaxTTL` is exactly the information `New` needs to
validate the combination: a backend that returns `0` can hold a session
indefinitely, so a zero TTL (never expires) is legal; a backend that
returns a positive duration cannot, so a zero TTL against it is rejected
outright, and any TTL longer than that duration is also rejected. Neither
option's closure can see the other's value while it runs — `WithTTL` never
sees which backend was chosen, and `WithBackend` never sees the requested
TTL — so, as with every other cross-field invariant in this chapter, the
check waits for `New` to have applied both.

### Create writes, Get reads and reaps

`Create` computes an absolute expiry time from the configured TTL at
creation time and stores it alongside the session data; a zero TTL stores a
zero `time.Time`, which `Get` treats as "never expires". `Get` is where
expiry is actually enforced: it compares the stored expiry against the
current time from the injected clock, and if the session has expired it
deletes the entry before reporting not-found — an expired session does not
linger in memory waiting for a separate sweep.

Create `session.go`:

```go
package session

import (
	"fmt"
	"sync"
	"time"
)

// Backend describes a pluggable storage backend's identity and its TTL
// capability: MaxTTL is the longest session lifetime the backend can honor,
// or 0 if the backend can hold a session indefinitely.
type Backend interface {
	Name() string
	MaxTTL() time.Duration
}

// MemoryBackend keeps sessions only in process memory. It can hold a
// session indefinitely.
type MemoryBackend struct{}

func (MemoryBackend) Name() string          { return "memory" }
func (MemoryBackend) MaxTTL() time.Duration { return 0 }

// RedisBackend simulates a Redis-backed store, which this deployment caps
// at a one-hour key expiry.
type RedisBackend struct{}

func (RedisBackend) Name() string          { return "redis" }
func (RedisBackend) MaxTTL() time.Duration { return time.Hour }

// DatabaseBackend simulates a relational-database-backed store, which this
// deployment caps at a 24-hour session lifetime before a cleanup job reaps
// it regardless of activity.
type DatabaseBackend struct{}

func (DatabaseBackend) Name() string          { return "database" }
func (DatabaseBackend) MaxTTL() time.Duration { return 24 * time.Hour }

type sessionEntry struct {
	data      []byte
	expiresAt time.Time // zero value means the session never expires
}

// Manager creates, reads, and deletes sessions against a pluggable backend,
// enforcing a single TTL policy compatible with that backend's capability.
type Manager struct {
	backend Backend
	ttl     time.Duration
	now     func() time.Time

	mu       sync.Mutex
	sessions map[string]sessionEntry
}

// Option configures a Manager and may reject invalid input.
type Option func(*Manager) error

// New builds a Manager, seeding a MemoryBackend and an unlimited TTL by
// default, then applying opts. It is the single validation boundary: the
// TTL must not be negative, and it must be compatible with the backend's
// MaxTTL — a backend with a finite MaxTTL requires a finite, non-zero TTL
// that does not exceed it.
func New(opts ...Option) (*Manager, error) {
	m := &Manager{
		backend:  MemoryBackend{},
		ttl:      0,
		now:      time.Now,
		sessions: make(map[string]sessionEntry),
	}
	for _, opt := range opts {
		if err := opt(m); err != nil {
			return nil, err
		}
	}

	if m.ttl < 0 {
		return nil, fmt.Errorf("ttl must not be negative, got %s", m.ttl)
	}
	if max := m.backend.MaxTTL(); max > 0 {
		if m.ttl == 0 {
			return nil, fmt.Errorf("backend %q requires a finite ttl (no unlimited sessions), max is %s", m.backend.Name(), max)
		}
		if m.ttl > max {
			return nil, fmt.Errorf("ttl %s exceeds backend %q max ttl %s", m.ttl, m.backend.Name(), max)
		}
	}
	return m, nil
}

// WithBackend sets the storage backend.
func WithBackend(b Backend) Option {
	return func(m *Manager) error {
		if b == nil {
			return fmt.Errorf("backend is nil")
		}
		m.backend = b
		return nil
	}
}

// WithTTL sets the session lifetime. A zero TTL means sessions never
// expire, which is only compatible with a backend whose MaxTTL is 0.
func WithTTL(d time.Duration) Option {
	return func(m *Manager) error {
		m.ttl = d
		return nil
	}
}

// WithClock injects the clock used to compute and check expiry.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		m.now = now
		return nil
	}
}

// Create stores a new session for id, expiring it after the configured TTL
// (or never, if the TTL is zero).
func (m *Manager) Create(id string, data []byte) error {
	if id == "" {
		return fmt.Errorf("session id must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var expiresAt time.Time
	if m.ttl > 0 {
		expiresAt = m.now().Add(m.ttl)
	}
	m.sessions[id] = sessionEntry{data: data, expiresAt: expiresAt}
	return nil
}

// Get returns the session data for id if it exists and has not expired. An
// expired session is removed and reported as not found.
func (m *Manager) Get(id string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && !m.now().Before(entry.expiresAt) {
		delete(m.sessions, id)
		return nil, false
	}
	return entry.data, true
}

// Delete removes id's session, if any.
func (m *Manager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// BackendName reports the configured backend's name.
func (m *Manager) BackendName() string { return m.backend.Name() }
```

### The runnable demo

The demo creates a session on the Redis backend with a 30-minute TTL,
confirms it is readable, advances the injected clock past that TTL and
shows it is gone, then attempts to build a manager with a TTL longer than
Redis's one-hour maximum and shows construction is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/user-session-storage-backend"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	mgr, err := session.New(
		session.WithBackend(session.RedisBackend{}),
		session.WithTTL(30*time.Minute),
		session.WithClock(clock),
	)
	if err != nil {
		panic(err)
	}

	if err := mgr.Create("sess-1", []byte("user=alice")); err != nil {
		panic(err)
	}
	data, ok := mgr.Get("sess-1")
	fmt.Printf("backend: %s, found: %t, data: %s\n", mgr.BackendName(), ok, data)

	current = current.Add(45 * time.Minute) // past the 30-minute TTL
	_, ok = mgr.Get("sess-1")
	fmt.Printf("found after 45m: %t\n", ok)

	_, err = session.New(
		session.WithBackend(session.RedisBackend{}),
		session.WithTTL(2*time.Hour),
	)
	fmt.Printf("ttl exceeding redis max rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
backend: redis, found: true, data: user=alice
found after 45m: false
ttl exceeding redis max rejected: true
```

### Tests

`TestNewValidation` tables construction across all three backends,
including the exact boundary where the TTL equals `MaxTTL` (allowed) and
where it exceeds it (rejected), plus the memory backend's unlimited-TTL
case. `TestCreateRejectsEmptyID` guards a basic input mistake.
`TestSessionExpiresAfterTTL` and `TestUnlimitedTTLNeverExpires` prove expiry
behavior against an injected clock rather than a real sleep.
`TestDelete` covers removal.

Create `session_test.go`:

```go
package session

import (
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults: memory, unlimited ttl"},
		{name: "negative ttl", opts: []Option{WithTTL(-time.Second)}, wantErr: true},
		{name: "nil backend", opts: []Option{WithBackend(nil)}, wantErr: true},
		{name: "nil clock", opts: []Option{WithClock(nil)}, wantErr: true},
		{
			name:    "redis requires a finite ttl",
			opts:    []Option{WithBackend(RedisBackend{})},
			wantErr: true,
		},
		{
			name: "redis ttl within max",
			opts: []Option{WithBackend(RedisBackend{}), WithTTL(30 * time.Minute)},
		},
		{
			name:    "redis ttl exceeds max",
			opts:    []Option{WithBackend(RedisBackend{}), WithTTL(2 * time.Hour)},
			wantErr: true,
		},
		{
			name: "redis ttl equal to max is allowed",
			opts: []Option{WithBackend(RedisBackend{}), WithTTL(time.Hour)},
		},
		{
			name: "memory backend allows unlimited ttl",
			opts: []Option{WithBackend(MemoryBackend{}), WithTTL(0)},
		},
		{
			name: "database ttl within max",
			opts: []Option{WithBackend(DatabaseBackend{}), WithTTL(12 * time.Hour)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCreateRejectsEmptyID(t *testing.T) {
	t.Parallel()

	mgr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Create("", []byte("x")); err == nil {
		t.Fatal("expected error for empty session id")
	}
}

func TestSessionExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	current := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr, err := New(
		WithBackend(RedisBackend{}),
		WithTTL(30*time.Minute),
		WithClock(func() time.Time { return current }),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Create("sess-1", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	data, ok := mgr.Get("sess-1")
	if !ok || string(data) != "payload" {
		t.Fatalf("Get() = (%q, %t), want (payload, true)", data, ok)
	}

	current = current.Add(45 * time.Minute)
	if _, ok := mgr.Get("sess-1"); ok {
		t.Fatal("session should have expired after 45 minutes with a 30-minute ttl")
	}
}

func TestUnlimitedTTLNeverExpires(t *testing.T) {
	t.Parallel()

	current := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr, err := New(WithClock(func() time.Time { return current }))
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Create("sess-1", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	current = current.Add(365 * 24 * time.Hour)
	if _, ok := mgr.Get("sess-1"); !ok {
		t.Fatal("a zero-ttl session should never expire")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	mgr, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Create("sess-1", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	mgr.Delete("sess-1")
	if _, ok := mgr.Get("sess-1"); ok {
		t.Fatal("session should be gone after Delete")
	}
}
```

## Review

The manager is correct when a TTL can never be configured that the chosen
backend cannot actually deliver on, and when expiry is enforced the same
way regardless of which backend variant is plugged in — `Get` never asks
`Backend` anything at read time, it only compares the timestamp `Create`
already computed. That split is what keeps `Backend` a two-method interface
instead of a full storage adapter: capability description (`MaxTTL`) is a
constructor-time concern, and the actual read/write/expire logic is generic
across every backend. The `MaxTTL` check follows the same shape as every
other cross-field validation in this chapter — seed defaults, apply every
option, then check the combination once, in `New`.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Redis EXPIRE command](https://redis.io/docs/latest/commands/expire/)
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-object-storage-codec-factory.md](26-object-storage-codec-factory.md) | Next: [28-schema-validator-rule-engine.md](28-schema-validator-rule-engine.md)
