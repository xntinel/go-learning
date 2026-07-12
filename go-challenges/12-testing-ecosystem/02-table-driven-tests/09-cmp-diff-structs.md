# Exercise 9: Comparing Rich Structs with go-cmp Options

Real domain structs carry volatile fields — a generated ID, a `CreatedAt`
timestamp — and slices that represent sets, where order and nil-vs-empty should not
matter. `reflect.DeepEqual` and `==` fail on all of these, and report a bare
`false` when they do. This module builds a `NormalizeUser` transform and tests it
with `cmp.Diff` plus `cmpopts`, then contrasts the same comparison under
`reflect.DeepEqual` to show why it is inadequate.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. It uses `github.com/google/go-cmp`; nothing here imports another exercise.

## What you'll build

```text
usernorm/                 independent module: example.com/usernorm
  go.mod                  go 1.26; requires github.com/google/go-cmp
  user.go                 User + NormalizeUser (trim, lowercase, defaults, tags)
  cmd/
    demo/
      main.go             normalizes a raw user, prints the result
  user_test.go            table + shared []cmp.Option + reflect.DeepEqual contrast
```

- Files: `user.go`, `cmd/demo/main.go`, `user_test.go`.
- Implement: `NormalizeUser(User) User` that trims the name, lowercases and trims the email, defaults an empty role to `member`, cleans the tag list, and stamps `CreatedAt`.
- Test: a table of `{name, in, want}` compared with one shared `[]cmp.Option` (`IgnoreFields` for `CreatedAt`/`ID`, `EquateEmpty`, `SortSlices`), plus a test showing `reflect.DeepEqual` wrongly distinguishes nil and empty slices.
- Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Why reflect.DeepEqual is the wrong tool here

`NormalizeUser` produces a `User` with three properties that defeat naive equality.
It stamps `CreatedAt` with `time.Now()`, so two runs never produce the same
timestamp — a comparison that includes `CreatedAt` can never pass. It cleans the
`Tags` slice, which may come out as an empty non-nil slice or a nil slice depending
on the input — and `reflect.DeepEqual` treats `nil` and `[]string{}` as *unequal*,
a distinction almost no domain cares about. And tags are conceptually a *set*, so
`["go", "api"]` and `["api", "go"]` should compare equal, which order-sensitive
equality rejects. On top of all that, `reflect.DeepEqual` and `==` report only
`false`, telling you nothing about *which* field differs.

`go-cmp` solves each of these with an option, and the senior move is to build the
option list *once* and share it across the whole table:

- `cmpopts.IgnoreFields(User{}, "CreatedAt", "ID")` drops the volatile fields from
  the comparison entirely, so the timestamp and generated ID never cause a
  mismatch.
- `cmpopts.EquateEmpty()` makes a nil slice and an empty slice compare equal, which
  is what the domain means.
- `cmpopts.SortSlices(less)` sorts set-like slices before comparing, so tag order
  is irrelevant.

The assertion is `if diff := cmp.Diff(tc.want, got, opts...); diff != "" {
t.Fatalf("(-want +got):\n%s", diff) }`. The empty-string-means-equal contract makes
the diff *itself* the failure message, naming the differing field — the readable
output `reflect.DeepEqual` never gives you. For numeric fields that carry rounding
error, `cmpopts.EquateApprox(fraction, delta)` plays the same role as
`EquateEmpty` does for slices; this transform has no float field, but the option is
the tool you reach for when it does.

Create `user.go`:

```go
package usernorm

import (
	"strings"
	"time"
)

// User is a domain user with a generated ID and creation timestamp.
type User struct {
	ID        string
	Name      string
	Email     string
	Role      string
	Tags      []string
	CreatedAt time.Time
}

// NormalizeUser cleans a raw user into canonical form: trimmed name, trimmed and
// lowercased email, a default role, a cleaned tag list, and a fresh timestamp.
func NormalizeUser(raw User) User {
	u := User{
		ID:        raw.ID,
		Name:      strings.TrimSpace(raw.Name),
		Email:     strings.ToLower(strings.TrimSpace(raw.Email)),
		Role:      strings.TrimSpace(raw.Role),
		CreatedAt: time.Now(),
	}
	if u.ID == "" {
		u.ID = "usr-generated"
	}
	if u.Role == "" {
		u.Role = "member"
	}
	for _, tag := range raw.Tags {
		if t := strings.TrimSpace(tag); t != "" {
			u.Tags = append(u.Tags, t)
		}
	}
	return u
}
```

### The runnable demo

The demo normalizes a messy raw user and prints the stable fields (not the
timestamp, which changes every run).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/usernorm"
)

func main() {
	u := usernorm.NormalizeUser(usernorm.User{
		Name:  "  Alice  ",
		Email: "  ALICE@Example.COM ",
		Tags:  []string{" go ", "", " api "},
	})
	fmt.Printf("name=%q email=%q role=%q tags=%v\n", u.Name, u.Email, u.Role, u.Tags)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name="Alice" email="alice@example.com" role="member" tags=[go api]
```

### The tests

One `[]cmp.Option` is built once and passed to every `cmp.Diff` in the table. The
`empty_tags` row proves `EquateEmpty` treats the normalized nil tag list as equal
to an expected `[]string{}`; the `unsorted_tags` row proves `SortSlices` makes tag
order irrelevant. `TestReflectDeepEqualInadequate` is the contrast: it shows
`reflect.DeepEqual` reporting two users unequal solely because one has a nil and
the other an empty tag slice, while `cmp.Diff` with the options equates them.

Create `user_test.go`:

```go
package usernorm

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func opts() []cmp.Option {
	return []cmp.Option{
		cmpopts.IgnoreFields(User{}, "CreatedAt", "ID"),
		cmpopts.EquateEmpty(),
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	}
}

func TestNormalizeUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   User
		want User
	}{
		{
			name: "trims_and_lowercases",
			in:   User{Name: "  Alice  ", Email: " ALICE@Example.COM ", Role: "admin", Tags: []string{" go ", " api "}},
			want: User{Name: "Alice", Email: "alice@example.com", Role: "admin", Tags: []string{"go", "api"}},
		},
		{
			name: "defaults_role",
			in:   User{Name: "Bob", Email: "bob@example.com"},
			want: User{Name: "Bob", Email: "bob@example.com", Role: "member"},
		},
		{
			name: "unsorted_tags",
			in:   User{Name: "Carol", Email: "c@example.com", Tags: []string{"zeta", "alpha"}},
			want: User{Name: "Carol", Email: "c@example.com", Role: "member", Tags: []string{"alpha", "zeta"}},
		},
		{
			name: "empty_tags",
			in:   User{Name: "Dave", Email: "d@example.com", Tags: []string{"  ", ""}},
			want: User{Name: "Dave", Email: "d@example.com", Role: "member", Tags: []string{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeUser(tc.in)
			if diff := cmp.Diff(tc.want, got, opts()...); diff != "" {
				t.Fatalf("NormalizeUser(%+v) mismatch (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

func TestReflectDeepEqualInadequate(t *testing.T) {
	t.Parallel()
	a := User{Name: "x", Tags: nil}
	b := User{Name: "x", Tags: []string{}}

	// reflect.DeepEqual wrongly distinguishes nil from empty slice.
	if reflect.DeepEqual(a, b) {
		t.Fatal("reflect.DeepEqual unexpectedly equated nil and empty slice")
	}
	// go-cmp with EquateEmpty treats them as equal, which is what the domain means.
	if diff := cmp.Diff(a, b, opts()...); diff != "" {
		t.Fatalf("cmp.Diff should equate nil and empty slice, got:\n%s", diff)
	}
}
```

## Review

The transform is correct when its output matches the expected canonical user under
the option set, and the shared `opts()` is what makes the table readable: every row
is compared the same way, and a mismatch prints a field-named diff instead of a
bare `false`. The `empty_tags` and `unsorted_tags` rows are not filler — they are
the two cases that `reflect.DeepEqual` gets wrong, and `TestReflectDeepEqualInadequate`
makes that failure explicit so the reason for reaching past the stdlib is on the
record.

Build the option list once. Reconstructing options per row is harmless but noisy;
the point is that the comparison policy — ignore these volatile fields, equate
nil and empty, sort these slices — is a single decision applied uniformly. When a
new volatile field appears (a `Version`, an `UpdatedAt`), it goes in the
`IgnoreFields` list once and every row inherits it.

## Resources

- [google/go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp) — `cmp.Diff` and the empty-diff-means-equal contract.
- [cmpopts](https://pkg.go.dev/github.com/google/go-cmp/cmp/cmpopts) — `IgnoreFields`, `EquateEmpty`, `SortSlices`, `EquateApprox`.
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual) — the stdlib comparison and its nil-vs-empty behavior.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-table-plus-fuzz.md](10-table-plus-fuzz.md)
