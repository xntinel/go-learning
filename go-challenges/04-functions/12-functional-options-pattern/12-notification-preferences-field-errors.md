# Exercise 12: Notification Preferences With Field-Scoped Collect-All Validation

**Nivel: Intermedio** — validacion rapida (un test corto).

A settings form is the worst place to fail fast: a user who fixes their email
only to be told next that their phone number was also wrong will not thank
you for hiding that second problem. This module builds a notification
preferences constructor where every option runs regardless of earlier
failures, and each failure is tagged with the field it belongs to.

## What you'll build

```text
notifyprefs/                independent module: example.com/notifyprefs
  go.mod                     go 1.24
  notifyprefs.go              Preferences, FieldError, ValidationError, Option, New,
                              WithEmail, WithSMS, WithSlackWebhook, WithDigestFrequency
  notifyprefs_test.go          table test plus a field-inspection test
```

- Implement `Option func(*Preferences) *FieldError` and a `New` that keeps
  applying every option, collecting failures into a `*ValidationError` with a
  `Get(field string) error` accessor.
- Test an all-valid case and a case with two invalid fields where
  `ValidationError.Get` finds each by name and a third, valid field is clean.

Set up the module:

```bash
mkdir -p ~/go-exercises/notifyprefs
cd ~/go-exercises/notifyprefs
go mod init example.com/notifyprefs
go mod edit -go=1.24
```

Every other module in this lesson uses `Option func(*T) error`. Here the
signature is `func(*Preferences) *FieldError`, because a plain `error` cannot
carry which field it belongs to — and without a field name, a settings form
cannot highlight the right input box. `New` does not return on the first
failing option: it runs every option, appends each non-nil `*FieldError` to
`ValidationError.Fields`, and only after the loop finishes checks whether
anything was collected. This is the same collect-all idea as `errors.Join`,
but each failure keeps its field name instead of being flattened into a
string.

Create `notifyprefs.go`:

```go
package notifyprefs

import (
	"fmt"
	"strings"
)

// FieldError names the field an option failed to set.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Err) }
func (e *FieldError) Unwrap() error { return e.Err }

// ValidationError aggregates every FieldError collected while building
// Preferences, so a form can show every problem at once.
type ValidationError struct {
	Fields []*FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Error()
	}
	return "invalid preferences: " + strings.Join(msgs, "; ")
}

// Get returns the error recorded for field, or nil if that field was valid.
func (e *ValidationError) Get(field string) error {
	for _, f := range e.Fields {
		if f.Field == field {
			return f.Err
		}
	}
	return nil
}

type Preferences struct {
	email     string
	sms       string
	slackHook string
	digest    string
}

// Option configures Preferences and returns a *FieldError naming the field it
// failed to set, or nil.
type Option func(*Preferences) *FieldError

// New seeds a default digest frequency and applies every option, collecting
// every failure instead of stopping at the first one.
func New(opts ...Option) (*Preferences, error) {
	p := &Preferences{digest: "daily"}
	var verr ValidationError
	for _, opt := range opts {
		if ferr := opt(p); ferr != nil {
			verr.Fields = append(verr.Fields, ferr)
		}
	}
	if len(verr.Fields) > 0 {
		return nil, &verr
	}
	return p, nil
}

func WithEmail(addr string) Option {
	return func(p *Preferences) *FieldError {
		if !strings.Contains(addr, "@") {
			return &FieldError{Field: "email", Err: fmt.Errorf("%q is not a valid address", addr)}
		}
		p.email = addr
		return nil
	}
}

func WithSMS(number string) Option {
	return func(p *Preferences) *FieldError {
		if len(number) < 7 {
			return &FieldError{Field: "sms", Err: fmt.Errorf("%q is too short to be a phone number", number)}
		}
		p.sms = number
		return nil
	}
}

func WithSlackWebhook(url string) Option {
	return func(p *Preferences) *FieldError {
		if !strings.HasPrefix(url, "https://") {
			return &FieldError{Field: "slack", Err: fmt.Errorf("%q must use https", url)}
		}
		p.slackHook = url
		return nil
	}
}

func WithDigestFrequency(freq string) Option {
	return func(p *Preferences) *FieldError {
		switch freq {
		case "daily", "weekly", "realtime":
			p.digest = freq
			return nil
		default:
			return &FieldError{Field: "digest", Err: fmt.Errorf("%q must be daily, weekly, or realtime", freq)}
		}
	}
}

func (p *Preferences) Email() string           { return p.email }
func (p *Preferences) SMS() string             { return p.sms }
func (p *Preferences) SlackWebhook() string    { return p.slackHook }
func (p *Preferences) DigestFrequency() string { return p.digest }
```

Create `notifyprefs_test.go`:

```go
package notifyprefs

import (
	"errors"
	"testing"
)

func TestNewAllValid(t *testing.T) {
	t.Parallel()

	p, err := New(
		WithEmail("ops@example.com"),
		WithSMS("+15551234567"),
		WithSlackWebhook("https://hooks.slack.com/services/xyz"),
		WithDigestFrequency("weekly"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.DigestFrequency() != "weekly" {
		t.Errorf("DigestFrequency() = %q", p.DigestFrequency())
	}
}

func TestNewCollectsEveryFieldError(t *testing.T) {
	t.Parallel()

	_, err := New(
		WithEmail("not-an-email"),
		WithSMS("+15551234567"), // valid, should not appear
		WithDigestFrequency("hourly"),
	)
	if err == nil {
		t.Fatal("expected a *ValidationError, got nil")
	}

	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error is not a *ValidationError: %v (%T)", err, err)
	}
	if len(verr.Fields) != 2 {
		t.Fatalf("Fields = %d, want 2: %v", len(verr.Fields), verr.Fields)
	}
	if verr.Get("email") == nil {
		t.Error("expected an error recorded for field \"email\"")
	}
	if verr.Get("digest") == nil {
		t.Error("expected an error recorded for field \"digest\"")
	}
	if verr.Get("sms") != nil {
		t.Errorf("field \"sms\" should be valid, got error: %v", verr.Get("sms"))
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The builder is correct when a caller with three broken fields learns about all
three in one call, not one per retry, and when `ValidationError.Get` can
answer "is this field the problem" because `FieldError` kept the field name
attached instead of losing it in a formatted string. A plain `error` return
works when the caller only needs to know *that* something failed; a richer
return type is worth the extra plumbing when the caller needs to know *what*.

## Resources

- [errors.As](https://pkg.go.dev/errors#As)
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-worker-pool-overflow-options.md](11-worker-pool-overflow-options.md) | Next: [13-mutually-exclusive-auth-options.md](13-mutually-exclusive-auth-options.md)
