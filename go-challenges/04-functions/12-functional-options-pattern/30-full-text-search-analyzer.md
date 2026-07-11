# Exercise 30: Full-Text Indexer With Tokenizer, Stemmer, and Filter Chain Options

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An analyzer chain that lets callers compose a tokenizer, a stemmer, and any
number of filters has an ordering problem hiding inside it: a stopword
filter that expects lowercased tokens is wrong if it runs before a
lowercase step exists, and a filter that expects stemmed tokens is wrong if
no stemmer was ever configured. This module builds that chain with
functional options, validating every step's declared dependencies the
instant it is registered.

## What you'll build

```text
analyzer/                        independent module: example.com/full-text-search-analyzer
  go.mod                         go 1.24
  analyzer.go                    Step, Analyzer, Option, New, WithTokenizer, WithStep,
                                  Analyze, StepNames
  cmd/
    demo/
      main.go                    a lowercase+stemmer+stopword chain, then a rejected chain
  analyzer_test.go                step-validation table, chain execution, tokenizer override
```

- Files: `analyzer.go`, `cmd/demo/main.go`, `analyzer_test.go`.
- Implement: an `Analyzer` built by `New(opts ...Option) (*Analyzer, error)` whose `WithStep` registers a named token-transformation stage and rejects it immediately if any of its declared dependencies has not already been registered earlier in the chain.
- Test: every step-validation case including a dependency on a step that never runs, a dependency on the implicit tokenizer stage, a dependency satisfied by an earlier step, and the case where the same dependency would be satisfied by a *later* step (still rejected), plus full chain execution and a tokenizer override.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/analyzer/cmd/demo
cd ~/go-exercises/analyzer
go mod init example.com/full-text-search-analyzer
go mod edit -go=1.24
```

### Validating order at registration, not after the fact

Every step in the chain runs in exactly the order it was registered — there
is no separate sorting or scheduling pass. That is what makes it possible to
validate a step's dependencies the moment `WithStep` registers it, rather
than waiting for `New` to finish applying every option: by the time a given
`WithStep` call's closure runs, every step registered by an *earlier*
`WithStep` call is already in `a.steps`, and "tokenizer" is always
implicitly available since tokenizing is the one stage that unconditionally
runs first. So `WithStep("stopword", []string{"lowercase"}, fn)` can check
right then whether a step named `"lowercase"` already exists — no more, no
less. This is the same principle the schema validator's duplicate-rule
check used earlier in this chapter: when configuration accumulates in call
order, a later option can safely inspect what earlier options have already
committed.

### Why a later step can't retroactively satisfy an earlier dependency

`TestWithStepValidation`'s last case registers `"stopword"` (which requires
`"lowercase"`) *before* `"lowercase"` itself is registered, and expects
`New` to reject it even though `"lowercase"` is added immediately
afterward. This is not a limitation of the validation — it is the entire
point. `Analyze` runs steps strictly in registration order, so if
`"stopword"` were allowed to run before `"lowercase"`, it would filter on
un-lowercased tokens no matter what gets registered later; there is no
"later" from the chain's point of view. A dependency declares "this step
must have already run," and only order, not eventual presence, can make
that true.

Create `analyzer.go`:

```go
package analyzer

import (
	"fmt"
	"strings"
)

// Step is one stage of an analyzer chain: a named token transformation that
// may declare which earlier stages it requires to have already run.
type Step struct {
	Name     string
	Requires []string
	Apply    func([]string) []string
}

// Analyzer tokenizes raw text and then runs it through an ordered chain of
// steps (a stemmer, filters, or any other token transformation).
type Analyzer struct {
	tokenize func(string) []string
	steps    []Step
}

// Option configures an Analyzer and may reject invalid input.
type Option func(*Analyzer) error

// New builds an Analyzer, defaulting to a whitespace tokenizer and an empty
// step chain, then applies opts in order.
func New(opts ...Option) (*Analyzer, error) {
	a := &Analyzer{tokenize: strings.Fields}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// WithTokenizer replaces the function that splits raw text into tokens.
func WithTokenizer(fn func(string) []string) Option {
	return func(a *Analyzer) error {
		if fn == nil {
			return fmt.Errorf("tokenizer function is nil")
		}
		a.tokenize = fn
		return nil
	}
}

// WithStep appends a named step to the analyzer chain (a stemmer or a
// filter, for instance). Because steps apply and accumulate in call order,
// each step's Requires can be checked the instant it is registered against
// every step already in the chain — "tokenizer" is always implicitly
// satisfied, since tokenizing always runs first — which is what lets New
// reject a chain whose order does not preserve the semantics a step
// depends on (a filter that needs stemmed tokens registered before any
// stemmer exists, for example) without a second validation pass.
func WithStep(name string, requires []string, apply func([]string) []string) Option {
	return func(a *Analyzer) error {
		if name == "" {
			return fmt.Errorf("step name must not be empty")
		}
		if name == "tokenizer" {
			return fmt.Errorf("step name %q is reserved", name)
		}
		if apply == nil {
			return fmt.Errorf("step %q has a nil apply function", name)
		}
		for _, s := range a.steps {
			if s.Name == name {
				return fmt.Errorf("duplicate step name: %q", name)
			}
		}

		available := map[string]bool{"tokenizer": true}
		for _, s := range a.steps {
			available[s.Name] = true
		}
		for _, req := range requires {
			if !available[req] {
				return fmt.Errorf("step %q requires %q, which has not run yet in the chain", name, req)
			}
		}

		a.steps = append(a.steps, Step{Name: name, Requires: requires, Apply: apply})
		return nil
	}
}

// Analyze tokenizes text and runs the result through every configured step
// in registration order.
func (a *Analyzer) Analyze(text string) []string {
	tokens := a.tokenize(text)
	for _, s := range a.steps {
		tokens = s.Apply(tokens)
	}
	return tokens
}

// StepNames returns the registered step names in registration order.
func (a *Analyzer) StepNames() []string {
	names := make([]string, len(a.steps))
	for i, s := range a.steps {
		names[i] = s.Name
	}
	return names
}
```

### The runnable demo

The demo composes a lowercase filter, a naive suffix-stripping stemmer that
depends on it, and a stopword filter that also depends on it, then analyzes
a sentence and prints the resulting tokens. It then shows a chain that
requires a stemmer before one has ever been registered getting rejected at
construction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/full-text-search-analyzer"
)

func lowercase(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = strings.ToLower(t)
	}
	return out
}

// stem is a deliberately naive suffix-stripping stemmer, sufficient to
// demonstrate chain ordering without a real stemming library.
func stem(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		switch {
		case strings.HasSuffix(t, "ing"):
			out[i] = strings.TrimSuffix(t, "ing")
		case strings.HasSuffix(t, "ed"):
			out[i] = strings.TrimSuffix(t, "ed")
		case strings.HasSuffix(t, "s"):
			out[i] = strings.TrimSuffix(t, "s")
		default:
			out[i] = t
		}
	}
	return out
}

func removeStopwords(stopwords map[string]bool) func([]string) []string {
	return func(tokens []string) []string {
		var out []string
		for _, t := range tokens {
			if !stopwords[t] {
				out = append(out, t)
			}
		}
		return out
	}
}

func main() {
	stopwords := map[string]bool{"the": true, "are": true, "and": true}

	a, err := analyzer.New(
		analyzer.WithStep("lowercase", nil, lowercase),
		analyzer.WithStep("stemmer", []string{"lowercase"}, stem),
		analyzer.WithStep("stopword", []string{"lowercase"}, removeStopwords(stopwords)),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("chain: %v\n", a.StepNames())
	fmt.Printf("analyzed: %v\n", a.Analyze("The Runners are Running and jumping"))

	_, err = analyzer.New(
		analyzer.WithStep("dedupe", []string{"stemmer"}, func(t []string) []string { return t }),
	)
	fmt.Printf("dependency on unregistered stemmer rejected: %t\n", err != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
chain: [lowercase stemmer stopword]
analyzed: [runner runn jump]
dependency on unregistered stemmer rejected: true
```

### Tests

`TestWithStepValidation` tables every registration failure, including the
key ordering case: a step whose dependency would eventually exist later in
the chain is still rejected, because `WithStep` only ever looks backward.
`TestWithTokenizerRejectsNil` guards the other option.
`TestAnalyzeAppliesStepsInRegistrationOrder` runs the full lowercase →
stemmer → stopword chain and asserts the exact resulting tokens.
`TestWithTokenizerOverride` proves a custom tokenizer is honored.
`TestStepNamesPreservesRegistrationOrder` guards the ordering the
dependency check depends on.

Create `analyzer_test.go`:

```go
package analyzer

import (
	"reflect"
	"strings"
	"testing"
)

func identity(tokens []string) []string { return tokens }

func TestWithStepValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "empty step chain is valid"},
		{
			name:    "reserved name tokenizer",
			opts:    []Option{WithStep("tokenizer", nil, identity)},
			wantErr: true,
		},
		{name: "empty step name", opts: []Option{WithStep("", nil, identity)}, wantErr: true},
		{name: "nil apply function", opts: []Option{WithStep("s1", nil, nil)}, wantErr: true},
		{
			name:    "duplicate step name",
			opts:    []Option{WithStep("s1", nil, identity), WithStep("s1", nil, identity)},
			wantErr: true,
		},
		{
			name:    "dependency on a step not yet registered",
			opts:    []Option{WithStep("stopword", []string{"stemmer"}, identity)},
			wantErr: true,
		},
		{
			name: "dependency on tokenizer is always satisfied",
			opts: []Option{WithStep("first", []string{"tokenizer"}, identity)},
		},
		{
			name: "dependency on an earlier step is satisfied",
			opts: []Option{
				WithStep("lowercase", nil, identity),
				WithStep("stopword", []string{"lowercase"}, identity),
			},
		},
		{
			name: "dependency on a later step fails even if it is added eventually",
			opts: []Option{
				WithStep("stopword", []string{"lowercase"}, identity),
				WithStep("lowercase", nil, identity),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestWithTokenizerRejectsNil(t *testing.T) {
	t.Parallel()

	if _, err := New(WithTokenizer(nil)); err == nil {
		t.Fatal("expected error for a nil tokenizer")
	}
}

func lowercase(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = strings.ToLower(t)
	}
	return out
}

func stem(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		switch {
		case strings.HasSuffix(t, "ing"):
			out[i] = strings.TrimSuffix(t, "ing")
		case strings.HasSuffix(t, "ed"):
			out[i] = strings.TrimSuffix(t, "ed")
		case strings.HasSuffix(t, "s"):
			out[i] = strings.TrimSuffix(t, "s")
		default:
			out[i] = t
		}
	}
	return out
}

func removeStopwords(stopwords map[string]bool) func([]string) []string {
	return func(tokens []string) []string {
		var out []string
		for _, t := range tokens {
			if !stopwords[t] {
				out = append(out, t)
			}
		}
		return out
	}
}

func TestAnalyzeAppliesStepsInRegistrationOrder(t *testing.T) {
	t.Parallel()

	stopwords := map[string]bool{"the": true, "are": true, "and": true}

	a, err := New(
		WithStep("lowercase", nil, lowercase),
		WithStep("stemmer", []string{"lowercase"}, stem),
		WithStep("stopword", []string{"lowercase"}, removeStopwords(stopwords)),
	)
	if err != nil {
		t.Fatal(err)
	}

	got := a.Analyze("The Runners are Running and jumping")
	want := []string{"runner", "runn", "jump"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Analyze() = %v, want %v", got, want)
	}
}

func TestWithTokenizerOverride(t *testing.T) {
	t.Parallel()

	a, err := New(WithTokenizer(func(s string) []string {
		return strings.Split(s, ",")
	}))
	if err != nil {
		t.Fatal(err)
	}

	got := a.Analyze("a,b,c")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Analyze() = %v, want %v", got, want)
	}
}

func TestStepNamesPreservesRegistrationOrder(t *testing.T) {
	t.Parallel()

	a, err := New(
		WithStep("first", nil, identity),
		WithStep("second", []string{"first"}, identity),
	)
	if err != nil {
		t.Fatal(err)
	}
	names := a.StepNames()
	if !reflect.DeepEqual(names, []string{"first", "second"}) {
		t.Fatalf("StepNames() = %v, want [first second]", names)
	}
}
```

## Review

The analyzer chain is correct when a step's declared dependency always
means "already ran," never "exists somewhere in the configuration" — those
are different claims, and only the first one matches what `Analyze`
actually does by running steps strictly in registration order. Checking
dependencies inside `WithStep` itself, against the steps accumulated so
far, is what makes that guarantee free: there is no separate topological
sort or second pass over the finished chain, because the chain's
registration order *is* its execution order, and `WithStep` can see
everything that has already been committed to it. Reserving the name
`"tokenizer"` for the one stage that is always implicitly first keeps that
reasoning simple — a dependency on `"tokenizer"` never needs a special case
elsewhere, it is just always in the `available` set.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [Elasticsearch: analyzers, tokenizers, and token filters](https://www.elastic.co/guide/en/elasticsearch/reference/current/analysis-anatomy.html)
- [Porter stemming algorithm](https://tartarus.org/martin/PorterStemmer/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-distributed-cache-replication.md](29-distributed-cache-replication.md) | Next: [31-feature-flag-evaluator-context.md](31-feature-flag-evaluator-context.md)
