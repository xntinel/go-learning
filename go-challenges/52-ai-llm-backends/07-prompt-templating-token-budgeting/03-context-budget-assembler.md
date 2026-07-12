# Exercise 3: A context-window budget assembler with rune-safe truncation

The real on-the-job task behind a RAG or agent prompt is admission control: given a
fixed context window, a reserved output allowance, a mandatory system prompt, and an
ordered list of candidate turns and chunks, fit the highest-value items into what
remains and evict the rest under a deterministic policy — never overflowing, always
keeping the system prompt. This exercise builds that `Budget` assembler plus a
token-boundary-safe `TruncateToTokens` that encodes, slices, and decodes so it never
cuts a multibyte rune or corrupts a token.

This module is self-contained: its own `go mod init`, its own `TokenCounter`
interface (it does not import Exercise 2), its own demo and tests. It imports the
tiktoken tokenizer for boundary-safe truncation, so the offline gate will not fetch
that module and the build step is expected to fail without network access. The bar
is `gofmt`-clean, real verified APIs, and prose that matches the code; the budgeting
logic itself is exercised in tests with a deterministic in-package counter.

## What you'll build

```text
budget/                     independent module: example.com/budget
  go.mod                    go 1.26; requires tiktoken-go/tokenizer
  budget.go                 TokenCounter; Item; Budget; Result; Assemble; TruncateToTokens; CodecCounter; sentinels
  cmd/
    demo/
      main.go               fit three turns into a window, then truncate a string to 4 tokens
  budget_test.go            no-overflow, system-retained, eviction order, reserve error, property test
```

- Files: `budget.go`, `cmd/demo/main.go`, `budget_test.go`.
- Implement: a `Budget` with `Assemble(system string, items []Item) (Result, error)` that always keeps the system prompt, fits items by priority under a deterministic policy, and returns kept/dropped plus tokens used; `TruncateToTokens(codec, text, max)` using encode/slice/decode; a `CodecCounter` adapter; and sentinels `ErrOutputReserveTooLarge` and `ErrBudgetExceeded`.
- Test: kept tokens plus reserve never exceed the window; the system prompt is always retained; lowest-priority/oldest items are dropped first; `ErrOutputReserveTooLarge` when the reserve is at least the window; truncation output is valid UTF-8 and its count stays within the limit; a property test that assembling then counting never exceeds the declared budget.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/tiktoken-go/tokenizer@latest
```

### The allocation model

The window is shared between the prompt and the completion:
`window = system + kept-input + reserved-output`. `Assemble` enforces that equation.
First it rejects a reserve that is at least the whole window
(`ErrOutputReserveTooLarge`) — you cannot reserve all of the window for output and
still send a prompt. Then it counts the mandatory system prompt, which is *always*
retained; if the system prompt plus the reserve already exceed the window, there is
no room for anything and it returns `ErrBudgetExceeded`. Whatever is left,
`available = window - reserve - systemTokens`, is the space items compete for.

The eviction policy is deterministic: candidates are ordered by priority descending,
with more-recent items (higher original index) winning ties, then greedily admitted
while they fit. This keeps the highest-value and most-recent context and drops the
oldest, lowest-priority items first — the sane default the concepts argue for. The
returned `Kept` and `Dropped` slices preserve the original chronological order so the
assembled prompt reads coherently, independent of the admission order.

### Boundary-safe truncation

`TruncateToTokens` is the piece that must never be done with `text[:n]`. Cutting on a
byte offset can slice a multibyte rune in half (invalid UTF-8) and always risks
cutting mid-token, changing meaning. The only correct approach is to encode the text
to token IDs, slice the ID slice to the limit, and decode back. If the truncation
boundary happens to land inside a multibyte character, the decoded bytes can be
invalid UTF-8, so the result is passed through `strings.ToValidUTF8` to drop the
trailing invalid bytes — guaranteeing both a valid string and a hard bound on the
token count.

Create `budget.go`:

```go
package budget

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tiktoken-go/tokenizer"
)

// Sentinel errors for the two ways a budget cannot be satisfied.
var (
	// ErrOutputReserveTooLarge is returned when the reserved output is at least
	// the whole window, leaving no room for a prompt.
	ErrOutputReserveTooLarge = errors.New("output reserve is at least the window")
	// ErrBudgetExceeded is returned when the mandatory system prompt plus the
	// reserve already exceed the window.
	ErrBudgetExceeded = errors.New("system prompt plus reserve exceeds the window")
)

// TokenCounter counts tokens for a string. It is the only dependency Assemble has
// on tokenization, so the counter can be exact or approximate.
type TokenCounter interface {
	Count(text string) (int, error)
}

// CodecCounter adapts a tiktoken Codec to TokenCounter.
type CodecCounter struct {
	Codec tokenizer.Codec
}

// Count returns the exact token count via the underlying codec.
func (c CodecCounter) Count(text string) (int, error) { return c.Codec.Count(text) }

// Item is a candidate piece of context: a conversation turn or a retrieved chunk.
// Higher Priority is more valuable and is kept first.
type Item struct {
	ID       string
	Text     string
	Priority int
}

// Result reports the assembly decision.
type Result struct {
	Kept    []Item // in original order
	Dropped []Item // in original order
	Used    int    // tokens used by system + kept items
}

// Budget fits context into a window while reserving room for the completion.
type Budget struct {
	Window        int
	OutputReserve int
	counter       TokenCounter
}

// NewBudget builds a Budget over a token window with a reserved output allowance.
func NewBudget(window, outputReserve int, counter TokenCounter) *Budget {
	return &Budget{Window: window, OutputReserve: outputReserve, counter: counter}
}

// Assemble fits items into window - reserve - system, keeping the system prompt and
// the highest-priority, most-recent items. It never lets kept + reserve exceed the
// window.
func (b *Budget) Assemble(system string, items []Item) (Result, error) {
	if b.OutputReserve >= b.Window {
		return Result{}, fmt.Errorf("budget: reserve %d, window %d: %w", b.OutputReserve, b.Window, ErrOutputReserveTooLarge)
	}
	sysTokens, err := b.counter.Count(system)
	if err != nil {
		return Result{}, fmt.Errorf("budget: count system: %w", err)
	}
	if sysTokens+b.OutputReserve > b.Window {
		return Result{}, fmt.Errorf("budget: system %d + reserve %d, window %d: %w", sysTokens, b.OutputReserve, b.Window, ErrBudgetExceeded)
	}

	type cand struct {
		item   Item
		index  int
		tokens int
	}
	cands := make([]cand, len(items))
	for i, it := range items {
		n, err := b.counter.Count(it.Text)
		if err != nil {
			return Result{}, fmt.Errorf("budget: count item %q: %w", it.ID, err)
		}
		cands[i] = cand{item: it, index: i, tokens: n}
	}

	// Admission order: highest priority first, ties broken by most recent.
	order := make([]cand, len(cands))
	copy(order, cands)
	slices.SortStableFunc(order, func(a, b cand) int {
		if a.item.Priority != b.item.Priority {
			return cmp.Compare(b.item.Priority, a.item.Priority)
		}
		return cmp.Compare(b.index, a.index)
	})

	remaining := b.Window - b.OutputReserve - sysTokens
	kept := make(map[int]bool, len(order))
	used := sysTokens
	for _, c := range order {
		if c.tokens <= remaining {
			kept[c.index] = true
			remaining -= c.tokens
			used += c.tokens
		}
	}

	res := Result{Used: used}
	for i, c := range cands { // cands is in original order
		if kept[i] {
			res.Kept = append(res.Kept, c.item)
		} else {
			res.Dropped = append(res.Dropped, c.item)
		}
	}
	return res, nil
}

// TruncateToTokens shortens text to at most max tokens without cutting a multibyte
// rune or corrupting a token: encode to IDs, slice, decode, then drop any trailing
// invalid UTF-8 left by a boundary landing inside a multibyte character.
func TruncateToTokens(codec tokenizer.Codec, text string, max int) (string, error) {
	if max <= 0 {
		return "", nil
	}
	ids, _, err := codec.Encode(text)
	if err != nil {
		return "", fmt.Errorf("budget: encode: %w", err)
	}
	if len(ids) <= max {
		return text, nil
	}
	out, err := codec.Decode(ids[:max])
	if err != nil {
		return "", fmt.Errorf("budget: decode: %w", err)
	}
	return strings.ToValidUTF8(out, ""), nil
}
```

### The runnable demo

The demo fits three turns into a 60-token window with a 10-token output reserve,
using a rune-based counter so the budgeting numbers are deterministic, then truncates
a sentence to four tokens with the tiktoken codec.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"unicode/utf8"

	"example.com/budget"
	"github.com/tiktoken-go/tokenizer"
)

// runeCounter counts one token per rune, so the demo's budgeting math is exact.
type runeCounter struct{}

func (runeCounter) Count(text string) (int, error) { return utf8.RuneCountInString(text), nil }

func main() {
	b := budget.NewBudget(60, 10, runeCounter{})
	system := "You are a triage assistant." // 27 runes
	items := []budget.Item{
		{ID: "turn-1", Text: "disk is full", Priority: 1},
		{ID: "turn-2", Text: "restarted the node", Priority: 2},
		{ID: "turn-3", Text: "latency spiking now", Priority: 3},
	}

	res, err := b.Assemble(system, items)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("used=%d reserve=%d window=%d\n", res.Used, 10, 60)
	for _, it := range res.Kept {
		fmt.Printf("kept %s\n", it.ID)
	}
	for _, it := range res.Dropped {
		fmt.Printf("dropped %s\n", it.ID)
	}

	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		log.Fatal(err)
	}
	short, err := budget.TruncateToTokens(codec, "the quick brown fox jumps over the lazy dog", 4)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("truncated: %q\n", short)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
used=46 reserve=10 window=60
kept turn-3
dropped turn-1
dropped turn-2
truncated: "the quick brown fox"
```

With the system prompt at 27 tokens and a 10-token reserve, only 23 tokens remain.
The highest-priority `turn-3` (19 tokens) fits; the two older, lower-priority turns
do not, and are dropped in original order. Kept plus reserve is 56, safely under 60.

### Tests

The budgeting tests use an in-package rune counter, so they are exact and depend on
no network — the truncation test uses the real codec and exercises multibyte input.
The table asserts the two invariants that make a budget trustworthy: kept tokens plus
the reserve never exceed the window, and the system prompt is always present. A
dedicated case checks eviction order, another asserts `ErrOutputReserveTooLarge`, and
a property-style test hammers randomized item sizes and confirms the assembled result
never busts the budget.

Create `budget_test.go`:

```go
package budget

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// runeCounter counts one token per rune for exact, deterministic budgeting tests.
type runeCounter struct{}

func (runeCounter) Count(text string) (int, error) { return utf8.RuneCountInString(text), nil }

func TestAssembleNeverOverflows(t *testing.T) {
	t.Parallel()
	b := NewBudget(30, 5, runeCounter{})
	items := []Item{
		{ID: "a", Text: strings.Repeat("x", 10), Priority: 1},
		{ID: "b", Text: strings.Repeat("y", 10), Priority: 2},
		{ID: "c", Text: strings.Repeat("z", 10), Priority: 3},
	}
	res, err := b.Assemble("sys", items) // "sys" = 3 tokens
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if res.Used+b.OutputReserve > b.Window {
		t.Fatalf("overflow: used %d + reserve %d > window %d", res.Used, b.OutputReserve, b.Window)
	}
}

func TestSystemAlwaysRetained(t *testing.T) {
	t.Parallel()
	// A window that fits the system prompt and reserve but no items must still
	// succeed and account for the system tokens.
	b := NewBudget(12, 2, runeCounter{})
	res, err := b.Assemble("system!", []Item{{ID: "x", Text: strings.Repeat("q", 50), Priority: 9}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if res.Used != 7 { // len("system!") == 7 runes
		t.Fatalf("Used = %d, want 7 (system only)", res.Used)
	}
	if len(res.Kept) != 0 || len(res.Dropped) != 1 {
		t.Fatalf("kept=%d dropped=%d, want 0 kept and 1 dropped", len(res.Kept), len(res.Dropped))
	}
}

func TestEvictionOrder(t *testing.T) {
	t.Parallel()
	items := []Item{
		{ID: "low", Text: strings.Repeat("a", 8), Priority: 1},
		{ID: "high", Text: strings.Repeat("b", 8), Priority: 5},
	}
	// Each item is 8 tokens; a 12-token window holds only one, so the low-priority
	// item must be the one dropped.
	b := NewBudget(12, 0, runeCounter{})
	res, err := b.Assemble("", items)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].ID != "high" {
		t.Fatalf("kept = %v, want only the high-priority item", res.Kept)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].ID != "low" {
		t.Fatalf("dropped = %v, want the low-priority item", res.Dropped)
	}
}

func TestReserveTooLarge(t *testing.T) {
	t.Parallel()
	b := NewBudget(100, 100, runeCounter{})
	_, err := b.Assemble("sys", nil)
	if !errors.Is(err, ErrOutputReserveTooLarge) {
		t.Fatalf("err = %v, want ErrOutputReserveTooLarge", err)
	}
}

func TestSystemPlusReserveExceedsWindow(t *testing.T) {
	t.Parallel()
	b := NewBudget(10, 6, runeCounter{})
	_, err := b.Assemble("system prompt", nil) // 13 tokens + 6 reserve > 10
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

func TestAssembleBudgetProperty(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		window := 20 + r.IntN(200)
		reserve := r.IntN(window)
		n := r.IntN(12)
		items := make([]Item, n)
		for i := range items {
			items[i] = Item{
				ID:       fmt.Sprintf("i%d", i),
				Text:     strings.Repeat("t", 1+r.IntN(40)),
				Priority: r.IntN(4),
			}
		}
		b := NewBudget(window, reserve, runeCounter{})
		res, err := b.Assemble("s", items) // "s" = 1 token, always fits here
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		if res.Used+reserve > window {
			t.Fatalf("busted budget: used %d + reserve %d > window %d", res.Used, reserve, window)
		}
		if len(res.Kept)+len(res.Dropped) != n {
			t.Fatalf("lost items: kept %d + dropped %d != %d", len(res.Kept), len(res.Dropped), n)
		}
	}
}

func TestTruncateToTokensValidUTF8(t *testing.T) {
	t.Parallel()
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Multibyte input: CJK characters, each three bytes and often split across
	// BPE tokens, so a byte-offset cut would corrupt them.
	text := "hello 世界 budgeting under a hard ceiling 日本語 テスト"
	for _, max := range []int{1, 2, 3, 5, 8} {
		got, err := TruncateToTokens(codec, text, max)
		if err != nil {
			t.Fatalf("TruncateToTokens: %v", err)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncated to %d tokens is not valid UTF-8: %q", max, got)
		}
		n, err := codec.Count(got)
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n > max {
			t.Fatalf("truncated count %d exceeds max %d", n, max)
		}
	}
}

// Your turn: add a summarize-then-fit policy. Give Budget an optional
// Summarize func(Item) (Item, error); when an item does not fit, summarize it and
// retry before dropping. Then assert here that a large item is kept in summarized
// form instead of dropped.
func TestSummarizeHook(t *testing.T) {
	t.Parallel()
	b := NewBudget(12, 0, runeCounter{})
	res, err := b.Assemble("", []Item{{ID: "big", Text: strings.Repeat("x", 50), Priority: 1}})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	// Current behavior with no hook: the oversized item is dropped.
	if len(res.Dropped) != 1 || res.Dropped[0].ID != "big" {
		t.Fatalf("dropped = %v, want the oversized item dropped until you add Summarize", res.Dropped)
	}
}

func ExampleBudget_Assemble() {
	b := NewBudget(25, 5, runeCounter{})
	items := []Item{
		{ID: "old", Text: "aaaaa", Priority: 1},         // 5 tokens, low priority
		{ID: "recent", Text: "bbbbbbbbbb", Priority: 5}, // 10 tokens, high priority
	}
	res, _ := b.Assemble("system", items) // "system" = 6 tokens
	fmt.Printf("used=%d kept=%d dropped=%d\n", res.Used, len(res.Kept), len(res.Dropped))
	// Output: used=16 kept=1 dropped=1
}
```

## Review

The assembler is correct when it never overflows and never drops the system prompt.
`TestAssembleNeverOverflows` and `TestAssembleBudgetProperty` prove the first across
fixed and randomized inputs; `TestSystemAlwaysRetained` proves the second even when
no item fits. `TestEvictionOrder` pins the policy — highest priority survives, lowest
is dropped first — and `TestReserveTooLarge` plus `TestSystemPlusReserveExceedsWindow`
prove the two sentinel paths, which is how a caller distinguishes "reserve is
misconfigured" from "system prompt is simply too big for this window."

The mistakes to avoid are the concepts made concrete. Do not budget to the full
window and forget the output reserve — `Used + OutputReserve` must fit, and the
tests fail the instant it does not. Do not truncate on byte or character offsets:
`TestTruncateToTokensValidUTF8` feeds multibyte CJK precisely because a naive
`text[:n]` would produce invalid UTF-8 there, whereas encode/slice/decode plus
`strings.ToValidUTF8` guarantees a valid string within the token bound. And keep the
eviction policy deterministic — the sort is stable and total, so the same input
always yields the same kept/dropped decision, which is what makes the whole layer
testable.

## Resources

- [`tiktoken-go/tokenizer`](https://pkg.go.dev/github.com/tiktoken-go/tokenizer) — `Codec.Encode`/`Decode` for boundary-safe truncation and `Codec.Count`.
- [`strings.ToValidUTF8`](https://pkg.go.dev/strings#ToValidUTF8) and [`unicode/utf8.ValidString`](https://pkg.go.dev/unicode/utf8#ValidString) — guaranteeing the truncated result is valid UTF-8.
- [`slices.SortStableFunc`](https://pkg.go.dev/slices#SortStableFunc) and [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the deterministic, total ordering the eviction policy relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-token-counting.md](02-token-counting.md) | Next: [../08-llm-resilience-and-caching/00-concepts.md](../08-llm-resilience-and-caching/00-concepts.md)
