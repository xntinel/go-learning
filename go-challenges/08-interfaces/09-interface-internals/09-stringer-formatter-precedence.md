# Exercise 9: Render A Domain Type In Logs With Stringer And Formatter

A domain type that must never leak its full value into a log line — an API key, a
token — is a job for `fmt.Stringer` and, when you need verb-level control,
`fmt.Formatter`. This module implements both and pins the runtime precedence `fmt`
uses: `Formatter` over `error` over `Stringer`.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
apikey/                     independent module: example.com/apikey
  go.mod                    go 1.26
  apikey.go                 APIKey with String and Format; failKey (error + Stringer)
  cmd/
    demo/
      main.go               prints an APIKey under %v, %+v, %#v, width/precision
  apikey_test.go            verb table, width/precision, error-over-Stringer precedence
```

- Files: `apikey.go`, `cmd/demo/main.go`, `apikey_test.go`.
- Implement: `APIKey` with a safe `String()` and a `Format(fmt.State, rune)` handling `%v`/`%+v`/`%#v` and honoring width/precision; a `failKey` implementing both `error` and `Stringer`.
- Test: `%v` renders via `String`, `%+v`/`%#v` take Formatter branches, width/precision are honored via `fmt.State`; a type implementing both `error` and `Stringer` renders via `error`; the key is never fully printed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The precedence fmt applies, and why

When `fmt` formats an argument it checks interfaces in a fixed order. First it asks
whether the value implements `fmt.Formatter`; if so it hands off entirely to
`Format(f State, verb rune)` and does nothing else. Only if the value is not a
`Formatter` does `fmt` — for the string-like verbs `v`, `s`, `q`, `x`, `X` — check
whether it implements `error` (call `Error()`) and then `fmt.Stringer` (call
`String()`), before finally falling back to reflection-based default formatting.
So the precedence is Formatter, then error, then Stringer. A type that implements
both `error` and `Stringer` renders through `Error()`; a type that implements
`Formatter` controls every verb itself.

`APIKey` uses this deliberately. It carries a `prefix` (safe to show) and a
`secret` (never shown). Its `String()` returns `prefix + "..."`, the safe default
for any code that prints it with `%v` or `%s` without thinking. Because it also
implements `Formatter`, `fmt` routes *all* its formatting through `Format`, where
each verb is handled explicitly:

- plain `%v` and `%s` delegate to `String()` — the safe short form;
- `%+v` adds the prefix label but still no secret;
- `%#v` renders a Go-syntax-ish form that still omits the secret.

Inside `Format`, the `fmt.State` gives verb flags and sizing: `f.Flag('+')` and
`f.Flag('#')` distinguish the verbs, `f.Width()`/`f.Precision()` report the
`%10.4v`-style modifiers, and `f.Write` emits bytes. Honoring width and precision
by hand is the price of implementing `Formatter`: `fmt` no longer pads for you, so
`Format` applies precision (truncate) then width (pad) itself. The critical
property under test is that no branch ever writes `secret` — a `Formatter` that
forgets a verb would fall through to something that leaks, so every path is
covered.

`failKey` exists only to demonstrate the error-over-Stringer rule: it implements
both, and `fmt.Sprintf("%v", failKey{})` must render its `Error()` text, not its
`String()` text.

Create `apikey.go`:

```go
package apikey

import (
	"fmt"
	"io"
	"strings"
)

// APIKey is a credential that must never render its secret in a log line. It
// implements fmt.Stringer for the safe default and fmt.Formatter for verb-level
// control; fmt routes all formatting through Format because Formatter wins.
type APIKey struct {
	prefix string
	secret string
}

// New builds an APIKey from a visible prefix and a secret.
func New(prefix, secret string) APIKey {
	return APIKey{prefix: prefix, secret: secret}
}

// String is the safe default: the prefix and an ellipsis, never the secret.
func (k APIKey) String() string {
	return k.prefix + "..."
}

// Format controls every verb. It never emits the secret. It honors width and
// precision from the State because a Formatter is responsible for its own
// padding.
func (k APIKey) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v':
		switch {
		case f.Flag('#'):
			writePadded(f, fmt.Sprintf("apikey.APIKey{prefix:%q}", k.prefix))
		case f.Flag('+'):
			writePadded(f, "APIKey{prefix:"+k.prefix+"}")
		default:
			writePadded(f, k.String())
		}
	case 's':
		writePadded(f, k.String())
	default:
		fmt.Fprintf(f, "%%!%c(apikey.APIKey=%s)", verb, k.String())
	}
}

// writePadded applies the State's precision (truncate) then width (pad) to s,
// respecting the '-' left-justify flag.
func writePadded(f fmt.State, s string) {
	if p, ok := f.Precision(); ok && p < len(s) {
		s = s[:p]
	}
	if w, ok := f.Width(); ok && w > len(s) {
		pad := strings.Repeat(" ", w-len(s))
		if f.Flag('-') {
			io.WriteString(f, s)
			io.WriteString(f, pad)
		} else {
			io.WriteString(f, pad)
			io.WriteString(f, s)
		}
		return
	}
	io.WriteString(f, s)
}

// failKey implements both error and fmt.Stringer to demonstrate that fmt renders
// it via Error(), not String().
type failKey struct{}

func (failKey) Error() string  { return "key-error" }
func (failKey) String() string { return "key-stringer" }

// NewFailKey returns a value implementing both error and fmt.Stringer.
func NewFailKey() error { return failKey{} }
```

### The runnable demo

The demo prints one `APIKey` under several verbs and a width/precision modifier so
you can see the Formatter branches and the padding, and prints the `failKey` to
show `error` beating `Stringer`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/apikey"
)

func main() {
	k := apikey.New("sk_live", "supersecret")

	fmt.Printf("%%v:    %v\n", k)
	fmt.Printf("%%+v:   %+v\n", k)
	fmt.Printf("%%#v:   %#v\n", k)
	fmt.Printf("%%12v:  %12v|\n", k)
	fmt.Printf("%%-12v: %-12v|\n", k)
	fmt.Printf("%%.4v:  %.4v\n", k)

	fmt.Printf("error+Stringer via %%v: %v\n", apikey.NewFailKey())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
%v:    sk_live...
%+v:   APIKey{prefix:sk_live}
%#v:   apikey.APIKey{prefix:"sk_live"}
%12v:    sk_live...|
%-12v: sk_live...  |
%.4v:  sk_l
error+Stringer via %v: key-error
```

`%v` uses `String`, `%+v`/`%#v` take the Formatter branches, `%12v` right-pads to
width 12, `%-12v` left-justifies, `%.4v` truncates to 4 runes, and the
error-implementing value renders via `Error()`.

### Tests

The verb table asserts each verb produces its branch and never contains the secret.
`TestWidthAndPrecision` checks padding and truncation flow through `fmt.State`.
`TestErrorBeatsStringer` pins the precedence: a value implementing both renders via
`Error()`. `TestSecretNeverPrinted` sweeps every verb and asserts the secret is
absent from all of them.

Create `apikey_test.go`:

```go
package apikey

import (
	"fmt"
	"strings"
	"testing"
)

func TestVerbTable(t *testing.T) {
	t.Parallel()

	k := New("sk_live", "supersecret")
	tests := []struct {
		verb string
		want string
	}{
		{"%v", "sk_live..."},
		{"%s", "sk_live..."},
		{"%+v", "APIKey{prefix:sk_live}"},
		{"%#v", `apikey.APIKey{prefix:"sk_live"}`},
	}
	for _, tc := range tests {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()
			got := fmt.Sprintf(tc.verb, k)
			if got != tc.want {
				t.Fatalf("Sprintf(%q) = %q, want %q", tc.verb, got, tc.want)
			}
		})
	}
}

func TestWidthAndPrecision(t *testing.T) {
	t.Parallel()

	k := New("sk_live", "supersecret") // String() = "sk_live..." (10 chars)

	if got := fmt.Sprintf("%12v", k); got != "  sk_live..." {
		t.Fatalf("%%12v = %q, want two-space right pad", got)
	}
	if got := fmt.Sprintf("%-12v", k); got != "sk_live...  " {
		t.Fatalf("%%-12v = %q, want two-space left pad", got)
	}
	if got := fmt.Sprintf("%.4v", k); got != "sk_l" {
		t.Fatalf("%%.4v = %q, want truncation to 4", got)
	}
}

func TestErrorBeatsStringer(t *testing.T) {
	t.Parallel()

	// failKey implements both error and Stringer; fmt renders via Error().
	got := fmt.Sprintf("%v", NewFailKey())
	if got != "key-error" {
		t.Fatalf("%%v of error+Stringer = %q, want key-error", got)
	}
	if strings.Contains(got, "stringer") {
		t.Fatalf("Stringer should not win over error, got %q", got)
	}
}

func TestSecretNeverPrinted(t *testing.T) {
	t.Parallel()

	k := New("sk_live", "supersecret")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%12v", "%.4v", "%d", "%x"} {
		if out := fmt.Sprintf(verb, k); strings.Contains(out, "supersecret") {
			t.Fatalf("verb %q leaked the secret: %q", verb, out)
		}
	}
}

func ExampleAPIKey() {
	k := New("sk_live", "supersecret")
	fmt.Printf("%v\n", k)
	fmt.Printf("%+v\n", k)
	// Output:
	// sk_live...
	// APIKey{prefix:sk_live}
}
```

## Review

The type is correct when every formatting path is safe and the precedence holds.
Because `APIKey` implements `Formatter`, `fmt` routes all verbs through `Format`,
so the safety argument is simple: no branch writes `secret`, and the `default`
branch renders a bad-verb marker with only the safe `String()`. That is why
`TestSecretNeverPrinted` can sweep even nonsensical verbs like `%d` and stay
clean. The precedence to remember is Formatter over error over Stringer:
implementing `Formatter` takes full control; implementing both `error` and
`Stringer` renders via `Error()`. Honoring width and precision is your job once you
implement `Formatter` — `fmt` stops padding for you — so apply precision then
width from the `fmt.State`. Run `go vet ./...`; its printf analysis checks these
verbs against their arguments.

## Resources

- [fmt package: Formatter, Stringer, State](https://pkg.go.dev/fmt#Formatter) — the interfaces and the `State` passed to a custom formatter.
- [fmt package overview: printing](https://pkg.go.dev/fmt#hdr-Printing) — the documented order in which fmt consults Formatter, error, and Stringer.
- [fmt.State](https://pkg.go.dev/fmt#State) — `Width`, `Precision`, `Flag`, and `Write` used to honor modifiers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-json-marshaler-receiver-itab.md](08-json-marshaler-receiver-itab.md) | Next: [10-typeswitch-ordering-benchmark.md](10-typeswitch-ordering-benchmark.md)
