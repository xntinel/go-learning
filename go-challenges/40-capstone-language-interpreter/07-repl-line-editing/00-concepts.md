# 7. REPL with Line Editing — Concepts

A bare `fmt.Scanln` REPL is painful to use: no arrow keys, no history, no completion, and every line is evaluated in a fresh environment so `let x = 5` is forgotten by the next prompt. This lesson builds a production-quality interactive loop for the Monkey interpreter. The hard part is not any single feature but the layering: raw terminal I/O must be isolated behind a narrow interface so the entire editing logic can be unit-tested without a real terminal, and the editing primitives — the line buffer, the history ring, the completer, the pretty-printer — must each stand on their own before they are wired together. This file is the conceptual foundation: read it once and you will have the model you need to reason through each exercise, which builds the REPL piece by piece as independent, self-contained Go modules.

## Concepts

### Raw vs Cooked Terminal Mode

A terminal driver sits between the keyboard and the program, and by default it runs in *cooked* (canonical) mode. In cooked mode the driver buffers a whole line, lets the user edit it with Backspace, echoes every keystroke to the screen itself, and only hands the finished line to the program when Enter is pressed. That is exactly what you want for `cat` or a shell pipeline, and exactly what you do not want for an interactive editor: you never see the individual keystrokes, you cannot react to an arrow key, and the cursor is wherever the driver decided to put it.

*Raw* mode turns all of that off. Every byte the user types is delivered to the program immediately, with no line buffering, no automatic echo, and no driver-side editing. Special keys like Ctrl+C stop being signals and become ordinary bytes (0x03) that the program reads like any other. The price of this control is total responsibility: the program must now echo printable characters itself, redraw the line after every edit, interpret arrow keys, and translate Backspace into an actual deletion. Nothing happens on screen unless the program writes it.

The editing model that falls out of raw mode is a loop: read one byte, decide what it means (printable character, control key, or the start of an escape sequence), mutate an in-memory line buffer, then redraw the visible line from that buffer. Because the program owns the screen, the redraw can be made idempotent — it always reconstructs the line from a known state — so it can run on every keystroke without flicker.

### golang.org/x/term: MakeRaw, Restore, IsTerminal

Go's standard library does not expose the `termios` flags needed to switch modes, so the canonical tool is the external module `golang.org/x/term`. Three functions carry the whole job.

`term.MakeRaw(fd int) (*term.State, error)` puts the file descriptor into raw mode and returns the *previous* state as an opaque `*term.State`. The return value is the single most important detail of the whole API: it is the only way back to cooked mode, so it must be kept and handed to `Restore`. `term.Restore(fd int, oldState *term.State) error` reverts the descriptor to the state captured by `MakeRaw`. The fd in both calls is `int(os.Stdin.Fd())`; raw mode applies to the descriptor, not to a Go `*os.File` wrapper.

The discipline that prevents a wrecked terminal is to defer the restore in the same function that made the change: call `MakeRaw`, check the error, then immediately `defer Restore(fd, oldState)`. Any path that leaves that function — a normal return, an early error, or a panic that unwinds the stack — runs the deferred restore and hands the user back a working shell. A program that calls `MakeRaw` and skips `Restore` leaves the terminal in raw mode after it exits: no echo, no line editing, and the only fix is the blind command `reset`.

`term.IsTerminal(fd int) bool` reports whether a descriptor is an interactive terminal at all. It is the guard that lets the same code run both ways: when stdin is a real terminal the REPL enters raw mode, and when stdin is a pipe or a file (a test harness, a `printf | repl` invocation) it skips raw mode entirely and just reads bytes until EOF. Calling `MakeRaw` on a non-terminal fails, so the `IsTerminal` check is not optional politeness; it is what keeps the program usable under redirection.

### ANSI Escape Sequences for Cursor Movement and Clearing

Once the program owns the screen it controls the cursor and the line contents with ANSI (VT100) escape sequences: short byte strings beginning with the escape byte `\x1b` (ESC, 0x1b) that the terminal interprets as commands rather than printing. The handful this REPL needs:

```text
\r          carriage return: move the cursor to column 1 of the current line
\x1b[K      EL (Erase in Line): erase from the cursor to the end of the line
\x1b[nG     CHA (Cursor Horizontal Absolute): move the cursor to column n (1-based)
\x1b[2J     ED (Erase in Display): clear the whole screen
\x1b[H      CUP (Cursor Position): move the cursor to the top-left corner
```

The redraw after every keystroke is built from the first three: emit `\r` to return to column 1, `\x1b[K` to wipe whatever was there, then write the prompt followed by the entire buffer, and finally `\x1b[nG` to place the cursor at the correct column. That column is computed from the prompt width plus the cursor's rune index plus one (columns are 1-based). The sequence is idempotent because it always starts by going home and erasing, so calling it on every byte produces a stable, flicker-free line regardless of what was previously displayed.

Escape sequences also arrive *from* the keyboard. An arrow key does not send a single byte; it sends `ESC [ A` for Up, `ESC [ B` for Down, `ESC [ C` for Right, `ESC [ D` for Left, and Home/End/Delete send longer sequences like `ESC [ H`, `ESC [ F`, and `ESC [ 3 ~`. So when the read loop sees an `\x1b` byte, it must keep reading the following bytes until a terminating letter or `~` arrives, then dispatch on the assembled sequence. The same `\x1b[` prefix is thus both an input grammar (decode what the user pressed) and an output grammar (tell the terminal what to do).

### The Rune-Based Line Buffer

The editable line is held in memory as a slice of runes with an integer cursor, never as a `[]byte`. Monkey identifiers can contain multi-byte UTF-8 characters, and every editing operation — insert here, delete the character before the cursor, jump to the previous word — is defined in terms of *characters*, not bytes. If the buffer were `[]byte` and the cursor a byte offset, moving the cursor left by one or slicing `buf[:cursor]` could land in the middle of a multi-byte rune and produce garbled output or a panic. Storing `[]rune` makes the cursor a rune index, so all arithmetic is in character units and a conversion to `string` happens only at render time.

Each mutation is small and total: insert a rune at the cursor and advance; delete the rune before the cursor (Backspace) or at the cursor (Delete); move left, right, to the start (Home/Ctrl+A) or end (End/Ctrl+E); delete the previous word (Ctrl+W) by consuming trailing whitespace then the run of non-whitespace before it; delete to end of line (Ctrl+K). Returning a boolean from the movement and deletion operations lets the caller know whether anything actually changed, which is the signal for whether a redraw is even necessary. Keeping this buffer free of any terminal dependency is what makes it trivially testable: a test drives it with method calls and asserts on `String()` and `Cursor()`, no terminal required.

### History as a Navigation Cursor, with Persistence

Command history is a bounded slice of past entries plus a separate integer that acts as a navigation cursor. After every `Add`, that cursor is reset to `len(entries)` — the position "after the last entry, nothing selected" — so the next Up press starts from the most recent line. Up moves the cursor back one entry, Down moves it forward; both clamp at the ends, and moving forward past the last entry returns to an empty line. When the user recalls a line, edits it, and presses Enter, it is the *edited* text that gets added; the stored history entry is never mutated in place.

Two refinements matter for feel. Consecutive-duplicate suppression — if the same expression is entered three times in a row, only one copy is stored — is the single highest-value UX detail, because without it the history fills with noise and Up has to be pressed repeatedly to step over repeats. And persistence across sessions: on startup the REPL loads a history file (one entry per line) so Up recalls work from a previous run, and on a clean exit it writes the file back. A missing history file on load is not an error — a first run simply starts empty — and the loaded entries are trimmed to the size bound so an old file cannot grow the ring without limit.

### Continuation Detection Without a Full Parse

Multi-line input (a function literal that opens a brace on one line and closes it later) is detected by counting unmatched brackets, not by running the real parser. The heuristic scans the accumulated input and adds one for each `(`, `[`, or `{` and subtracts one for each `)`, `]`, or `}`, ignoring anything inside a double-quoted string. A positive depth means the expression is incomplete, so the REPL shows a continuation prompt and keeps reading; a depth of zero means it is ready to evaluate. The crucial detail is that the depth is computed over the *entire accumulated buffer*, joining all collected lines, never line by line — a lone closing `}` has depth -1 on its own and would be misread as complete if scanned in isolation, when in fact it is the delimiter that finishes a multi-line block.

This is deliberately a heuristic and not a parse. It is O(n) and runs on every Enter, it correctly ignores brackets inside string literals, and it knowingly mishandles brackets inside comments or multi-line strings — cases that produce a spurious continuation prompt the user cancels with an empty line. For an interactive REPL that trade-off is the right one: cheap, predictable, and good enough that the rare false continuation costs one keystroke to escape.

### Persisting the Environment Across Inputs

A REPL is not a sequence of independent evaluations; it is a single session with memory. `let x = 5` on one line must make `x` available on every later line, which means the interpreter's `Environment` — the map from names to bound values — is created once, before the read loop starts, and threaded through every call to the evaluator. The loop reads and edits text, but the environment lives outside the loop and outlives each individual input.

The clean way to express this is to capture the environment in the evaluator closure the REPL is given: the REPL itself knows nothing about scopes or bindings, it just calls `eval(input)` for each complete expression, and the closure it was handed holds the one long-lived environment. A `:clear` command resets that environment to start fresh. The mistake this design prevents is constructing a new environment inside the per-line evaluation — which would make every binding evaporate at the next prompt and quietly defeat the entire point of an interactive session.

### Separating I/O from Logic, Completion, and Pretty-Printing

Three more boundaries keep the system testable and extensible. First, the run loop never touches `golang.org/x/term` directly; it depends on a narrow interface — read a byte, enter raw mode, exit raw mode, report whether this is a terminal — that the real terminal implements with `x/term` and a mock implements with an in-memory byte buffer. Every editing test scripts a session against the mock; the real terminal is exercised only by the demo binary. Second, tab completion is an interface with a single `Complete(prefix) []string` method, so the default keyword-and-identifier completer can be swapped for one that walks the interpreter's scope chain without changing the loop. Third, value display goes through a pretty-printer that formats each interpreter value type with ANSI color (integers yellow, strings green, booleans cyan, functions blue, errors red, null gray), with a no-color mode that strips the codes for non-terminal output and for string-comparison tests. Each of these is built and verified in isolation first, then assembled into the full loop.

## Common Mistakes

### Not Restoring the Terminal on Panic

Wrong: calling `term.MakeRaw` and only restoring on the normal return path, so a panic or an early error return skips the restore.

What happens: the process exits with the terminal still in raw mode. The shell shows no echo and does no line editing; the user is stuck until they blindly type `reset` and Enter.

Fix: immediately after a successful `MakeRaw`, `defer` the `Restore` in the same function. A deferred call runs on every exit path including a panic unwind, so the terminal is always handed back in cooked mode. Make the restore safe to call with a nil saved state so deferring it unconditionally never itself panics.

### Slicing a Rune Buffer by Byte Offset

Wrong: storing the line as `[]byte` while treating the cursor as a character position, then slicing `buf[:cursor]`.

What happens: on a multi-byte Unicode identifier the slice boundary falls in the middle of a rune. Output is garbled and a re-decode can panic.

Fix: store the buffer as `[]rune`. Every cursor value is a rune index, all insert and delete arithmetic stays in character units, and the buffer is converted to a `string` only at render time with `string(buf)`.

### Calling the Bracket Counter on Each Line Separately

Wrong: testing `BracketDepth(newLine) > 0` on each freshly entered line instead of on the whole accumulated input.

What happens: a line that merely closes a block, like `}`, has depth -1 on its own, which is not greater than zero, so the REPL treats it as a complete standalone expression instead of the closing delimiter of the multi-line input that came before it.

Fix: accumulate the lines and run the counter over their join. Only the bracket depth of the entire pending buffer decides whether more input is needed.

### Recreating the Environment on Every Input

Wrong: constructing a fresh interpreter environment inside the per-line evaluation function.

What happens: every binding made with `let` is discarded the instant the line finishes, so `x` is undefined on the very next prompt and the session has no memory.

Fix: create the environment once, before the read loop, and capture it in the evaluator closure (or store it on the REPL). The loop reuses the same environment for the life of the session; only an explicit `:clear` resets it.

### Not Resetting the History Cursor After Add

Wrong: leaving the navigation cursor pointing into the middle of the list after an `Add`.

What happens: the user submits a line, presses Up, and lands on a stale position — often skipping the entry just submitted — because the cursor was never returned to the end of the list.

Fix: have `Add` reset the navigation cursor to `len(entries)` as its final step, so the next Up always starts from the most recent entry.

Next: [01-line-editing-buffer.md](01-line-editing-buffer.md)
