# 4. SQL Lexer and Tokenizer — Concepts

A SQL lexer converts a raw string into a flat sequence of typed tokens before any parsing begins. The hard parts are not the happy path — they are the failure modes: unterminated strings, nested block comments, case-insensitive keyword recognition, multi-line position tracking, and the guarantee that the lexer never panics on any input. This file is the conceptual foundation: read it once and you will have everything you need to reason through each of the exercises, which build a hand-written, single-pass Go lexer for SQL piece by piece as independent, self-contained Go modules with no external dependencies.

## Concepts

### The Single-Pass Scanner Model

A SQL lexer is a finite automaton that reads the source left to right, consuming one byte at a time. The two-offset cursor model used here — one index for the current byte, one for the next byte to read — is the same model Go's own toolchain scanner uses: the standard library's `go/scanner` names those two offsets `offset` and `rdOffset` (with the current rune in `ch`). This lexer keeps the identical two-offset idea but spells the offsets `pos` and `readPos`. The `Lexer` struct stores four values:

```text
input   string   // the full source, immutable
pos     int      // byte index of ch (the current byte)
readPos int      // byte index of the next byte to consume
ch      byte     // the byte at pos; 0 at EOF
```

`readChar()` advances `pos` to `readPos`, loads `input[readPos]` into `ch`, and increments `readPos`. `peekChar()` returns `input[readPos]` without advancing. This gives O(1) lookahead and no allocations during scanning. When `readPos` reaches `len(input)`, `ch` is set to `0` (the null byte, which SQL does not use as a printable character), and the lexer returns `TokenEOF` on every subsequent call.

### Token Representation

Each token carries five fields: type, literal, byte offset (`Pos`), line, and column. Byte offsets allow the parser and error reporter to highlight the exact span. Line and column are 1-based, matching what editors and SQL clients report to users.

The `Literal` field holds:

- the raw text for identifiers and literals (`my_column`, `42`, `it's`),
- the canonical uppercase spelling for keywords (`SELECT`, not `select`).

Storing uppercase in keywords means the parser can compare `tok.Literal == "SELECT"` without calling `strings.ToUpper` at every parse site.

### Keyword Recognition

SQL keywords are case-insensitive: `SELECT`, `select`, and `Select` all mean the same thing. The idiomatic Go approach lexes any alphabetic sequence as an identifier first, then calls `strings.ToUpper` and looks up the result in a `map[string]TokenType`. The map is built once at package init and never mutated. This keeps the scanner loop simple — one call to `isLetter` per byte — and concentrates keyword logic in a single lookup.

### String Literals and Identifier Quoting

SQL standard single-quote strings escape embedded quotes by doubling: `'it''s a string'` produces the string value `it's a string`. The lexer peeks one byte ahead when it sees `'`: if the next byte is also `'`, it consumes both and appends one `'` to the literal buffer; otherwise `'` ends the string.

Double-quoted identifiers (`"column name"`) permit spaces and reserved words as identifier names. They follow the same doubling rule for embedded `"`. The `Literal` field of the resulting `TokenQIdent` token holds the unquoted, unescaped value.

### Nested Block Comments

SQL supports `--` line comments and `/* ... */` block comments. Some dialects (PostgreSQL included) allow nested block comments: `/* outer /* inner */ still outer */`. A regex cannot match these because nesting is not a regular language — it requires a counter. The lexer increments the counter on `/*`, decrements on `*/`, and only ends the comment when the counter returns to zero.

### Error Tokens, Never Panics

The lexer must be total: on any input, including unterminated strings, unclosed block comments, and unknown byte values, it must return a `TokenError` with a descriptive `Literal`. It must never panic. This is the contract the parser depends on: the parser will encounter the error token and report it through normal error handling rather than a crash.

### Case Folding: Keywords vs Quoted Identifiers

Case handling in SQL is not uniform, and the lexer must get the boundaries right. Three rules apply:

- Keywords are case-insensitive: `SELECT`, `select`, and `Select` are the same token. This lexer canonicalizes them to uppercase in `Literal`, so every parse site can compare against `"SELECT"` without re-folding.
- Quoted identifiers (`"Foo"`) are case-sensitive and must be preserved byte-for-byte. `"Foo"`, `"foo"`, and `"FOO"` are three distinct column names; folding them would silently merge schemas. The lexer copies the unquoted bytes verbatim into `TokenQIdent`.
- Unquoted identifiers are where dialects disagree. The SQL standard folds them to upper case; PostgreSQL folds them to lower case (a documented deviation in the Postgres lexical-structure docs). To avoid baking a dialect choice into the scanner, this lexer leaves an unquoted identifier's bytes unchanged in `TokenIdent` and defers any folding to a later name-resolution stage. The lexer's only case decision is the keyword canonicalization above, which is dialect-independent.

The single design rule: fold exactly one thing (keyword spelling), preserve everything else, and never fold a quoted identifier.

### Maximal Munch (Longest Match)

At each position the scanner consumes the longest sequence that forms a valid token — the maximal-munch (or longest-match) rule. `>=` is one token, not `>` then `=`; `<>` is one token; an identifier runs until the first byte that is not a letter, digit, or underscore. This is why `NextToken` tests the two-character operator forms (`<=`, `>=`, `<>`, `!=`) before falling through to the single-character ones, and why `readIdentOrKeyword` and `readNumber` loop greedily rather than stopping at the first acceptable prefix.

The one deliberate departure is the trailing decimal dot. Pure maximal munch would read `42.col` as the float `42.` followed by `col`; the lexer instead consumes the fractional dot only when a digit follows it, so `42.col` becomes `42`, `.`, `col`. This trades strict longest-match for the tokenization the parser actually wants, and it is the kind of dialect-aware exception that belongs in the lexer rather than the grammar.

### Lookahead at the Lexer/Parser Boundary

The lexer needs only O(1) lookahead: `peekChar` inspects exactly one byte ahead, which is enough to resolve every maximal-munch decision (`-` vs `--`, `/` vs `/*`, `<` vs `<=`, a fractional dot vs a punctuation dot). It deliberately never needs unbounded lookahead, because each token is a function of the current byte plus at most one peek. Decisions that require more context are pushed up to the parser: whether `LEFT` introduces a `LEFT JOIN` or names a column, whether `(` opens a subquery or a grouped expression, whether `NOT` belongs to `NOT NULL`, `NOT IN`, or a boolean prefix. Keeping the lexer context-free — a pure function from bytes to a flat token stream — concentrates all grammatical ambiguity in one place and keeps the scanner a tight inner loop.

### Comment Nesting and the Counting Argument

Line comments (`--`) run to the end of the line and never nest; the lexer simply discards bytes up to `\n` or EOF. Block comments are the interesting case. The SQL standard's `/* */` comments do not nest, but PostgreSQL nests them on purpose so a programmer can comment out a region that already contains a block comment. Nesting makes the construct non-regular: a fixed-state machine cannot match balanced `/* */` pairs, so a regex such as `/\*.*?\*/` is wrong on `/* a /* b */ c */` — it stops at the first `*/` and leaves `c */` dangling. The depth counter is the minimal extra state, a single integer, that recognizes the nested (context-free) structure: increment on `/*`, decrement on `*/`, end at zero, and report `TokenError` if EOF arrives while depth is still positive.

### Numeric-Literal Grammar

The numeric grammar this lexer accepts, with the edge cases that matter:

```text
integer    = digit { digit }                         -- 0, 42, 100
decimal    = digit { digit } "." digit { digit }     -- 3.14   (digit required after ".")
scientific = ( integer | decimal ) ( "e" | "E" ) [ "+" | "-" ] digit { digit }
```

Notable points. `1e10` is a float even with no dot. `1.5e-3` and `9.9E+6` carry a signed exponent. A lone `.` is punctuation (`TokenDot`), not a number. `1.` is lexed as the integer `1` then `TokenDot`, because the fractional dot requires a following digit (the `42.col` rule above). A malformed exponent — `1e`, `1e+` with no digits after — is returned as a `TokenError`, never a silently truncated `1`. The lexer does not consume a leading sign (`-1` is `TokenMinus` then `1`; the sign belongs to the expression grammar) and does not handle hexadecimal or `0x` forms; both are deliberate scope choices documented at the boundary. The Postgres numeric-constant grammar is the reference for these rules.

### Error Tokens and Recovery

The error token is a value in the stream, not an exception. It carries `Pos`, `Line`, and `Col` alongside a human-readable `Literal`, which gives a reporter the `line:col: message` form users expect and — more importantly — lets a recovering parser resynchronize. On hitting a `TokenError`, a parser can skip forward to the next statement boundary (`;`) and keep parsing the rest of the script, surfacing several diagnostics from one pass instead of aborting on the first. Because the error token is totally ordered with the rest of the stream, position information is preserved and the parser's control flow stays linear; a panic, by contrast, discards position and unwinds the stack. Treating `TokenEOF` and `TokenError` symmetrically — both stop a tokenize loop and both end an iterator — means every downstream loop has exactly one termination test.

## Common Mistakes

### Treating a Trailing Dot as Part of a Float

Wrong: consuming `table.column` as the identifier `table`, then trying to lex `.column` as a float, which fails because `.` is not followed by a digit.

What happens: the number reader starts on the digit, reaches `.`, sees the next char is `c` (not a digit), and must decide. If it consumes the dot unconditionally, `.column` becomes a malformed token.

Fix: check `isDigit(l.peekChar())` before consuming the decimal dot. A lone `.` after an integer is left for the next `NextToken` call, which returns `TokenDot`.

### Stopping a Block Comment at the First `*/`

Wrong: `depth` always 1; as soon as `*` followed by `/` is seen, the comment ends.

What happens: `/* outer /* inner */ still outer */` — the comment ends after `inner */`, and `still outer */` is tokenized as identifiers and an error for the stray `*/`.

Fix: the depth counter. Increment on `/*`, decrement on `*/`, and only break when `depth == 0`.

### Reading Keywords as Identifiers

Wrong: comparing `l.ch` to `'S'`, `'E'`, `'L'`, ... inside the scanner loop.

What happens: case sensitivity breaks (`select != SELECT`), and the scan loop balloons to handle dozens of keywords character by character.

Fix: lex alphabetic sequences uniformly as identifiers, then do one `strings.ToUpper` and one map lookup in `lookupIdent`. The scanner stays simple; the keyword table is the single source of truth.

### Panicking on Unknown Input

Wrong: `panic(fmt.Sprintf("unexpected: %c", l.ch))`.

What happens: any input containing `@`, `#`, or a non-ASCII byte crashes the process.

Fix: return a `TokenError` with the offending character and its position in the `Literal` field. The contract is that the lexer never panics.

### Forgetting to Advance Past a Single-Char Token

Wrong: returning the token for `(` without calling `readChar()`.

What happens: `NextToken` is called again, `l.ch` is still `(`, and the same token is returned in an infinite loop.

Fix: keep the advance responsibility in one place. A `readSingle` helper does not advance; the caller (`NextToken`'s `default` branch) calls `l.readChar()` after it returns.

---

Next: [01-token-types.md](01-token-types.md)
