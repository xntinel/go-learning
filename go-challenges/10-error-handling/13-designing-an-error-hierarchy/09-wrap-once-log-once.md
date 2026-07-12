# Exercise 9: Wrapping at Boundaries Without Double-Logging

The last discipline of the hierarchy: annotate an error with `%w` as it crosses
each boundary so the message gains call-site context while keeping its identity, but
log it at *exactly one* boundary so one failed request produces one log line. This
exercise builds a repo → service → handler chain that does exactly that, and
contrasts `%w` (preserves `errors.Is`) with `%v` (severs it).

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
wrap-once-log-once/                module example.com/wrap-once-log-once
  go.mod
  layered.go                       Repo/Service/Handler chain; %w wrapping; log once; SeveredLoad (%v contrast)
  cmd/demo/main.go                 one request, one slog line, then Is via %w vs %v
  layered_test.go                  %w preserves Is after two wraps; %v severs; exactly one log record
```

- Files: `layered.go`, `cmd/demo/main.go`, `layered_test.go`.
- Implement: a `Repo.Get` that wraps `ErrUserNotFound` with `%w`, a `Service.Load` that wraps again with `%w` and does not log, a `Handler.Handle` that logs exactly once with `slog`, and a `SeveredLoad` that wraps with `%v` for contrast.
- Test: the top-level error still `errors.Is` the leaf after two `%w` wraps; the `%v` chain no longer matches; a captured `slog` handler emits exactly one record for one failed request.
- Verify: `go test -count=1 -race ./...`

### Two rules: wrap on the way up, log at the top

`%w` and `%v` look interchangeable and are not. `fmt.Errorf("service.Load: %w",
err)` wraps `err` so `Unwrap` returns it and `errors.Is`/`As` keep walking —
identity is preserved. `fmt.Errorf("service.Load: %v", err)` renders `err` into the
format string as text and returns an error with no `Unwrap` — identity is severed,
and `errors.Is(result, ErrUserNotFound)` is now false. The chain here proves it:
`Repo.Get` wraps the leaf with `%w`, `Service.Load` wraps again with `%w`, and after
two boundaries `errors.Is(err, ErrUserNotFound)` is still true. `SeveredLoad` does
the same annotation with `%v`, and the match is lost. The rule is simple: wrap with
`%w` to add context without losing the category; reach for `%v` only when you
*intend* to hide the underlying error's identity (rare, and a deliberate choice).

The second rule is about logging. Each layer *could* log — and if each does, one
failed request emits three lines (repo, service, handler), which buries the signal
and triples the volume. The discipline is log-once: the lower layers only *wrap*
(adding context to the error value), and exactly one boundary — the top, where the
request outcome is decided — *logs*. `Handler.Handle` logs with `slog` and returns
the error; `Repo` and `Service` never touch a logger. The wrapped message carries
the full call path (`service.Load: repo.Get "missing": user not found`) in a single
line, so you lose no context by logging once.

Keep the per-frame annotation short. A `%w` at every function with a paragraph of
prose produces an unreadable message; a few words of call-site context per boundary
is enough to reconstruct the path.

Create `layered.go`:

```go
package layered

import (
	"errors"
	"fmt"
	"log/slog"
)

// ErrUserNotFound is the leaf category the repository returns.
var ErrUserNotFound = errors.New("user not found")

// Repo is the lowest layer. It wraps the leaf with %w and short call-site context.
type Repo struct {
	present map[string]bool
}

func NewRepo(ids ...string) *Repo {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return &Repo{present: m}
}

func (r *Repo) Get(id string) error {
	if !r.present[id] {
		return fmt.Errorf("repo.Get %q: %w", id, ErrUserNotFound)
	}
	return nil
}

// Service is the middle layer. It adds context with %w and does NOT log.
type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service { return &Service{repo: repo} }

func (s *Service) Load(id string) error {
	if err := s.repo.Get(id); err != nil {
		return fmt.Errorf("service.Load: %w", err)
	}
	return nil
}

// Handler is the top layer. It logs exactly once, here, and nowhere below.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Handle returns the error to the caller after logging it once at this boundary.
func (h *Handler) Handle(id string) error {
	err := h.svc.Load(id)
	if err != nil {
		h.log.Error("request failed", "user_id", id, "err", err.Error())
	}
	return err
}

// SeveredLoad mirrors Service.Load but wraps with %v instead of %w, which turns
// the chain into an opaque string and severs identity. It exists to contrast.
func SeveredLoad(repo *Repo, id string) error {
	if err := repo.Get(id); err != nil {
		return fmt.Errorf("service.Load: %v", err)
	}
	return nil
}
```

### The runnable demo

The demo runs one failing request through the chain. The single log line carries the
whole call path; then it shows `errors.Is` returning true for the `%w` chain and
false for the `%v` one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"example.com/wrap-once-log-once"
)

func main() {
	// Strip the volatile time attribute so the demo output is stable.
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, opts))

	repo := layered.NewRepo("u1")
	h := layered.NewHandler(layered.NewService(repo), log)

	err := h.Handle("missing")
	fmt.Printf("Is ErrUserNotFound (via %%w): %v\n", errors.Is(err, layered.ErrUserNotFound))

	severed := layered.SeveredLoad(repo, "missing")
	fmt.Printf("Is ErrUserNotFound (via %%v): %v\n", errors.Is(severed, layered.ErrUserNotFound))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
level=ERROR msg="request failed" user_id=missing err="service.Load: repo.Get \"missing\": user not found"
Is ErrUserNotFound (via %w): true
Is ErrUserNotFound (via %v): false
```

### Tests

The tests capture a `slog` handler into a buffer to *count* log records — the
assertion that one request yields exactly one line — and assert the identity
contrast: the `%w` chain still `errors.Is` the leaf after two wraps, the `%v` chain
does not.

Create `layered_test.go`:

```go
package layered

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestWrapPreservesIdentity(t *testing.T) {
	t.Parallel()
	repo := NewRepo("u1")
	svc := NewService(repo)

	err := svc.Load("missing") // two %w wraps: repo then service
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatal("wrapped chain no longer Is ErrUserNotFound after two wraps")
	}
}

func TestSeveredChainLosesIdentity(t *testing.T) {
	t.Parallel()
	repo := NewRepo("u1")
	err := SeveredLoad(repo, "missing")
	if errors.Is(err, ErrUserNotFound) {
		t.Fatal("severed chain still Is ErrUserNotFound; the v-verb should sever identity")
	}
	// The message still mentions the text, but the identity is gone.
	if !strings.Contains(err.Error(), "user not found") {
		t.Fatal("severed message should still contain the rendered text")
	}
}

func TestLogsExactlyOncePerRequest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewHandler(NewService(NewRepo("u1")), log)

	err := h.Handle("missing")
	if err == nil {
		t.Fatal("expected an error for a missing user")
	}

	records := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) != "" {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("emitted %d log records for one request; want 1", records)
	}
}

func TestSuccessLogsNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewHandler(NewService(NewRepo("u1")), log)

	if err := h.Handle("u1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("success emitted a log line: %q", buf.String())
	}
}
```

## Review

The chain is correct when two properties hold: the top-level error still
`errors.Is` the leaf category after every boundary has wrapped it with `%w`, and one
failed request produces exactly one log record. The `%v` contrast is the trap to
internalize — `%v` looks like a harmless formatting choice but it severs the chain,
so a handler that maps categories with `errors.Is` silently starts defaulting to
500 the moment a middle layer switched to `%v`. Keep every boundary on `%w`, keep
the annotations short, and log at the single boundary that owns the request outcome.
Logging at every layer is the other half of the anti-pattern: it triples the volume
and buries the one line that matters.

## Resources

- [`fmt.Errorf` and `%w`](https://pkg.go.dev/fmt#Errorf) — wrapping that preserves identity, versus `%v` that does not.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging, `NewJSONHandler`, and `HandlerOptions.ReplaceAttr`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the walk that `%w` keeps alive and `%v` breaks.

---

Back to [08-machine-readable-error-codes.md](08-machine-readable-error-codes.md) | Next: [../14-error-observability/00-concepts.md](../14-error-observability/00-concepts.md)
