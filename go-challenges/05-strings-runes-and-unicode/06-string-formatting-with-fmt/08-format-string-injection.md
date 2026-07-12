# Exercise 8: Preventing Format-String Injection from User Input

An error/response builder must never pass untrusted input as the *format* string.
`fmt.Errorf(userMsg)` lets attacker-controlled `%` sequences corrupt the output or
expose internals. This exercise builds the safe builder, demonstrates the
vulnerable pattern's real corruption, and wires in `go vet` as the automated guard.

This module is fully self-contained: its own `go mod init`, code, demo, and tests.

## What you'll build

```text
safefmt/                   independent module: example.com/safefmt
  go.mod                   go 1.24
  safefmt.go               RejectRequest; WrapUser; both use a constant format
  cmd/
    demo/
      main.go              runnable demo: adversarial input passes through verbatim
  safefmt_test.go          verbatim-passthrough + adversarial table + no-artifact tests
```

- Files: `safefmt.go`, `cmd/demo/main.go`, `safefmt_test.go`.
- Implement: `RejectRequest(userInput string) string` returning `request rejected: <input>` via a constant format, and `WrapUser(userMsg string, cause error) error` wrapping with a constant format plus `%w`.
- Test: adversarial input containing `%s`/`%d`/`%n`/`%%` is emitted verbatim with no `%!` artifacts; the safe wrap preserves the cause for `errors.Is`; a table of adversarial inputs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The vulnerability, and why it corrupts

The rule is one line: the format string is always a constant literal, the data is
always an argument. Break it and untrusted `%` sequences in the data are
interpreted as verbs against an argument list that does not have them. The
vulnerable code (do not write this):

```go
// VULNERABLE — never do this. userMsg is attacker-controlled.
func rejectBad(userMsg string) string {
	return fmt.Sprintf(userMsg) // userMsg is the FORMAT string
}
```

Feed it real adversarial input and `fmt` produces corruption, because there are no
arguments to satisfy the verbs the attacker injected:

```text
input:  "user said hello %s and %d"
output: "user said hello %!s(MISSING) and %!d(MISSING)"

input:  "100% done"
output: "100%!d(MISSING)one"
```

Beyond corruption, verbs like `%#v` or `%p` in the format can expose internal
representation of whatever *does* get passed, turning a log line into an
information leak. And a stray `%` from an innocent user (a "100% done" status)
silently mangles the message. There is no safe amount of trust here — the data
never belongs in the format position.

The fix is trivial and total: a constant format, data as an argument.

```go
func rejectGood(userMsg string) string {
	return fmt.Sprintf("request rejected: %s", userMsg) // userMsg is an ARGUMENT
}
```

Now every byte of `userMsg`, including `%s` and `%`, is copied verbatim — `%s` as
a verb only ever appears in the constant part you control.

### go vet is the automated guard

You do not have to rely on discipline. `go vet`'s `printf` analyzer flags a
non-constant format string. On modern Go (1.24+) the check reports, for the
vulnerable code above:

```text
./safefmt.go:NN:NN: non-constant format string in call to fmt.Sprintf
```

This runs as part of `go test`/CI and as part of this curriculum's gate — which is
exactly why the vulnerable form is shown here only as an illustrative snippet and
never as compiled code: committing it would fail `go vet` and therefore the build.
The safe builders below are what the module actually compiles, and they are
`go vet`-clean.

Create `safefmt.go`:

```go
package safefmt

import "fmt"

// RejectRequest builds a rejection message. userInput is untrusted, so it is an
// ARGUMENT to a constant format, never the format itself. Any % sequences in the
// input are emitted verbatim.
func RejectRequest(userInput string) string {
	return fmt.Sprintf("request rejected: %s", userInput)
}

// WrapUser annotates a cause with an untrusted user message, safely: the message
// is an argument, and %w preserves the cause for errors.Is/As.
func WrapUser(userMsg string, cause error) error {
	return fmt.Errorf("user error %q: %w", userMsg, cause)
}
```

### The runnable demo

The demo feeds the safe builder the exact adversarial strings that break the
vulnerable version, and shows them passing through untouched.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/safefmt"
)

func main() {
	inputs := []string{
		"user said hello %s and %d",
		"100% done",
		"%#v %p %n",
	}
	for _, in := range inputs {
		fmt.Println(safefmt.RejectRequest(in))
	}

	cause := errors.New("timeout")
	err := safefmt.WrapUser("bad %s input", cause)
	fmt.Println(err)
	fmt.Println("Is timeout:", errors.Is(err, cause))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request rejected: user said hello %s and %d
request rejected: 100% done
request rejected: %#v %p %n
user error "bad %s input": timeout
Is timeout: true
```

### Tests

`TestVerbatimPassthrough` is the core: a table of adversarial inputs must appear
verbatim in the output, and the output must contain no `%!` corruption artifact.
`TestWrapPreservesCause` checks the safe wrap keeps the cause matchable by
`errors.Is` even though the user message is untrusted. `TestNoNoverbArtifact`
scans a swept set for the `%!` marker that only appears when a verb went unfed.

Create `safefmt_test.go`:

```go
package safefmt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var adversarial = []string{
	"user said hello %s and %d",
	"100% done",
	"%#v %p %n",
	"plain message",
	"%%%%%%",
	"drop table %s; --",
}

func TestVerbatimPassthrough(t *testing.T) {
	t.Parallel()

	for _, in := range adversarial {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			out := RejectRequest(in)
			if !strings.Contains(out, in) {
				t.Fatalf("input %q not emitted verbatim: %q", in, out)
			}
			if strings.Contains(out, "%!") {
				t.Fatalf("output has a NOVERB artifact: %q", out)
			}
			if out != "request rejected: "+in {
				t.Fatalf("output = %q, want prefix + verbatim input", out)
			}
		})
	}
}

func TestWrapPreservesCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	err := WrapUser("login failed for %s", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error lost its cause: %v", err)
	}
	if strings.Contains(err.Error(), "%!") {
		t.Fatalf("wrapped error has a NOVERB artifact: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "login failed for %s") {
		t.Fatalf("wrapped error dropped the user message: %q", err.Error())
	}
}

func Example() {
	fmt.Println(RejectRequest("give me %s or %d"))
	// Output: request rejected: give me %s or %d
}
```

Prove `go vet` is the guard by writing the vulnerable form into a scratch file and
running the analyzer — it must report a non-constant format string:

```bash
go vet ./...   # clean for the safe builders in this module
```

## Review

The builder is safe when untrusted data is always an argument to a constant format
— `TestVerbatimPassthrough` proves adversarial `%`-laden input is copied byte for
byte with no `%!` artifact, and `TestWrapPreservesCause` proves the safe wrap keeps
`errors.Is` working. The vulnerable form (`fmt.Sprintf(userInput)`) is shown only
illustratively because it corrupts output (`%!s(MISSING)`), can leak internals via
`%#v`/`%p`, and — decisively — fails `go vet`'s non-constant-format-string check,
which runs in the gate and would block the build. Treat the vet warning as a hard
error, never a suggestion. The single habit that prevents the entire class of bug:
the format string is a literal you wrote, the data is an argument.

## Resources

- [`go vet` and the printf analyzer](https://pkg.go.dev/cmd/vet) — non-constant format strings and verb/arg checks.
- [`fmt` package](https://pkg.go.dev/fmt) — how verbs consume arguments and produce `%!` markers when they cannot.
- [`fmt.Errorf`](https://pkg.go.dev/fmt#Errorf) — safe wrapping with a constant format and `%w`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-sscanf-parse-legacy-line.md](09-sscanf-parse-legacy-line.md)
