# 1. Lexer and Tokenizer — Concepts

A lexer is the first stage of every language implementation: it turns raw source text into a flat stream of typed tokens that a parser can consume one at a time. The happy path is deceptively easy — split on spaces, classify each chunk — and that ease is a trap, because almost all of the real difficulty lives in the edges. Multi-character operators share prefixes, so `<` and `<=` and `<<` cannot be decided from the first character alone. Identifiers may be UTF-8, so a byte offset and a visual column no longer move in lockstep. Numeric literals come in four bases with underscore separators and an optional fractional or exponent tail, and a stray `.` must be a member access in one context and a decimal point in another. Strings carry escape sequences that can be malformed, and a good lexer reports the problem and keeps going rather than aborting the whole file. Comments must be stripped without swallowing the newline that follows them. This file is the conceptual foundation for building such a lexer in Go for the Monkey programming language, the teaching language from Thorsten Ball's "Writing An Interpreter In Go." Read it once and the exercise that follows — a single, self-contained lexer package — becomes a matter of turning each idea below into code.

## Concepts

### What a Lexer Is, and the Single-Pass Model

A lexer (also called a scanner or tokenizer) consumes a string and produces a sequence of tokens. A token is the smallest meaningful unit of the language: a keyword, an identifier, a literal, an operator, a delimiter. Crucially, a token is more than a type — it carries the original text (its literal) and a position (line, column, byte offset) so that later stages can produce diagnostics that point at the exact place a problem occurred.

The design used here is a single-pass, character-at-a-time scanner. There is no separate "read the whole file, split into words" phase; instead the lexer holds a cursor into the input and, each time it is asked for the next token, consumes exactly the characters that token needs and stops. This streaming model matters because a parser does not want all tokens at once — it wants the next one, on demand, and it wants to stop as soon as it has what it needs. The same engine supports a batch convenience wrapper that simply calls "next token" in a loop until it sees the end-of-file token, so both styles are available from one implementation, and a test that the two agree token-for-token is a cheap, powerful correctness check.

### One-Character Look-Ahead Disambiguates Operators

The scanner tracks two byte positions: the offset of the current character and the offset of the next one. A small helper decodes the next character without advancing the cursor — a one-character "peek." That single peek is the entire mechanism behind every two-character operator in the language. When the current character is `=`, the lexer peeks: a following `=` means the equality operator `==`, a following `>` means the arrow `=>`, and anything else means plain assignment `=`. The same pattern handles `!=`, `<=`, `>=`, `&&`, `||`, and `..`.

The reason look-ahead is necessary rather than optional is that these operators share prefixes. The lexer cannot emit a token for `=` the instant it sees `=`, because the next character might turn it into a different token entirely. It must look before it commits. One character of look-ahead is exactly enough for Monkey because Monkey has no three-character operators; a language with `<<=` or `...` would need a second peek or a small pushback buffer. This is the lexer-level shadow of a general parsing idea: the number of symbols you must look ahead to decide what you are reading is a property of the grammar, and keeping it small (here, one) keeps the scanner simple.

A subtle but important discipline follows from this: when a two-character token is recognized, the scanner advances the cursor twice and returns immediately. The classic bug is to advance for the first character, branch on the peek, advance again inside the branch, and then fall through to a shared "advance once more" at the bottom — which skips the character after the operator. Each operator case must own its advances and return, with no shared trailing advance.

### Token Types as Named String Constants

A token's type is modeled as a defined string type whose constant values are the canonical display names: `"=="` for equality, `"IDENT"` for an identifier, `"let"` for the keyword. Choosing a string over an integer enum is a deliberate trade. The benefit is that diagnostics and test failures are self-describing: a failure that reads `got IDENT, want let` needs no lookup table to interpret, and printing a token during debugging shows something meaningful instead of `7`. The cost is that the compiler cannot check a switch over token types for exhaustiveness the way it can warn about an integer enum paired with a code generator, and a misspelled `"IDNET"` is a silent string, not a compile error. For a teaching lexer the readability wins; production compilers typically use an integer type with a generated `String()` method to recover the display names while keeping exhaustiveness and compactness.

### UTF-8 Identifiers: Byte Offset versus Rune Column

Go source strings are sequences of bytes, and UTF-8 encodes a character (a rune) in one to four bytes. An ASCII letter is one byte; the Greek letter α (U+03B1) is two; many CJK characters are three; some emoji are four. This means you cannot advance the cursor by one byte per character and expect correctness — you must decode each character with a UTF-8 decoder that returns both the rune and its byte width, and advance the byte cursor by that width. Doing so guarantees that slicing the input between two recorded byte offsets always yields valid UTF-8, which is what lets an identifier be captured simply as `input[start:end]`.

Two different position counters then become necessary because they answer two different questions. The byte offset answers "where in the byte stream is this?" — the right unit for slicing and for byte-addressed tooling. The column answers "what column would an editor show?" — and editors count characters, not bytes. So the lexer increments the column by exactly one per rune, regardless of how many bytes that rune occupied, while it advances the byte offset by the rune's byte width. For pure ASCII the two move together and the distinction is invisible; the moment a multi-byte character appears, they diverge, and a lexer that conflates them reports a column that an editor would disagree with. A test that tokenizes a line beginning with α and checks that a later token has the byte offset and the rune column it expects pins this invariant exactly.

### Line and Column Tracking Across Newlines

Positions are only useful if they survive newlines correctly. The convention every editor uses is that the newline character itself belongs to the line it ends, and the first character after it begins the next line at column one. The clean way to implement this is to advance the line counter lazily: when the cursor moves off a character, check whether the character just left behind was a newline, and if so increment the line and reset the column before counting the new character. That ordering — bump the line in response to the previous newline, then count the current character — is what makes the newline land on line N and the next character land on line N+1 at column 1. Getting the order wrong by one step is the difference between an error message that points at the right line and one that is off by one for the rest of the file.

### Identifiers, Keywords, and a Pre-Computed Map

An identifier starts with a letter or underscore and continues with letters, digits, or underscores; "letter" is defined by Unicode, not just ASCII, so non-Latin identifiers work. The scanner reads the maximal run of identifier characters, then has to decide whether that run is a plain identifier or a reserved keyword like `fn`, `let`, or `return`. The standard technique is a package-level map from keyword text to token type, consulted once per identifier. If the text is in the map, the keyword's type is returned; otherwise the type is the generic identifier.

This map is the single point of contact between the lexer and the language's vocabulary, which is exactly why it is worth isolating: adding a keyword is a one-line change, and the rest of the scanner is grammar-agnostic. The map is built once as a package-level variable and never mutated after initialization, so it is safe to read concurrently without any locking — a property worth stating explicitly, because a map that were mutated at runtime would not be.

### Numeric Literals: Four Bases, Separators, Floats, and the Ambiguous Dot

Numbers are the richest token to scan. The lexer supports decimal (`42`), hexadecimal (`0xFF`), octal (`0o77`), and binary (`0b1010`) integers, each allowing underscore digit-group separators (`1_000_000`, `0xDEAD_BEEF`), plus floating-point with a fractional part and an optional exponent (`3.14`, `1.5e10`, `2.3E-4`). The base is selected by peeking at the character after a leading `0`: `x`, `o`, or `b` (in either case) switches into the corresponding digit class, and the scanner then consumes the maximal run of valid digits for that base, treating underscore as an always-allowed separator.

The genuinely tricky part is the decimal point, because `.` is overloaded: it is a decimal point in `3.14` but a member-access operator in `x.Len`. The rule that resolves the ambiguity is to consume a `.` as part of a number only when the character after it is a digit. With that guard, `3.14` lexes as one float while `x.Len` lexes as three tokens — identifier, dot, identifier — because the character after the dot in `x.Len` is a letter, not a digit. The same maximal-munch philosophy governs the exponent: an `e` or `E`, an optional sign, then digits. A point worth being honest about for a teaching lexer is that it validates shape, not full legality — it does not reject a malformed `0x` with no digits or a stray double underscore; catching those is a refinement, and the recovery machinery below is where such checks would report without aborting.

### String Literals, Escape Decoding, and Error Recovery

A string literal is opened by a double quote and runs until the matching close quote. Inside, a backslash introduces an escape sequence: `\n`, `\t`, `\r`, `\\`, `\"`, and `\0` are decoded into their actual runes and written into the token's literal, so the literal a downstream stage receives is the decoded value, not the raw source spelling. This is a meaningful design choice — decoding at lex time means the parser and evaluator never have to re-interpret escapes — and it requires accumulating the decoded characters into a buffer rather than slicing the input, because the decoded text differs from the source text.

Two things can go wrong, and how the lexer responds to them is the heart of good error design. An unterminated string (the input ends, or a raw newline appears, before the closing quote) and an invalid escape (a backslash followed by an unrecognized character) are both errors — but neither should stop the scan. Instead the lexer records the error and continues, a strategy called error recovery. For an unrecognized character generally, it emits a special illegal token, records the error, and moves on. The payoff is that a single run surfaces every lexical error in the file at once, so a developer fixes them in one pass rather than recompiling after each one. The alternative — return on the first error — hides every later problem behind the first, which is a poor experience and easy to avoid.

### Sentinel Errors and `errors.Is`

Collecting errors raises the question of how callers should classify them. Matching on the human-readable message ("contains the word unterminated") is brittle: it breaks the moment the wording is improved. The idiomatic Go answer is sentinel errors — package-level error values, one per category — combined with a custom error type that wraps the relevant sentinel and exposes it through an `Unwrap` method. A caller then writes `errors.Is(err, ErrUnterminatedString)`, which walks the unwrap chain and compares identities, and stays correct no matter how the message text changes. The message still carries the human detail (which line, which character), while the wrapped sentinel carries the machine-checkable category. This separation — stable identity for code, rich text for humans — is the pattern to reach for whenever errors need to be both classified and read.

## Common Mistakes

### Advancing Twice for a Two-Character Token but Still Falling Through to a Trailing Advance

The tempting structure is to branch on the peek, advance an extra time for the two-character case, and then run a single shared `advance` at the bottom of the operator handling. The shared advance fires after the two internal advances and skips the character immediately after the operator, so input like `==x` loses the `x`. The fix is structural: each two-character case performs its own advances and returns immediately, with no shared trailing advance to fall into. Reading the code, every branch that recognizes a token should end in a `return`.

### Reading the Current Character Before Priming the Cursor

A freshly constructed lexer has a zero-valued cursor: byte position 0 and current character 0 (the zero rune), which is also the end-of-file sentinel. Inspecting the current character before the constructor has run its first advance therefore sees end-of-file even for a non-empty input, and any slice taken at that moment is empty or wrong. The fix is to prime the cursor once at the end of construction so that the current character holds the first real character before any token is requested. Forgetting this makes the very first token wrong in a way that is easy to misattribute to the scanning logic.

### Counting Columns in Bytes Instead of Runes

If the column is advanced by the byte width of each character rather than by one, then a file that opens with a two-byte character reports the next token at column 3 where an editor shows column 2, and the error grows with every multi-byte character on the line. The byte offset and the column answer different questions and must be tracked with different increments: the offset by byte width, the column by one per rune. Conflating them produces diagnostics that disagree with the user's editor precisely in the files (non-ASCII) where good diagnostics matter most.

### Consuming a Dot Into a Number Unconditionally

Treating any `.` after a digit run as a decimal point turns `x.Len()` — or `1.method()` in a language that allows it — into a malformed float and breaks member access. The guard is to look one character past the dot and only treat it as fractional when a digit follows; otherwise the dot is its own token and the number ends before it. The same one-character look-ahead that disambiguates operators disambiguates the decimal point.

### Stopping at the First Error

Returning an error from the per-token routine the instant something is illegal collapses the whole file to its first problem; every later error is invisible until the first is fixed, and the user pays a recompile per error. The recovery design instead emits an illegal token, appends the error to an internal slice, and continues scanning, so one run reports every lexical error. Surface the collected errors as a second return value from the batch entry point rather than as a control-flow interruption.

### Classifying Errors by Their Message Text

Testing or branching on `strings.Contains(msg, "unterminated")` couples behavior to wording and breaks on any rephrasing, even when the category is unchanged. Wrap a per-category sentinel error and expose it via `Unwrap`, then classify with `errors.Is(err, ErrUnterminatedString)`. The message remains free to carry human detail; the identity check remains stable.

---

Next: [01-monkey-lexer.md](01-monkey-lexer.md)
