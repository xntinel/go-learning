# Exercise 2: Selecting Subtrees with -run and -skip in CI

The value of named subtests is that CI can address them. This exercise builds a
registration-request validator with a grouped subtest tree, then drives it purely
through the `go test` CLI: run one case, run one subtree, or exclude a slow
subtree — the exact moves a pipeline makes to run a fast suite on every push and
the expensive subtree nightly.

This module is fully self-contained: its own `go mod init`, validator, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
regvalidate/                independent module: example.com/regvalidate
  go.mod                    go 1.26
  validate.go               type Registration; func Validate(Registration) error; sentinel errors
  cmd/
    demo/
      main.go               runnable demo: validate a good and a bad registration
  validate_test.go          grouped subtests: valid/*, invalid/*, slow/*
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate(Registration) error` with sentinel errors
  (`ErrNameRequired`, `ErrBadEmail`, `ErrUnderage`).
- Test: subtests grouped under `valid`, `invalid`, and `slow` so each subtree is
  addressable by name.
- Verify: `go test -count=1 -race ./...`, then the `-run`/`-skip` invocations below.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/04-subtests-and-t-run/02-run-filter-and-skip/cmd/demo
cd go-solutions/12-testing-ecosystem/04-subtests-and-t-run/02-run-filter-and-skip
```

### How -run and -skip carve the tree

`-run` takes a regexp that the test runner splits on `/`, applying one pattern per
name segment of the subtest path. `TestValidate/valid/minimal` has three
segments. So:

- `-run 'TestValidate/valid/.*'` runs `TestValidate`, then its `valid` child, then
  every leaf under `valid` — the fast happy-path subtree.
- `-run 'TestValidate/invalid/bad_email$'` runs exactly one leaf.
- `-run 'TestValidate/[^/]+$'` matches a second segment that contains no further
  slash — the leaf-level groups directly under `TestValidate` that have no
  children of their own.
- `-skip 'slow'` (Go 1.20+) is the inverse filter: run everything *except*
  subtests whose name matches `slow`, which is how the push pipeline drops the
  expensive subtree while the nightly cron runs it with `-run 'TestValidate/slow'`.

Two mechanical facts bite people. The regexp is unanchored unless you anchor it,
so `-run 'TestValidate/valid'` also matches a sibling named `valid_extended`; use
`$` when you mean one name. And names substitute spaces with underscores, so a
`t.Run("bad email", …)` is addressed as `bad_email`, never `bad email`.

The validator uses package-level sentinel errors wrapped with `%w`, so a caller
(and a test) can classify a failure with `errors.Is` rather than string-matching
the message — the idiom every validation layer should follow.

Create `validate.go`:

```go
package regvalidate

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors let callers classify a validation failure with errors.Is.
var (
	ErrNameRequired = errors.New("name is required")
	ErrBadEmail     = errors.New("email is malformed")
	ErrUnderage     = errors.New("must be at least 18")
)

// Registration is the inbound signup DTO.
type Registration struct {
	Name  string
	Email string
	Age   int
}

// Validate checks a registration and returns the first rule it violates, wrapped
// with %w so the caller can match it with errors.Is.
func Validate(r Registration) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("validate name: %w", ErrNameRequired)
	}
	at := strings.IndexByte(r.Email, '@')
	if at <= 0 || at == len(r.Email)-1 {
		return fmt.Errorf("validate email %q: %w", r.Email, ErrBadEmail)
	}
	if r.Age < 18 {
		return fmt.Errorf("validate age %d: %w", r.Age, ErrUnderage)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/regvalidate"
)

func main() {
	good := regvalidate.Registration{Name: "Ada", Email: "ada@example.com", Age: 37}
	bad := regvalidate.Registration{Name: "Bo", Email: "not-an-email", Age: 41}

	for _, r := range []regvalidate.Registration{good, bad} {
		switch err := regvalidate.Validate(r); {
		case err == nil:
			fmt.Printf("%s: ok\n", r.Name)
		case errors.Is(err, regvalidate.ErrBadEmail):
			fmt.Printf("%s: rejected (bad email)\n", r.Name)
		default:
			fmt.Printf("%s: rejected (%v)\n", r.Name, err)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Ada: ok
Bo: rejected (bad email)
```

### Tests

The subtests are grouped so each subtree is independently addressable. The cases
run serially (no `t.Parallel`) precisely so the `-v` output below is
deterministic — parallel subtests interleave their `RUN`/`PASS` lines. The `slow`
group stands in for an expensive integration subtree that the push pipeline skips.

Create `validate_test.go`:

```go
package regvalidate

import (
	"errors"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cases := []struct {
			name string
			in   Registration
		}{
			{"minimal", Registration{Name: "A", Email: "a@b.co", Age: 18}},
			{"full", Registration{Name: "Ada Lovelace", Email: "ada@example.com", Age: 37}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if err := Validate(tc.in); err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.in, err)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		cases := []struct {
			name string
			in   Registration
			want error
		}{
			{"empty_name", Registration{Name: "  ", Email: "a@b.co", Age: 20}, ErrNameRequired},
			{"bad_email", Registration{Name: "A", Email: "no-at", Age: 20}, ErrBadEmail},
			{"underage", Registration{Name: "A", Email: "a@b.co", Age: 17}, ErrUnderage},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := Validate(tc.in)
				if !errors.Is(err, tc.want) {
					t.Fatalf("Validate(%+v) = %v, want errors.Is %v", tc.in, err, tc.want)
				}
			})
		}
	})

	// A stand-in for an expensive integration subtree. The push pipeline runs
	// `go test -skip slow`; the nightly cron runs `go test -run TestValidate/slow`.
	t.Run("slow", func(t *testing.T) {
		t.Run("bulk_batch", func(t *testing.T) {
			for i := range 1000 {
				r := Registration{Name: "u", Email: "u@example.com", Age: 18 + i%50}
				if err := Validate(r); err != nil {
					t.Fatalf("bulk case %d: %v", i, err)
				}
			}
		})
	})
}
```

### Driving it from the CLI

Run the fast happy-path subtree only:

```bash
go test -run 'TestValidate/valid/.*' -v
```

Expected output (excerpt):

```text
=== RUN   TestValidate
=== RUN   TestValidate/valid
=== RUN   TestValidate/valid/minimal
=== RUN   TestValidate/valid/full
--- PASS: TestValidate (0.00s)
    --- PASS: TestValidate/valid (0.00s)
        --- PASS: TestValidate/valid/minimal (0.00s)
        --- PASS: TestValidate/valid/full (0.00s)
PASS
```

Exclude the slow subtree, as the push pipeline does:

```bash
go test -skip 'slow' -v
```

The `TestValidate/slow` group and its `bulk_batch` child do not appear; `valid`
and `invalid` and their leaves run. Run only the slow subtree, as the nightly does:

```bash
go test -run 'TestValidate/slow' -v
```

Only `TestValidate`, `TestValidate/slow`, and `TestValidate/slow/bulk_batch` run.

## Review

The point of this exercise is the addressing scheme, not the validator. Because
every group and leaf has a stable, unique, space-free name, `-run` and `-skip` can
select any subtree without a code change: `-run 'TestValidate/valid/.*'` for the
happy path, `-skip 'slow'` on push, `-run 'TestValidate/slow'` nightly. The two
traps to internalize: the pattern is a regexp, so anchor with `$` when a name is a
prefix of another; and a segment absent from the pattern matches everything at
that level, which is why `-run TestValidate` alone runs the entire tree. Confirm
your sentinel errors are matchable with `errors.Is` — that is what lets the
`invalid` cases assert a specific rule rather than a brittle message string.

## Resources

- [go test flags (-run, -skip) — cmd/go](https://pkg.go.dev/cmd/go#hdr-Testing_flags)
- [testing.T.Run — pkg.go.dev](https://pkg.go.dev/testing#T.Run)
- [Using Subtests and Sub-benchmarks — Go Blog](https://go.dev/blog/subtests)

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-nested-subtest-hierarchy.md](03-nested-subtest-hierarchy.md)
