# Exercise 18: Message Schema Version Negotiation: Compatibility Check and Fallback

**Nivel: Intermedio** — validacion rapida (un test corto).

A consumer reading from a shared topic will eventually see a message
produced by a newer (or older) version of the producer than the one it was
built against. Silently trying to parse a schema it does not understand
risks corrupting downstream state; the safer default is to check the
message's declared version against what the consumer supports and route
anything it cannot handle to a dead-letter queue instead of guessing. This
module is fully self-contained: its own `go mod init`, all code inline, its
own test file.

## What you'll build

```text
schemaver/                  independent module: example.com/message-schema-version-negotiation
  go.mod                    go 1.24
  route.go                  Route(headers, supported) (destination string, reason error)
  route_test.go             table: missing header, blank header, supported, unsupported
```

- Files: `route.go`, `route_test.go`.
- Implement: `Route(headers map[string]string, supported map[string]bool) (destination string, reason error)`, where the comma-ok header lookup `if v, ok := headers["Schema-Version"]; !ok` distinguishes a header that was never sent from one sent blank, and a second comma-ok check against `supported` decides the destination.
- Test: a table over a missing header, a header sent blank, a supported version, and an unsupported version, asserting both the destination and the reason with `errors.Is`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the header lookup needs comma-ok, not a default-value read

A plain map read (`headers["Schema-Version"]`) returns the zero value —
an empty string — whether the header key was never set or was set to
`""` explicitly. Those are different failure modes worth distinguishing in
a dead-letter log: a message with no version header at all suggests an old
producer that predates the versioning scheme, while a message with an
explicitly blank version suggests a producer bug. The comma-ok form
`v, ok := headers["Schema-Version"]` keeps that distinction available to
the guard, which is exactly the same idiom the lesson's concepts file
covers for HTTP headers, applied here to a message envelope instead.

Create `route.go`:

```go
// Package schemaver decides, from a message's declared schema version
// header, whether the consumer should process it or route it to a
// dead-letter queue.
package schemaver

import (
	"errors"
	"strings"
)

// ErrMissingVersion means the message carried no usable Schema-Version
// header (either absent or blank).
var ErrMissingVersion = errors.New("missing schema version header")

// ErrUnsupportedVersion means the message declared a version the consumer
// does not know how to parse.
var ErrUnsupportedVersion = errors.New("unsupported schema version")

const (
	DestinationProcessor  = "processor"
	DestinationDeadLetter = "dead-letter"
)

// Route decides where a message should go based on headers["Schema-Version"]
// and the set of versions this consumer supports. reason is nil when
// destination is DestinationProcessor, and one of ErrMissingVersion or
// ErrUnsupportedVersion when destination is DestinationDeadLetter.
func Route(headers map[string]string, supported map[string]bool) (destination string, reason error) {
	v, ok := headers["Schema-Version"]
	if !ok || strings.TrimSpace(v) == "" {
		return DestinationDeadLetter, ErrMissingVersion
	}

	if _, ok := supported[v]; ok {
		return DestinationProcessor, nil
	}

	return DestinationDeadLetter, ErrUnsupportedVersion
}
```

### Tests

The table checks the missing-header and blank-header cases separately even
though both currently map to `ErrMissingVersion`, because they exercise
different branches of the comma-ok guard, plus a supported and an
unsupported version.

Create `route_test.go`:

```go
package schemaver

import (
	"errors"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	supported := map[string]bool{"v1": true, "v2": true}

	tests := []struct {
		name     string
		headers  map[string]string
		wantDest string
		wantErr  error
	}{
		{
			name:     "missing header goes to dead-letter",
			headers:  map[string]string{},
			wantDest: DestinationDeadLetter,
			wantErr:  ErrMissingVersion,
		},
		{
			name:     "blank header goes to dead-letter",
			headers:  map[string]string{"Schema-Version": "  "},
			wantDest: DestinationDeadLetter,
			wantErr:  ErrMissingVersion,
		},
		{
			name:     "supported version goes to the processor",
			headers:  map[string]string{"Schema-Version": "v2"},
			wantDest: DestinationProcessor,
			wantErr:  nil,
		},
		{
			name:     "unsupported version goes to dead-letter",
			headers:  map[string]string{"Schema-Version": "v99"},
			wantDest: DestinationDeadLetter,
			wantErr:  ErrUnsupportedVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dest, err := Route(tc.headers, supported)
			if dest != tc.wantDest {
				t.Errorf("destination = %q, want %q", dest, tc.wantDest)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("reason = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("reason = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`Route` returns a destination and a typed reason rather than a bare bool,
which is what lets the dead-letter consumer log *why* a message was
rejected instead of just that it was — the difference between an old
producer and a broken one is exactly the kind of thing an on-call engineer
needs when triaging a growing dead-letter queue. Carry this forward: any
router that can reject for more than one reason should return a typed
reason alongside the destination, not just a boolean.

## Resources

- [Confluent: Schema Evolution and Compatibility](https://docs.confluent.io/platform/current/schema-registry/fundamentals/schema-evolution.html) — the production version of this compatibility problem.
- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the two-result map index this router relies on twice.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-batch-window-flush-decision.md](17-batch-window-flush-decision.md) | Next: [19-dns-lookup-cache-stale-fallback.md](19-dns-lookup-cache-stale-fallback.md)
