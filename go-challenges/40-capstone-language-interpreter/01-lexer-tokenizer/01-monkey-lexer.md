# Exercise 1: A Complete Lexer for the Monkey Language

This exercise builds the whole lexer in one self-contained package: token definitions, a single-pass scanner with one-character look-ahead, UTF-8-aware position tracking, numeric literals in four bases, string escape decoding, comment skipping, and error recovery that collects every lexical error in one run. The four concerns — token data, the scanner, the demo, and the tests — are one tightly-coupled unit, so they live together in one Go module rather than being split into pieces that would each have to duplicate the scanner to stand alone. Work through the explanation alongside the code; the design choices were argued in the concepts file, and here you see them turn into a program that runs.

## What you'll build

```text
monkey-lexer/
  go.mod
  lexer/
    token.go        TokenType, Token, token-type constants, keywords map, lookupIdent,
                    sentinel errors (ErrUnterminatedString, ErrInvalidEscape, ErrIllegalCharacter)
    lexer.go        Lexer, New, NextToken, Tokenize, Errors, LexError (+Unwrap),
                    readChar, peekChar, skipWhitespaceAndComments, readIdentifier,
                    readNumber, readString, decodeEscape, isIdentStart/Continue, isHexDigit
    lexer_test.go   Example + table tests for operators, keywords, numbers, strings,
                    UTF-8 offset/column, error recovery, line tracking, batch==stream
  cmd/
    demo/
      main.go       tokenize a Monkey fibonacci program and print every token's position
```

- Files: `lexer/token.go`, `lexer/lexer.go`, `lexer/lexer_test.go`, `cmd/demo/main.go`.
- Implement: `TokenType`, `Token`, the token-type constants, `keywords`/`lookupIdent`, the three sentinel errors; `Lexer`, `New`, `NextToken`, `Tokenize`, `Errors`, `LexError` with `Unwrap`, and the scanning helpers `readChar`, `peekChar`, `skipWhitespaceAndComments`, `readIdentifier`, `readNumber`, `readString`, `decodeEscape`, `isIdentStart`, `isIdentContinue`, `isHexDigit`.
- Test: operators (single and two-character), keywords versus identifiers, Unicode identifiers, byte-offset-versus-rune-column, integers in four bases, floats, the ambiguous dot, strings and escapes, the three error categories, error recovery, line/column tracking, comment skipping, and batch/stream agreement.
- Verify: `go test -count=1 -race ./...`

### Token types and the data the scanner produces

A token needs a type, the original text, and a position. The type is modeled as `type TokenType string` so the constant values are also their display names: the equality operator's type is literally `"=="`, an identifier's is `"IDENT"`, the `let` keyword's is `"let"`. That makes a failing test read `got IDENT, want let` with no lookup table, at the cost of no compiler exhaustiveness check on a switch — the trade argued in the concepts file. The `Token` struct then carries both an `Offset` (byte position, for slicing the source) and a `Column` (rune position, for editor-style diagnostics); these agree for ASCII and diverge for any multi-byte character, which is why both exist.

Keywords are distinguished from identifiers by a single package-level map consulted after an identifier is scanned. The map is built once and never mutated, so it is safe to read without locking, and adding a keyword is a one-line edit. The three sentinel errors are package-level values that a custom error type will wrap, so callers classify failures with `errors.Is` rather than by matching message text.

Create `lexer/token.go`:

```go
package lexer

import "errors"

// TokenType classifies a scanned token. The string value is the display name
// used in error messages and test output.
type TokenType string

const (
	// Special
	ILLEGAL TokenType = "ILLEGAL"
	EOF     TokenType = "EOF"

	// Literals
	IDENT  TokenType = "IDENT"
	INT    TokenType = "INT"
	FLOAT  TokenType = "FLOAT"
	STRING TokenType = "STRING"

	// Single-character operators
	ASSIGN   TokenType = "="
	PLUS     TokenType = "+"
	MINUS    TokenType = "-"
	BANG     TokenType = "!"
	ASTERISK TokenType = "*"
	SLASH    TokenType = "/"
	PERCENT  TokenType = "%"
	LT       TokenType = "<"
	GT       TokenType = ">"
	DOT      TokenType = "."

	// Two-character operators
	EQ     TokenType = "=="
	NEQ    TokenType = "!="
	LTE    TokenType = "<="
	GTE    TokenType = ">="
	AND    TokenType = "&&"
	OR     TokenType = "||"
	ARROW  TokenType = "=>"
	DOTDOT TokenType = ".."

	// Delimiters
	COMMA     TokenType = ","
	SEMICOLON TokenType = ";"
	COLON     TokenType = ":"
	LPAREN    TokenType = "("
	RPAREN    TokenType = ")"
	LBRACE    TokenType = "{"
	RBRACE    TokenType = "}"
	LBRACKET  TokenType = "["
	RBRACKET  TokenType = "]"

	// Keywords
	FUNCTION TokenType = "fn"
	LET      TokenType = "let"
	TRUE     TokenType = "true"
	FALSE    TokenType = "false"
	IF       TokenType = "if"
	ELSE     TokenType = "else"
	RETURN   TokenType = "return"
)

// keywords maps source text to keyword token types. Pre-computed at init time;
// never mutated, so no synchronization is needed.
var keywords = map[string]TokenType{
	"fn":     FUNCTION,
	"let":    LET,
	"true":   TRUE,
	"false":  FALSE,
	"if":     IF,
	"else":   ELSE,
	"return": RETURN,
}

// lookupIdent returns the keyword type for s, or IDENT if s is not a keyword.
func lookupIdent(s string) TokenType {
	if tt, ok := keywords[s]; ok {
		return tt
	}
	return IDENT
}

// Token is a single lexical unit produced by the lexer.
type Token struct {
	Type    TokenType
	Literal string
	Line    int // 1-indexed line number of the first character
	Column  int // 1-indexed column in runes (not bytes) of the first character
	Offset  int // byte offset of the first character from the start of input
}

// Sentinel errors wrapped by LexError. Use errors.Is to test for a category.
var (
	ErrUnterminatedString = errors.New("unterminated string literal")
	ErrInvalidEscape      = errors.New("invalid escape sequence")
	ErrIllegalCharacter   = errors.New("illegal character")
)
```

### The scanner core: cursor, look-ahead, and the dispatch switch

The `Lexer` holds the input plus a cursor expressed as two byte positions (`pos` for the current character, `readPos` for the next), the decoded current rune `ch` (with `0` meaning end-of-input), and the running `line`/`col`. `New` constructs it and immediately calls `readChar` once to prime `ch` with the first character — without that priming call, `ch` would be the zero rune (end-of-input) and the first token would be wrong, which is one of the classic pitfalls.

`readChar` is the only place the cursor moves, and it does three things in a deliberate order. It decodes the next rune with `utf8.DecodeRuneInString`, which returns the rune and its byte width; it advances `readPos` by that byte width so the next slice boundary stays on a valid UTF-8 edge; and before counting the new character it checks whether the character it is leaving behind was a newline, bumping `line` and resetting `col` if so. That ordering is what places the newline on line N and the next character on line N+1 at column 1, matching editors. The column is incremented by exactly one per rune while the byte offset tracks bytes, so the two diverge precisely for multi-byte characters. `peekChar` decodes the next rune without advancing — the one-character look-ahead the operator logic depends on.

`NextToken` first skips whitespace and `//` comments, then snapshots the position of the token it is about to read (`off`, `line`, `col`) so the returned token points at its first character even though scanning will move the cursor past it. The big `switch` dispatches on the current rune. Every prefix-sharing operator peeks to decide: `=` becomes `==`, `=>`, or `=`; `!`, `<`, `>` extend with `=`; `&` and `|` require a doubled partner and otherwise are illegal; `.` extends to `..` only when followed by another dot. Each multi-character case advances the cursor itself and returns immediately — there is no shared trailing advance to fall through into, which is the bug that would otherwise eat the character after a two-character operator. Anything not an operator or delimiter falls to the default: an identifier start begins an identifier, a digit begins a number, and anything else is recorded as an illegal character and scanning continues.

Create `lexer/lexer.go`:

```go
package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LexError describes a single lexical error. It wraps one of the sentinel
// errors (ErrUnterminatedString, ErrInvalidEscape, ErrIllegalCharacter) via
// Unwrap so callers can use errors.Is for category checks.
type LexError struct {
	Err    error
	Msg    string
	Line   int
	Column int
}

func (e *LexError) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Column, e.Msg)
}

func (e *LexError) Unwrap() error { return e.Err }

// Lexer scans Monkey source text into a token stream. It is a single-pass
// scanner with one-character look-ahead via peekChar. Column is tracked in
// runes; Offset is tracked in bytes; they diverge for multi-byte characters.
type Lexer struct {
	input   string
	pos     int  // byte offset of l.ch
	readPos int  // byte offset of the next character to read
	ch      rune // current character; 0 means EOF
	line    int  // 1-indexed line number of l.ch
	col     int  // 1-indexed column in runes of l.ch
	errors  []*LexError
}

// New returns a Lexer primed on input: l.ch holds the first character.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1}
	l.readChar()
	return l
}

// Errors returns all lexical errors collected so far.
func (l *Lexer) Errors() []*LexError { return l.errors }

// Tokenize is a convenience wrapper: it scans the entire input and returns all
// tokens (including the terminal EOF) plus any lexical errors.
func Tokenize(input string) ([]Token, []*LexError) {
	l := New(input)
	var tokens []Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == EOF {
			break
		}
	}
	return tokens, l.Errors()
}

// NextToken returns the next token. After EOF, subsequent calls return EOF again.
func (l *Lexer) NextToken() Token {
	l.skipWhitespaceAndComments()

	off := l.pos
	line := l.line
	col := l.col

	if l.ch == 0 {
		return l.tok(EOF, "", off, line, col)
	}

	switch l.ch {
	case '=':
		switch l.peekChar() {
		case '=':
			l.readChar()
			l.readChar()
			return l.tok(EQ, "==", off, line, col)
		case '>':
			l.readChar()
			l.readChar()
			return l.tok(ARROW, "=>", off, line, col)
		}
		l.readChar()
		return l.tok(ASSIGN, "=", off, line, col)

	case '!':
		if l.peekChar() == '=' {
			l.readChar()
			l.readChar()
			return l.tok(NEQ, "!=", off, line, col)
		}
		l.readChar()
		return l.tok(BANG, "!", off, line, col)

	case '<':
		if l.peekChar() == '=' {
			l.readChar()
			l.readChar()
			return l.tok(LTE, "<=", off, line, col)
		}
		l.readChar()
		return l.tok(LT, "<", off, line, col)

	case '>':
		if l.peekChar() == '=' {
			l.readChar()
			l.readChar()
			return l.tok(GTE, ">=", off, line, col)
		}
		l.readChar()
		return l.tok(GT, ">", off, line, col)

	case '&':
		if l.peekChar() == '&' {
			l.readChar()
			l.readChar()
			return l.tok(AND, "&&", off, line, col)
		}
		ch := l.ch
		l.readChar()
		return l.illegalTok(off, line, col, ch)

	case '|':
		if l.peekChar() == '|' {
			l.readChar()
			l.readChar()
			return l.tok(OR, "||", off, line, col)
		}
		ch := l.ch
		l.readChar()
		return l.illegalTok(off, line, col, ch)

	case '.':
		if l.peekChar() == '.' {
			l.readChar()
			l.readChar()
			return l.tok(DOTDOT, "..", off, line, col)
		}
		l.readChar()
		return l.tok(DOT, ".", off, line, col)

	case '+':
		l.readChar()
		return l.tok(PLUS, "+", off, line, col)
	case '-':
		l.readChar()
		return l.tok(MINUS, "-", off, line, col)
	case '*':
		l.readChar()
		return l.tok(ASTERISK, "*", off, line, col)
	case '/':
		l.readChar()
		return l.tok(SLASH, "/", off, line, col)
	case '%':
		l.readChar()
		return l.tok(PERCENT, "%", off, line, col)
	case ',':
		l.readChar()
		return l.tok(COMMA, ",", off, line, col)
	case ';':
		l.readChar()
		return l.tok(SEMICOLON, ";", off, line, col)
	case ':':
		l.readChar()
		return l.tok(COLON, ":", off, line, col)
	case '(':
		l.readChar()
		return l.tok(LPAREN, "(", off, line, col)
	case ')':
		l.readChar()
		return l.tok(RPAREN, ")", off, line, col)
	case '{':
		l.readChar()
		return l.tok(LBRACE, "{", off, line, col)
	case '}':
		l.readChar()
		return l.tok(RBRACE, "}", off, line, col)
	case '[':
		l.readChar()
		return l.tok(LBRACKET, "[", off, line, col)
	case ']':
		l.readChar()
		return l.tok(RBRACKET, "]", off, line, col)

	case '"':
		return l.readString(off, line, col)

	default:
		if isIdentStart(l.ch) {
			return l.readIdentifier(off, line, col)
		}
		if unicode.IsDigit(l.ch) {
			return l.readNumber(off, line, col)
		}
		ch := l.ch
		l.readChar()
		return l.illegalTok(off, line, col, ch)
	}
}

// readChar advances the lexer by one rune. After the last character, l.ch == 0.
// Line tracking: l.line is incremented when the previous l.ch was '\n', so
// the newline itself is on line N and the character after it is on line N+1.
func (l *Lexer) readChar() {
	if l.readPos >= len(l.input) {
		if l.ch == '\n' {
			l.line++
			l.col = 0
		}
		l.ch = 0
		l.pos = len(l.input)
		return
	}
	r, w := utf8.DecodeRuneInString(l.input[l.readPos:])
	l.pos = l.readPos
	l.readPos += w
	if l.ch == '\n' {
		l.line++
		l.col = 0
	}
	l.col++
	l.ch = r
}

// peekChar returns the character after l.ch without consuming it.
func (l *Lexer) peekChar() rune {
	if l.readPos >= len(l.input) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.input[l.readPos:])
	return r
}

// skipWhitespaceAndComments consumes ASCII whitespace and // line comments.
func (l *Lexer) skipWhitespaceAndComments() {
	for {
		switch l.ch {
		case ' ', '\t', '\r', '\n':
			l.readChar()
		case '/':
			if l.peekChar() != '/' {
				return
			}
			for l.ch != '\n' && l.ch != 0 {
				l.readChar()
			}
		default:
			return
		}
	}
}

// tok constructs a Token with the given fields.
func (l *Lexer) tok(tt TokenType, lit string, off, line, col int) Token {
	return Token{Type: tt, Literal: lit, Offset: off, Line: line, Column: col}
}

// illegalTok records a LexError and returns an ILLEGAL token.
func (l *Lexer) illegalTok(off, line, col int, ch rune) Token {
	msg := fmt.Sprintf("illegal character %q", ch)
	l.errors = append(l.errors, &LexError{
		Err: ErrIllegalCharacter, Msg: msg, Line: line, Column: col,
	})
	return Token{
		Type: ILLEGAL, Literal: string(ch),
		Offset: off, Line: line, Column: col,
	}
}

// isIdentStart reports whether r can begin an identifier.
func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

// isIdentContinue reports whether r can continue an identifier.
func isIdentContinue(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// readIdentifier scans from the current position and returns an IDENT or
// keyword token. On return, l.ch is the first character after the identifier.
func (l *Lexer) readIdentifier(off, line, col int) Token {
	start := l.pos
	for isIdentContinue(l.ch) {
		l.readChar()
	}
	lit := l.input[start:l.pos]
	return l.tok(lookupIdent(lit), lit, off, line, col)
}

// readNumber scans an integer or floating-point literal.
//
// Supported forms:
//   - Decimal:     42, 1_000_000
//   - Hexadecimal: 0xFF, 0xDEAD_BEEF
//   - Octal:       0o77, 0o644
//   - Binary:      0b1010, 0b1111_0000
//   - Float:       3.14, 1.5e10, 2.3E-4
//
// On return, l.ch is the first character after the literal.
func (l *Lexer) readNumber(off, line, col int) Token {
	start := l.pos
	tt := INT

	if l.ch == '0' {
		switch l.peekChar() {
		case 'x', 'X':
			l.readChar() // consume '0'
			l.readChar() // consume 'x'/'X'
			for isHexDigit(l.ch) || l.ch == '_' {
				l.readChar()
			}
			return l.tok(INT, l.input[start:l.pos], off, line, col)
		case 'o', 'O':
			l.readChar()
			l.readChar()
			for (l.ch >= '0' && l.ch <= '7') || l.ch == '_' {
				l.readChar()
			}
			return l.tok(INT, l.input[start:l.pos], off, line, col)
		case 'b', 'B':
			l.readChar()
			l.readChar()
			for l.ch == '0' || l.ch == '1' || l.ch == '_' {
				l.readChar()
			}
			return l.tok(INT, l.input[start:l.pos], off, line, col)
		}
	}

	// Decimal integer, possibly followed by a fractional or exponent part.
	for unicode.IsDigit(l.ch) || l.ch == '_' {
		l.readChar()
	}

	// Fractional part: only if next char after '.' is a digit (avoids
	// consuming the '.' in a method call like x.Len()).
	if l.ch == '.' && unicode.IsDigit(l.peekChar()) {
		tt = FLOAT
		l.readChar() // consume '.'
		for unicode.IsDigit(l.ch) || l.ch == '_' {
			l.readChar()
		}
	}

	// Exponent part.
	if l.ch == 'e' || l.ch == 'E' {
		tt = FLOAT
		l.readChar()
		if l.ch == '+' || l.ch == '-' {
			l.readChar()
		}
		for unicode.IsDigit(l.ch) {
			l.readChar()
		}
	}

	return l.tok(tt, l.input[start:l.pos], off, line, col)
}

// readString scans a double-quoted string literal. Recognized escape sequences
// are decoded into the Literal field. An unterminated string (EOF or unescaped
// newline) appends a LexError and returns the partial literal.
//
// Recognized escapes: \n \t \r \\ \" \0
// Any other \X is an ErrInvalidEscape.
func (l *Lexer) readString(off, line, col int) Token {
	l.readChar() // consume opening '"'
	var b strings.Builder
	for {
		switch l.ch {
		case 0:
			l.errors = append(l.errors, &LexError{
				Err:    ErrUnterminatedString,
				Msg:    fmt.Sprintf("unterminated string literal started at line %d", line),
				Line:   line,
				Column: col,
			})
			return Token{Type: STRING, Literal: b.String(), Offset: off, Line: line, Column: col}
		case '\n':
			l.errors = append(l.errors, &LexError{
				Err:    ErrUnterminatedString,
				Msg:    fmt.Sprintf("unterminated string literal: newline at line %d", l.line),
				Line:   line,
				Column: col,
			})
			l.readChar()
			return Token{Type: STRING, Literal: b.String(), Offset: off, Line: line, Column: col}
		case '"':
			l.readChar() // consume closing '"'
			return Token{Type: STRING, Literal: b.String(), Offset: off, Line: line, Column: col}
		case '\\':
			l.readChar() // consume '\'
			r, ok := decodeEscape(l.ch)
			if !ok {
				l.errors = append(l.errors, &LexError{
					Err:    ErrInvalidEscape,
					Msg:    fmt.Sprintf("invalid escape sequence \\%c", l.ch),
					Line:   l.line,
					Column: l.col,
				})
				b.WriteRune('\\')
				b.WriteRune(l.ch)
			} else {
				b.WriteRune(r)
			}
			l.readChar()
		default:
			b.WriteRune(l.ch)
			l.readChar()
		}
	}
}

// decodeEscape maps a single escape character to its decoded rune.
func decodeEscape(ch rune) (rune, bool) {
	switch ch {
	case 'n':
		return '\n', true
	case 't':
		return '\t', true
	case 'r':
		return '\r', true
	case '\\':
		return '\\', true
	case '"':
		return '"', true
	case '0':
		return 0, true
	}
	return 0, false
}

// isHexDigit reports whether r is a valid hexadecimal digit.
func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'f') ||
		(r >= 'A' && r <= 'F')
}
```

### How identifiers, numbers, and strings actually scan

`readIdentifier` records the starting byte offset, consumes the maximal run of identifier-continue characters, then slices `input[start:pos]` and hands it to `lookupIdent`. Because the cursor moves in whole runes and the slice is taken on byte boundaries the cursor visited, the slice is always valid UTF-8 — so a Greek or CJK identifier comes through intact and is classified as `IDENT` unless it happens to be one of the seven keywords.

`readNumber` shows the four-base design. A leading `0` is the signal to peek: `x`/`X`, `o`/`O`, or `b`/`B` switches into hexadecimal, octal, or binary, after which the loop consumes the digits valid for that base plus underscores and returns an `INT`. With no base prefix the scanner consumes decimal digits, then — and this is the ambiguity resolver — it consumes a `.` as a fractional point only when `peekChar` reports a digit immediately after it. That guard is exactly why `x.Len` lexes as identifier-dot-identifier while `3.14` lexes as a single float: in `x.Len` the character after the dot is a letter, so the number (here, nothing) ends and the dot becomes its own token. An `e`/`E` with an optional sign adds an exponent and marks the literal `FLOAT`.

`readString` is where decoding and recovery meet. It accumulates into a `strings.Builder` because the decoded literal differs from the source spelling — `\n` in the source becomes one newline rune in the literal. Hitting end-of-input or a raw newline before the closing quote records an `ErrUnterminatedString` and returns the partial literal rather than aborting; a backslash followed by an unrecognized character records an `ErrInvalidEscape`, writes the two characters through verbatim, and continues. Each recorded `LexError` wraps a sentinel and exposes it through `Unwrap`, so `errors.Is(err, ErrUnterminatedString)` works without touching the message text. That is the whole point of error recovery: one scan surfaces every problem, and the categories are checkable by identity.

### The runnable demo

The demo tokenizes a small but complete Monkey program — a recursive fibonacci with a comment, a function literal, comparison and arithmetic operators, and nested blocks — and prints each token's line, column, type, and literal. It exercises comment skipping (the `// compute fibonacci` line never appears as a token), keyword versus identifier classification, the two-character `<=`, and multi-line position tracking all at once. With valid source there are no errors, so the program ends by reporting the token count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/monkey-lexer/lexer"
)

const src = `// compute fibonacci
let fib = fn(n) {
	if n <= 1 {
		return n;
	}
	return fib(n - 1) + fib(n - 2);
};
let result = fib(10);`

func main() {
	tokens, errs := lexer.Tokenize(src)

	fmt.Println("Tokens:")
	for _, tok := range tokens {
		if tok.Type == lexer.EOF {
			break
		}
		fmt.Printf("  %d:%-3d %-12s %q\n",
			tok.Line, tok.Column, tok.Type, tok.Literal)
	}

	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "Lexical errors:")
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, " ", e)
		}
		os.Exit(1)
	}

	fmt.Printf("\n%d tokens (excluding EOF)\n", len(tokens)-1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Tokens:
  2:1   let          "let"
  2:5   IDENT        "fib"
  2:9   =            "="
  2:11  fn           "fn"
  2:13  (            "("
  2:14  IDENT        "n"
  2:15  )            ")"
  2:17  {            "{"
  3:2   if           "if"
  3:5   IDENT        "n"
  3:7   <=           "<="
  3:10  INT          "1"
  3:12  {            "{"
  4:3   return       "return"
  4:10  IDENT        "n"
  4:11  ;            ";"
  5:2   }            "}"
  6:2   return       "return"
  6:9   IDENT        "fib"
  6:12  (            "("
  6:13  IDENT        "n"
  6:15  -            "-"
  6:17  INT          "1"
  6:18  )            ")"
  6:20  +            "+"
  6:22  IDENT        "fib"
  6:25  (            "("
  6:26  IDENT        "n"
  6:28  -            "-"
  6:30  INT          "2"
  6:31  )            ")"
  6:32  ;            ";"
  7:1   }            "}"
  7:2   ;            ";"
  8:1   let          "let"
  8:5   IDENT        "result"
  8:12  =            "="
  8:14  IDENT        "fib"
  8:17  (            "("
  8:18  INT          "10"
  8:20  )            ")"
  8:21  ;            ";"

42 tokens (excluding EOF)
```

The first token is `let` at line 2, because the comment on line 1 is skipped and contributes no token; the count at the end is the length of the token slice minus the terminal EOF.

### Tests

The tests pin every behavior the scanner promises. The two `Example` functions double as compiled documentation: `go test` runs them and compares their printed output against the `// Output:` block, so a regression in token-type names fails the suite. The table tests cover single- and two-character operators, keyword-versus-identifier classification, integers in all four bases with separators, floats, the ambiguous dot (`x.Len` must be three tokens, not a float), strings and their escapes, and the three error categories checked with `errors.Is`. `TestUTF8ByteOffsetVsRuneColumn` is the one that would catch a column-in-bytes bug: in `α = 1` the `=` is the third rune but byte offset 3, so it asserts `Column == 3` and `Offset == 3` separately. `TestErrorRecovery` proves that two illegal characters do not stop the scan and the valid tokens still come through, and `TestBatchAndStreamingAgree` proves the streaming and batch interfaces produce identical tokens including positions.

Create `lexer/lexer_test.go`:

```go
package lexer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ExampleTokenize verifies token type output for a basic expression.
func ExampleTokenize() {
	tokens, _ := Tokenize("let x = 5 + 10;")
	var types []string
	for _, tok := range tokens {
		if tok.Type == EOF {
			break
		}
		types = append(types, string(tok.Type))
	}
	fmt.Println(strings.Join(types, " "))
	// Output:
	// let IDENT = INT + INT ;
}

// ExampleLexer_NextToken shows the streaming interface on a function definition.
func ExampleLexer_NextToken() {
	l := New(`fn(x) { return x + 1; }`)
	var types []string
	for {
		tok := l.NextToken()
		if tok.Type == EOF {
			break
		}
		types = append(types, string(tok.Type))
	}
	fmt.Println(strings.Join(types, " "))
	// Output:
	// fn ( IDENT ) { return IDENT + INT ; }
}

func TestTokenizeOperators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src     string
		wantTyp TokenType
		wantLit string
	}{
		// Single-character
		{"=", ASSIGN, "="},
		{"!", BANG, "!"},
		{"<", LT, "<"},
		{">", GT, ">"},
		{"+", PLUS, "+"},
		{"-", MINUS, "-"},
		{"*", ASTERISK, "*"},
		{"/", SLASH, "/"},
		{"%", PERCENT, "%"},
		{".", DOT, "."},
		// Two-character
		{"==", EQ, "=="},
		{"!=", NEQ, "!="},
		{"<=", LTE, "<="},
		{">=", GTE, ">="},
		{"&&", AND, "&&"},
		{"||", OR, "||"},
		{"=>", ARROW, "=>"},
		{"..", DOTDOT, ".."},
	}

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			toks, _ := Tokenize(tc.src)
			got := toks[0]
			if got.Type != tc.wantTyp || got.Literal != tc.wantLit {
				t.Errorf("Tokenize(%q): got {%s %q}, want {%s %q}",
					tc.src, got.Type, got.Literal, tc.wantTyp, tc.wantLit)
			}
		})
	}
}

func TestTokenizeKeywords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src  string
		want TokenType
	}{
		{"fn", FUNCTION},
		{"let", LET},
		{"true", TRUE},
		{"false", FALSE},
		{"if", IF},
		{"else", ELSE},
		{"return", RETURN},
	}

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			toks, _ := Tokenize(tc.src)
			if toks[0].Type != tc.want {
				t.Errorf("Tokenize(%q): got %s, want %s", tc.src, toks[0].Type, tc.want)
			}
			// keyword literal equals the source text
			if toks[0].Literal != tc.src {
				t.Errorf("Tokenize(%q): literal = %q, want %q", tc.src, toks[0].Literal, tc.src)
			}
		})
	}
}

func TestIdentifierNotKeyword(t *testing.T) {
	t.Parallel()

	toks, _ := Tokenize("foobar")
	if toks[0].Type != IDENT {
		t.Fatalf("got %s, want IDENT", toks[0].Type)
	}
	if toks[0].Literal != "foobar" {
		t.Fatalf("literal = %q, want %q", toks[0].Literal, "foobar")
	}
}

func TestUnicodeIdentifier(t *testing.T) {
	t.Parallel()

	// Greek identifier: alpha is U+03B1, two bytes in UTF-8.
	toks, _ := Tokenize("αβγ")
	if toks[0].Type != IDENT {
		t.Fatalf("got %s, want IDENT for Unicode identifier", toks[0].Type)
	}
	if toks[0].Literal != "αβγ" {
		t.Fatalf("literal = %q, want %q", toks[0].Literal, "αβγ")
	}
	// Column is 1 (first rune), Offset is 0 (first byte). They agree here
	// because the identifier starts at position 0.
	if toks[0].Column != 1 {
		t.Fatalf("column = %d, want 1", toks[0].Column)
	}
	if toks[0].Offset != 0 {
		t.Fatalf("offset = %d, want 0", toks[0].Offset)
	}
}

func TestUTF8ByteOffsetVsRuneColumn(t *testing.T) {
	t.Parallel()

	// "α = 1": α is U+03B1 (2 bytes, bytes 0-1), space is 1 byte (byte 2),
	// so '=' is the third rune (Column 3) at byte offset 3.
	toks, _ := Tokenize("α = 1")
	eq := toks[1]
	if eq.Type != ASSIGN {
		t.Fatalf("toks[1].Type = %s, want ASSIGN", eq.Type)
	}
	if eq.Column != 3 {
		t.Errorf("'=' Column = %d, want 3 (third rune)", eq.Column)
	}
	if eq.Offset != 3 {
		t.Errorf("'=' Offset = %d, want 3 (byte 3)", eq.Offset)
	}
}

func TestTokenizeIntegers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src string
		lit string
	}{
		{"42", "42"},
		{"1_000_000", "1_000_000"},
		{"0xFF", "0xFF"},
		{"0xDEAD_BEEF", "0xDEAD_BEEF"},
		{"0o77", "0o77"},
		{"0o644", "0o644"},
		{"0b1010", "0b1010"},
		{"0b1111_0000", "0b1111_0000"},
	}

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			toks, _ := Tokenize(tc.src)
			got := toks[0]
			if got.Type != INT {
				t.Errorf("Tokenize(%q): type = %s, want INT", tc.src, got.Type)
			}
			if got.Literal != tc.lit {
				t.Errorf("Tokenize(%q): literal = %q, want %q", tc.src, got.Literal, tc.lit)
			}
		})
	}
}

func TestTokenizeFloats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src string
	}{
		{"3.14"},
		{"1.5e10"},
		{"2.3E-4"},
		{"0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			toks, _ := Tokenize(tc.src)
			if toks[0].Type != FLOAT {
				t.Errorf("Tokenize(%q): type = %s, want FLOAT", tc.src, toks[0].Type)
			}
			if toks[0].Literal != tc.src {
				t.Errorf("Tokenize(%q): literal = %q, want %q", tc.src, toks[0].Literal, tc.src)
			}
		})
	}
}

func TestDotNotConsumedByFloat(t *testing.T) {
	t.Parallel()

	// "x.Len" must NOT produce a FLOAT; the '.' is a DOT token.
	toks, _ := Tokenize("x.Len")
	want := []TokenType{IDENT, DOT, IDENT, EOF}
	if len(toks) != len(want) {
		t.Fatalf("Tokenize(%q): got %d tokens, want %d", "x.Len", len(toks), len(want))
	}
	for i, w := range want {
		if toks[i].Type != w {
			t.Errorf("toks[%d].Type = %s, want %s", i, toks[i].Type, w)
		}
	}
}

func TestTokenizeString(t *testing.T) {
	t.Parallel()

	toks, errs := Tokenize(`"hello, world"`)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := toks[0]
	if got.Type != STRING {
		t.Fatalf("type = %s, want STRING", got.Type)
	}
	if got.Literal != "hello, world" {
		t.Fatalf("literal = %q, want %q", got.Literal, "hello, world")
	}
}

func TestStringEscapeSequences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		src  string
		want string
	}{
		{`"\n"`, "\n"},
		{`"\t"`, "\t"},
		{`"\r"`, "\r"},
		{`"\\"`, "\\"},
		{`"\""`, "\""},
	}

	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()
			toks, errs := Tokenize(tc.src)
			if len(errs) != 0 {
				t.Fatalf("Tokenize(%q): unexpected errors: %v", tc.src, errs)
			}
			if toks[0].Literal != tc.want {
				t.Errorf("Tokenize(%q): literal = %q, want %q",
					tc.src, toks[0].Literal, tc.want)
			}
		})
	}
}

func TestUnterminatedString(t *testing.T) {
	t.Parallel()

	_, errs := Tokenize(`"hello`)
	if len(errs) == 0 {
		t.Fatal("want a LexError for unterminated string")
	}
	if !errors.Is(errs[0], ErrUnterminatedString) {
		t.Errorf("errors.Is(errs[0], ErrUnterminatedString) = false; err = %v", errs[0])
	}
}

func TestInvalidEscapeSequence(t *testing.T) {
	t.Parallel()

	_, errs := Tokenize(`"\q"`)
	if len(errs) == 0 {
		t.Fatal("want a LexError for invalid escape")
	}
	if !errors.Is(errs[0], ErrInvalidEscape) {
		t.Errorf("errors.Is(errs[0], ErrInvalidEscape) = false; err = %v", errs[0])
	}
}

func TestIllegalCharacter(t *testing.T) {
	t.Parallel()

	toks, errs := Tokenize("@")
	if len(errs) == 0 {
		t.Fatal("want a LexError for illegal character")
	}
	if !errors.Is(errs[0], ErrIllegalCharacter) {
		t.Errorf("errors.Is(errs[0], ErrIllegalCharacter) = false; err = %v", errs[0])
	}
	if toks[0].Type != ILLEGAL {
		t.Errorf("toks[0].Type = %s, want ILLEGAL", toks[0].Type)
	}
}

func TestErrorRecovery(t *testing.T) {
	t.Parallel()

	// Two illegal characters; the lexer must continue and produce both errors.
	toks, errs := Tokenize("let @ x $ = 5;")
	if len(errs) < 2 {
		t.Fatalf("want at least 2 errors, got %d: %v", len(errs), errs)
	}
	// Despite errors, valid tokens must still be present.
	var types []string
	for _, tok := range toks {
		if tok.Type != ILLEGAL && tok.Type != EOF {
			types = append(types, string(tok.Type))
		}
	}
	want := "let IDENT = INT ;"
	got := strings.Join(types, " ")
	if got != want {
		t.Errorf("valid token types = %q, want %q", got, want)
	}
}

func TestLineAndColumnTracking(t *testing.T) {
	t.Parallel()

	src := "let\nx = 5"
	toks, _ := Tokenize(src)
	// "let" is on line 1, col 1
	if toks[0].Line != 1 || toks[0].Column != 1 {
		t.Errorf("let: line=%d col=%d, want 1:1", toks[0].Line, toks[0].Column)
	}
	// "x" is on line 2, col 1
	if toks[1].Line != 2 || toks[1].Column != 1 {
		t.Errorf("x: line=%d col=%d, want 2:1", toks[1].Line, toks[1].Column)
	}
	// "=" is on line 2, col 3 (x=1, space=2, ==3)
	if toks[2].Line != 2 || toks[2].Column != 3 {
		t.Errorf("=: line=%d col=%d, want 2:3", toks[2].Line, toks[2].Column)
	}
}

func TestCommentSkipping(t *testing.T) {
	t.Parallel()

	toks, _ := Tokenize("// this is a comment\nlet x = 1;")
	// The comment must be invisible; the first token is "let".
	if toks[0].Type != LET {
		t.Fatalf("toks[0].Type = %s, want let (comment not skipped)", toks[0].Type)
	}
}

func TestBatchAndStreamingAgree(t *testing.T) {
	t.Parallel()

	src := `let add = fn(x, y) { return x + y; };`

	batchToks, _ := Tokenize(src)

	l := New(src)
	var streamToks []Token
	for {
		tok := l.NextToken()
		streamToks = append(streamToks, tok)
		if tok.Type == EOF {
			break
		}
	}

	if len(batchToks) != len(streamToks) {
		t.Fatalf("token count: batch=%d stream=%d", len(batchToks), len(streamToks))
	}
	for i := range batchToks {
		b := batchToks[i]
		s := streamToks[i]
		if b.Type != s.Type || b.Literal != s.Literal ||
			b.Line != s.Line || b.Column != s.Column {
			t.Errorf("token[%d]: batch=%+v, stream=%+v", i, b, s)
		}
	}
}
```

## Review

The scanner is correct when the awkward cases behave. Confirm that `x.Len` lexes as identifier, dot, identifier rather than a float — a single greedy `.` rule that ignores the character after the dot is the most common way to break member access. Confirm that a line beginning with a multi-byte character keeps `Offset` (bytes) and `Column` (runes) distinct: `α = 1` puts the `=` at column 3 and byte offset 3, and a scanner that advances the column by byte width would report column 4. Confirm that two illegal characters in one input produce two errors and still let every valid token through; a scanner that returns on the first error hides the rest. And confirm that the streaming `NextToken` loop and the batch `Tokenize` produce identical tokens down to their positions, since the two share one engine and any divergence signals a state bug.

The pitfalls worth watching for are the ones the concepts file named, now visible in this code. Each two-character operator case advances the cursor itself and returns; if you refactor toward a shared trailing advance, the character after `==`, `<=`, or `&&` will be skipped. `New` must call `readChar` once before any token is requested, or the first token reads the zero rune as end-of-input. Column tracking must add one per rune while the offset adds the byte width — never the byte width for both. Errors must be appended and scanning continued, never returned mid-scan, and they must be classified with `errors.Is` against the sentinels rather than by inspecting message text, so that improving a message never breaks a caller. Run `go test -count=1 -race ./...` and `gofmt -l .`: the race detector guards the (read-only, init-time) keyword map usage, and the `Example` functions fail loudly if any token-type display name drifts.

## Resources

- [Writing An Interpreter In Go — Thorsten Ball](https://interpreterbook.com/) — the Monkey language definition and a reference lexer this exercise is modeled on.
- [pkg.go.dev/unicode/utf8](https://pkg.go.dev/unicode/utf8) — `DecodeRuneInString`, the rune-and-width decode that `readChar` and `peekChar` rely on.
- [go.dev/ref/spec#Lexical_elements](https://go.dev/ref/spec#Lexical_elements) — Go's own lexical grammar, a precise reference for identifier, integer, and float syntax.
- [Rob Pike — Lexical Scanning in Go](https://go.dev/talks/2011/lex.slide) — the classic talk on a goroutine-and-channel lexer design; this exercise uses the simpler synchronous model for clarity.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../02-pratt-parser/00-concepts.md](../02-pratt-parser/00-concepts.md)
