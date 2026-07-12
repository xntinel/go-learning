# Exercise 5: Redacting PII and secrets from logs with ReplaceAttr

Error logs carry the request that failed, and failed requests carry passwords,
tokens, and PII. A single unredacted line in a log backend is a breach. This
module builds a `ReplaceAttr` function that redacts sensitive keys to a `REDACTED`
marker at every nesting depth — centralized, auditable, and depth-aware via the
`groups` path — so no call site has to remember to scrub.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
redactlog/                   independent module: example.com/redactlog
  go.mod                     go 1.25
  redact.go                  RedactAttr(groups, a) redacting sensitive keys and any key under a sensitive group
  cmd/
    demo/
      main.go                runnable demo: a request payload logged with secrets scrubbed
  redact_test.go             sensitive keys REDACTED at top level and nested; groups path used for depth
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: a `RedactAttr(groups []string, a slog.Attr) slog.Attr` that redacts known sensitive keys (`password`, `token`, `authorization`, `email`) anywhere, and redacts *every* key nested under a group flagged sensitive (e.g. `credentials`), using the `groups` path.
- Test: log a struct/group containing `password` and `email`; assert those show `REDACTED` and non-sensitive keys show their real value; include a nested group to prove the `groups` path drives depth-aware matching.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### How ReplaceAttr sees every attribute, at every depth

`slog.HandlerOptions.ReplaceAttr` is a hook the built-in handlers call for every
attribute before writing it — including attributes *inside groups*, where it is
called with `groups` set to the slice of enclosing group names. That is the whole
mechanism that makes centralized redaction possible: you do not have to walk the
payload yourself; the handler walks it and hands you each leaf attribute with its
path. Return a replacement `slog.Attr` (here, the same key with value
`"REDACTED"`) and that is what gets written; return the attr unchanged to keep it;
return the zero `slog.Attr{}` to drop it entirely.

This redactor matches on two axes, which is why it takes `groups`:

- By key: any attribute whose (lowercased) key is in a sensitive set —
  `password`, `token`, `authorization`, `email` — is redacted no matter where it
  sits. This catches a top-level `password` and a `user.email` nested three
  groups deep alike, because `ReplaceAttr` is called at every depth.
- By group: any attribute *nested under* a group named as sensitive (e.g. a
  `credentials` group) is redacted regardless of its own key, because a group you
  have decided is secret should not leak any of its members. This axis is exactly
  what the `groups []string` parameter is for — without it you could only match on
  the leaf key and could not say "everything under credentials is secret."

Two correctness notes. First, the built-in attributes `time`, `level`, `msg` are
passed to `ReplaceAttr` too (with empty `groups`); a redactor must not clobber
them, which this one does not because their keys are not sensitive. Second,
redaction happens at *write* time in the handler, so the in-memory value the
program holds is untouched — you are scrubbing the log rendering, not the data.

Create `redact.go`:

```go
package redactlog

import (
	"log/slog"
	"strings"
)

// Redacted is the marker written in place of a sensitive value.
const Redacted = "REDACTED"

// sensitiveKeys are redacted wherever they appear, at any nesting depth.
var sensitiveKeys = map[string]bool{
	"password":      true,
	"token":         true,
	"authorization": true,
	"email":         true,
}

// sensitiveGroups: every attribute nested under one of these groups is redacted,
// whatever its own key. This is why RedactAttr needs the groups path.
var sensitiveGroups = map[string]bool{
	"credentials": true,
}

// RedactAttr is a slog HandlerOptions.ReplaceAttr hook. It redacts sensitive keys
// anywhere and any attribute nested under a sensitive group.
func RedactAttr(groups []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, Redacted)
	}
	for _, g := range groups {
		if sensitiveGroups[strings.ToLower(g)] {
			return slog.String(a.Key, Redacted)
		}
	}
	return a
}
```

### The runnable demo

The demo logs a realistic failed-login error whose payload holds an email, a
password, and a nested `credentials` group — the shape that leaks secrets in the
wild — and shows every sensitive field scrubbed while the safe `user_id` and
`attempt` survive. Time is suppressed for determinism.

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/redactlog"
)

func main() {
	opts := &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return redactlog.RedactAttr(groups, a)
	}}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, opts))

	logger.Error("login failed",
		"err", "invalid credentials",
		slog.Group("request",
			slog.String("user_id", "u-42"),
			slog.String("email", "alice@example.com"),
			slog.String("password", "hunter2"),
			slog.Int("attempt", 3),
		),
		slog.Group("credentials",
			slog.String("api_key", "sk-live-abc123"),
		),
	)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"ERROR","msg":"login failed","err":"invalid credentials","request":{"user_id":"u-42","email":"REDACTED","password":"REDACTED","attempt":3},"credentials":{"api_key":"REDACTED"}}
```

### Tests

The tests unmarshal the JSON and assert both that sensitive fields are redacted
and that non-sensitive ones survive — a redactor that scrubs everything is as
useless as one that scrubs nothing. `TestNestedRedaction` puts `email`/`password`
inside a group to prove `ReplaceAttr` fires at depth; `TestSensitiveGroup` proves
the `groups` path is used by redacting a non-sensitive *key* purely because it
sits under the `credentials` group.

Create `redact_test.go`:

```go
package redactlog

import (
	"bytes"
	"encoding/json"
	"testing"

	"log/slog"
)

func logJSON(t *testing.T, args ...any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{ReplaceAttr: RedactAttr}
	logger := slog.New(slog.NewJSONHandler(&buf, opts))
	logger.Error("msg", args...)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", buf.String(), err)
	}
	return m
}

func TestTopLevelRedaction(t *testing.T) {
	t.Parallel()
	m := logJSON(t, "password", "hunter2", "user_id", "u-1")
	if m["password"] != Redacted {
		t.Fatalf("password = %v, want %s", m["password"], Redacted)
	}
	if m["user_id"] != "u-1" {
		t.Fatalf("user_id = %v, want u-1 (non-sensitive must survive)", m["user_id"])
	}
}

func TestNestedRedaction(t *testing.T) {
	t.Parallel()
	m := logJSON(t, slog.Group("request",
		slog.String("email", "a@b.com"),
		slog.String("name", "alice"),
	))
	req, ok := m["request"].(map[string]any)
	if !ok {
		t.Fatalf("request is %T, want object", m["request"])
	}
	if req["email"] != Redacted {
		t.Fatalf("request.email = %v, want %s (nested key must be redacted)", req["email"], Redacted)
	}
	if req["name"] != "alice" {
		t.Fatalf("request.name = %v, want alice", req["name"])
	}
}

func TestSensitiveGroup(t *testing.T) {
	t.Parallel()
	// api_key is not in sensitiveKeys; it is redacted only because it is under
	// the credentials group, which proves the groups path is consulted.
	m := logJSON(t, slog.Group("credentials", slog.String("api_key", "sk-live-1")))
	creds, ok := m["credentials"].(map[string]any)
	if !ok {
		t.Fatalf("credentials is %T, want object", m["credentials"])
	}
	if creds["api_key"] != Redacted {
		t.Fatalf("credentials.api_key = %v, want %s (redacted by group path)", creds["api_key"], Redacted)
	}
}

func TestBuiltinsUntouched(t *testing.T) {
	t.Parallel()
	m := logJSON(t, "user_id", "u-1")
	if m["msg"] != "msg" || m["level"] != "ERROR" {
		t.Fatalf("built-in attrs clobbered: level=%v msg=%v", m["level"], m["msg"])
	}
}
```

## Review

The redactor is correct when it scrubs the sensitive and preserves the rest, at
every depth. `TestTopLevelRedaction` and `TestNestedRedaction` prove both axes of
"keep the useful, drop the secret"; `TestSensitiveGroup` is the one that proves
the `groups []string` parameter is actually load-bearing — `api_key` is redacted
solely because of its group path, not its key. `TestBuiltinsUntouched` guards the
subtle failure mode where a too-eager redactor mangles `time`/`level`/`msg`.

The mistake this exercise prevents is trusting each call site to scrub. There are
hundreds of log call sites and one will forget; a central `ReplaceAttr` is the
one place to audit and the one place to update when a new sensitive field appears.
The limitation to keep honest: this matches by key and group, so a secret logged
under an innocuously-named key (`data`, `blob`) still slips through — redaction is
a safety net, not a substitute for not logging whole opaque payloads in the first
place.

## Resources

- [`slog.HandlerOptions`](https://pkg.go.dev/log/slog#HandlerOptions) — the `ReplaceAttr` signature and when it is called (including inside groups).
- [`slog.Attr` and `slog.Value`](https://pkg.go.dev/log/slog#Attr) — building the replacement attr and inspecting `Value.Kind`.
- [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) — what must never reach a log and why central redaction is required.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-panic-recovery-observability.md](06-panic-recovery-observability.md)
