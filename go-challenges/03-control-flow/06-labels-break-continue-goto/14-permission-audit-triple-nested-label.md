# Exercise 14: One label, three nesting levels

**Nivel: Intermedio** — validacion rapida (un test corto).

An access-control audit walks orgs, then each org's users, then each user's
permissions — three loops deep. Every previous exercise in this lesson jumps
across two nesting levels at most. This one proves a label can skip straight
from the innermost loop past an entire middle loop, not just the one loop it
sits directly inside.

## What you'll build

```text
audit/                       independent module: example.com/audit
  go.mod                     go 1.24
  audit.go                   Perm, User, Org, Audit
  audit_test.go              table test: skip sibling user, next org, cap abort, no violations
```

Set up the module:

```bash
mkdir -p ~/go-exercises/audit
cd ~/go-exercises/audit
go mod init example.com/audit
go mod edit -go=1.24
```

Create `audit.go`:

```go
package audit

// Perm is one permission grant on a user.
type Perm struct {
	Name      string
	Forbidden bool
}

// User is one account inside an org, with the permissions it holds.
type User struct {
	Name  string
	Perms []Perm
}

// Org groups the users audited together.
type Org struct {
	Name  string
	Users []User
}

// Audit walks orgs, then each org's users, then each user's perms — three
// levels deep. The moment a forbidden perm is found on a user, that user is
// flagged and the audit moves straight to the NEXT ORG (labeled continue):
// the org already has one confirmed offender, so its remaining users are
// not worth checking, skipping BOTH the rest of this user's perms and the
// rest of this org's users in one statement. Once maxFlags offenders have
// been found across all orgs, the entire audit stops (labeled break),
// skipping every remaining org and user outright.
func Audit(orgs []Org, maxFlags int) (flagged []string, aborted bool) {
orgs:
	for _, org := range orgs {
		for _, user := range org.Users {
			for _, perm := range user.Perms {
				if !perm.Forbidden {
					continue
				}
				flagged = append(flagged, org.Name+"/"+user.Name)
				if len(flagged) >= maxFlags {
					aborted = true
					break orgs
				}
				continue orgs
			}
		}
	}
	return flagged, aborted
}
```

### Why the label reaches past a whole loop, not just one

`continue orgs` and `break orgs` both fire from inside the PERMS loop, which is
two levels below the loop they name. A bare `continue` there would advance to
the next permission of the same user; even a `continue` labeled on the USERS
loop would only advance to the next user of the same org. Naming the `orgs`
label from the perms loop skips both remaining levels at once: the rest of
this user's perms, and the rest of this org's users. That is the concept this
exercise adds beyond the two-level examples earlier in the lesson — a label's
reach is not bounded by how many loops sit between it and the statement that
targets it.

Create `audit_test.go`:

```go
package audit

import "testing"

func perm(name string, forbidden bool) Perm { return Perm{Name: name, Forbidden: forbidden} }

func TestAudit(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		orgs        []Org
		maxFlags    int
		wantFlagged []string
		wantAborted bool
	}{
		"no forbidden perms anywhere": {
			orgs: []Org{
				{Name: "acme", Users: []User{{Name: "alice", Perms: []Perm{perm("read", false)}}}},
			},
			maxFlags:    10,
			wantFlagged: nil,
		},
		"second user of a flagged org is never checked": {
			orgs: []Org{
				{
					Name: "acme",
					Users: []User{
						{Name: "alice", Perms: []Perm{perm("read", false), perm("admin", true)}},
						{Name: "bob", Perms: []Perm{perm("admin", true)}},
					},
				},
			},
			maxFlags:    10,
			wantFlagged: []string{"acme/alice"},
		},
		"a violation in one org still lets the next org be audited": {
			orgs: []Org{
				{Name: "acme", Users: []User{{Name: "alice", Perms: []Perm{perm("admin", true)}}}},
				{Name: "globex", Users: []User{{Name: "carol", Perms: []Perm{perm("admin", true)}}}},
			},
			maxFlags:    10,
			wantFlagged: []string{"acme/alice", "globex/carol"},
		},
		"reaching maxFlags aborts before the next org is touched": {
			orgs: []Org{
				{Name: "acme", Users: []User{{Name: "alice", Perms: []Perm{perm("admin", true)}}}},
				{Name: "globex", Users: []User{{Name: "carol", Perms: []Perm{perm("admin", true)}}}},
			},
			maxFlags:    1,
			wantFlagged: []string{"acme/alice"},
			wantAborted: true,
		},
		"second forbidden perm on the same user does not double-flag": {
			orgs: []Org{
				{Name: "acme", Users: []User{{Name: "alice", Perms: []Perm{perm("admin", true), perm("root", true)}}}},
			},
			maxFlags:    10,
			wantFlagged: []string{"acme/alice"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			flagged, aborted := Audit(tc.orgs, tc.maxFlags)
			if len(flagged) != len(tc.wantFlagged) {
				t.Fatalf("flagged = %v, want %v", flagged, tc.wantFlagged)
			}
			for i := range flagged {
				if flagged[i] != tc.wantFlagged[i] {
					t.Fatalf("flagged[%d] = %q, want %q", i, flagged[i], tc.wantFlagged[i])
				}
			}
			if aborted != tc.wantAborted {
				t.Fatalf("aborted = %v, want %v", aborted, tc.wantAborted)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The audit is correct when each org contributes at most one flagged user — the
first forbidden perm found — and a second user in the same org is never even
inspected once that happens. The "second user is never checked" test is the
proof: `bob`'s forbidden `admin` perm would also flag him if the code ever
reached his perms loop, and the test's flagged slice having exactly one entry
confirms it did not. `maxFlags` reaching its cap is the other exit: `break
orgs` from the same nesting depth stops the audit outright, so a violation in
a later org never gets the chance to add a second entry.

## Resources

- [Go Specification: Labeled statements](https://go.dev/ref/spec#Labeled_statements) — a label's scope is the entire function body.
- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-log-severity-scan-labeled-break.md](13-log-severity-scan-labeled-break.md) | Next: [15-binary-protocol-frame-parser.md](15-binary-protocol-frame-parser.md)
