# Exercise 3: String and Quoted-Identifier Literals

SQL has two doubled-quote conventions, and the lexer must get both exactly right. A single-quoted string escapes an embedded quote by doubling it (`'it''s'` means `it's`), and a double-quoted identifier escapes an embedded double-quote the same way (`"col""x"` names the column `col"x`). This exercise assembles the full lexer and concentrates on the two readers that decode those literals — peeking one byte ahead to tell an escape from a terminator — and on the error path when a quote is never closed.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             TokenType, the token constant block, IsKeyword, Token
keywords.go          the keyword map and lookupIdent
lexer.go             Lexer, NextToken, readString, readQuotedIdent, the readers
cmd/
  demo/
    main.go          tokenize a statement with strings and quoted identifiers
lexer_test.go        string escapes, quoted-identifier escapes, unterminated errors
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `cmd/demo/main.go`, `lexer_test.go`.
- Implement: the `readString` and `readQuotedIdent` methods on `Lexer` (the `''`/`""` doubling rule), inside the full baseline lexer.
- Test: `lexer_test.go` covers single-quote escapes, empty strings, double-quote identifier escapes, and the unterminated-string and unterminated-quoted-identifier error tokens.
- Verify: `go test -run 'TestTokenize|TestUnterminated|TestRoundTrip' -race ./...`

### Why a doubling escape, and why a builder instead of a slice

SQL strings cannot use C-style backslash escapes in the standard dialect; the only way to put a quote inside a single-quoted string is to write it twice. That choice has a pleasant property — the source never needs a separate escape character — but it forces the reader to decode rather than slice. When `readString` sees a `'`, it peeks one byte ahead: if the next byte is also `'`, the pair is an escaped quote, so it appends a single `'` to a `strings.Builder` and consumes both bytes; otherwise the `'` is the terminator and the string ends. Because the decoded value differs from the raw source bytes (two quotes become one), the literal cannot be a sub-slice of the input — it has to be built up in a buffer. That is why both readers accumulate into a `strings.Builder` and store `buf.String()` in the token, not an `input[start:end]` slice.

Quoted identifiers (`"column name"`) work the same way, with `"` as the quote, and exist for a different reason: they let an identifier contain spaces, punctuation, or a reserved word, and they make the name case-sensitive. The reader is structurally identical to the string reader — same peek-ahead, same doubling rule, same builder — but it emits `TokenQIdent` and the engine treats its bytes as a name rather than a value. The two readers stay separate functions precisely because their token types and downstream meaning differ even though their byte mechanics match.

The unterminated cases are where the never-panic contract earns its keep. If the input ends (`ch == 0`) before the closing quote arrives, the reader must return a `TokenError` whose `Literal` names the failure and carries the start position, not walk off the end of the buffer. Both readers test `ch == 0` at the top of every loop iteration, so an unterminated literal is a clean, positioned error token rather than a crash.

Create `token.go`:

```go
package lexer

import "fmt"

// TokenType identifies the category of a SQL token.
type TokenType int

const (
	// Special tokens.
	TokenIllegal TokenType = iota
	TokenError
	TokenEOF

	// Literals.
	TokenIdent  // unquoted identifier
	TokenQIdent // double-quoted identifier
	TokenInt    // integer literal: 42
	TokenFloat  // float literal: 3.14, 1.5e10
	TokenString // single-quoted string literal

	// Keywords — sentinels bracket the keyword block for IsKeyword.
	keywordStart
	TokenSelect
	TokenFrom
	TokenWhere
	TokenInsert
	TokenInto
	TokenValues
	TokenUpdate
	TokenSet
	TokenDelete
	TokenCreate
	TokenTable
	TokenDrop
	TokenIndex
	TokenOn
	TokenAnd
	TokenOr
	TokenNot
	TokenNull
	TokenTrue
	TokenFalse
	TokenOrder
	TokenBy
	TokenAsc
	TokenDesc
	TokenLimit
	TokenOffset
	TokenJoin
	TokenLeft
	TokenRight
	TokenInner
	TokenOuter
	TokenGroup
	TokenHaving
	TokenAs
	TokenDistinct
	TokenCount
	TokenSum
	TokenAvg
	TokenMin
	TokenMax
	TokenIn
	TokenBetween
	TokenLike
	TokenIs
	TokenExists
	TokenPrimary
	TokenKey
	TokenInteger
	TokenText
	TokenReal
	TokenBoolean
	TokenBegin
	TokenCommit
	TokenRollback
	keywordEnd

	// Operators.
	TokenPlus     // +
	TokenMinus    // -
	TokenAsterisk // *
	TokenSlash    // /
	TokenEq       // =
	TokenNeq      // != or <>
	TokenLt       // <
	TokenGt       // >
	TokenLtEq     // <=
	TokenGtEq     // >=

	// Punctuation.
	TokenLParen    // (
	TokenRParen    // )
	TokenComma     // ,
	TokenSemicolon // ;
	TokenDot       // .
	TokenColon     // :

	// Comments — only emitted when KeepComments is true.
	TokenLineComment  // -- ...
	TokenBlockComment // /* ... */
)

// IsKeyword reports whether t is a SQL keyword.
func (t TokenType) IsKeyword() bool {
	return t > keywordStart && t < keywordEnd
}

// Token is a single lexical unit in a SQL source string.
type Token struct {
	Type    TokenType
	Literal string // raw text for identifiers/literals; canonical uppercase for keywords
	Pos     int    // byte offset of the first character in the source
	Line    int    // 1-based line number
	Col     int    // 1-based column number
}

// String returns a debug representation.
func (tok Token) String() string {
	return fmt.Sprintf("Token(%d, %q, %d:%d)", tok.Type, tok.Literal, tok.Line, tok.Col)
}
```

Create `keywords.go`:

```go
package lexer

import "strings"

// keywords maps the canonical uppercase spelling of each SQL keyword to its
// token type. Built once at package init; never mutated after that.
var keywords map[string]TokenType

func init() {
	keywords = map[string]TokenType{
		"SELECT":   TokenSelect,
		"FROM":     TokenFrom,
		"WHERE":    TokenWhere,
		"INSERT":   TokenInsert,
		"INTO":     TokenInto,
		"VALUES":   TokenValues,
		"UPDATE":   TokenUpdate,
		"SET":      TokenSet,
		"DELETE":   TokenDelete,
		"CREATE":   TokenCreate,
		"TABLE":    TokenTable,
		"DROP":     TokenDrop,
		"INDEX":    TokenIndex,
		"ON":       TokenOn,
		"AND":      TokenAnd,
		"OR":       TokenOr,
		"NOT":      TokenNot,
		"NULL":     TokenNull,
		"TRUE":     TokenTrue,
		"FALSE":    TokenFalse,
		"ORDER":    TokenOrder,
		"BY":       TokenBy,
		"ASC":      TokenAsc,
		"DESC":     TokenDesc,
		"LIMIT":    TokenLimit,
		"OFFSET":   TokenOffset,
		"JOIN":     TokenJoin,
		"LEFT":     TokenLeft,
		"RIGHT":    TokenRight,
		"INNER":    TokenInner,
		"OUTER":    TokenOuter,
		"GROUP":    TokenGroup,
		"HAVING":   TokenHaving,
		"AS":       TokenAs,
		"DISTINCT": TokenDistinct,
		"COUNT":    TokenCount,
		"SUM":      TokenSum,
		"AVG":      TokenAvg,
		"MIN":      TokenMin,
		"MAX":      TokenMax,
		"IN":       TokenIn,
		"BETWEEN":  TokenBetween,
		"LIKE":     TokenLike,
		"IS":       TokenIs,
		"EXISTS":   TokenExists,
		"PRIMARY":  TokenPrimary,
		"KEY":      TokenKey,
		"INTEGER":  TokenInteger,
		"TEXT":     TokenText,
		"REAL":     TokenReal,
		"BOOLEAN":  TokenBoolean,
		"BEGIN":    TokenBegin,
		"COMMIT":   TokenCommit,
		"ROLLBACK": TokenRollback,
	}
}

// lookupIdent returns the keyword token type when ident is a SQL keyword
// (comparison is case-insensitive), or TokenIdent for a plain identifier.
func lookupIdent(ident string) TokenType {
	if tt, ok := keywords[strings.ToUpper(ident)]; ok {
		return tt
	}
	return TokenIdent
}
```

Create `lexer.go`:

```go
package lexer

import (
	"fmt"
	"strings"
)

// Lexer tokenizes a SQL source string. Create one with New; call NextToken
// repeatedly until it returns a Token with Type == TokenEOF or TokenError.
type Lexer struct {
	input   string
	pos     int  // byte index of ch (the current byte)
	readPos int  // byte index of the next byte to consume
	ch      byte // the byte at pos; 0 at EOF

	line int // 1-based line number of ch
	col  int // 1-based column number of ch

	// KeepComments, when true, causes NextToken to emit TokenLineComment and
	// TokenBlockComment tokens instead of silently discarding comments.
	KeepComments bool
}

// New creates a Lexer for input and reads the first byte.
func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1, col: 0}
	l.readChar()
	return l
}

// readChar advances the lexer by one byte.
func (l *Lexer) readChar() {
	if l.readPos >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPos]
	}
	l.pos = l.readPos
	l.readPos++
	if l.ch == '\n' {
		l.line++
		l.col = 0
	} else {
		l.col++
	}
}

// peekChar returns the next byte without consuming it. Returns 0 at EOF.
func (l *Lexer) peekChar() byte {
	if l.readPos >= len(l.input) {
		return 0
	}
	return l.input[l.readPos]
}

// skipWhitespace consumes ASCII whitespace without emitting a token.
func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\r' || l.ch == '\n' {
		l.readChar()
	}
}

// NextToken returns the next token in the SQL source. After TokenEOF or
// TokenError is returned, all subsequent calls also return TokenEOF.
func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	startLine := l.line
	startCol := l.col
	startPos := l.pos

	switch {
	case l.ch == 0:
		return Token{Type: TokenEOF, Literal: "", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '-' && l.peekChar() == '-':
		return l.readLineComment(startPos, startLine, startCol)

	case l.ch == '/' && l.peekChar() == '*':
		return l.readBlockComment(startPos, startLine, startCol)

	case l.ch == '\'':
		return l.readString(startPos, startLine, startCol)

	case l.ch == '"':
		return l.readQuotedIdent(startPos, startLine, startCol)

	case isLetter(l.ch):
		return l.readIdentOrKeyword(startPos, startLine, startCol)

	case isDigit(l.ch):
		return l.readNumber(startPos, startLine, startCol)

	case l.ch == '<' && l.peekChar() == '>':
		l.readChar()
		l.readChar()
		return Token{Type: TokenNeq, Literal: "<>", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '<' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenLtEq, Literal: "<=", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '>' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenGtEq, Literal: ">=", Pos: startPos, Line: startLine, Col: startCol}

	case l.ch == '!' && l.peekChar() == '=':
		l.readChar()
		l.readChar()
		return Token{Type: TokenNeq, Literal: "!=", Pos: startPos, Line: startLine, Col: startCol}

	default:
		tok := l.readSingle(startPos, startLine, startCol)
		l.readChar()
		return tok
	}
}

// readSingle returns a token for the current single-byte operator or
// punctuation character, without advancing the lexer. The caller advances.
func (l *Lexer) readSingle(pos, line, col int) Token {
	ch := l.ch
	var tt TokenType
	switch ch {
	case '+':
		tt = TokenPlus
	case '-':
		tt = TokenMinus
	case '*':
		tt = TokenAsterisk
	case '/':
		tt = TokenSlash
	case '=':
		tt = TokenEq
	case '<':
		tt = TokenLt
	case '>':
		tt = TokenGt
	case '(':
		tt = TokenLParen
	case ')':
		tt = TokenRParen
	case ',':
		tt = TokenComma
	case ';':
		tt = TokenSemicolon
	case '.':
		tt = TokenDot
	case ':':
		tt = TokenColon
	default:
		return Token{
			Type:    TokenError,
			Literal: fmt.Sprintf("unexpected character %q at %d:%d", ch, line, col),
			Pos:     pos,
			Line:    line,
			Col:     col,
		}
	}
	return Token{Type: tt, Literal: string(ch), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readLineComment(pos, line, col int) Token {
	l.readChar() // consume first -
	l.readChar() // consume second -
	start := l.pos
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	text := strings.TrimSpace(l.input[start:l.pos])
	if l.KeepComments {
		return Token{Type: TokenLineComment, Literal: text, Pos: pos, Line: line, Col: col}
	}
	return l.NextToken()
}

func (l *Lexer) readBlockComment(pos, line, col int) Token {
	l.readChar() // consume /
	l.readChar() // consume *
	depth := 1
	var buf strings.Builder
	for depth > 0 {
		if l.ch == 0 {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated block comment starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '/' && l.peekChar() == '*' {
			depth++
			buf.WriteByte(l.ch)
			l.readChar()
			buf.WriteByte(l.ch)
			l.readChar()
			continue
		}
		if l.ch == '*' && l.peekChar() == '/' {
			depth--
			if depth == 0 {
				l.readChar() // consume *
				l.readChar() // consume /
				break
			}
			buf.WriteByte(l.ch)
			l.readChar()
			buf.WriteByte(l.ch)
			l.readChar()
			continue
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	if l.KeepComments {
		return Token{Type: TokenBlockComment, Literal: buf.String(), Pos: pos, Line: line, Col: col}
	}
	return l.NextToken()
}

func (l *Lexer) readString(pos, line, col int) Token {
	l.readChar() // consume opening '
	var buf strings.Builder
	for {
		if l.ch == 0 {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated string literal starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '\'' {
			if l.peekChar() == '\'' {
				// SQL standard escape: '' -> one '
				buf.WriteByte('\'')
				l.readChar()
				l.readChar()
				continue
			}
			l.readChar() // consume closing '
			break
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	return Token{Type: TokenString, Literal: buf.String(), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readQuotedIdent(pos, line, col int) Token {
	l.readChar() // consume opening "
	var buf strings.Builder
	for {
		if l.ch == 0 {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("unterminated quoted identifier starting at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		if l.ch == '"' {
			if l.peekChar() == '"' {
				// SQL standard escape: "" -> one "
				buf.WriteByte('"')
				l.readChar()
				l.readChar()
				continue
			}
			l.readChar() // consume closing "
			break
		}
		buf.WriteByte(l.ch)
		l.readChar()
	}
	return Token{Type: TokenQIdent, Literal: buf.String(), Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readIdentOrKeyword(pos, line, col int) Token {
	start := l.pos
	for isLetter(l.ch) || isDigit(l.ch) || l.ch == '_' {
		l.readChar()
	}
	lit := l.input[start:l.pos]
	tt := lookupIdent(lit)
	if tt != TokenIdent {
		// Keyword: the Literal field holds the canonical uppercase spelling.
		return Token{Type: tt, Literal: strings.ToUpper(lit), Pos: pos, Line: line, Col: col}
	}
	return Token{Type: TokenIdent, Literal: lit, Pos: pos, Line: line, Col: col}
}

func (l *Lexer) readNumber(pos, line, col int) Token {
	start := l.pos
	tt := TokenInt
	for isDigit(l.ch) {
		l.readChar()
	}
	// Optional fractional part: only when the '.' is followed by a digit,
	// so that a trailing dot (e.g., "table.col") is not swallowed.
	if l.ch == '.' && isDigit(l.peekChar()) {
		tt = TokenFloat
		l.readChar() // consume .
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	// Optional exponent.
	if l.ch == 'e' || l.ch == 'E' {
		tt = TokenFloat
		l.readChar() // consume e/E
		if l.ch == '+' || l.ch == '-' {
			l.readChar()
		}
		if !isDigit(l.ch) {
			return Token{
				Type:    TokenError,
				Literal: fmt.Sprintf("malformed numeric literal at %d:%d", line, col),
				Pos:     pos,
				Line:    line,
				Col:     col,
			}
		}
		for isDigit(l.ch) {
			l.readChar()
		}
	}
	return Token{Type: tt, Literal: l.input[start:l.pos], Pos: pos, Line: line, Col: col}
}

// Tokenize is a convenience wrapper that returns all tokens for input,
// ending with a single TokenEOF (or TokenError on failure). Useful in
// tests and demos.
func Tokenize(input string) []Token {
	l := New(input)
	var tokens []Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF || tok.Type == TokenError {
			break
		}
	}
	return tokens
}

func isLetter(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}
```

The whole baseline ships here so the module is independently buildable; this exercise's narrative and tests focus on `readString` and `readQuotedIdent`, but the scanner, number reader, and comment readers travel with them unchanged.

### The runnable demo

The demo tokenizes a statement that exercises both literal forms — a quoted identifier with a space and a single-quoted string with a doubled quote — and prints each token's `Literal` with `%q` so the decoding is visible: the surrounding quotes are gone and the doubled `''` has collapsed to one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/strings-and-identifiers"
)

func main() {
	input := `SELECT "first name" FROM t WHERE note = 'it''s ok';`
	for _, tok := range lexer.Tokenize(input) {
		if tok.Type == lexer.TokenEOF {
			break
		}
		fmt.Printf("%q\n", tok.Literal)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"SELECT"
"first name"
"FROM"
"t"
"WHERE"
"note"
"="
"it's ok"
";"
```

`"first name"` comes back as the identifier `first name` (one token, space preserved), and `'it''s ok'` comes back as the string value `it's ok` (the doubled quote folded to one). Neither literal is a slice of the source; both were rebuilt in a `strings.Builder`.

### Tests

The tests pin the decoding and the failure modes. `TestTokenizeStringLiteral` walks the empty string, a plain string, and one and several doubled-quote escapes; `TestTokenizeQuotedIdentifier` does the same for double-quoted identifiers including an embedded escaped quote; `TestUnterminatedStringError` and `TestUnterminatedQuotedIdentError` assert that a missing closing quote yields a positioned `TokenError` rather than a panic; and `TestRoundTrip` confirms that for a statement without escapes, concatenating literals reproduces the non-whitespace source.

Create `lexer_test.go`:

```go
package lexer

import (
	"strings"
	"testing"
)

func TestTokenizeStringLiteral(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{`'hello'`, "hello"},
		{`'it''s'`, "it's"},    // SQL standard escape: '' -> '
		{`''`, ""},             // empty string
		{`'a''b''c'`, "a'b'c"}, // multiple escapes
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if toks[0].Type != TokenString || toks[0].Literal != tc.want {
				t.Fatalf("Tokenize(%q): got Type=%v Literal=%q, want TokenString %q",
					tc.input, toks[0].Type, toks[0].Literal, tc.want)
			}
		})
	}
}

func TestTokenizeQuotedIdentifier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{`"column name"`, "column name"},
		{`"col""quote"`, `col"quote`}, // doubled double-quote inside quoted identifier
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if toks[0].Type != TokenQIdent || toks[0].Literal != tc.want {
				t.Fatalf("got Type=%v Literal=%q, want TokenQIdent %q",
					toks[0].Type, toks[0].Literal, tc.want)
			}
		})
	}
}

func TestUnterminatedStringError(t *testing.T) {
	t.Parallel()

	toks := Tokenize("'no closing quote")
	if toks[0].Type != TokenError {
		t.Fatalf("expected TokenError, got %v", toks[0].Type)
	}
	if !strings.Contains(toks[0].Literal, "unterminated string") {
		t.Fatalf("error literal does not mention unterminated string: %q", toks[0].Literal)
	}
}

func TestUnterminatedQuotedIdentError(t *testing.T) {
	t.Parallel()

	toks := Tokenize(`"no close`)
	if toks[0].Type != TokenError {
		t.Fatalf("expected TokenError, got %v", toks[0].Type)
	}
	if !strings.Contains(toks[0].Literal, "unterminated quoted identifier") {
		t.Fatalf("error literal: %q", toks[0].Literal)
	}
}

// TestRoundTrip verifies that concatenating all token Literals reproduces
// the non-whitespace portion of the original source for a simple statement.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	input := "SELECT id,name FROM users WHERE id=1;"
	toks := Tokenize(input)
	var got strings.Builder
	for _, tok := range toks {
		if tok.Type == TokenEOF {
			break
		}
		got.WriteString(tok.Literal)
	}
	expected := strings.ReplaceAll(input, " ", "")
	if got.String() != expected {
		t.Fatalf("round trip failed:\n got: %q\nwant: %q", got.String(), expected)
	}
}
```

## Review

The literal readers are correct when an escape is decoded and a terminator ends the token, and the two are told apart by exactly one peek. Confirm `'it''s'` becomes `it's` and `"col""x"` becomes `col"x`, that an empty `''` is a valid zero-length string, and that the decoded value lives in a fresh buffer rather than aliasing the source. The error path matters just as much: an input that ends before the closing quote must return a `TokenError` whose `Literal` mentions the unterminated construct and whose position points at the opening quote, never an out-of-bounds read.

The mistake to avoid is treating the literal as a slice of the input. The moment a `''` collapses to a single `'`, the decoded value is shorter than its source span, so `input[start:end]` is wrong; only a builder that appends one quote per escaped pair produces the right value. The second mistake is forgetting the `ch == 0` guard inside the loop, which turns an unterminated literal from a clean error token into a buffer overrun.

## Resources

- [PostgreSQL: String Constants](https://www.postgresql.org/docs/current/sql-syntax-lexical.html#SQL-SYNTAX-STRINGS) — the standard single-quote string and its `''` doubling rule.
- [PostgreSQL: Identifiers and Key Words](https://www.postgresql.org/docs/current/sql-syntax-lexical.html#SQL-SYNTAX-IDENTIFIERS) — quoted identifiers, their `""` escape, and their case sensitivity.
- [pkg.go.dev: strings.Builder](https://pkg.go.dev/strings#Builder) — the zero-allocation-amortized buffer the readers accumulate decoded bytes into.

---

Back to [02-scanner-and-keywords.md](02-scanner-and-keywords.md) | Next: [04-numbers-and-comments.md](04-numbers-and-comments.md)
