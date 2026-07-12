# Exercise 14: Backfill a Missing Audit Reason from a Defer

Every access decision needs a reason for the audit log — "why was this allowed or
denied" — but a resolver with several branches makes it easy for one branch to
forget to set one. A deferred closure acts as a safety net: if the call is ending
without error and no branch set a reason, it fills in a default so the audit trail
is never blank.

**Nivel: Intermedio** — validacion rapida (un test corto).

## What you'll build

```text
accessdecision/               independent module: example.com/accessdecision
  go.mod
  accessdecision.go            Request; Decide (defer backfills empty reason)
  accessdecision_test.go        admin, viewer read/write, unknown role, missing role
```

- Files: `accessdecision.go`, `accessdecision_test.go`.
- Implement: `Decide(req Request) (allowed bool, reason string, err error)` where a deliberately incomplete branch forgets to set `reason`, and a deferred closure backfills a default whenever the call ends without error and `reason` is still empty.
- Test: admin and viewer branches carry explicit reasons; an unrecognized role gets the backfilled default; a request with a missing role errors and leaves `reason` empty.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The guard only fires when it should

The deferred closure checks both `err` and `reason`, not just one of them:

```go
defer func() {
    if err == nil && reason == "" {
        reason = "no reason recorded for decision"
    }
}()
```

An error path leaves `reason` empty on purpose — there is nothing to log a reason
for when the request itself was invalid — so the guard is careful not to fire
there. It only patches the case where a decision was made but the branch that made
it forgot to explain itself.

Create `accessdecision.go`:

```go
package accessdecision

import "fmt"

// Request describes who is asking to do what.
type Request struct {
	Role   string
	Action string
}

// Decide resolves an access request. Every audit log line needs a reason, but
// a decision has several branches and it is easy for one of them to return
// without setting one. The deferred closure is a safety net keyed on two
// named results at once: if the call is ending without error and no branch
// set a reason, it backfills a default so the audit trail is never blank.
// This only works because reason is a named result the defer can inspect
// after the branch has already run.
func Decide(req Request) (allowed bool, reason string, err error) {
	defer func() {
		if err == nil && reason == "" {
			reason = "no reason recorded for decision"
		}
	}()

	switch req.Role {
	case "":
		err = fmt.Errorf("missing role")
		return
	case "admin":
		allowed = true
		reason = "admin role grants all actions"
		return
	case "viewer":
		if req.Action == "read" {
			allowed = true
			reason = "viewer role permits read"
			return
		}
		allowed = false
		reason = "viewer role permits read only"
		return
	default:
		// This branch forgets to set reason — the deferred guard above
		// backfills it rather than letting an unlabeled decision through.
		allowed = false
		return
	}
}
```

### Tests

Create `accessdecision_test.go`:

```go
package accessdecision

import "testing"

func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		req         Request
		wantAllowed bool
		wantReason  string
		wantErr     bool
	}{
		{
			name: "admin is allowed with explicit reason", req: Request{Role: "admin", Action: "delete"},
			wantAllowed: true, wantReason: "admin role grants all actions",
		},
		{
			name: "viewer read is allowed with explicit reason", req: Request{Role: "viewer", Action: "read"},
			wantAllowed: true, wantReason: "viewer role permits read",
		},
		{
			name: "viewer write is denied with explicit reason", req: Request{Role: "viewer", Action: "write"},
			wantAllowed: false, wantReason: "viewer role permits read only",
		},
		{
			name: "unknown role denied with backfilled reason", req: Request{Role: "guest", Action: "read"},
			wantAllowed: false, wantReason: "no reason recorded for decision",
		},
		{
			name: "missing role errors and reason stays empty", req: Request{Role: "", Action: "read"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			allowed, reason, err := Decide(tt.req)

			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if reason != "" {
					t.Errorf("reason = %q, want empty on error", reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed != tt.wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

This exercise applies the defer-reads-named-results idiom to a field that carries
no error information at all — `reason` exists purely for the audit log, and the
guard's job is enforcing that it is never silently blank on a successful decision.
The default branch in `Decide` is written the way a real omission looks: it
compiles, it returns the right `allowed` value, and only the backfilled reason
in the test output reveals that nobody explained the denial. The mistake to avoid
is having the guard also run on the error path, which would fabricate a reason
for a request that was never actually evaluated.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Go Spec: Switch statements](https://go.dev/ref/spec#Switch_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-cache-fallback-consistent-results.md](13-cache-fallback-consistent-results.md) | Next: [15-resumable-scanner-position-cursor.md](15-resumable-scanner-position-cursor.md)
