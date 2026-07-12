# Exercise 4: slog.LogValuer — A Credential Type That Cannot Leak In Logs

The worst production incident a `String()` method causes is not a wrong string —
it is a secret printed in cleartext to a log aggregator that a hundred people can
read. This module builds a credential type that redacts on every path:
`slog.LogValuer` for structured logs, and `String()` for `%s`/`%v`. It also shows
the complementary pattern — expanding a value into safe sub-fields with
`slog.GroupValue`.

Self-contained module: own `go mod init`, code, demo, and tests.

## What you'll build

```text
redact/                     independent module: example.com/redact
  go.mod
  redact.go                 type APIKey string; LogValue()+String() redact; Credential GroupValue
  cmd/
    demo/
      main.go               slog a struct containing an APIKey and a Credential
  redact_test.go            capture slog JSON; assert secret absent, REDACTED present; group fields
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: an `APIKey string` whose `LogValue()` returns `slog.StringValue("REDACTED")` and whose `String()` also redacts; a `Credential` whose `LogValue()` returns a `slog.GroupValue` of safe fields (user id, key prefix) without the full key.
- Test: capture `slog.NewJSONHandler` output into a `bytes.Buffer` and assert the raw secret never appears while `REDACTED` does; `%v`/`%s` are redacted; the group emits `key_prefix` but not the full key.
- Verify: `go test -count=1 -race ./...`

### Why String() alone is not enough

A common half-fix is to give a secret type a `String()` that returns `"REDACTED"`
and assume it is safe. It is not. `slog` does not format values through `fmt` by
default — a `JSONHandler` reflects over the attribute's value and, for a string
kind, writes the underlying bytes. So an `APIKey` with only `String()` still leaks
its cleartext into structured logs. The interface `slog` actually consults is
`slog.LogValuer`: `interface { LogValue() slog.Value }`. When an attribute's value
implements it, the handler calls `LogValue()` and logs *that* instead. Returning
`slog.StringValue("REDACTED")` is therefore the leak-proof redaction: there is no
handler path that reaches the secret, because the value itself answers "REDACTED".

You still implement `String()` (also redacting) to cover the non-`slog` paths:
`fmt.Sprintf("%v", key)`, error messages built with `%s`, and any code that prints
the containing struct. Belt and suspenders — the two interfaces cover the two
formatting systems (`slog` and `fmt`) a secret can travel through.

`slog` resolves `LogValuer`s recursively with loop protection, so `LogValue()` may
return another `LogValuer` and `slog` keeps resolving until it reaches a concrete
value, bounded so a self-referential value cannot hang the logger.

### The GroupValue complement

Total redaction is right for a raw secret, but observability often needs *some*
safe shape: which user, which key prefix, so an on-call engineer can correlate
without seeing the secret. `slog.GroupValue(attrs...)` expands one value into a set
of sub-attributes. The `Credential` type below logs as a group of `user_id` and
`key_prefix` (the first few characters, a common non-sensitive fingerprint) and
deliberately omits the full key. This is how you keep logs useful and safe at once.

Create `redact.go`:

```go
package redact

import "log/slog"

// APIKey is a secret credential. It redacts on every representation path:
// slog.LogValuer for structured logs, String() for fmt verbs.
type APIKey string

// LogValue is what slog logs instead of the secret. No handler path can reach
// the cleartext.
func (k APIKey) LogValue() slog.Value {
	return slog.StringValue("REDACTED")
}

// String redacts for fmt (%v, %s) and for error messages.
func (k APIKey) String() string {
	return "REDACTED"
}

// prefix returns the first n characters of the key, or the whole key if shorter.
// A short prefix is a non-sensitive fingerprint safe to log.
func (k APIKey) prefix(n int) string {
	s := string(k)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Credential ties a key to a user. It logs as a group of safe fields, never the
// full key.
type Credential struct {
	UserID string
	Key    APIKey
}

// LogValue expands the credential into safe sub-attributes: the user id and a
// short key prefix. The full key is never emitted.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user_id", c.UserID),
		slog.String("key_prefix", c.Key.prefix(4)),
	)
}
```

### The runnable demo

The demo logs a struct containing an `APIKey` and a `Credential` through a JSON
handler writing to stdout, so you can see redaction and the safe group side by
side. The output is deterministic because `slog`'s JSON handler writes fields in
attribute order (the `time` attribute is removed via `ReplaceAttr` so the demo
output is stable).

Create `cmd/demo/main.go`:

```go
package main

import (
	"log/slog"
	"os"

	"example.com/redact"
)

func main() {
	// Drop the time attribute so the output is stable for the doc.
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	log := slog.New(h)

	key := redact.APIKey("sk_live_5f3c8a91b2")
	log.Info("auth attempt", "api_key", key)

	cred := redact.Credential{UserID: "u_42", Key: key}
	log.Info("issued token", "credential", cred)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"auth attempt","api_key":"REDACTED"}
{"level":"INFO","msg":"issued token","credential":{"user_id":"u_42","key_prefix":"sk_l"}}
```

### Tests

The tests capture real handler output into a `bytes.Buffer` and make assertions on
the bytes, which is the only honest way to prove a secret does not leak.
`TestSecretNeverInLog` asserts the raw key substring is absent and `REDACTED` is
present. `TestFmtRedacts` covers the `fmt` paths. `TestGroupExposesOnlyPrefix`
asserts the group contains the prefix but not the full key.

Create `redact_test.go`:

```go
package redact

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secret = "sk_live_5f3c8a91b2"

func newLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	return slog.New(h), &buf
}

func TestSecretNeverInLog(t *testing.T) {
	t.Parallel()
	log, buf := newLogger()
	log.Info("auth", "api_key", APIKey(secret))

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked into log: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected REDACTED in log, got: %s", out)
	}
}

func TestFmtRedacts(t *testing.T) {
	t.Parallel()
	k := APIKey(secret)
	for _, got := range []string{
		fmt.Sprintf("%v", k),
		fmt.Sprintf("%s", k),
		fmt.Sprint(k),
	} {
		if got != "REDACTED" {
			t.Errorf("fmt rendered %q, want REDACTED", got)
		}
		if strings.Contains(got, secret) {
			t.Errorf("secret leaked via fmt: %q", got)
		}
	}
}

func TestGroupExposesOnlyPrefix(t *testing.T) {
	t.Parallel()
	log, buf := newLogger()
	log.Info("issued", "credential", Credential{UserID: "u_42", Key: APIKey(secret)})

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("full key leaked in group: %s", out)
	}
	if !strings.Contains(out, `"key_prefix":"sk_l"`) {
		t.Fatalf("expected key_prefix in group, got: %s", out)
	}
	if !strings.Contains(out, `"user_id":"u_42"`) {
		t.Fatalf("expected user_id in group, got: %s", out)
	}
}

func ExampleAPIKey_LogValue() {
	fmt.Println(APIKey("sk_live_secret").LogValue())
	// Output: REDACTED
}
```

## Review

Redaction is correct only when *every* representation path is closed, and the two
paths are `slog` and `fmt`. `LogValue()` closes the structured-logging path — the
one that matters most, because that is where secrets go to be indexed and
searched — and `String()` closes the `fmt` path. The buffer-capture tests are the
proof: they assert on the actual bytes a handler produced, so a regression that
reintroduces the cleartext fails loudly. The `Credential` group shows the mature
version of the pattern: you rarely want to log *nothing* about a credential, you
want to log the safe fingerprint, and `slog.GroupValue` is the tool. Never rely on
`String()` alone for a secret; a `JSONHandler` does not call it, and the cleartext
walks straight into the log.

## Resources

- [log/slog: LogValuer](https://pkg.go.dev/log/slog#LogValuer) — the interface `slog` consults, with the redaction example.
- [log/slog: Value and GroupValue](https://pkg.go.dev/log/slog#Value) — constructing redacted and grouped values.
- [slog handler guide](https://github.com/golang/example/blob/master/slog-handler-guide/README.md) — how handlers resolve `LogValuer` with loop protection.

---

Back to [03-enum-text-round-trip.md](03-enum-text-round-trip.md) | Next: [05-fmt-formatter-verbs-and-flags.md](05-fmt-formatter-verbs-and-flags.md)
