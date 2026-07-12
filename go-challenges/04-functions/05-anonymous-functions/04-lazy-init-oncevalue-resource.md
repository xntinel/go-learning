# Exercise 4: Thread-Safe Lazy Resource with sync.OnceValue/OnceValues

An expensive resource — a compiled set of validation regexps, a parsed template —
should be built once, lazily, on first use, and shared safely across goroutines.
The old way was a package-global pointer guarded by `sync.Once` and hand-rolled
double-checked locking, which is easy to get subtly wrong. Go 1.21's
`sync.OnceValue` and `sync.OnceValues` wrap an initialization function literal and
return a memoized getter that runs the body at most once. This module builds both.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
lazyres/                      module example.com/lazyres
  go.mod
  lazy.go                     Validators (OnceValue) and LazyTemplate (OnceValues)
  lazy_test.go                concurrent init-once, identical values, cached error
  cmd/demo/main.go            compile regexps and render a template lazily
```

- Files: `lazy.go`, `lazy_test.go`, `cmd/demo/main.go`.
- Implement: `Validators` whose compiled regexp map is built by a `sync.OnceValue` literal, and `LazyTemplate` whose parse is a `sync.OnceValues[*template.Template, error]` literal; the init literals close over the configuration and an atomic call counter.
- Test: under `-race`, many goroutines call the getter and the init runs exactly once (atomic counter == 1) and every caller receives the identical instance; the `OnceValues` variant returns the cached error on repeat calls without re-running.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/04-lazy-init-oncevalue-resource/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/04-lazy-init-oncevalue-resource
```

### OnceValue and OnceValues over a hand-rolled sync.Once

`sync.OnceValue(f func() T) func() T` returns a function that, no matter how many
goroutines call it concurrently, runs `f` exactly once, caches the result, and
returns that same `T` to every caller. `sync.OnceValues(f func() (T1, T2)) func()
(T1, T2)` does the same for a value-and-error pair, caching *both* — so an
initialization that fails returns the same error on every subsequent call and never
re-runs the failing body. These replace the `sync.Once` + package-global pattern:
you no longer declare a nullable global, a `sync.Once`, and a `Do` closure that
assigns the global under the lock, a shape whose hand-written variants often hide a
visibility bug or an accidental re-init.

The init literal is where the configuration is captured. `NewValidators` closes the
`patterns` map into a `sync.OnceValue` literal that compiles each pattern with
`regexp.MustCompile`; the compilation is deferred until the first `Match` call and
then shared. To *prove* the once-only property, the literal also bumps an
`atomic.Int64` that the struct exposes — a real test can assert it reached exactly
1 after a concurrent stampede.

`NewLazyTemplate` uses `sync.OnceValues` because `template.Parse` is fallible: the
literal returns `(*template.Template, error)`, and a bad template text caches the
error so every `Render` sees the same failure without re-parsing.

Create `lazy.go`:

```go
package lazyres

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
)

// Validators lazily compiles a set of named regexps exactly once, on first use.
type Validators struct {
	get   func() map[string]*regexp.Regexp
	calls *atomic.Int64
}

// NewValidators returns a Validators that compiles patterns on the first Match.
// The OnceValue literal closes over patterns and the call counter.
func NewValidators(patterns map[string]string) *Validators {
	var calls atomic.Int64
	get := sync.OnceValue(func() map[string]*regexp.Regexp {
		calls.Add(1)
		m := make(map[string]*regexp.Regexp, len(patterns))
		for name, p := range patterns {
			m[name] = regexp.MustCompile(p)
		}
		return m
	})
	return &Validators{get: get, calls: &calls}
}

// Match reports whether s matches the named pattern. Unknown names report false.
func (v *Validators) Match(name, s string) bool {
	re, ok := v.get()[name]
	if !ok {
		return false
	}
	return re.MatchString(s)
}

// InitCount reports how many times the compile literal ran (for tests).
func (v *Validators) InitCount() int64 { return v.calls.Load() }

// LazyTemplate parses a template exactly once, caching a parse error.
type LazyTemplate struct {
	get   func() (*template.Template, error)
	calls *atomic.Int64
}

// NewLazyTemplate returns a LazyTemplate that parses text on the first Render.
func NewLazyTemplate(name, text string) *LazyTemplate {
	var calls atomic.Int64
	get := sync.OnceValues(func() (*template.Template, error) {
		calls.Add(1)
		return template.New(name).Parse(text)
	})
	return &LazyTemplate{get: get, calls: &calls}
}

// Render executes the template against data, returning the cached parse error if
// the template text was invalid.
func (l *LazyTemplate) Render(data any) (string, error) {
	tmpl, err := l.get()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// InitCount reports how many times the parse literal ran (for tests).
func (l *LazyTemplate) InitCount() int64 { return l.calls.Load() }
```

### The runnable demo

The demo builds a validator set and a template, then uses them; the regexps compile
and the template parses only on first use.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazyres"
)

func main() {
	v := lazyres.NewValidators(map[string]string{
		"slug": `^[a-z0-9-]+$`,
	})
	fmt.Println("my-post-1:", v.Match("slug", "my-post-1"))
	fmt.Println("Bad Slug: ", v.Match("slug", "Bad Slug"))

	tmpl := lazyres.NewLazyTemplate("greet", "Hello {{.Name}}")
	out, err := tmpl.Render(map[string]string{"Name": "alice"})
	if err != nil {
		fmt.Println("render error:", err)
		return
	}
	fmt.Println(out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
my-post-1: true
Bad Slug:  false
Hello alice
```

### Tests

`TestValidatorsInitOnce` launches fifty goroutines that all read the compiled
`email` regexp; it asserts the init literal ran exactly once and every goroutine
received the *identical* `*regexp.Regexp` pointer, proving the value is shared, not
recompiled. `TestLazyTemplateCachesError` parses an invalid template and calls
`Render` twice, asserting both calls return the cached error and the parse literal
ran only once.

Create `lazy_test.go`:

```go
package lazyres

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestValidatorsInitOnce(t *testing.T) {
	t.Parallel()
	v := NewValidators(map[string]string{"email": `^\S+@\S+$`})

	const n = 50
	ptrs := make([]*regexp.Regexp, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ptrs[idx] = v.get()["email"]
		}(i)
	}
	wg.Wait()

	if got := v.InitCount(); got != 1 {
		t.Fatalf("init ran %d times, want 1", got)
	}
	for i := 1; i < n; i++ {
		if ptrs[i] != ptrs[0] {
			t.Fatal("callers received different compiled regexp instances")
		}
	}
}

func TestLazyTemplateCachesError(t *testing.T) {
	t.Parallel()
	tmpl := NewLazyTemplate("bad", "Hello {{.Name") // missing closing braces

	_, err1 := tmpl.Render(nil)
	_, err2 := tmpl.Render(nil)
	if err1 == nil || err2 == nil {
		t.Fatalf("Render errors = %v, %v; want both non-nil", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("cached error differs: %v vs %v", err1, err2)
	}
	if got := tmpl.InitCount(); got != 1 {
		t.Fatalf("parse ran %d times, want 1", got)
	}
}

func TestLazyTemplateRenders(t *testing.T) {
	t.Parallel()
	tmpl := NewLazyTemplate("greet", "Hi {{.}}")
	out, err := tmpl.Render("bob")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "bob") {
		t.Fatalf("rendered %q, want it to contain bob", out)
	}
}

func ExampleValidators_Match() {
	v := NewValidators(map[string]string{"digit": `^\d+$`})
	fmt.Println(v.Match("digit", "123"), v.Match("digit", "12a"))
	// Output: true false
}
```

## Review

The lazy getter is correct when initialization happens exactly once under any
amount of concurrency and every caller shares the same result. `TestValidatorsInitOnce`
proves both with an atomic counter inside the literal (must equal 1) and pointer
identity across fifty racing goroutines. The `OnceValues` variant additionally
caches the error: `TestLazyTemplateCachesError` shows a failed parse returns the
same error twice while running the body only once — which is exactly what a
hand-rolled `sync.Once` global tends to get wrong (re-running the failing init or
losing the error). Reach for `OnceValue`/`OnceValues` instead of a global-plus-Once
whenever the resource is expensive, shared, and built from captured configuration;
keep a real `Clock`-style abstraction only when production needs to *control* the
value, not merely build it once.

## Resources

- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue)
- [sync.OnceValues](https://pkg.go.dev/sync#OnceValues)
- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile)
- [text/template](https://pkg.go.dev/text/template)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-deferred-closure-named-return-observability.md](03-deferred-closure-named-return-observability.md) | Next: [05-http-middleware-function-literal.md](05-http-middleware-function-literal.md)
