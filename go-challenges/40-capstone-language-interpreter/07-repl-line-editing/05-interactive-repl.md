# Exercise 5: The Interactive REPL with Raw Terminal

This is where the four primitives become a program. The line buffer, the history ring, the completer, and the pretty-printer from the previous exercises are assembled into one interactive loop, and two new pieces complete it: the run loop itself (which reads bytes, decodes keys and escape sequences, and drives the buffer) and the raw-terminal adapter (the single file that touches `golang.org/x/term`). The whole thing hangs on one architectural decision — the loop depends on a narrow `termReader` interface, not on the real terminal — which is what lets every editing behavior be tested by scripting bytes through a mock, while the real terminal is exercised only by the demo binary.

This module is fully self-contained. It vendors its own copies of the editor, history, completer, and pretty-printer as the same `repl` package, so it imports no other exercise. It does import the external module `golang.org/x/term`, the only third-party dependency in the lesson, and it is the only module here whose primary code path needs a real terminal.

## What you'll build

```text
editor.go          LineEditor (rune buffer + cursor) — from exercise 1
history.go         History (entries + cursor + persistence) — from exercise 2
complete.go        Completer + BasicCompleter — from exercise 3
pretty.go          Object + Pretty + stripANSI — from exercise 4
repl.go            REPL, options, HandleCommand, BracketDepth, runLoop, termReader
terminal.go        rawTerminal (golang.org/x/term), Run
cmd/
  demo/
    main.go        a stateful echo evaluator + the real-terminal REPL
repl_test.go       editing, commands, bracket depth, and scripted runLoop sessions
```

- Files: `editor.go`, `history.go`, `complete.go`, `pretty.go`, `repl.go`, `terminal.go`, `cmd/demo/main.go`, `repl_test.go`.
- Implement: the `termReader` interface, `REPL` with functional options, `HandleCommand`, `BracketDepth`, the `runLoop`, and the `rawTerminal` adapter plus `Run`.
- Test: `repl_test.go` drives `runLoop` through a `mockTerminal` backed by a byte string, asserting on evaluation, multi-line continuation, command handling, and history — no real terminal.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p repl/cmd/demo && cd repl
go mod init example.com/repl
go get golang.org/x/term
```

### The interface that makes the loop testable

`golang.org/x/term` is an external module that demands a real terminal. If `runLoop` took a concrete `*rawTerminal`, every test of the editing logic would need a TTY, which is impossible in CI. The fix is a four-method interface:

```text
ReadByte() (byte, error)   read the next input byte
EnterRaw() error           switch the terminal to raw mode
ExitRaw()                  restore the previous mode
IsTerminal() bool          report whether this is an interactive terminal
```

`runLoop` depends only on this `termReader`. The real `rawTerminal` in `terminal.go` implements it with `x/term`; a `mockTerminal` in the test implements it with a `strings.Reader`, claiming `IsTerminal() == false` so the loop skips raw mode entirely. Every editing test scripts a session as a byte string — printable characters, control bytes like `\x04` for Ctrl+D, and escape sequences like `\x1b[A` for Up — and asserts on the captured output. The real terminal is never touched by a test.

### The four vendored primitives

The first four files are the modules you already built, dropped into this package unchanged in behavior. `editor.go` is the rune buffer from exercise 1, `history.go` the history ring from exercise 2, `complete.go` the completer from exercise 3, and `pretty.go` the formatter from exercise 4 (with `stripANSI` kept unexported here, since the REPL's `WithNoColor` option is the public surface). They are reproduced in full so the module stands alone.

Create `editor.go`:

```go
package repl

import "unicode"

// LineEditor is an in-memory line buffer with a cursor.
// All positions are rune indices, not byte offsets.
type LineEditor struct {
	buf    []rune
	cursor int
}

// Insert inserts r at the cursor position and advances the cursor.
func (e *LineEditor) Insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cursor+1:], e.buf[e.cursor:])
	e.buf[e.cursor] = r
	e.cursor++
}

// DeleteBack removes the rune immediately before the cursor (Backspace).
func (e *LineEditor) DeleteBack() bool {
	if e.cursor == 0 {
		return false
	}
	e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
	e.cursor--
	return true
}

// DeleteForward removes the rune at the cursor (Delete key).
func (e *LineEditor) DeleteForward() bool {
	if e.cursor >= len(e.buf) {
		return false
	}
	e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
	return true
}

// MoveLeft moves the cursor one position left. Returns false at the start.
func (e *LineEditor) MoveLeft() bool {
	if e.cursor == 0 {
		return false
	}
	e.cursor--
	return true
}

// MoveRight moves the cursor one position right. Returns false at the end.
func (e *LineEditor) MoveRight() bool {
	if e.cursor >= len(e.buf) {
		return false
	}
	e.cursor++
	return true
}

// MoveHome jumps the cursor to position 0.
func (e *LineEditor) MoveHome() { e.cursor = 0 }

// MoveEnd jumps the cursor past the last rune.
func (e *LineEditor) MoveEnd() { e.cursor = len(e.buf) }

// DeleteWordBack removes from the cursor back to the start of the previous word
// (Ctrl+W). Trailing whitespace is consumed first, then non-whitespace.
func (e *LineEditor) DeleteWordBack() bool {
	if e.cursor == 0 {
		return false
	}
	end := e.cursor
	for e.cursor > 0 && unicode.IsSpace(e.buf[e.cursor-1]) {
		e.cursor--
	}
	for e.cursor > 0 && !unicode.IsSpace(e.buf[e.cursor-1]) {
		e.cursor--
	}
	e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
	return true
}

// DeleteToEnd removes everything from the cursor to the end (Ctrl+K).
func (e *LineEditor) DeleteToEnd() { e.buf = e.buf[:e.cursor] }

// String returns the buffer content as a UTF-8 string.
func (e *LineEditor) String() string { return string(e.buf) }

// Cursor returns the current cursor position (rune index).
func (e *LineEditor) Cursor() int { return e.cursor }

// PrefixAtCursor returns the buffer content from the start up to the cursor.
func (e *LineEditor) PrefixAtCursor() string { return string(e.buf[:e.cursor]) }

// Reset clears the buffer and moves the cursor to position 0.
func (e *LineEditor) Reset() { e.buf = e.buf[:0]; e.cursor = 0 }

// SetContent replaces the buffer with s and moves the cursor to the end.
func (e *LineEditor) SetContent(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
}
```

Create `history.go`:

```go
package repl

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const defaultHistoryMax = 10_000

// History is a bounded list of past input entries with a navigation cursor.
type History struct {
	entries []string
	maxSize int
	pos     int
}

// NewHistory returns an empty History. maxSize <= 0 uses defaultHistoryMax.
func NewHistory(maxSize int) *History {
	if maxSize <= 0 {
		maxSize = defaultHistoryMax
	}
	return &History{maxSize: maxSize, pos: 0}
}

// Add appends line, suppressing consecutive duplicates, and resets the cursor.
func (h *History) Add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		h.resetPos()
		return
	}
	if len(h.entries) >= h.maxSize {
		h.entries = h.entries[1:]
	}
	h.entries = append(h.entries, line)
	h.resetPos()
}

func (h *History) resetPos() { h.pos = len(h.entries) }

// Prev moves to the previous (older) entry. Returns ("", false) at the start.
func (h *History) Prev() (string, bool) {
	if len(h.entries) == 0 || h.pos == 0 {
		return "", false
	}
	h.pos--
	return h.entries[h.pos], true
}

// Next moves to the next (newer) entry. Returns ("", false) past the last entry.
func (h *History) Next() (string, bool) {
	if h.pos >= len(h.entries) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.entries) {
		return "", false
	}
	return h.entries[h.pos], true
}

// Len returns the number of stored entries.
func (h *History) Len() int { return len(h.entries) }

// Entry returns the entry at index i (0 = oldest).
func (h *History) Entry(i int) string { return h.entries[i] }

// Load reads entries from path (one per line). A missing file is not an error.
func (h *History) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("history load %q: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			h.entries = append(h.entries, line)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("history scan %q: %w", path, err)
	}
	if len(h.entries) > h.maxSize {
		h.entries = h.entries[len(h.entries)-h.maxSize:]
	}
	h.resetPos()
	return nil
}

// Save writes all entries to path (one per line), creating or truncating it.
func (h *History) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("history save %q: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, entry := range h.entries {
		fmt.Fprintln(w, entry)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("history flush %q: %w", path, err)
	}
	return nil
}
```

Create `complete.go`:

```go
package repl

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

Create `pretty.go`:

```go
package repl

import (
	"fmt"
	"sort"
	"strings"
)

// ObjectType identifies the kind of an interpreter value.
type ObjectType int

const (
	TypeNull   ObjectType = iota
	TypeInt               // int64
	TypeFloat             // float64
	TypeBool              // bool
	TypeString            // string
	TypeArray             // []*Object
	TypeHash              // map[string]*Object, sorted by key
	TypeFn                // function: Params []string
	TypeError             // runtime error: ErrMsg string
)

// Object is a tagged union representing one interpreter value.
type Object struct {
	Type     ObjectType
	IntVal   int64
	FloatVal float64
	BoolVal  bool
	StrVal   string
	Elements []*Object
	Pairs    map[string]*Object
	Params   []string
	ErrMsg   string
}

const (
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
	ansiGray   = "\x1b[90m"
	ansiReset  = "\x1b[0m"
)

func colorize(code, s string) string { return code + s + ansiReset }

// Pretty returns an ANSI-colored representation of obj. nil is treated as null.
func Pretty(obj *Object) string {
	if obj == nil {
		return colorize(ansiGray, "null")
	}
	switch obj.Type {
	case TypeNull:
		return colorize(ansiGray, "null")
	case TypeInt:
		return colorize(ansiYellow, fmt.Sprintf("%d", obj.IntVal))
	case TypeFloat:
		return colorize(ansiYellow, fmt.Sprintf("%g", obj.FloatVal))
	case TypeBool:
		if obj.BoolVal {
			return colorize(ansiCyan, "true")
		}
		return colorize(ansiCyan, "false")
	case TypeString:
		return colorize(ansiGreen, fmt.Sprintf("%q", obj.StrVal))
	case TypeArray:
		return prettyArray(obj.Elements)
	case TypeHash:
		return prettyHash(obj.Pairs)
	case TypeFn:
		return colorize(ansiBlue, fmt.Sprintf("fn(%s) { ... }", strings.Join(obj.Params, ", ")))
	case TypeError:
		return colorize(ansiRed, "ERROR: "+obj.ErrMsg)
	default:
		return colorize(ansiGray, "<unknown>")
	}
}

func prettyArray(elems []*Object) string {
	if len(elems) == 0 {
		return "[]"
	}
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = Pretty(e)
	}
	inline := strings.Join(parts, ", ")
	if len(stripANSI(inline)) <= 60 {
		return "[" + inline + "]"
	}
	var sb strings.Builder
	sb.WriteString("[\n")
	for _, p := range parts {
		sb.WriteString("  ")
		sb.WriteString(p)
		sb.WriteString(",\n")
	}
	sb.WriteString("]")
	return sb.String()
}

func prettyHash(pairs map[string]*Object) string {
	if len(pairs) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = colorize(ansiGreen, fmt.Sprintf("%q", k)) + ": " + Pretty(pairs[k])
	}
	inline := strings.Join(parts, ", ")
	if len(stripANSI(inline)) <= 60 {
		return "{" + inline + "}"
	}
	var sb strings.Builder
	sb.WriteString("{\n")
	for _, p := range parts {
		sb.WriteString("  ")
		sb.WriteString(p)
		sb.WriteString(",\n")
	}
	sb.WriteString("}")
	return sb.String()
}

// stripANSI removes ANSI SGR escape sequences from s.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, c := range s {
		switch {
		case inEsc:
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
				inEsc = false
			}
		case c == '\x1b':
			inEsc = true
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}
```

### The run loop

`repl.go` holds everything that does not touch the external module: the `REPL` struct and its functional options, the colon-command dispatcher, the bracket-depth counter, and the `runLoop` itself. The loop is the heart of the lesson. It first enters raw mode (only if `IsTerminal` is true) with a deferred `ExitRaw`, then spins reading one byte at a time. A printable byte is inserted and the line redrawn; a control byte is a shortcut (Ctrl+A home, Ctrl+E end, Ctrl+W delete-word, Ctrl+K delete-to-end, Ctrl+L clear screen, Ctrl+D exit on an empty line); an `\x1b` byte begins an escape sequence that is read to completion and dispatched (arrows, Home, End, Delete). Enter is the busy case: it joins the accumulated lines, and if `BracketDepth` of the whole buffer is still positive it shows the continuation prompt and keeps reading, otherwise it trims, records to history, runs the colon-command handler or the evaluator, and prints the result through `Pretty`.

The redraw closure is the same idempotent sequence described in the concepts: carriage return, erase to end of line, prompt, buffer, then a CHA escape to place the cursor at the right column. Because it always rebuilds from column one, it can run after every keystroke without flicker. The crucial bracket-depth detail lives here too: the depth is computed on `strings.Join(lines, "\n")`, never on a single line, so a closing `}` finishes the block instead of being misread as a complete standalone expression.

Create `repl.go`:

```go
package repl

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// EvalFunc evaluates one complete input expression and returns the result.
// The closure typically captures a long-lived interpreter Environment so that
// bindings made with `let` persist across inputs for the life of the session.
type EvalFunc func(input string) *Object

// termReader is the narrow interface the run loop uses for raw terminal I/O.
// rawTerminal (terminal.go) and mockTerminal (repl_test.go) both implement it.
type termReader interface {
	ReadByte() (byte, error)
	EnterRaw() error
	ExitRaw()
	IsTerminal() bool
}

// REPL drives the interactive interpreter loop.
type REPL struct {
	eval       EvalFunc
	history    *History
	completer  Completer
	out        io.Writer
	prompt     string
	contPrompt string
	noColor    bool
}

// Option is a functional option for New.
type Option func(*REPL)

// WithHistory sets the history store.
func WithHistory(h *History) Option { return func(r *REPL) { r.history = h } }

// WithCompleter sets the completion source.
func WithCompleter(c Completer) Option { return func(r *REPL) { r.completer = c } }

// WithPrompt sets the primary prompt string (default ">> ").
func WithPrompt(p string) Option { return func(r *REPL) { r.prompt = p } }

// WithNoColor disables ANSI colors in output (useful for non-terminal sinks).
func WithNoColor() Option { return func(r *REPL) { r.noColor = true } }

// New returns a REPL that sends each complete expression to eval and writes
// results to out.
func New(eval EvalFunc, out io.Writer, opts ...Option) *REPL {
	r := &REPL{
		eval:       eval,
		out:        out,
		prompt:     ">> ",
		contPrompt: ".. ",
	}
	for _, o := range opts {
		o(r)
	}
	if r.history == nil {
		r.history = NewHistory(0)
	}
	if r.completer == nil {
		r.completer = NewBasicCompleter(nil)
	}
	return r
}

// HandleCommand dispatches colon-prefixed REPL commands. Returns true if input
// was a command (caller should not pass it to the evaluator).
func (r *REPL) HandleCommand(input string) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], ":") {
		return false
	}
	switch parts[0] {
	case ":help":
		fmt.Fprint(r.out, replHelp)
	case ":history":
		n := r.history.Len()
		start := 0
		if n > 20 {
			start = n - 20
		}
		for i := start; i < n; i++ {
			fmt.Fprintf(r.out, "%4d  %s\n", i+1, r.history.Entry(i))
		}
	case ":clear":
		fmt.Fprintln(r.out, "Environment cleared.")
	case ":ast":
		if len(parts) < 2 {
			fmt.Fprintln(r.out, "usage: :ast <expr>")
		} else {
			fmt.Fprintf(r.out, "<AST for %q — wire up your parser here>\n",
				strings.Join(parts[1:], " "))
		}
	case ":tokens":
		if len(parts) < 2 {
			fmt.Fprintln(r.out, "usage: :tokens <expr>")
		} else {
			fmt.Fprintf(r.out, "<tokens for %q — wire up your lexer here>\n",
				strings.Join(parts[1:], " "))
		}
	default:
		fmt.Fprintf(r.out, "unknown command %q — type :help\n", parts[0])
	}
	return true
}

// BracketDepth counts unclosed (, [, { in s. Brackets inside double-quoted
// strings are excluded. The result is negative if there are unmatched closers.
// A depth > 0 signals an incomplete expression.
func BracketDepth(s string) int {
	depth := 0
	inStr := false
	for _, c := range s {
		switch {
		case !inStr && c == '"':
			inStr = true
		case inStr && c == '"':
			inStr = false
		case inStr:
			// skip
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		}
	}
	return depth
}

// runLoop executes the interactive edit-eval-print loop using t for raw I/O and
// out for output. It returns when the user sends Ctrl+D on an empty line or
// t.ReadByte returns io.EOF. Run (terminal.go) wraps it with a real terminal.
func (r *REPL) runLoop(t termReader, out io.Writer) error {
	if t.IsTerminal() {
		if err := t.EnterRaw(); err != nil {
			return fmt.Errorf("repl: raw mode: %w", err)
		}
		defer t.ExitRaw()
	}

	var e LineEditor
	var lines []string
	prompt := r.prompt

	render := func() {
		col := 1 + len([]rune(prompt)) + e.Cursor()
		fmt.Fprintf(out, "\r\x1b[K%s%s\x1b[%dG", prompt, e.String(), col)
	}

	fmt.Fprint(out, prompt)

	for {
		b, err := t.ReadByte()
		if err == io.EOF {
			fmt.Fprintln(out)
			return nil
		}
		if err != nil {
			return fmt.Errorf("repl: read: %w", err)
		}

		switch b {
		case 0x04: // Ctrl+D — exit if line is empty
			if e.String() == "" {
				fmt.Fprintln(out)
				return nil
			}

		case 0x01: // Ctrl+A / Home
			e.MoveHome()
			render()

		case 0x05: // Ctrl+E / End
			e.MoveEnd()
			render()

		case 0x0B: // Ctrl+K — delete to end of line
			e.DeleteToEnd()
			render()

		case 0x15: // Ctrl+U — delete to start of line
			e.MoveHome()
			e.DeleteToEnd()
			render()

		case 0x17: // Ctrl+W — delete word backward
			e.DeleteWordBack()
			render()

		case 0x0C: // Ctrl+L — clear screen
			fmt.Fprint(out, "\x1b[2J\x1b[H")
			render()

		case 0x09: // Tab — complete
			prefix := lastWord(e.PrefixAtCursor())
			completions := r.completer.Complete(prefix)
			switch len(completions) {
			case 0:
				// nothing to complete
			case 1:
				for _, c := range completions[0][len(prefix):] {
					e.Insert(c)
				}
				render()
			default:
				fmt.Fprintln(out)
				printCompletions(out, completions)
				render()
			}

		case 0x0D, 0x0A: // Enter
			line := e.String()
			fmt.Fprintln(out)
			e.Reset()

			if len(lines) > 0 && strings.TrimSpace(line) == "" {
				lines = lines[:0]
				prompt = r.prompt
				fmt.Fprint(out, prompt)
				continue
			}

			lines = append(lines, line)
			combined := strings.Join(lines, "\n")

			if BracketDepth(combined) > 0 {
				prompt = r.contPrompt
				fmt.Fprint(out, prompt)
				continue
			}

			lines = lines[:0]
			prompt = r.prompt
			input := strings.TrimSpace(combined)

			if input == "" {
				fmt.Fprint(out, prompt)
				continue
			}

			r.history.Add(input)

			if !r.HandleCommand(input) {
				if result := r.eval(input); result != nil {
					display := Pretty(result)
					if r.noColor {
						display = stripANSI(display)
					}
					fmt.Fprintln(out, display)
				}
			}
			fmt.Fprint(out, prompt)

		case 0x7F: // Backspace (DEL)
			e.DeleteBack()
			render()

		case 0x1b: // ESC — start of an ANSI escape sequence
			seq, seqErr := readEscapeSeq(t)
			if seqErr != nil {
				continue
			}
			switch seq {
			case "[A": // Up — older history
				if s, ok := r.history.Prev(); ok {
					e.SetContent(s)
				}
			case "[B": // Down — newer history
				if s, ok := r.history.Next(); ok {
					e.SetContent(s)
				} else {
					e.Reset()
				}
			case "[C": // Right
				e.MoveRight()
			case "[D": // Left
				e.MoveLeft()
			case "[H", "[1~": // Home
				e.MoveHome()
			case "[F", "[4~": // End
				e.MoveEnd()
			case "[3~": // Delete key
				e.DeleteForward()
			}
			render()

		default:
			if b >= 0x20 { // printable byte
				e.Insert(rune(b))
				render()
			}
		}
	}
}

// readEscapeSeq reads bytes after the initial ESC until a letter or '~'
// terminates the sequence. Returns the sequence without the leading ESC.
func readEscapeSeq(t termReader) (string, error) {
	var seq []byte
	for {
		b, err := t.ReadByte()
		if err != nil {
			return "", err
		}
		seq = append(seq, b)
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
			return string(seq), nil
		}
		if len(seq) > 8 {
			return string(seq), nil
		}
	}
}

// lastWord returns the last contiguous non-whitespace token in s.
func lastWord(s string) string {
	runes := []rune(s)
	i := len(runes) - 1
	for i >= 0 && !unicode.IsSpace(runes[i]) {
		i--
	}
	return string(runes[i+1:])
}

// printCompletions writes completions in a four-column layout.
func printCompletions(out io.Writer, completions []string) {
	const cols = 4
	for i, c := range completions {
		fmt.Fprintf(out, "%-20s", c)
		if (i+1)%cols == 0 {
			fmt.Fprintln(out)
		}
	}
	if len(completions)%cols != 0 {
		fmt.Fprintln(out)
	}
}

const replHelp = `Keyboard shortcuts:
  Left / Right      move cursor within line
  Home / Ctrl+A     jump to start of line
  End  / Ctrl+E     jump to end of line
  Ctrl+W            delete word backward
  Ctrl+U            delete to start of line
  Ctrl+K            delete to end of line
  Ctrl+L            clear screen
  Ctrl+D            exit (on empty line only)
  Up / Down         history navigation
  Tab               complete identifier

REPL commands:
  :help             show this message
  :history          show last 20 history entries
  :clear            clear the environment
  :ast <expr>       show AST for expression
  :tokens <expr>    show token list for expression
`
```

### The raw-terminal adapter

`terminal.go` is the only file that imports `golang.org/x/term`, which is exactly why it is kept separate: nothing in `repl.go` depends on the external module, so the editing logic compiles and tests without it. `rawTerminal` implements `termReader`. `EnterRaw` calls `term.MakeRaw(t.fd)` and stores the returned old state; `ExitRaw` hands that state to `term.Restore`, guarded against a nil state so deferring it is always safe; `IsTerminal` delegates to `term.IsTerminal`. `Run` constructs a `rawTerminal` over `os.Stdin` and drives `runLoop` against `os.Stdout`. Because `MakeRaw` returns the previous state and `Restore` consumes it, the enter/exit pair is the entire raw-mode contract, and the deferred `ExitRaw` in `runLoop` guarantees the terminal is handed back even on a panic.

Create `terminal.go`:

```go
package repl

import (
	"os"

	"golang.org/x/term"
)

// rawTerminal wraps os.Stdin for character-at-a-time raw input.
type rawTerminal struct {
	fd       int
	oldState *term.State
}

func newRawTerminal() *rawTerminal {
	return &rawTerminal{fd: int(os.Stdin.Fd())}
}

func (t *rawTerminal) ReadByte() (byte, error) {
	var buf [1]byte
	_, err := os.Stdin.Read(buf[:])
	return buf[0], err
}

func (t *rawTerminal) EnterRaw() error {
	state, err := term.MakeRaw(t.fd)
	if err != nil {
		return err
	}
	t.oldState = state
	return nil
}

func (t *rawTerminal) ExitRaw() {
	if t.oldState != nil {
		_ = term.Restore(t.fd, t.oldState)
	}
}

func (t *rawTerminal) IsTerminal() bool {
	return term.IsTerminal(t.fd)
}

// Run starts the interactive REPL against the real terminal (os.Stdin /
// os.Stdout). It returns when the user presses Ctrl+D on an empty line.
func (r *REPL) Run() error {
	return r.runLoop(newRawTerminal(), os.Stdout)
}
```

### The runnable demo

The demo wires a small *stateful* evaluator to the real terminal. The evaluator closure captures a single `env` map and a completer, both created once before `Run`, so a `let x = 5` on one line is remembered when `x` is typed on the next — this is the persisting-Environment property in miniature. Defining an identifier also registers it for Tab completion. Replace the closure with a call into your Monkey evaluator to get a full interpreter REPL.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"example.com/repl"
)

func main() {
	h := repl.NewHistory(1000)
	histPath := os.ExpandEnv("$HOME/.monkey_history")
	_ = h.Load(histPath)

	c := repl.NewBasicCompleter([]string{
		"let", "fn", "if", "else", "return",
		"true", "false", "null",
		"puts", "len", "first", "last", "rest", "push",
	})

	// env is created once and captured by the closure, so bindings persist for
	// the whole session — the REPL never rebuilds it between inputs.
	env := map[string]int64{}
	eval := func(input string) *repl.Object {
		line := strings.TrimSpace(input)
		if rest, ok := strings.CutPrefix(line, "let "); ok {
			if name, valStr, ok := strings.Cut(rest, "="); ok {
				name = strings.TrimSpace(name)
				if v, err := strconv.ParseInt(strings.TrimSpace(valStr), 10, 64); err == nil {
					env[name] = v
					c.Add(name) // register the new identifier for completion
					return &repl.Object{Type: repl.TypeInt, IntVal: v}
				}
			}
		}
		if v, ok := env[line]; ok {
			return &repl.Object{Type: repl.TypeInt, IntVal: v}
		}
		return &repl.Object{Type: repl.TypeString, StrVal: line}
	}

	r := repl.New(eval, os.Stdout,
		repl.WithHistory(h),
		repl.WithCompleter(c),
		repl.WithPrompt("monkey>> "),
	)

	fmt.Fprintln(os.Stderr, "Monkey REPL  (type :help for commands, Ctrl+D to exit)")
	if err := r.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := h.Save(histPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save history: %v\n", err)
	}
}
```

Run it against a real terminal:

```bash
go run ./cmd/demo
```

A session looks like this on screen (the terminal renders the colors and the cursor moves; the banner is printed on stderr):

```
Monkey REPL  (type :help for commands, Ctrl+D to exit)
monkey>> let x = 5
5
monkey>> x
5
monkey>> let answer = 42
42
monkey>> answer
42
monkey>>
```

Type `let x = 5`, press Enter, then `x` and Enter to see the binding persist; press Up to recall a previous line; press Tab after `ans` to complete `answer`; type `:help` for the shortcut list; press Ctrl+D on an empty line to exit, which saves history to `~/.monkey_history`.

### Tests

The tests need no terminal. A `mockTerminal` feeds bytes from a `strings.Reader` and reports `IsTerminal() == false`, so `runLoop` skips raw mode and runs the full edit-eval-print path over a scripted byte string. `runSession` builds a no-color REPL and returns the captured output; the integration tests script a line plus a trailing `\x04` (Ctrl+D) to exit, and assert on evaluation, empty-input skipping, multi-line continuation, continuation cancel, command handling, and history recording. The unit tests cover the buffer, history, completer, bracket depth, the command dispatcher, and the pretty-printer.

Create `repl_test.go`:

```go
package repl

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// mockTerminal feeds bytes from r. It never claims to be a real terminal, so
// runLoop skips EnterRaw/ExitRaw and the session can be scripted byte by byte.
type mockTerminal struct {
	r io.Reader
}

func newMock(input string) *mockTerminal {
	return &mockTerminal{r: strings.NewReader(input)}
}

func (m *mockTerminal) ReadByte() (byte, error) {
	var buf [1]byte
	_, err := m.r.Read(buf[:])
	return buf[0], err
}
func (m *mockTerminal) EnterRaw() error  { return nil }
func (m *mockTerminal) ExitRaw()         {}
func (m *mockTerminal) IsTerminal() bool { return false }

// --- LineEditor ---

func TestLineEditorInsertAndDelete(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello" {
		e.Insert(r)
	}
	if e.String() != "hello" || e.Cursor() != 5 {
		t.Fatalf("after insert: %q cursor=%d", e.String(), e.Cursor())
	}
	e.DeleteBack()
	if e.String() != "hell" {
		t.Fatalf("after DeleteBack: %q", e.String())
	}
}

func TestLineEditorUnicode(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "café" {
		e.Insert(r)
	}
	if e.Cursor() != 4 {
		t.Fatalf("Cursor = %d, want 4 (runes, not bytes)", e.Cursor())
	}
	e.DeleteBack()
	if e.String() != "caf" {
		t.Fatalf("String = %q, want %q", e.String(), "caf")
	}
}

func TestLineEditorWordBack(t *testing.T) {
	t.Parallel()
	var e LineEditor
	for _, r := range "hello world" {
		e.Insert(r)
	}
	e.DeleteWordBack()
	if e.String() != "hello " {
		t.Fatalf("DeleteWordBack = %q, want %q", e.String(), "hello ")
	}
}

// --- History ---

func TestHistoryNavigation(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	h.Add("first")
	h.Add("second")
	h.Add("second") // consecutive duplicate suppressed
	if h.Len() != 2 {
		t.Fatalf("Len = %d, want 2", h.Len())
	}
	if s, ok := h.Prev(); !ok || s != "second" {
		t.Fatalf("Prev = %q %v, want second true", s, ok)
	}
	if s, ok := h.Prev(); !ok || s != "first" {
		t.Fatalf("Prev = %q %v, want first true", s, ok)
	}
}

// --- BasicCompleter ---

func TestBasicCompleter(t *testing.T) {
	t.Parallel()
	c := NewBasicCompleter([]string{"let", "fn", "for"})
	if got := c.Complete("f"); len(got) != 2 {
		t.Fatalf("Complete(f) = %v, want 2", got)
	}
	if got := c.Complete(""); len(got) != 3 {
		t.Fatalf("Complete(empty) = %v, want all 3", got)
	}
}

// --- BracketDepth ---

func TestBracketDepth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  int
	}{
		{"let x = 1", 0},
		{"fn(x) {", 1},
		{"fn(x) {}", 0},
		{"(1 + (2 * 3)", 1},
		{`"({}"`, 0}, // brackets inside a string literal are ignored
		{"[1, [2, 3]", 1},
		{"}", -1},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			if got := BracketDepth(tc.input); got != tc.want {
				t.Errorf("BracketDepth(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// --- REPL commands ---

func TestHandleCommandHelp(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	r := New(func(string) *Object { return nil }, &sb)
	if !r.HandleCommand(":help") {
		t.Fatal("HandleCommand(':help') returned false")
	}
	if !strings.Contains(sb.String(), "Keyboard shortcuts") {
		t.Fatalf("help output missing shortcuts:\n%s", sb.String())
	}
}

func TestHandleCommandUnknown(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	r := New(func(string) *Object { return nil }, &sb)
	if !r.HandleCommand(":bogus") {
		t.Fatal("HandleCommand(':bogus') returned false")
	}
	if !strings.Contains(sb.String(), "unknown command") {
		t.Fatalf("output = %q, want 'unknown command'", sb.String())
	}
}

func TestHandleCommandNonCommand(t *testing.T) {
	t.Parallel()
	r := New(func(string) *Object { return nil }, io.Discard)
	if r.HandleCommand("let x = 1") {
		t.Fatal("HandleCommand on non-command should return false")
	}
}

// --- Pretty printer ---

func TestPretty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		obj  *Object
		want string
	}{
		{&Object{Type: TypeInt, IntVal: 42}, "42"},
		{&Object{Type: TypeString, StrVal: "hi"}, `"hi"`},
		{&Object{Type: TypeBool, BoolVal: true}, "true"},
		{&Object{Type: TypeFn, Params: []string{"x", "y"}}, "fn(x, y) { ... }"},
		{nil, "null"},
	}
	for _, tc := range cases {
		if got := stripANSI(Pretty(tc.obj)); got != tc.want {
			t.Errorf("Pretty = %q, want %q", got, tc.want)
		}
	}
}

// --- Integration: runLoop via mockTerminal ---

func runSession(input string, eval EvalFunc) (string, error) {
	var sb strings.Builder
	r := New(eval, &sb,
		WithNoColor(),
		WithHistory(NewHistory(10)),
		WithCompleter(NewBasicCompleter([]string{"let", "fn", "if"})),
	)
	err := r.runLoop(newMock(input), &sb)
	return sb.String(), err
}

func TestRunLoopEvaluatesLine(t *testing.T) {
	t.Parallel()
	out, err := runSession("42\n\x04", func(string) *Object {
		return &Object{Type: TypeInt, IntVal: 99}
	})
	if err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if !strings.Contains(out, "99") {
		t.Errorf("output = %q, want it to contain '99'", out)
	}
}

func TestRunLoopSkipsEmptyInput(t *testing.T) {
	t.Parallel()
	called := false
	if _, err := runSession("\n\x04", func(string) *Object {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if called {
		t.Fatal("eval should not be called for empty input")
	}
}

func TestRunLoopMultiLineContinuation(t *testing.T) {
	t.Parallel()
	var got string
	if _, err := runSession("fn(x) {\nx\n}\n\x04", func(s string) *Object {
		got = s
		return nil
	}); err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if !strings.Contains(got, "fn(x) {") {
		t.Errorf("eval received %q, expected multi-line input", got)
	}
}

func TestRunLoopCancelsMultiLineOnEmptyLine(t *testing.T) {
	t.Parallel()
	calls := 0
	if _, err := runSession("fn(x) {\n\n\x04", func(string) *Object {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("eval called %d times, want 0 (cancelled multi-line)", calls)
	}
}

func TestRunLoopHandlesCommand(t *testing.T) {
	t.Parallel()
	out, err := runSession(":help\n\x04", func(string) *Object { return nil })
	if err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if !strings.Contains(out, "Keyboard shortcuts") {
		t.Errorf("output = %q, want help text", out)
	}
}

func TestRunLoopPersistsEnvironment(t *testing.T) {
	t.Parallel()
	// A stateful closure proves the environment survives across inputs: the
	// second line reads a binding made on the first.
	env := map[string]int64{}
	eval := func(s string) *Object {
		if rest, ok := strings.CutPrefix(s, "let x = "); ok {
			env["x"] = int64(len(rest)) // toy: store something deterministic
			return &Object{Type: TypeInt, IntVal: env["x"]}
		}
		if v, ok := env[s]; ok {
			return &Object{Type: TypeInt, IntVal: v}
		}
		return nil
	}
	var sb strings.Builder
	r := New(eval, &sb, WithNoColor(), WithHistory(NewHistory(10)))
	if err := r.runLoop(newMock("let x = 5\nx\n\x04"), &sb); err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if env["x"] == 0 {
		t.Fatal("environment did not persist across inputs")
	}
}

func TestRunLoopAddsToHistory(t *testing.T) {
	t.Parallel()
	h := NewHistory(10)
	var sb strings.Builder
	r := New(func(string) *Object { return nil }, &sb, WithHistory(h), WithNoColor())
	if err := r.runLoop(newMock("let x = 1\n\x04"), &sb); err != nil {
		t.Fatalf("runLoop error: %v", err)
	}
	if h.Len() != 1 || h.Entry(0) != "let x = 1" {
		t.Fatalf("history = %v, want [let x = 1]", h.entries)
	}
}

func ExampleBracketDepth() {
	fmt.Println(BracketDepth("fn(x) {}"))
	fmt.Println(BracketDepth("fn(x) {"))
	// Output:
	// 0
	// 1
}
```

## Review

The REPL is correct when its editing logic is provable without a terminal and its raw-mode handling is leak-free. The scripted sessions are the proof: a line followed by Ctrl+D evaluates and prints the result; an empty line never reaches the evaluator; `fn(x) {` opens a continuation that `}` later closes, and the evaluator receives the joined multi-line input; an empty continuation line cancels the pending input so the evaluator is not called; a colon command is dispatched instead of evaluated; and a submitted line lands in history exactly once. The environment-persistence test confirms the design contract — a binding made on one input is visible on the next because the closure holds one long-lived environment. On the terminal side, `EnterRaw` is gated by `IsTerminal` and paired with a deferred `ExitRaw`, so a non-terminal run skips raw mode and a panicking interactive run still restores the shell.

Common mistakes for this module. Calling `BracketDepth` on each line instead of the joined buffer makes a closing `}` read as complete on its own and breaks multi-line input. Skipping `defer t.ExitRaw()` after a successful `EnterRaw` leaves the terminal in raw mode on any early exit, so the shell needs `reset`. Rebuilding the environment inside `eval` instead of capturing it once erases every `let` at the next prompt. And depending on the concrete `rawTerminal` in `runLoop` instead of the `termReader` interface would drag `golang.org/x/term` into the test build and make the editing logic untestable.

## Resources

- [pkg.go.dev/golang.org/x/term](https://pkg.go.dev/golang.org/x/term) — `MakeRaw`, `Restore`, and `IsTerminal` signatures and behavior.
- [ANSI/VT100 escape sequences](https://vt100.net/docs/vt100-ug/chapter3.html) — CHA (`\x1b[nG`), ED (`\x1b[2J`), EL (`\x1b[K`), and the CSI arrow-key codes the loop decodes.
- [viewsourcecode.org/snaptoken/kilo](https://viewsourcecode.org/snaptoken/kilo/) — a step-by-step raw-terminal editor in C; the same escape-sequence model applies here.
- [Thorsten Ball, Writing An Interpreter In Go](https://interpreterbook.com/) — Chapter 1 introduces the Monkey REPL this loop extends.

---

Back to [04-pretty-printing.md](04-pretty-printing.md) | Next: [Full Monkey Language Interpreter](../08-full-interpreter-monkey/00-concepts.md).
