# Exercise 13: Webhook Authenticator With Mutually Exclusive Auth Modes

**Nivel: Intermedio** — validacion rapida (un test corto).

An outbound webhook signer authenticates one way at a time: basic auth or a
bearer token, never both. Ordinary last-writer-wins options only replace the
one field they touch, which is not enough here — switching modes must also
erase the other mode's credentials. This module proves both directions of
the switch clean up after themselves.

## What you'll build

```text
webhookauth/                independent module: example.com/webhookauth
  go.mod                     go 1.24
  webhookauth.go              AuthMode, Authenticator, Option, New, WithBasicAuth,
                              WithBearerToken
  webhookauth_test.go          tests for both switch directions and rejected input
```

- Implement `New(opts ...Option) (*Authenticator, error)` where each auth
  option sets its own mode's fields and clears the other mode's fields, plus
  a "no auth configured" invariant.
- Test that `WithBearerToken` after `WithBasicAuth` clears the basic
  credentials, the reverse clears the token, no auth option is an error, and
  empty credentials are rejected.

Set up the module:

```bash
mkdir -p ~/go-exercises/webhookauth
cd ~/go-exercises/webhookauth
go mod init example.com/webhookauth
go mod edit -go=1.24
```

An earlier module in this lesson showed the *same* field being overwritten by
a later call. Here the rule is stronger: `WithBasicAuth` and `WithBearerToken`
each own a whole group of fields, and applying one after the other must leave
exactly one group populated and the other zeroed — otherwise a caller built
with basic auth then switched to a bearer token would carry a stale password
`Mode()` says is not in use. `New` also rejects the case where no auth option
was ever applied, since no single option can see that the others were never
called.

Create `webhookauth.go`:

```go
package webhookauth

import "fmt"

type AuthMode int

const (
	NoAuth AuthMode = iota
	BasicAuth
	BearerAuth
)

type Authenticator struct {
	mode        AuthMode
	basicUser   string
	basicPass   string
	bearerToken string
}

type Option func(*Authenticator) error

// New applies opts in order, then checks that at least one auth mode was
// actually configured.
func New(opts ...Option) (*Authenticator, error) {
	a := &Authenticator{}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}
	if a.mode == NoAuth {
		return nil, fmt.Errorf("no authentication configured: call WithBasicAuth or WithBearerToken")
	}
	return a, nil
}

// WithBasicAuth switches to HTTP basic auth, clearing any bearer token a
// previous option set so only one mode is ever active.
func WithBasicAuth(user, pass string) Option {
	return func(a *Authenticator) error {
		if user == "" || pass == "" {
			return fmt.Errorf("basic auth requires a non-empty user and password")
		}
		a.mode = BasicAuth
		a.basicUser = user
		a.basicPass = pass
		a.bearerToken = ""
		return nil
	}
}

// WithBearerToken switches to bearer-token auth, clearing any basic-auth
// credentials a previous option set.
func WithBearerToken(token string) Option {
	return func(a *Authenticator) error {
		if token == "" {
			return fmt.Errorf("bearer token must not be empty")
		}
		a.mode = BearerAuth
		a.bearerToken = token
		a.basicUser = ""
		a.basicPass = ""
		return nil
	}
}

func (a *Authenticator) Mode() AuthMode      { return a.mode }
func (a *Authenticator) BasicUser() string   { return a.basicUser }
func (a *Authenticator) BasicPass() string   { return a.basicPass }
func (a *Authenticator) BearerToken() string { return a.bearerToken }
```

Create `webhookauth_test.go`:

```go
package webhookauth

import "testing"

func TestNoAuthConfiguredIsRejected(t *testing.T) {
	t.Parallel()

	if _, err := New(); err == nil {
		t.Fatal("expected error when no auth option is applied, got nil")
	}
}

func TestBearerAfterBasicClearsBasicCredentials(t *testing.T) {
	t.Parallel()

	a, err := New(WithBasicAuth("alice", "s3cret"), WithBearerToken("tok_abc123"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Mode() != BearerAuth {
		t.Fatalf("Mode() = %v, want BearerAuth", a.Mode())
	}
	if a.BasicUser() != "" || a.BasicPass() != "" {
		t.Errorf("basic credentials survived: user=%q pass=%q, want both empty", a.BasicUser(), a.BasicPass())
	}
}

func TestBasicAfterBearerClearsBearerToken(t *testing.T) {
	t.Parallel()

	a, err := New(WithBearerToken("tok_abc123"), WithBasicAuth("alice", "s3cret"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Mode() != BasicAuth {
		t.Fatalf("Mode() = %v, want BasicAuth", a.Mode())
	}
	if a.BearerToken() != "" {
		t.Errorf("BearerToken() = %q, want empty: switching modes must clear it", a.BearerToken())
	}
}

func TestInvalidCredentialsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  Option
	}{
		{"empty basic user", WithBasicAuth("", "pass")},
		{"empty basic password", WithBasicAuth("alice", "")},
		{"empty bearer token", WithBearerToken("")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := New(tt.opt); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The authenticator is correct when exactly one mode's credentials are ever
observable, no matter which order the two options were called in. Both
"clears" tests fail if either option forgets to zero the other mode's
fields — the real risk in a mutually exclusive design, since it is easy to
remember to set your own fields and easy to forget to clear someone else's.
The absence check closes the remaining gap: a fresh `Authenticator` must not
silently pass as "authenticated with nothing."

## Resources

- [RFC 7617: The 'Basic' HTTP Authentication Scheme](https://datatracker.ietf.org/doc/html/rfc7617)
- [RFC 6750: OAuth 2.0 Bearer Token Usage](https://datatracker.ietf.org/doc/html/rfc6750)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-notification-preferences-field-errors.md](12-notification-preferences-field-errors.md) | Next: [14-paginated-lister-options.md](14-paginated-lister-options.md)
