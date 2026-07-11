# Exercise 3: Ship A Module Whose Implementation Downstream Cannot Import

When you publish a library, the parts you keep exported become a contract you owe
consumers forever. The way to keep that contract small — and stay free to refactor —
is to expose a thin public package at the module root and push every implementation
detail under a module-root `internal/`. Nothing outside your module can reach the
implementation, so you can rewrite it without a breaking change.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
widget/                         module example.com/widget
  go.mod
  widget.go                     public surface: type Client; New, Slug; re-exported sentinels
  widget_test.go                tests only the exported surface (the consumer's path)
  internal/impl/impl.go         real logic: Sanitizer, Slug, ErrEmpty, ErrTooLong
  internal/impl/impl_test.go    white-box test of the real logic
  cmd/demo/main.go              runnable demo using only the public API
```

- Files: `internal/impl/impl.go`, `internal/impl/impl_test.go`, `widget.go`, `widget_test.go`, `cmd/demo/main.go`.
- Implement: a `Sanitizer` under `internal/impl` that turns raw input into a slug with validation; a public `widget.Client` that wraps it and forwards, plus re-exported sentinel errors.
- Test: a white-box test drives `impl.Sanitizer` directly; a public-surface test drives `widget.Client` and matches the re-exported sentinels with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/widget/internal/impl ~/go-exercises/widget/cmd/demo
cd ~/go-exercises/widget
go mod init example.com/widget
```

### Why the implementation goes under a module-root internal

The parent of a module-root `internal/` directory is the module root itself, so the
allow-list is the entire module and nothing else. That is the maximal hiding scope:
every package in `example.com/widget` may import `internal/impl`, and no other
module — no matter how it is imported, replaced, or vendored — ever can. When a
downstream service depends on `example.com/widget`, the compiler physically prevents
it from writing `import "example.com/widget/internal/impl"`. You can therefore change
`impl`'s types, split it, or rewrite it entirely between releases without breaking
anyone, because no consumer was ever permitted to depend on it.

The public package `widget` is the opposite: it is small and stable on purpose. It
does not put the real logic on display; it wraps `impl.Sanitizer` in its own
`Client` type and forwards the one method consumers need. The sentinels are
re-exported as package-level variables that alias the real ones, so a consumer can
still match errors with `errors.Is(err, widget.ErrEmpty)` without ever seeing the
`impl` package. This is the whole strategy in one file layout: publish the front
door, hide the machinery.

Do not invert this. Putting `widget.go` (the front door) under `internal` would make
the library unimportable — `internal` is for the machinery, never the entry point.
The proof that an external module cannot reach `impl` is exactly the
`use of internal package` build error from the CI-guard exercise; here we rely on
the rule and design around it.

Create `internal/impl/impl.go`. This is where the real work lives:

```go
// Package impl holds the widget implementation. It is under a module-root
// internal directory, so only example.com/widget may import it.
package impl

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for slug validation; callers match them with errors.Is.
var (
	ErrEmpty   = errors.New("impl: empty input")
	ErrTooLong = errors.New("impl: slug exceeds max length")
)

// Sanitizer turns raw text into a URL-safe slug, bounded by maxLen.
type Sanitizer struct {
	maxLen int
}

// New returns a Sanitizer that rejects slugs longer than maxLen (0 = unbounded).
func New(maxLen int) *Sanitizer {
	return &Sanitizer{maxLen: maxLen}
}

// Slug normalizes raw into a lowercase, hyphenated slug. It returns ErrEmpty
// (wrapped) for blank input and ErrTooLong (wrapped) when the result exceeds
// the configured max length.
func (s *Sanitizer) Slug(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", fmt.Errorf("slug: %w", ErrEmpty)
	}
	t = strings.ToLower(t)
	t = strings.Join(strings.Fields(t), "-")
	if s.maxLen > 0 && len(t) > s.maxLen {
		return "", fmt.Errorf("slug %q: %w", t, ErrTooLong)
	}
	return t, nil
}
```

Create `widget.go`. This is the entire public surface — a thin wrapper plus
re-exported sentinels:

```go
// Package widget is the public API of the module. All real logic lives under
// internal/impl, which no downstream module can import.
package widget

import "example.com/widget/internal/impl"

// Re-exported sentinels so consumers can match errors without importing impl.
var (
	ErrEmpty   = impl.ErrEmpty
	ErrTooLong = impl.ErrTooLong
)

// Client is the public handle. It hides impl.Sanitizer behind a stable type.
type Client struct {
	inner *impl.Sanitizer
}

// New returns a Client whose slugs are bounded by maxLen (0 = unbounded).
func New(maxLen int) *Client {
	return &Client{inner: impl.New(maxLen)}
}

// Slug normalizes raw into a URL-safe slug, forwarding to the hidden impl.
func (c *Client) Slug(raw string) (string, error) {
	return c.inner.Slug(raw)
}
```

### The runnable demo

The demo uses only the exported surface — `widget.New`, `Client.Slug`, and the
re-exported `widget.ErrEmpty` — exactly the path a real consumer has. It never
mentions `impl`, because it could not import it even if it wanted to.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/widget"
)

func main() {
	c := widget.New(20)

	slug, _ := c.Slug("  Platform  Team  ")
	fmt.Println("slug:", slug)

	_, err := c.Slug("   ")
	fmt.Println("blank is ErrEmpty:", errors.Is(err, widget.ErrEmpty))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slug: platform-team
blank is ErrEmpty: true
```

### Tests

Two test files, at two altitudes. `internal/impl/impl_test.go` is a white-box test
of the real logic — legal because it is `package impl`, inside the module. `widget_test.go`
tests only the exported surface, proving the consumer path works and that the
re-exported sentinels match with `errors.Is`.

Create `internal/impl/impl_test.go`:

```go
package impl

import (
	"errors"
	"testing"
)

func TestSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		maxLen  int
		in      string
		want    string
		wantErr error
	}{
		{"normalizes spacing and case", 0, "  Platform  Team ", "platform-team", nil},
		{"blank input", 0, "   ", "", ErrEmpty},
		{"exceeds max length", 4, "too long", "", ErrTooLong},
		{"within max length", 20, "ops", "ops", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := New(tc.maxLen).Slug(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Slug(%q) err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Slug(%q) unexpected err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

Create `widget_test.go`:

```go
package widget

import (
	"errors"
	"testing"
)

func TestPublicSurface(t *testing.T) {
	t.Parallel()

	c := New(0)

	got, err := c.Slug("Release Notes")
	if err != nil {
		t.Fatalf("Slug: unexpected err = %v", err)
	}
	if want := "release-notes"; got != want {
		t.Fatalf("Slug = %q, want %q", got, want)
	}

	if _, err := c.Slug(""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Slug(\"\") err = %v, want ErrEmpty via re-export", err)
	}
}
```

## Review

The design is correct when the only way a consumer touches the logic is through
`widget.Client`, and the sentinels flow out through re-exported variables so
`errors.Is` still works. The white-box `impl` test proves the machinery; the
`widget` test proves the consumer path — if you ever needed to test `impl` through
`widget` only, you would be losing coverage the internal test gives you cheaply.

The trap is inverting the layout: never place the public package under `internal`,
or the module becomes unimportable. And do not leak `impl` types onto the public
surface — if `New` returned `*impl.Sanitizer` instead of `*widget.Client`, consumers
would hold a type from a package they cannot import, which is confusing and couples
them to a name you meant to keep private. Keep the wrapper thin and the sentinels
re-exported, and the module stays refactorable for its whole life.

## Resources

- [Organizing a Go module](https://go.dev/doc/modules/layout) — official guidance on `internal/` and public surface.
- [Go Modules Reference: Internal packages](https://go.dev/ref/mod#internal-packages) — module-root `internal` hides from all other modules.
- [`errors`](https://pkg.go.dev/errors) — `errors.Is` and sentinel-error matching across a re-export.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-prove-internal-rule-in-ci.md](02-prove-internal-rule-in-ci.md) | Next: [04-internal-postgres-repository.md](04-internal-postgres-repository.md)
