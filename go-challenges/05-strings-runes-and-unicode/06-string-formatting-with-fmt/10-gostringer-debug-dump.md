# Exercise 10: Debug Dumps — %v vs %+v vs %#v and GoStringer

A debug endpoint dumps a `Config` struct. Choosing `%v`, `%+v`, or `%#v` is a
deliberate decision about audience, and `%#v` will happily print a secret in
Go-syntax unless the type implements `fmt.GoStringer`. This exercise builds the
dump, pins the three renderings, and shows why `%#v` belongs behind a debug flag.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
dbgdump/                   independent module: example.com/dbgdump
  go.mod                   go 1.24
  dbgdump.go               type Config; Secret (Stringer+GoStringer); PlainSecret (Stringer only)
  cmd/
    demo/
      main.go              runnable demo: the same Config under all three verbs
  dbgdump_test.go          distinct-output + GoStringer-override + leak-contrast tests
```

- Files: `dbgdump.go`, `cmd/demo/main.go`, `dbgdump_test.go`.
- Implement: `Config` with a `Secret` field implementing both `Stringer` and `GoStringer` (so `%#v` is redacted), and a `PlainSecret` implementing only `Stringer` (so `%#v` leaks the raw value) to prove the contrast.
- Test: `%v`, `%+v`, `%#v` produce distinct golden strings on the same struct; the `GoStringer` type overrides `%#v`; a `Stringer`-only type participates in `%v` but leaks under `%#v`; nested field names appear under `%+v`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Three verbs, three audiences

`%v`, `%+v`, and `%#v` render the same value for three different readers:

- `%v` is the default: struct fields as bare values in braces,
  `{db.internal 5432 svc [REDACTED]}`. Compact, for a quick glance, but you cannot
  tell which value is which field.
- `%+v` adds field *names*: `{Host:db.internal Port:5432 User:svc
  Password:[REDACTED]}`. This is the operator's view — readable in a log without a
  struct definition in hand. Nested structs get names too.
- `%#v` is Go-syntax: `dbgdump.Config{Host:"db.internal", Port:5432, User:"svc",
  Password:"[REDACTED]"}`. This is the debugger's view — you could paste it back
  into source. It quotes strings and shows the type name.

The dispatch difference is the trap. `%v` and `%+v` call a type's `String()`
(Stringer); `%#v` does **not** — it calls `GoString()` (GoStringer) if present, and
otherwise falls back to the reflected Go-syntax representation, which for a
`string`-based secret is the **raw quoted value**. So a `Secret` that redacts under
`%v` via `String()` will still print its plaintext under `%#v` unless it *also*
implements `GoString()`. The exercise makes this concrete with two types:

- `Secret` implements both `String()` (returns `[REDACTED]`) and `GoString()`
  (returns `"[REDACTED]"`), so it is safe under every verb, including `%#v`.
- `PlainSecret` implements only `String()`. It redacts under `%v`/`%+v` but under
  `%#v` reflection prints `"hunter2"` — a real leak. This is why `%#v` should be
  gated behind an explicit debug flag and why a credential type needs `GoString()`,
  not just `String()`.

Create `dbgdump.go`:

```go
package dbgdump

import "fmt"

// Secret is a credential safe under every verb: String() redacts %v/%+v/%s and
// GoString() redacts %#v, so a Go-syntax dump cannot leak it.
type Secret string

func (s Secret) String() string   { return "[REDACTED]" }
func (s Secret) GoString() string { return `"[REDACTED]"` }

// PlainSecret redacts only through String(); it has no GoString(), so %#v falls
// back to reflection and prints the raw value. Present to demonstrate the leak.
type PlainSecret string

func (s PlainSecret) String() string { return "[REDACTED]" }

// Config is the struct a debug endpoint dumps.
type Config struct {
	Host     string
	Port     int
	User     string
	Password Secret
}

// Dump renders cfg for a debug view. modeGo selects the Go-syntax %#v form, which
// a caller should only enable behind an explicit debug flag.
func Dump(cfg Config, modeGo bool) string {
	if modeGo {
		return fmt.Sprintf("%#v", cfg)
	}
	return fmt.Sprintf("%+v", cfg)
}
```

### The runnable demo

The demo dumps one `Config` under all three verbs so you can see the three
audiences side by side, and shows the `PlainSecret` leak under `%#v`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dbgdump"
)

func main() {
	c := dbgdump.Config{Host: "db.internal", Port: 5432, User: "svc", Password: dbgdump.Secret("hunter2")}
	fmt.Printf("%%v:  %v\n", c)
	fmt.Printf("%%+v: %+v\n", c)
	fmt.Printf("%%#v: %#v\n", c)

	leak := dbgdump.PlainSecret("hunter2")
	fmt.Printf("plain %%v:  %v\n", leak)
	fmt.Printf("plain %%#v: %#v\n", leak)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
%v:  {db.internal 5432 svc [REDACTED]}
%+v: {Host:db.internal Port:5432 User:svc Password:[REDACTED]}
%#v: dbgdump.Config{Host:"db.internal", Port:5432, User:"svc", Password:"[REDACTED]"}
plain %v:  [REDACTED]
plain %#v: "hunter2"
```

### Tests

`TestThreeVerbsDistinct` pins the exact golden string for each verb, so the
semantic difference is nailed down. `TestGoStringerOverrides` proves the `Secret`
`%#v` is redacted (the `GoString` override fired). `TestPlainSecretLeaks` is the
cautionary contrast: `%v` redacts but `%#v` exposes the plaintext, documenting why
`%#v` needs a debug gate. `TestFieldNamesUnderPlusV` checks `%+v` includes field
names.

Create `dbgdump_test.go`:

```go
package dbgdump

import (
	"fmt"
	"strings"
	"testing"
)

var cfg = Config{Host: "db.internal", Port: 5432, User: "svc", Password: Secret("hunter2")}

func TestThreeVerbsDistinct(t *testing.T) {
	t.Parallel()

	v := fmt.Sprintf("%v", cfg)
	plusV := fmt.Sprintf("%+v", cfg)
	hashV := fmt.Sprintf("%#v", cfg)

	if v != "{db.internal 5432 svc [REDACTED]}" {
		t.Fatalf("%%v = %q", v)
	}
	if plusV != "{Host:db.internal Port:5432 User:svc Password:[REDACTED]}" {
		t.Fatalf("%%+v = %q", plusV)
	}
	if hashV != `dbgdump.Config{Host:"db.internal", Port:5432, User:"svc", Password:"[REDACTED]"}` {
		t.Fatalf("%%#v = %q", hashV)
	}
	if v == plusV || plusV == hashV || v == hashV {
		t.Fatal("the three verbs should produce distinct output")
	}
}

func TestGoStringerOverrides(t *testing.T) {
	t.Parallel()

	// %#v of the Secret alone routes through GoString(), not reflection.
	if got := fmt.Sprintf("%#v", Secret("hunter2")); got != `"[REDACTED]"` {
		t.Fatalf("Secret %%#v = %q, want redacted", got)
	}
	// And it never contains the plaintext.
	if strings.Contains(fmt.Sprintf("%#v", cfg), "hunter2") {
		t.Fatalf("Config %%#v leaked the secret")
	}
}

func TestPlainSecretLeaks(t *testing.T) {
	t.Parallel()

	p := PlainSecret("hunter2")
	if got := fmt.Sprintf("%v", p); got != "[REDACTED]" {
		t.Fatalf("PlainSecret %%v = %q, want [REDACTED]", got)
	}
	// %#v bypasses String() and reflection prints the raw value: the documented leak.
	if got := fmt.Sprintf("%#v", p); !strings.Contains(got, "hunter2") {
		t.Fatalf("expected %%#v to leak the raw PlainSecret, got %q", got)
	}
}

func TestFieldNamesUnderPlusV(t *testing.T) {
	t.Parallel()

	out := fmt.Sprintf("%+v", cfg)
	for _, name := range []string{"Host:", "Port:", "User:", "Password:"} {
		if !strings.Contains(out, name) {
			t.Fatalf("%%+v missing field name %q in %q", name, out)
		}
	}
	if strings.Contains(fmt.Sprintf("%v", cfg), "Host:") {
		t.Fatal("default verb should NOT include field names")
	}
}

func Example() {
	c := Config{Host: "h", Port: 1, User: "u", Password: Secret("p")}
	fmt.Println(Dump(c, false))
	// Output: {Host:h Port:1 User:u Password:[REDACTED]}
}
```

## Review

The dump is correct when the three verbs render the same struct three ways: `%v`
bare values, `%+v` field names for the operator, `%#v` Go-syntax for the debugger —
`TestThreeVerbsDistinct` pins all three. The security-relevant point is dispatch:
`%v`/`%+v` use `String()` but `%#v` uses `GoString()` (or reflection), so a secret
that only implements `Stringer` leaks under `%#v` — `TestPlainSecretLeaks`
documents exactly that, and `TestGoStringerOverrides` shows the fix is to implement
`GoString()` too. Gate `%#v` behind an explicit debug flag; it is the most
revealing verb and the one most likely to expose an internal you did not mean to
publish. Run `go test -race`; the types are immutable values.

## Resources

- [`fmt.GoStringer`](https://pkg.go.dev/fmt#GoStringer) — the `GoString()` interface invoked by `%#v`.
- [`fmt` package — printing](https://pkg.go.dev/fmt#hdr-Printing) — the `%v`/`%+v`/`%#v` definitions and Stringer dispatch.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — why it covers `%v`/`%s` but not `%#v`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../07-regular-expressions/00-concepts.md](../07-regular-expressions/00-concepts.md)
