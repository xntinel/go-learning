# Exercise 3: Tab Completion

Tab completion is the third standalone primitive of the REPL. The feature itself is small — given the prefix the user has typed, return the candidates that start with it — but the design choice that makes it valuable is expressing it as an *interface*. The run loop will depend on that interface, never on a concrete list, so the default keyword-and-identifier completer can later be swapped for a smarter one that walks the interpreter's scope chain, with no change to the loop. This exercise builds the interface and a flat-list default implementation in isolation.

A completer has exactly one job and one method, which is what makes it a clean extension point. The REPL extracts the word the cursor sits behind, asks the completer for matches, and decides what to do with them (complete inline if there is one, list them if there are several). All the REPL needs to know is `Complete(prefix) []string`; everything about *where* the candidates come from is the completer's business.

## What you'll build

```text
complete.go        Completer interface + BasicCompleter (flat word list)
cmd/
  demo/
    main.go        seed a completer, complete a few prefixes, add an identifier
complete_test.go   prefix match, dedup on Add, no-match, empty prefix returns all
```

- Files: `complete.go`, `cmd/demo/main.go`, `complete_test.go`.
- Implement: `Completer` interface, `BasicCompleter`, `NewBasicCompleter`, `Add`, `Complete`.
- Test: `complete_test.go` checks prefix matching, duplicate-skipping `Add`, the no-match case, and that an empty prefix returns every stored word in insertion order.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/07-repl-line-editing/03-tab-completion/cmd/demo && cd go-solutions/40-capstone-language-interpreter/07-repl-line-editing/03-tab-completion
```

### Interface first, implementation second

`Completer` is a one-method interface: `Complete(prefix string) []string`. Defining it first, before any concrete type, is the whole point — it states the contract the REPL programs against and leaves the source of candidates open. `BasicCompleter` is the default: a flat slice of words (the language keywords, the built-in function names, and any identifiers the session has defined so far) returned in insertion order. `NewBasicCompleter` seeds it from a slice, copying so the caller cannot mutate the completer's storage out from under it. `Add` appends a new word but skips it if already present, so registering an identifier on every `let` does not grow the list with duplicates.

`Complete` is a linear scan returning every word with the given prefix, preserving insertion order so the output is deterministic. An empty prefix matches everything, which is the natural "show me what is available" behavior when the user presses Tab on whitespace. Insertion order rather than sorted order is a deliberate choice: keywords seeded first stay first, and runtime identifiers appear after them in the order they were defined, which reads more naturally than an alphabetical jumble.

Create `complete.go`:

```go
package completion

import "strings"

// Completer supplies completion candidates for a given prefix.
type Completer interface {
	Complete(prefix string) []string
}

// BasicCompleter holds a flat list of words (keywords, built-ins, identifiers).
type BasicCompleter struct {
	words []string
}

// NewBasicCompleter returns a BasicCompleter seeded with words.
func NewBasicCompleter(words []string) *BasicCompleter {
	cp := make([]string, len(words))
	copy(cp, words)
	return &BasicCompleter{words: cp}
}

// Add inserts word, skipping duplicates.
func (c *BasicCompleter) Add(word string) {
	for _, w := range c.words {
		if w == word {
			return
		}
	}
	c.words = append(c.words, word)
}

// Complete returns every stored word that starts with prefix, in insertion order.
func (c *BasicCompleter) Complete(prefix string) []string {
	var out []string
	for _, w := range c.words {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}
```

The copy in `NewBasicCompleter` is a small but real correctness point: without it, a caller that keeps and later mutates the slice it passed in would silently change the completer's word list. Because `BasicCompleter` satisfies the `Completer` interface, the REPL can accept either it or any future scope-aware completer through the same parameter.

### The runnable demo

The demo seeds a completer with Monkey keywords, completes a couple of prefixes, registers a new identifier the way the REPL would after a `let`, and shows that the empty prefix lists everything. The scan is deterministic, so the printed slices are fixed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/completion"
)

func main() {
	c := completion.NewBasicCompleter([]string{
		"let", "fn", "if", "else", "return", "for",
	})

	fmt.Println("Complete(\"f\"):  ", c.Complete("f"))
	fmt.Println("Complete(\"re\"): ", c.Complete("re"))

	c.Add("returnValue") // as the REPL would after: let returnValue = ...
	fmt.Println("Complete(\"re\"): ", c.Complete("re"))

	fmt.Println("Complete(\"\"):   ", c.Complete(""))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Complete("f"):   [fn for]
Complete("re"):  [return]
Complete("re"):  [return returnValue]
Complete(""):    [let fn if else return for returnValue]
```

### Tests

The tests cover the four behaviors the REPL relies on: a prefix matching more than one word, `Add` refusing a duplicate, a prefix matching nothing returning an empty slice, and the empty prefix returning every word. The empty-prefix case is what a Tab on whitespace triggers, so it is a real path, not a curiosity.

Create `complete_test.go`:

```go
package completion

import (
	"fmt"
	"testing"
)

func TestComplete(t *testing.T) {
	t.Parallel()
	// Words starting with "f": "fn", "for" -> expect 2.
	c := NewBasicCompleter([]string{"let", "fn", "if", "else", "return", "for", "while"})
	got := c.Complete("f")
	if len(got) != 2 {
		t.Fatalf("Complete(%q) = %v, want 2 results", "f", got)
	}
}

func TestAddDeduplicates(t *testing.T) {
	t.Parallel()
	c := NewBasicCompleter(nil)
	c.Add("myVar")
	c.Add("myVar") // duplicate
	got := c.Complete("my")
	if len(got) != 1 {
		t.Fatalf("Complete(%q) = %v, want 1 result", "my", got)
	}
}

func TestNoMatch(t *testing.T) {
	t.Parallel()
	c := NewBasicCompleter([]string{"let", "fn"})
	got := c.Complete("x")
	if len(got) != 0 {
		t.Fatalf("Complete(%q) = %v, want empty", "x", got)
	}
}

func TestEmptyPrefixReturnsAll(t *testing.T) {
	t.Parallel()
	c := NewBasicCompleter([]string{"let", "fn", "if"})
	got := c.Complete("")
	if len(got) != 3 {
		t.Fatalf("Complete(%q) = %v, want all 3 words", "", got)
	}
}

func TestSeedIsCopied(t *testing.T) {
	t.Parallel()
	seed := []string{"let", "fn"}
	c := NewBasicCompleter(seed)
	seed[0] = "MUTATED" // must not affect the completer
	if got := c.Complete("let"); len(got) != 1 {
		t.Fatalf("Complete(%q) = %v, want the seed to have been copied", "let", got)
	}
}

func ExampleBasicCompleter_Complete() {
	c := NewBasicCompleter([]string{"let", "fn", "if", "else", "return"})
	for _, s := range c.Complete("el") {
		fmt.Println(s)
	}
	// Output:
	// else
}
```

## Review

The completer is correct when matching is by prefix, order is insertion order, and the storage is private. `Complete("f")` over a keyword list returns exactly the two `f`-words; `Complete("")` returns all of them; `Complete` on an unknown prefix returns an empty slice, not nil-versus-empty confusion. `Add` must skip a word already present so registering identifiers on every `let` cannot bloat the list, and the seed slice must be copied so a caller mutating its own slice cannot corrupt the completer — the `TestSeedIsCopied` case proves it. The `Example` function is checked by `go test` against its `// Output:` comment, so the documented behavior cannot silently drift.

Common mistakes for this module. Sorting the results changes the natural keyword-then-identifier order and surprises the user; keep insertion order. Skipping the copy in the constructor aliases the caller's slice, so a later mutation there silently rewrites the completer. And returning matches by substring rather than prefix makes Tab complete `et` to `let`, which is not what prefix completion means.

## Resources

- [pkg.go.dev/strings#HasPrefix](https://pkg.go.dev/strings#HasPrefix) — the prefix test at the heart of `Complete`.
- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — why a one-method interface is the right shape for a pluggable completer.
- [GNU Readline: completion](https://tiswww.case.edu/php/chet/readline/readline.html#Completion) — the completion model REPLs and shells adopted.

---

Back to [02-history-persistence.md](02-history-persistence.md) | Next: [04-pretty-printing.md](04-pretty-printing.md)
