# ORDER: Rewrite low-quality Go lessons to the canonical standard

You are correcting Go curriculum lessons under `challenges/go/`. This is a
substantial rewrite per file, NOT a find-and-replace and NOT mechanical
section deletion. Mechanical-only cleanup is forbidden and counts as a failed
lesson.

## Required reading before touching anything

1. `challenges/CURRICULUM-METHODOLOGY.md` — the source of truth. Especially:
   - **§5** canonical document skeleton.
   - **§14.1** Go quality rubric A-E + the verify gate (a lesson that fails
     A-E or the gate is a rewrite candidate, not a fix-later item).
   - **§15** self-consistency (the prose must describe what the code actually
     does).
   - **§0** form rules (English, no emojis, no banned sections).
2. Gold reference lessons (imitate structure and depth, do NOT copy content):
   - `challenges/go/07-structs-and-methods/01-struct-declaration-and-initialization/01-struct-declaration-and-initialization.md`
   - `challenges/go/10-error-handling/` (any lesson)

## Which files to fix

Only files that contain the banned metadata marker:

```bash
grep -rl --include="*.md" "bloom_level:" challenges/go/<CHAPTER>/
```

Do NOT touch any `.md` that does not match. Do NOT rename or reorder
files/folders. Keep the existing `# N. Title` H1.

## Canonical target shape (§5)

```
# N. Title
<short intro: what is distinctive/hard here; no patronizing opener>
## Concepts          (### subsections: the model, the hard "why", trade-offs, failure modes)
## Exercises         (2-3 real whole-task exercises that BUILD the artifact + a real *_test.go)
## Common Mistakes   (highest-value traps; wrong approach beside the fix, tied to THIS lesson's code)
## Verification      (exact procedure to check artifacts against ground truth)
## Summary           (tight bullets)
## What's Next        (one line; verify the relative link to the next .md resolves)
## Resources         (3-5 good sources, cited where factual)
```

Actions per file:
1. Delete the HTML metadata comment block
   (`difficulty/concepts/tools/bloom_level/prerequisites`).
2. Delete `## Prerequisites` ("Go 1.26+ installed" boilerplate) and
   `## Learning Objectives` (bolded Bloom verbs). Both banned by §5.
3. **Never delete `## The Problem` without migrating its content first.** In
   many lessons that section holds the problem statement, the hints, and the
   ONLY code. Move requirements into the intro and the code into
   `## Exercises`/`*_test.go`. If, after removing sections, a lesson is left
   with a placeholder `## Exercises` or a `## Verification` that references code
   that no longer exists, the lesson is BROKEN, not clean, and counts as a
   batch failure.
4. Rewrite `## Concepts` into `###` subsections with prose that teaches the
   why (semantics, trade-offs, failure modes).
5. Convert every `### Step N` / `### Intermediate Verification` into
   `## Exercises` with a real `*_test.go` (see EXERCISE QUALITY below).
6. Rewrite `## Common Mistakes` specific to THIS lesson (Wrong / what happens /
   Fix).
7. `## Verification` runs real commands: `gofmt -l`, `go vet`, `go build`,
   `go test -count=1 -race`. Ask the learner to add at least one test of their
   own.

## EXERCISE QUALITY (the highest-priority rule)

A library lesson is verified with a real `*_test.go`, NOT with a `main()` that
prints and an "Expected output" block. A demo `main()` does not fail when the
code regresses, does not run in CI, and does not teach testing.

Hard rules for exercises:
- NO `main()` + `fmt.Printf` + "Expected output" compared by eye as the
  verification mechanism. The verification must fail on its own.
- Every library lesson must include BOTH runnable demonstrations, IN ADDITION
  to the `*_test.go`:
  1. An `Example` function with an `// Output:` comment (auto-verified under
     `go test`):

     ```go
     func ExampleNew() {
     	s, _ := New(WithPort(443), WithHost("localhost"))
     	fmt.Printf("%s:%d\n", s.Addr(), s.Port())
     	// Output: localhost:443
     }
     ```

  2. A real `cmd/demo/main.go` that exercises the package, runnable with
     `go run ./cmd/demo`. Because `cmd/demo` is a separate `package main`, it
     can only touch EXPORTED API; if the demo needs to read fields, add small
     exported accessors (e.g. `func (s *Server) Port() int`) — do not export
     the raw fields just for the demo.
- The `*_test.go` is the verification and is never optional. A bare `main()`
  whose only check is an eyeballed printf block is forbidden.
- Each exercise builds the artifact; the final exercise is a real `*_test.go`:
  table-driven, `t.Parallel()`, and `errors.Is` for validation errors.
- Validation errors use sentinel errors wrapped with `%w`, so tests assert with
  `errors.Is` instead of matching strings.
- The lesson is a real package (e.g. `package server`), not `package main`, so
  tests in the same package can reach unexported fields — exactly like the gold
  `internal/user` lessons.
- `## Verification` runs `go test -count=1 -race ./...`, never `go run main.go`
  (unless it is a real CLI).

### Reference model: how functional-options exercises must look

Use this as the template for converting a `main()`-based lesson into a real,
testable one.

```markdown
## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/funcoptions
cd ~/go-exercises/funcoptions
go mod init example.com/funcoptions
```

This is a library, not a program: there is no `main`. You verify it with
`go test`.

### Exercise 1: The Server, the Option Type, and the Constructor

Create `server.go`:

```go
package server

import (
	"fmt"
	"log/slog"
	"time"
)

type Server struct {
	host         string
	port         int
	readTimeout  time.Duration
	writeTimeout time.Duration
	maxConns     int
	logger       *slog.Logger
}

type Option func(*Server) error

func New(opts ...Option) (*Server, error) {
	s := &Server{
		host:         "0.0.0.0",
		port:         8080,
		readTimeout:  5 * time.Second,
		writeTimeout: 10 * time.Second,
		maxConns:     100,
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
	}
	return s, nil
}
```

Defaults are set first; options override them in order. Later options win over
earlier ones — that property is pinned by a test below.

### Exercise 2: Options That Validate With Sentinel Errors

```go
package server

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidPort = errors.New("port must be between 1 and 65535")
	ErrEmptyHost   = errors.New("host must not be empty")
	ErrBadTimeout  = errors.New("timeout must be positive")
	ErrBadMaxConns = errors.New("max connections must be at least 1")
)

func WithPort(port int) Option {
	return func(s *Server) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%w: got %d", ErrInvalidPort, port)
		}
		s.port = port
		return nil
	}
}

func WithHost(host string) Option {
	return func(s *Server) error {
		if host == "" {
			return ErrEmptyHost
		}
		s.host = host
		return nil
	}
}

func WithReadTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return fmt.Errorf("read %w: got %s", ErrBadTimeout, d)
		}
		s.readTimeout = d
		return nil
	}
}

func WithMaxConns(n int) Option {
	return func(s *Server) error {
		if n < 1 {
			return fmt.Errorf("%w: got %d", ErrBadMaxConns, n)
		}
		s.maxConns = n
		return nil
	}
}
```

### Exercise 3: Composable Option Groups

```go
package server

import "time"

func WithProductionDefaults() Option {
	return func(s *Server) error {
		for _, opt := range []Option{
			WithReadTimeout(30 * time.Second),
			WithMaxConns(1000),
		} {
			if err := opt(s); err != nil {
				return err
			}
		}
		return nil
	}
}
```

### Exercise 4: Test the Contract

Create `server_test.go`. The tests are the verification — there is no `main` to
eyeball:

```go
package server

import (
	"errors"
	"testing"
	"time"
)

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	s, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if s.host != "0.0.0.0" || s.port != 8080 || s.maxConns != 100 {
		t.Fatalf("defaults wrong: %+v", s)
	}
}

func TestWithPortRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	for _, port := range []int{0, -1, 65536, 99999} {
		_, err := New(WithPort(port))
		if !errors.Is(err, ErrInvalidPort) {
			t.Errorf("WithPort(%d): err = %v, want ErrInvalidPort", port, err)
		}
	}
}

func TestWithPortAcceptsValid(t *testing.T) {
	t.Parallel()

	s, err := New(WithPort(443))
	if err != nil {
		t.Fatalf("WithPort(443) error = %v", err)
	}
	if s.port != 443 {
		t.Fatalf("port = %d, want 443", s.port)
	}
}

func TestLaterOptionsWin(t *testing.T) {
	t.Parallel()

	s, err := New(WithPort(3000), WithPort(4000))
	if err != nil {
		t.Fatal(err)
	}
	if s.port != 4000 {
		t.Fatalf("port = %d, want 4000 (last option wins)", s.port)
	}
}

func TestPresetThenOverride(t *testing.T) {
	t.Parallel()

	s, err := New(WithProductionDefaults(), WithPort(443))
	if err != nil {
		t.Fatal(err)
	}
	if s.maxConns != 1000 || s.port != 443 {
		t.Fatalf("preset+override wrong: maxConns=%d port=%d", s.maxConns, s.port)
	}
}

func TestNewStopsAtFirstInvalidOption(t *testing.T) {
	t.Parallel()

	_, err := New(WithHost("api.example.com"), WithReadTimeout(-1))
	if !errors.Is(err, ErrBadTimeout) {
		t.Fatalf("err = %v, want ErrBadTimeout", err)
	}
}
```

Your turn: add `TestWithHostRejectsEmpty` that calls `New(WithHost(""))` and
asserts `errors.Is(err, ErrEmptyHost)`.

## Verification

From `~/go-exercises/funcoptions`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification — there is no program to run.
```

## Verification regime (varies by chapter)

- **Compilable-offline chapters (01, 14-37):** extract each lesson's code into a
  temporary module and run the full §14.1 gate (`gofmt -l`, `go vet`,
  `go build`, `go test -race`). A lesson that does not pass is rewritten, not
  marked OK. `SKIP` for "no extractable code block" is unacceptable in a lesson
  that should have code.
- **Capstones (38-47), not compilable offline (Linux namespaces, cgroups,
  networking, external deps):** the gold bar is a full rewrite to §5 with
  realistic, substantial code (`cmd/<name>/main.go` + a serious `*_test.go`),
  validated with §15 (prose matches code) + `gofmt` + `go vet` on what is
  extractable. The build/test gate is explicitly deferred to a later networked
  run. In the report, state plainly: "not compiled offline; validated by §15 +
  gofmt + vet".

## Research the topic online BEFORE writing (quality lift)

Before rewriting a lesson, research authoritative sources to ground the
`## Concepts` in correct, current, deep material and to verify every API
signature you use. Do not write from memory alone. Prioritize, in order:

1. Official Go: `go.dev/ref/spec`, `go.dev/doc/effective_go`, the Go Blog
   (`go.dev/blog`), and `pkg.go.dev` for the exact stdlib signatures and
   behavior of every function/type you reference.
2. The actual source of any library the lesson names (e.g. for the adapter or
   options patterns: how `grpc`, `zap`, `cobra`, `net/http` really do it) so the
   lesson reflects real-world usage, not a toy caricature.
3. Recognized style/idiom references (Go Code Review Comments, the Uber Go Style
   Guide) for idiomatic patterns and common-mistake material.

Use what you learn to: (a) make the Concepts subsections explain the real "why"
and failure modes, (b) confirm the code compiles against the current stdlib,
(c) source the `## Common Mistakes` from real traps, (d) keep `## Resources`
to the 3-5 best primary sources you actually consulted (cite the specific page,
not a generic homepage). Never invent an API or a behavior — if a source
contradicts your memory, trust the source.

## Where to run the gate (avoid sandbox blocks)

Do NOT write outside the project directory (e.g. `/tmp`) — the sandbox rejects
it and you will stall. Create the temporary verification module under
`challenges/go/.verify/<chapter>/<lesson>/` inside the repo, run the gate there,
then delete it. If even that is blocked, DO NOT STOP: finish every rewrite in
the chapter and flag in the report which lessons you could not gate-check, so
the auditor runs them. Completing all rewrites is mandatory; self-verification
is best-effort.

## Self-consistency pass (§15) — run on every rewritten lesson

The gate tells you the code compiles; §15 tells you the prose matches the code.
Check every claim: if Common Mistakes says "the code wraps the error with %w",
the code must actually wrap with %w. If an exercise tells the learner to write a
test asserting behavior X, the lesson's code must actually implement X. A lesson
can pass the gate and still be wrong.

## Hard rules

- ALL Go code in fenced blocks MUST be indented with TABS, not spaces. Go
  source requires tabs; `gofmt -l` flags space-indented code and that fails the
  §14.1 gate. The gold lessons use tabs. Space-indented Go is an automatic
  batch failure. Verify with: extract a block and run `gofmt -l` — it must be
  empty.
- NO emojis or decorative symbols (check `✓ ⭐ ▶ ⚠ 🚀` etc). Plain text. Use
  "OK"/"FAIL" or "Wrong"/"Fix" for correct/incorrect. Only functional math
  operators are allowed.
- English everywhere.
- Do NOT invent Go APIs. Every stdlib signature must be real and compile with
  modern Go (the corpus pins `go 1.26`). Prefer the stdlib: use `strconv.Itoa`,
  `http.StatusText`, etc. — do NOT hand-roll what the stdlib already provides.
- No dead code: every branch must be reachable and, where it matters, exercised
  by a test.
- Do not touch files without `bloom_level:`. Do not rename/reorder. Keep the H1.
- Depth must match the gold examples; in capstones the code must be substantial,
  not a toy.

## Per-batch protocol

- One batch = one chapter. Process chapters independently; do not mix more than
  one chapter per run.
- After each batch, produce a report: (1) files touched, (2) per-lesson gate
  result (PASS/FAIL/SKIP-offline), (3) lessons needing a second pass, (4) any
  broken `What's Next` links.

## Suggested order

Pilot: `24-design-patterns-in-go`. Within it, recover deleted code for lessons
06-09 from git before rewriting:

```bash
git show HEAD:challenges/go/24-design-patterns-in-go/06-service-layer-pattern/06-service-layer-pattern.md
```

Then do the compilable chapters (01, 14-37), then the capstones (38-47).
