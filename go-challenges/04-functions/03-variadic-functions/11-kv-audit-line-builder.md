# Exercise 11: A Structured Audit-Line Builder in the Style of slog's Key-Value API

`slog`'s core ergonomic — a message followed by a flat `...any` of alternating
keys and values — shows up in audit trails, access logs, and job traces alike.
This module rebuilds that convention as `Line(event string, kvs ...any) string`,
so you feel exactly why the `[]any` you index in pairs is a real slice with real
failure modes, not a magic varargs list.

**Nivel: Intermedio** — validacion rapida (un test corto).

This module is self-contained: its own `go mod init`, demo, and one quick test.

## What you'll build

```text
kvaudit/                  independent module: example.com/kv-audit-line
  go.mod                  go 1.24
  audit.go                package audit; func Line(event string, kvs ...any) string
  cmd/
    demo/
      main.go             runnable demo: normal, bad-key, odd-value, splat cases
  audit_test.go            table test over the four required cases, splat call
```

- Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
- Implement: `Line(event string, kvs ...any) string` building `event k1=v1 k2=v2 ...` with `strings.Builder`.
- Test: a table covering mixed-type pairs, a non-string key, an odd trailing value, and zero kvs, splatting each row's `[]any` at the call site.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/11-kv-audit-line-builder/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/11-kv-audit-line-builder
go mod edit -go=1.24
```

### `...any` is a real `[]any` you index in steps of two

`kvs ...any` looks like magic at the call site — `Line("req", "method", "GET", "count", 3)` —
but inside the function `kvs` is an ordinary `[]any` of length 5. There is no
compiler-enforced pairing between keys and values; `Line` walks that slice two
elements at a time and decides, at runtime, whether each pair is well formed.
That is exactly what `slog.Logger.Info(msg, kv...)` does internally, and why it
degrades instead of panicking on bad input: a `for i := 0; i < len(kvs); i +=
2` loop simply runs out of a clean pair and needs a fallback.

Two fallbacks, both real slog-inspired behaviors: a **non-string key** cannot
become `key=value`, so it renders as `!BADKEY=<the offending key>` — the
sentinel key is fixed, and the value shown is the thing that was supposed to
be a key, so the bug is visible instead of a field silently vanishing. An
**odd trailing value** has no partner, so it renders as `!BADVAL=<the
orphan>` instead of being dropped.

Zero `kvs` is the trivial case: `Line("startup")` returns just `"startup"`,
with no trailing space — a common off-by-one if you always prepend a space
before each pair.

Create `audit.go`:

```go
// audit.go
package audit

import (
	"fmt"
	"strings"
)

// Line builds one logfmt-ish audit line: "event k1=v1 k2=v2 ...".
//
// kvs is read two slots at a time as alternating key/value pairs, mirroring
// slog's ...any convention:
//   - a well-formed pair renders as "key=value" using fmt's %v.
//   - a non-string key renders as "!BADKEY=<key>" (the value is dropped;
//     the offending key itself is what you need to see to find the bug).
//   - an odd trailing value (no partner) renders as "!BADVAL=<value>".
//   - zero kvs returns just the event, with no trailing space.
func Line(event string, kvs ...any) string {
	var b strings.Builder
	b.WriteString(event)

	for i := 0; i < len(kvs); i += 2 {
		b.WriteByte(' ')

		if i+1 >= len(kvs) {
			fmt.Fprintf(&b, "!BADVAL=%v", kvs[i])
			break
		}

		key, ok := kvs[i].(string)
		if !ok {
			fmt.Fprintf(&b, "!BADKEY=%v", kvs[i])
			continue
		}

		fmt.Fprintf(&b, "%s=%v", key, kvs[i+1])
	}

	return b.String()
}
```

### The runnable demo

The last call shows the other half of the convention: a caller already
holding its pairs in a `[]any` splats them in with `pairs...` — no rebuilding
the call as individual arguments.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/kv-audit-line"
)

func main() {
	fmt.Println(audit.Line("user.login", "user", "alice", "attempt", 1, "ok", true))
	fmt.Println(audit.Line("user.login", 42, "ignored"))
	fmt.Println(audit.Line("user.login", "user", "alice", "orphan"))
	fmt.Println(audit.Line("startup"))

	pairs := []any{"region", "us-east-1", "cold_start", false}
	fmt.Println(audit.Line("lambda.invoke", pairs...))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user.login user=alice attempt=1 ok=true
user.login !BADKEY=42
user.login user=alice !BADVAL=orphan
startup
lambda.invoke region=us-east-1 cold_start=false
```

### Test

One table covers the four required shapes of input. Note that `tc.kvs...` in
the call itself is the splat form: each row's `[]any` is already stored in a
variable and spread at the call site, the same pattern the demo's `pairs...`
uses.

Create `audit_test.go`:

```go
// audit_test.go
package audit

import "testing"

func TestLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event string
		kvs   []any
		want  string
	}{
		{"mixed types", "req", []any{"method", "GET", "count", 3, "ok", true}, "req method=GET count=3 ok=true"},
		{"non-string key", "req", []any{42, "ignored"}, "req !BADKEY=42"},
		{"odd trailing value", "req", []any{"a", 1, "orphan"}, "req a=1 !BADVAL=orphan"},
		{"zero kvs", "startup", nil, "startup"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Line(tc.event, tc.kvs...) // tc.kvs... splats the stored slice
			if got != tc.want {
				t.Errorf("Line(%q, %v) = %q, want %q", tc.event, tc.kvs, got, tc.want)
			}
		})
	}
}
```

## Review

`Line` is correct when a balanced list of pairs becomes `key=value` tokens, a
non-string key surfaces as `!BADKEY` instead of panicking or vanishing, an
unpaired trailing value surfaces as `!BADVAL` instead of being dropped, and
zero `kvs` returns the bare event with no stray space. The senior lesson:
`...any` buys convenient call-site syntax at the cost of moving key/value
correctness out of the type system and into runtime bookkeeping — the same
reason `slog` itself needs a `!BADKEY` sentinel, and why any wrapper around
`...any` needs the same defensive fallback plus a test for the malformed path.

## Resources

- [`log/slog`: the key-value `...any` convention and `!BADKEY`](https://pkg.go.dev/log/slog#Logger.Log)
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters) — the slice-splat form used at the demo's last call site.
- [`strings.Builder`](https://pkg.go.dev/strings#Builder) — accumulating the line without repeated string concatenation.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-splat-aliasing-and-copy-safety.md](10-splat-aliasing-and-copy-safety.md) | Next: [12-env-overlay-builder.md](12-env-overlay-builder.md)
