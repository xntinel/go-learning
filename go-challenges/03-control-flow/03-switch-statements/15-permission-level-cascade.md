# Exercise 15: Cascade Role Permissions With a Genuine fallthrough

**Nivel: Intermedio** — validacion rapida (un test corto).

Every one of this lesson's earlier warnings about `fallthrough` was about
misusing it to fake shared behavior between unrelated cases. This module is
the counter-example: a strict role hierarchy — owner outranks admin outranks
editor outranks viewer — where each higher role should hold every permission
the roles below it hold, which is precisely what `fallthrough` is for.

## What you'll build

```text
permission/                  independent module: example.com/permission-level-cascade
  go.mod                      go 1.24
  permission.go                package permission; ErrUnknownRole; PermissionsFor(role) ([]string, error)
  permission_test.go           table over each role's cumulative permission set, normalization, and an unknown role
```

- Implement: `PermissionsFor(role string) ([]string, error)` — normalize the role, then an expression switch ordered highest-to-lowest that uses `fallthrough` to accumulate permissions down the hierarchy.
- Test: a table covering each role's expected cumulative set, one case proving normalization, and an unknown role checked with `errors.Is`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/15-permission-level-cascade
cd go-solutions/03-control-flow/03-switch-statements/15-permission-level-cascade
go mod edit -go=1.24
```

### Why this is the correct use of fallthrough

Go's switch jumps straight to the matching case — it does not walk the cases
from the top. When `role` is `"admin"`, execution starts at `case "admin":`
directly; the `"owner"` case above it never runs. That single fact is what
makes cascading fallthrough safe here: entering at `"admin"` appends
`users:manage`, then `fallthrough` unconditionally continues into
`"editor"`'s body (`content:write`), then into `"viewer"`'s body
(`content:read`) — exactly the permissions an admin should have, and not one
more. This is different from the concepts file's warning about using
`fallthrough` to "share a body between two similar cases": there the two
cases are unrelated values that happen to want the same code to run once.
Here each case's body is genuinely meant to run *in addition to* every case
below it, for every role at or above that level — an accumulating cascade,
not a shortcut around duplication.

Create `permission.go`:

```go
package permission

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownRole marks a role that isn't part of the closed role hierarchy.
var ErrUnknownRole = errors.New("permission: unknown role")

// PermissionsFor returns the permission set granted to role. Roles form a
// strict hierarchy — owner includes everything admin has, admin includes
// everything editor has, and so on down to viewer — so the switch below
// deliberately falls through from the highest matched case down to the base
// case, accumulating permissions on the way.
func PermissionsFor(role string) ([]string, error) {
	role = strings.ToLower(strings.TrimSpace(role))

	var perms []string
	switch role {
	case "owner":
		perms = append(perms, "billing:manage")
		fallthrough
	case "admin":
		perms = append(perms, "users:manage")
		fallthrough
	case "editor":
		perms = append(perms, "content:write")
		fallthrough
	case "viewer":
		perms = append(perms, "content:read")
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	return perms, nil
}
```

Note that `fallthrough` is illegal in the final case (`"viewer"` has none)
and would be illegal in a type switch entirely — both restrictions the
concepts file calls out. The `default` here is unreachable from any of the
four named cases; it only runs when `role` matches none of them, so it never
sees a partially built `perms` slice.

### Test

`TestPermissionsFor` runs a table over each role's full cumulative
permission set — owner down to viewer — plus one entry with stray whitespace
and mixed case to prove normalization runs before the switch, and an unknown
role checked with `errors.Is` against `ErrUnknownRole`.

Create `permission_test.go`:

```go
package permission

import (
	"errors"
	"reflect"
	"testing"
)

func TestPermissionsFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role    string
		want    []string
		wantErr bool
	}{
		{"owner", []string{"billing:manage", "users:manage", "content:write", "content:read"}, false},
		{"admin", []string{"users:manage", "content:write", "content:read"}, false},
		{"editor", []string{"content:write", "content:read"}, false},
		{"viewer", []string{"content:read"}, false},
		{" Owner ", []string{"billing:manage", "users:manage", "content:write", "content:read"}, false},
		{"guest", nil, true},
	}

	for _, tc := range tests {
		got, err := PermissionsFor(tc.role)
		if tc.wantErr {
			if !errors.Is(err, ErrUnknownRole) {
				t.Errorf("PermissionsFor(%q) error = %v, want errors.Is match for ErrUnknownRole", tc.role, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("PermissionsFor(%q) unexpected error: %v", tc.role, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("PermissionsFor(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The cascade is correct when each role's permission set is exactly the union
of its own case and every case below it, never one level higher, and an
unrecognized role gets no permissions at all rather than defaulting to the
broadest set by accident. Carry this forward: `fallthrough` earns its place
exactly when a higher case's body is meant to run in addition to every case
beneath it — a genuine hierarchy — and stays banned everywhere else, where a
comma case list or an extracted helper says what you mean without the
unconditional jump.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the fallthrough statement and its restrictions.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the fail-closed sentinel for unknown roles.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-db-error-code-mapper.md](14-db-error-code-mapper.md) | Next: [16-circuit-breaker-state-machine.md](16-circuit-breaker-state-machine.md)
