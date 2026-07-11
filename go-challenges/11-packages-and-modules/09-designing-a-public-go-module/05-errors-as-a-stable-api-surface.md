# Exercise 5: Design the error contract: sentinel identity, typed errors, and wrapping

Errors are API. A sentinel promises identity through `errors.Is`; a typed error
promises fields through `errors.As`; wrapping with `%w` preserves both through the
chain. This exercise designs the `publicstr` error surface deliberately: keep
`ErrEmpty` as a matchable sentinel, add a typed `*SlugTooLongError` carrying the
offending length and limit, and wrap internal failures so callers branch on error
identity and type instead of fragile string matching.

This module is fully self-contained: its own `go mod init`, the library with the
error types, a demo, and tests that assert `errors.Is` and `errors.As` behavior.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  strings.go               ErrEmpty, SlugTooLongError, SlugifyBounded, ValidateSlug
  cmd/
    demo/
      main.go              runnable demo branching on Is/As
  strings_test.go          errors.Is(ErrEmpty); errors.As(&SlugTooLongError); wrap unwrap
```

- Files: `strings.go`, `cmd/demo/main.go`, `strings_test.go`.
- Implement: the `ErrEmpty` sentinel, a typed `*SlugTooLongError{Got, Limit int}` with an `Error()` method, `SlugifyBounded(s string, limit int)` that wraps the typed error with `%w`, and `ValidateSlug` that joins multiple field errors with `errors.Join`.
- Test: `errors.Is(err, ErrEmpty)` for empty input; `errors.As(err, &target)` exposes `Got`/`Limit`; a wrapped error still unwraps to both the sentinel and the typed error; `errors.Join` collects multiple.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/publicstr/cmd/demo
cd ~/go-exercises/publicstr
go mod init example.com/publicstr
```

### Sentinel identity versus typed fields versus wrapping

Three distinct promises live in an error surface, and a senior engineer picks the
right one per failure:

- A *sentinel* (`errors.New`) promises identity. A consumer writes
  `errors.Is(err, ErrEmpty)` and branches on "this exact failure". Use it when the
  only thing the caller needs to know is *which* failure occurred, with no extra
  data. Its value is frozen the moment consumers match it.
- A *typed error* promises structured detail. `*SlugTooLongError` carries `Got`
  and `Limit`; a consumer writes `errors.As(err, &tooLong)` and then reads
  `tooLong.Limit` to build a message or a metric. Use it when the caller needs
  *data about* the failure. The concrete type and its exported fields are frozen
  once consumers reach into them.
- *Wrapping* with `fmt.Errorf("...: %w", err)` threads both promises through
  layers of context. A wrapped `*SlugTooLongError` is still discoverable by
  `errors.As`, and a wrapped `ErrEmpty` is still discoverable by `errors.Is`, even
  under several layers — because `%w` records the cause and `Is`/`As` walk the
  chain via `Unwrap`.

`errors.Join` is the fourth tool: it combines several errors into one whose
`Unwrap() []error` exposes all of them, so a validation pass can report every
problem at once while each remains individually matchable by `Is`/`As`. That is
exactly what a config or request validator wants — not "the first thing that was
wrong" but "everything that was wrong".

The critical discipline: once these are published and consumers match on them,
changing `ErrEmpty`'s value, renaming `SlugTooLongError` or its fields, or dropping
the `%w` that makes a wrapped cause discoverable are all *breaking changes* — as
breaking as editing a function signature. Design the error surface once,
deliberately.

Create `strings.go`:

```go
package publicstr

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrEmpty is returned when the input reduces to no slug-able characters. Match
// it with errors.Is; its identity is frozen contract.
var ErrEmpty = errors.New("publicstr: empty string")

// SlugTooLongError reports that a produced slug exceeded a caller's limit. Match
// it with errors.As to read Got and Limit; the type and its fields are frozen
// contract.
type SlugTooLongError struct {
	Got   int // rune length of the produced slug
	Limit int // caller-supplied maximum
}

// Error implements the error interface.
func (e *SlugTooLongError) Error() string {
	return fmt.Sprintf("publicstr: slug too long: got %d runes, limit %d", e.Got, e.Limit)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case unicode.IsSpace(r), r == '-', r == '_':
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// SlugifyBounded produces a slug and enforces a rune-length limit. It returns
// ErrEmpty when the result is empty, and an error wrapping *SlugTooLongError when
// the slug exceeds limit. The wrap uses %w so callers can errors.As the typed
// error out of the contextual message.
func SlugifyBounded(s string, limit int) (string, error) {
	out := slugify(s)
	if out == "" {
		return "", ErrEmpty
	}
	if n := utf8.RuneCountInString(out); n > limit {
		return "", fmt.Errorf("slugify %q: %w", s, &SlugTooLongError{Got: n, Limit: limit})
	}
	return out, nil
}

// ValidateSlug checks each field and returns a single error joining every
// failure via errors.Join, so callers see all problems at once while each stays
// matchable by errors.Is / errors.As.
func ValidateSlug(fields map[string]string, limit int) error {
	var errs []error
	for name, value := range fields {
		if _, err := SlugifyBounded(value, limit); err != nil {
			errs = append(errs, fmt.Errorf("field %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/publicstr"
)

func main() {
	if _, err := publicstr.SlugifyBounded("   ", 20); errors.Is(err, publicstr.ErrEmpty) {
		fmt.Println("empty: matched ErrEmpty")
	}

	_, err := publicstr.SlugifyBounded("the quick brown fox", 5)
	var tooLong *publicstr.SlugTooLongError
	if errors.As(err, &tooLong) {
		fmt.Printf("too long: got %d, limit %d\n", tooLong.Got, tooLong.Limit)
	}

	verr := publicstr.ValidateSlug(map[string]string{"title": "ok"}, 5)
	fmt.Printf("valid title: %v\n", verr)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
empty: matched ErrEmpty
too long: got 19, limit 5
valid title: <nil>
```

### Tests

The tests prove the whole point: consumers branch on identity and type, never on
string content, and wrapping does not hide either.

Create `strings_test.go`:

```go
package publicstr

import (
	"errors"
	"strings"
	"testing"
)

func TestEmptyIsMatchable(t *testing.T) {
	t.Parallel()
	if _, err := SlugifyBounded("   ", 10); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestTooLongIsTyped(t *testing.T) {
	t.Parallel()
	_, err := SlugifyBounded("the quick brown fox", 5)
	var tooLong *SlugTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("err = %v, want *SlugTooLongError", err)
	}
	if tooLong.Limit != 5 {
		t.Fatalf("Limit = %d, want 5", tooLong.Limit)
	}
	if tooLong.Got <= tooLong.Limit {
		t.Fatalf("Got = %d, want > Limit %d", tooLong.Got, tooLong.Limit)
	}
}

func TestWrappedStillUnwraps(t *testing.T) {
	t.Parallel()
	_, err := SlugifyBounded("the quick brown fox", 5)
	// The error carries contextual text but the typed cause is still reachable.
	if !strings.Contains(err.Error(), "slugify") {
		t.Fatalf("error lost its context: %v", err)
	}
	var tooLong *SlugTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("wrapped error did not unwrap to *SlugTooLongError: %v", err)
	}
}

func TestValidateJoinsAllFailures(t *testing.T) {
	t.Parallel()
	err := ValidateSlug(map[string]string{
		"empty": "   ",
		"long":  "the quick brown fox",
	}, 5)
	if err == nil {
		t.Fatal("want joined error, got nil")
	}
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("joined error should contain ErrEmpty: %v", err)
	}
	var tooLong *SlugTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("joined error should contain *SlugTooLongError: %v", err)
	}
}

func TestValidatePassesWhenAllValid(t *testing.T) {
	t.Parallel()
	if err := ValidateSlug(map[string]string{"a": "hello", "b": "world"}, 20); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}
```

## Review

The error surface is correct when a consumer never needs to inspect a message
string: empty input is `errors.Is(err, ErrEmpty)`, an over-long slug is
`errors.As(err, &tooLong)` exposing `Got` and `Limit`, and both survive being
wrapped with `%w` and joined with `errors.Join`. The discipline to internalize is
that each of these is now frozen: change `ErrEmpty`'s identity, rename a field on
`SlugTooLongError`, or drop the `%w` and you silently break every consumer's error
handling — a break no compiler catches. Note `errors.As` needs a pointer to the
target pointer (`var tooLong *SlugTooLongError; errors.As(err, &tooLong)`); getting
that indirection wrong is the classic mistake. Prefer joining all validation
failures over returning the first, so callers can report everything wrong at once.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `Is`, `As`, `Join`, and `Unwrap` semantics.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — sentinels, `%w` wrapping, and when to use `Is` vs `As`.
- [Go 1.20 release notes: errors.Join](https://go.dev/doc/go1.20#errors) — the multi-error API and `Unwrap() []error`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-functional-options-for-an-evolvable-api.md](04-functional-options-for-an-evolvable-api.md) | Next: [06-shrink-the-surface-with-internal-packages.md](06-shrink-the-surface-with-internal-packages.md)
