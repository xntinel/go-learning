# Exercise 2: A Type-Safe Staged Builder

A validating builder catches a missing required field at run time, when `Build` returns an error. This exercise builds something stronger: a *staged* (or step) builder for an email message that catches a missing required field at *compile time*. By giving each construction stage its own interface type, the only method the caller can reach is the next required one, so a program that forgets `From`, `To`, or `Subject` does not compile, and `Build` needs no error return at all.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
email.go             Message, Format, the stage interfaces, the staged builder
cmd/
  demo/
    main.go          build a full message and a bare one; show what won't compile
email_test.go        required+optional fields, required-only, repeatable optionals, stage types
```

- Files: `email.go`, `cmd/demo/main.go`, `email_test.go`.
- Implement: `Message`; the stage interfaces `FromStep`, `ToStep`, `SubjectStep`, `ContentStep`; one unexported `builder` that satisfies them all; and `New() FromStep`.
- Test: `email_test.go` builds a message with and without optional fields, confirms optional setters repeat, and pins the interface type each stage returns.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/02-builder-pattern/02-staged-builder/cmd/demo && cd go-solutions/24-design-patterns-in-go/02-builder-pattern/02-staged-builder
```

### Why interfaces enforce order, and how Build loses its error

The whole technique rests on a single Go fact: a method set is fixed by the static type of the value you hold, not by the concrete value underneath. So if `New` hands the caller back a value typed as `FromStep`, and `FromStep` declares exactly one method, `From`, then `From` is the *only* thing the caller can call. Have `From` return a value typed as `ToStep`, whose one method is `To`, and the caller's only next move is `To`. Continue the chain — `To` returns `SubjectStep` (only `Subject`), `Subject` returns `ContentStep` — and you have encoded a mandatory order, `From -> To -> Subject`, directly in the types. Each arrow is a required field, and skipping one is not a run-time error to be tested for; it is a program that the compiler refuses to build, because the method you tried to call is not in the interface you are holding.

`ContentStep` is the terminal stage and is shaped differently on purpose. By the time the caller holds a `ContentStep`, every required field is set, so this is where the *optional* setters (`Cc`, `Bcc`, `Body`) and `Build` finally appear. The optional setters each return `ContentStep` again, which is what lets them be chained any number of times or omitted entirely — once you are in the terminal stage you stay there until you call `Build`. And because the type system has already guaranteed `From`, `To`, and `Subject` were supplied to reach this point, `Build` has nothing left to validate: it returns `Message`, not `(Message, error)`. The error return vanished because the error became unrepresentable.

One unexported concrete struct does all the work. `builder` holds the partial `Message` and implements every method of every stage interface; each method mutates one field and returns `b` — the same pointer — retyped as the next stage. The struct is unexported so that callers can never hold it directly (which would expose every method at once and defeat the staging); they only ever see it through a stage interface. `New` returns `FromStep`, the narrowest possible door into the chain.

The trade-off is honest: this form is more interfaces and a fixed order, so it pays off when the required set is small and stable and the compile-time safety is worth the rigidity. A method that two different stages both need is the one friction point — a concrete method can have only one return type — so the design keeps the first recipient as the single required `To` and routes additional recipients through `Cc`/`Bcc` rather than overloading `To`.

Create `email.go`:

```go
package email

import (
	"fmt"
	"strings"
)

// Message is the immutable product the staged builder yields. Once Build
// returns it, From, To, and Subject are guaranteed set: the type system made
// it impossible to reach Build without supplying them.
type Message struct {
	From    string
	To      []string
	Cc      []string
	Bcc     []string
	Subject string
	Body    string
}

// Format renders the message as a minimal RFC-5322-style header block followed
// by the body, purely so the demo and tests have something concrete to inspect.
func (m Message) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\n", m.From)
	fmt.Fprintf(&b, "To: %s\n", strings.Join(m.To, ", "))
	if len(m.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", strings.Join(m.Cc, ", "))
	}
	if len(m.Bcc) > 0 {
		fmt.Fprintf(&b, "Bcc: %s\n", strings.Join(m.Bcc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\n", m.Subject)
	b.WriteString("\n")
	b.WriteString(m.Body)
	return b.String()
}

// The staged interfaces. Each required step exposes exactly one method and
// returns the interface for the next step, so the only legal call sequence is
// From -> To -> Subject, after which the optional ContentStep methods and
// Build become available.

// FromStep is the entry stage: only From is callable.
type FromStep interface {
	From(addr string) ToStep
}

// ToStep is reached only after From: only To is callable.
type ToStep interface {
	To(addr string) SubjectStep
}

// SubjectStep is reached only after To: only Subject is callable.
type SubjectStep interface {
	Subject(s string) ContentStep
}

// ContentStep is the terminal stage. Every required field is now set, so the
// optional setters and Build are exposed. The optional setters return
// ContentStep so they can be chained any number of times (or not at all).
type ContentStep interface {
	Cc(addr string) ContentStep
	Bcc(addr string) ContentStep
	Body(s string) ContentStep
	Build() Message
}

// builder is the single concrete type that satisfies every stage interface. It
// is unexported: callers only ever hold it through a stage interface, which is
// what prevents them from calling a method out of order.
type builder struct {
	msg Message
}

// New starts the chain. It returns FromStep, so the caller's only option is to
// call From next; there is no way to reach Build from here.
func New() FromStep {
	return &builder{}
}

func (b *builder) From(addr string) ToStep {
	b.msg.From = addr
	return b
}

func (b *builder) To(addr string) SubjectStep {
	b.msg.To = append(b.msg.To, addr)
	return b
}

func (b *builder) Subject(s string) ContentStep {
	b.msg.Subject = s
	return b
}

func (b *builder) Cc(addr string) ContentStep {
	b.msg.Cc = append(b.msg.Cc, addr)
	return b
}

func (b *builder) Bcc(addr string) ContentStep {
	b.msg.Bcc = append(b.msg.Bcc, addr)
	return b
}

func (b *builder) Body(s string) ContentStep {
	b.msg.Body = s
	return b
}

func (b *builder) Build() Message {
	return b.msg
}
```

Notice there is not one `if` checking for a missing field anywhere in `Build`. That absence is the result: the only way to obtain a `ContentStep` to call `Build` on is to have travelled through `From`, `To`, and `Subject`, so the partial `Message` is necessarily complete.

### The runnable demo

The demo builds a full message with optional `Cc` and `Body`, then a bare message with required fields only, and prints both via `Format`. The comment block at the end records the two call sequences that *do not compile* — the real demonstration, since the point of a staged builder is the program you cannot write.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/staged-builder"
)

func main() {
	// The compiler walks the stages with us: New() yields a FromStep, so From
	// is the only method available; From yields a ToStep; and so on. Only after
	// Subject do Cc, Bcc, Body, and Build appear.
	msg := email.New().
		From("alice@example.com").
		To("bob@example.com").
		Subject("Lunch?").
		Cc("carol@example.com").
		Body("Are you free at noon?").
		Build()

	fmt.Println(msg.Format())
	fmt.Println("---")

	// The minimal legal message: required fields only, no optional setters.
	bare := email.New().
		From("ops@example.com").
		To("oncall@example.com").
		Subject("Deploy finished").
		Build()
	fmt.Println(bare.Format())

	// The following lines do NOT compile, which is the whole point:
	//
	//	email.New().Build()                 // FromStep has no Build
	//	email.New().Subject("skip the To")  // FromStep has no Subject
	//
	// Invalid construction is rejected at compile time, not at run time.
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
From: alice@example.com
To: bob@example.com
Cc: carol@example.com
Subject: Lunch?

Are you free at noon?
---
From: ops@example.com
To: oncall@example.com
Subject: Deploy finished
```

The bare message prints no `Cc` or `Bcc` line because `Format` omits empty optional headers, and its body is the empty string, so the message ends after the blank line that separates headers from body.

### Tests

A staged builder cannot be made to fail at run time, so the tests pin behaviour rather than errors. `TestBuild_RequiredAndOptionalFields` walks the full chain and checks every field landed. `TestBuild_RequiredOnly` confirms the optional fields stay empty and that `Format` omits their headers. `TestBuild_OptionalSettersAreRepeatable` confirms two `Cc` calls accumulate, which is the property the `ContentStep`-returning optionals exist to provide. `TestStageTypes` is the one that guards the compile-time contract itself: it assigns each stage's result to a variable of the expected interface type, so if a stage's return type ever regresses, this test stops compiling — the failure mode is a build error, exactly as it should be.

Create `email_test.go`:

```go
package email

import (
	"strings"
	"testing"
)

func TestBuild_RequiredAndOptionalFields(t *testing.T) {
	t.Parallel()

	msg := New().
		From("alice@example.com").
		To("bob@example.com").
		Subject("Lunch?").
		Cc("carol@example.com").
		Bcc("dan@example.com").
		Body("Are you free at noon?").
		Build()

	if msg.From != "alice@example.com" {
		t.Errorf("From = %q", msg.From)
	}
	if len(msg.To) != 1 || msg.To[0] != "bob@example.com" {
		t.Errorf("To = %v", msg.To)
	}
	if msg.Subject != "Lunch?" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if len(msg.Cc) != 1 || msg.Cc[0] != "carol@example.com" {
		t.Errorf("Cc = %v", msg.Cc)
	}
	if len(msg.Bcc) != 1 || msg.Bcc[0] != "dan@example.com" {
		t.Errorf("Bcc = %v", msg.Bcc)
	}
	if msg.Body != "Are you free at noon?" {
		t.Errorf("Body = %q", msg.Body)
	}
}

func TestBuild_RequiredOnly(t *testing.T) {
	t.Parallel()

	msg := New().
		From("ops@example.com").
		To("oncall@example.com").
		Subject("Deploy finished").
		Build()

	if len(msg.Cc) != 0 || len(msg.Bcc) != 0 || msg.Body != "" {
		t.Errorf("optional fields should be empty, got %+v", msg)
	}
	out := msg.Format()
	if strings.Contains(out, "Cc:") || strings.Contains(out, "Bcc:") {
		t.Errorf("Format must omit empty Cc/Bcc, got:\n%s", out)
	}
}

func TestBuild_OptionalSettersAreRepeatable(t *testing.T) {
	t.Parallel()

	msg := New().
		From("a@example.com").
		To("b@example.com").
		Subject("hi").
		Cc("c1@example.com").
		Cc("c2@example.com").
		Build()

	if len(msg.Cc) != 2 {
		t.Fatalf("want 2 Cc recipients, got %v", msg.Cc)
	}
}

// TestStageTypes pins the type returned by each stage, which is the compile-time
// contract the whole exercise rests on. If a stage's return type regresses, this
// stops compiling.
func TestStageTypes(t *testing.T) {
	t.Parallel()

	var fs FromStep = New()
	var ts ToStep = fs.From("a@example.com")
	var ss SubjectStep = ts.To("b@example.com")
	var cs ContentStep = ss.Subject("s")
	msg := cs.Build()
	if msg.From != "a@example.com" {
		t.Errorf("From = %q", msg.From)
	}
}
```

## Review

The builder is correct when the staging is enforced by types, not by checks. Confirm that `New` returns `FromStep` and not the concrete `*builder`, that each required setter returns the *next* stage interface, and that `ContentStep` is the only interface exposing `Build`. The proof that it works is negative and lives in the demo comment: `email.New().Build()` and `email.New().Subject(...)` must fail to compile, because `FromStep` has neither method. There is no run-time error path to test, which is precisely the safety the pattern buys.

Common mistakes for this form. The first is exporting the concrete builder or returning it from `New`; the moment a caller can hold the concrete type, every method is in scope at once and the staging evaporates — keep the struct unexported and hand back interfaces. The second is wanting one method name on two stages with different next-stage return types, which Go forbids because a concrete method has a single signature; resolve it by renaming (a required `To`, optional `Cc`/`Bcc`) rather than fighting the type system. The third is over-applying the pattern: every required field adds an interface and rigidifies the order, so reserve staging for a small, stable required set and prefer the validating builder of Exercise 1 when the rules are many or fluid. Run `go test -race -count=1 ./...`; a green build is itself part of the proof, since `TestStageTypes` only compiles when the stage types are intact.

## Resources

- [Go spec: Interface types](https://go.dev/ref/spec#Interface_types) — how a value's method set is fixed by its static interface type, the mechanism the staging relies on.
- [Effective Go: Interfaces and methods](https://go.dev/doc/effective_go#interfaces_and_types) — idiomatic use of small interfaces to constrain what a caller can do.
- [Builder in Go (Refactoring Guru)](https://refactoring.guru/design-patterns/builder/go/example) — the Builder pattern and the Director role this lesson's third exercise revisits.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-fluent-request-builder.md](01-fluent-request-builder.md) | Next: [03-immutable-builder-and-director.md](03-immutable-builder-and-director.md)
