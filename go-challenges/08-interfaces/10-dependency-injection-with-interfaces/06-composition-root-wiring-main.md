# Exercise 6: The Composition Root — Wiring Concrete Implementations In main()

Every service eventually needs one place where the abstractions become concrete:
where "a `Repository`" becomes "the in-memory store", "a `Logger`" becomes "a slog
JSON handler writing to stderr", and the pieces are bolted into an `http.Handler`.
That place is the composition root. This exercise builds a `Run(ctx, ln, stderr)`
that constructs the concrete adapters once, injects them down the stack, serves,
and shuts down cleanly — with a `run` indirection so it is testable.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
composition/                independent module: example.com/composition
  go.mod                    module example.com/composition
  service.go                Repository interface; UserService (business logic, no concrete imports)
  server.go                 newHandler wiring seam; inMemoryRepository adapter; Run composition root
  cmd/
    demo/
      main.go               a composition root: listener + context, calls Run, issues a request
  server_test.go            newHandler via httptest.Recorder; Run end-to-end over a loopback listener
```

- Files: `service.go`, `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: a `UserService` depending on a `Repository` interface; `newHandler(logger, svc)` that returns an `http.Handler`; a concrete `inMemoryRepository`; `Run(ctx, ln, stderr)` that builds the slog JSON logger, the concrete repo, the service, and the handler, serves on the listener, and shuts down on `ctx.Done`.
- Test: `newHandler` served by an `httptest.NewRecorder` through a fake repo; `Run` started over a `127.0.0.1:0` loopback listener, serving a real request through the injected stack, then returning cleanly when the context is cancelled.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/composition/cmd/demo
cd ~/go-exercises/composition
go mod init example.com/composition
```

### The one place allowed to import adapters

Draw the import arrows for this module. `service.go` — the business logic — imports
only `context`, `errors`, `fmt`: it names the `Repository` interface and knows
nothing concrete. `server.go` is the composition root: it is the only file that
constructs `inMemoryRepository`, builds a `slog.JSONHandler`, and assembles the
`http.Handler`. The arrow points from the concrete (server.go) toward the abstract
(service.go), never the reverse. Swap the in-memory repo for Postgres by editing
`server.go` alone; `service.go` does not change and does not even recompile against
a different driver. That is the dependency-inversion principle expressed as an
import rule, and confining adapter imports to the root is how you enforce it. (In a
larger program `service.go` would be its own package importing no adapter, and the
compiler would reject an accidental adapter import there; here the discipline is by
convention within one module.)

`Run` takes three parameters, and each is a seam that makes it testable. The
`context.Context` drives graceful shutdown: when it is cancelled, `Run` calls
`srv.Shutdown` and returns. The `net.Listener` decouples `Run` from a fixed port —
production passes a listener on `:8080`, the test passes one on `127.0.0.1:0` (an
OS-assigned free port on loopback), so the test never fights for a well-known port
and never touches the real network. The `io.Writer` for logs lets the test send
slog output to a buffer or `io.Discard` instead of the process's stderr. `main` is
then a three-line shim that constructs the real listener and a signal-aware context
and calls `Run`; because all the logic is in `Run`, `main` needs no test.

The shutdown dance is worth reading closely. `srv.Serve(ln)` runs in a goroutine
and, on a clean shutdown, returns `http.ErrServerClosed`; the goroutine sends that
into a buffered channel of size one so it can exit even after `Run` has moved on.
`Run` selects between the serve goroutine failing early (a bind error surfaces
immediately) and the context being cancelled (the normal path), where it gives
in-flight requests up to a bounded time to finish via `srv.Shutdown`.

Create `service.go`:

```go
package composition

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoUser is returned when a user id is not found.
var ErrNoUser = errors.New("user not found")

// User is the domain entity.
type User struct {
	ID   string
	Name string
}

// Repository is the narrow storage seam the business logic depends on. It names
// no concrete database; adapters satisfy it.
type Repository interface {
	UserByID(ctx context.Context, id string) (User, error)
}

// UserService is pure business logic. It imports no concrete adapter.
type UserService struct {
	repo Repository
}

// NewUserService injects the repository.
func NewUserService(repo Repository) *UserService {
	return &UserService{repo: repo}
}

// Greeting reads a user through the injected repository and formats a greeting.
func (s *UserService) Greeting(ctx context.Context, id string) (string, error) {
	u, err := s.repo.UserByID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("greeting %s: %w", id, err)
	}
	return "Hello, " + u.Name, nil
}
```

Create `server.go`:

```go
package composition

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// inMemoryRepository is a concrete Repository adapter. Only the composition
// root (this file) constructs it; business logic never imports it.
type inMemoryRepository struct {
	users map[string]User
}

func newInMemoryRepository() *inMemoryRepository {
	return &inMemoryRepository{users: map[string]User{
		"u1": {ID: "u1", Name: "Alice"},
		"u2": {ID: "u2", Name: "Bob"},
	}}
}

func (r *inMemoryRepository) UserByID(_ context.Context, id string) (User, error) {
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrNoUser
	}
	return u, nil
}

// newHandler is the wiring seam: it takes the injected logger and service and
// returns an http.Handler. It is pure wiring, so a test can drive it with fakes.
func newHandler(logger *slog.Logger, svc *UserService) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		greeting, err := svc.Greeting(r.Context(), id)
		if err != nil {
			logger.Info("user lookup failed", "id", id, "err", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		logger.Info("served greeting", "id", id)
		_, _ = io.WriteString(w, greeting)
	})
	return mux
}

// Run is the composition root. It constructs the concrete logger and repository
// once, injects them into the service and handler, serves on ln, and shuts down
// gracefully when ctx is cancelled. main is a thin shim over this.
func Run(ctx context.Context, ln net.Listener, stderr io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	repo := newInMemoryRepository()
	svc := NewUserService(repo)
	handler := newHandler(logger, svc)

	srv := &http.Server{
		Handler:     handler,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
```

### The runnable demo

`cmd/demo/main.go` is itself a composition root: it constructs a loopback listener
and a context, calls `Run` in a goroutine, issues one real request through the
wired stack, prints the result, and cancels to shut down. This is exactly the shape
of a production `main`, shrunk to a self-contained program.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"example.com/composition"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- composition.Run(ctx, ln, os.Stderr) }()

	url := "http://" + ln.Addr().String() + "/users/u1"
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("get:", err)
		cancel()
		return
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	fmt.Printf("status=%d body=%q\n", resp.StatusCode, string(body))

	cancel()
	<-done
}
```

Run it (slog logs go to stderr; the printed line goes to stdout):

```bash
go run ./cmd/demo 2>/dev/null
```

Expected output:

```
status=200 body="Hello, Alice"
```

### Tests

Two tests cover the two seams. `TestNewHandlerServes` exercises `newHandler` with a
fake repo and a discard logger through an `httptest.NewRecorder` — no port, no real
adapters, just the routing-and-formatting wiring. `TestRunWiresAndServes` exercises
the full composition root: it binds a loopback listener, starts `Run`, makes a real
HTTP request through the injected concrete stack, asserts a 200 and the body, then
cancels the context and asserts `Run` returns nil — proving graceful shutdown.

Create `server_test.go`:

```go
package composition

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeRepo struct {
	user User
	err  error
}

func (f fakeRepo) UserByID(_ context.Context, _ string) (User, error) {
	return f.user, f.err
}

func TestNewHandlerServes(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	svc := NewUserService(fakeRepo{user: User{ID: "u1", Name: "Alice"}})
	h := newHandler(logger, svc)

	req := httptest.NewRequest(http.MethodGet, "/users/u1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "Hello, Alice" {
		t.Fatalf("body = %q, want %q", got, "Hello, Alice")
	}
}

func TestNewHandlerNotFound(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	svc := NewUserService(fakeRepo{err: ErrNoUser})
	h := newHandler(logger, svc)

	req := httptest.NewRequest(http.MethodGet, "/users/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRunWiresAndServes(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, ln, io.Discard) }()

	url := "http://" + ln.Addr().String() + "/users/u1"
	var resp *http.Response
	for range 50 { // allow the goroutine a moment to begin serving
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "Hello, Alice" {
		cancel()
		t.Fatalf("body = %q, want %q", string(body), "Hello, Alice")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error on shutdown: %v", err)
	}
}
```

## Review

The wiring is correct when concrete construction lives only in `server.go` and the
business logic in `service.go` names only interfaces — the import arrow points from
concrete to abstract. `Run`'s three parameters are what make the root testable: a
context for shutdown, a listener so the test picks a free loopback port, and a
writer so logs go to a buffer. The mistakes to avoid are scattering adapter imports
into `service.go` (which re-inverts the dependency direction) and putting logic in
`main` where no test can reach it — that is why `Run` exists and `main` is a shim.
Run `go test -race` to confirm the shutdown path and the serve goroutine cooperate
without a data race.

## Resources

- [net/http: Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown semantics used by `Run`.
- [log/slog: NewJSONHandler](https://pkg.go.dev/log/slog#NewJSONHandler) — the concrete logger the composition root builds.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for driving handlers without a network.
- [Go 1.22 routing patterns](https://go.dev/blog/routing-enhancements) — the `GET /users/{id}` method-and-wildcard pattern.

---

Back to [05-consumer-defined-narrow-interfaces.md](05-consumer-defined-narrow-interfaces.md) | Next: [07-injected-clock-retry-backoff.md](07-injected-clock-retry-backoff.md)
