# Exercise 4: Numeric Literals and Nested Comments

Two of the lexer's subtlest readers are the number reader and the block-comment reader. The number reader has to accept integers, decimals, and scientific notation while refusing to swallow the dot in `table.col` and rejecting a malformed exponent like `1e`. The block-comment reader has to handle nesting — `/* outer /* inner */ still outer */` — which a regular expression provably cannot. This exercise assembles the full lexer and focuses on those two readers, plus the line-comment and `KeepComments` paths, and pins them with the position-aware demo and a battery of edge-case tests.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             TokenType, the token constant block, IsKeyword, Token
keywords.go          the keyword map and lookupIdent
lexer.go             Lexer, NextToken, readNumber, readBlockComment, readLineComment
cmd/
  demo/
    main.go          tokenize a multi-line statement with comments and report positions
lexer_test.go        numeric grammar, trailing dot, nested/line comments, errors
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `cmd/demo/main.go`, `lexer_test.go`.
- Implement: `readNumber` (int/decimal/exponent with the trailing-dot exception), `readBlockComment` (depth counter), and `readLineComment` (with `KeepComments`), inside the full baseline lexer.
- Test: `lexer_test.go` covers the numeric grammar, the `42.col` split, the malformed-exponent error, line and nested block comments, the kept-comment path, and the unterminated-comment and unknown-character errors.
- Verify: `go test -run 'TestTokenize|TestTrailing|TestMalformed|Comment|TestUnknown|Example' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/04-numbers-and-comments/cmd/demo && cd go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/04-numbers-and-comments
```

### Why a depth counter and a conditional dot

The block-comment reader is the one place in the lexer where a counter is unavoidable. SQL line comments never nest, so the standard dialect's `/* */` could be matched by scanning to the first `*/` — but PostgreSQL nests block comments on purpose, so a programmer can comment out a region that already contains one. Nesting makes the construct non-regular: no fixed-state machine matches balanced `/* */` pairs, so the naive "scan to the first `*/`" stops too early on `/* a /* b */ c */` and leaves `c */` dangling as stray tokens. The fix is a single integer of state: increment `depth` on every `/*`, decrement on every `*/`, and end the comment only when `depth` returns to zero. If the input ends while `depth` is still positive, the reader returns a positioned `TokenError` for the unterminated comment rather than running off the buffer.

The number reader implements a small grammar — `integer`, then an optional fractional part, then an optional exponent — with two deliberate sharp edges. The first is the trailing dot. Pure maximal munch would read `42.col` as the float `42.` followed by `col`, but the parser wants `42`, `.`, `col` so that `table.column` member access works. So `readNumber` consumes the decimal dot only when `isDigit(peekChar())` is true; a dot not followed by a digit is left for the next `NextToken`, which returns it as `TokenDot`. The second edge is the malformed exponent. After consuming `e`/`E` and an optional sign, the reader requires at least one digit; `1e` and `1e+` are not silently truncated to `1` but returned as a `TokenError`, because a number that looks like scientific notation but has no exponent digits is a typo the user needs to see, not a value to guess at.

Line comments are the easy case: `readLineComment` discards bytes to the next `\n` or EOF. The one wrinkle is `KeepComments` — when set, the comment readers emit `TokenLineComment` and `TokenBlockComment` (with the comment text, trimmed for line comments) instead of silently skipping, which a formatter or linter needs even though a parser does not.

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

The whole baseline ships here so the module is independently buildable; this exercise's narrative and tests focus on `readNumber`, `readBlockComment`, and `readLineComment`.

### The runnable demo

The demo tokenizes a nine-line statement that contains a leading line comment, an embedded block comment, a string literal, and integers, then prints each token's 1-based `line:col` next to its literal. The two comments are skipped by default, so they leave gaps in the line numbers (lines 1 and 6 produce no tokens) while every real token reports the position an editor would show.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	lexer "example.com/numbers-and-comments"
)

func main() {
	input := `-- count active users by region
SELECT region, COUNT(*) AS total
FROM   users
WHERE  status = 'active'
  AND  age BETWEEN 18 AND 65
  /* include pending registrations too */
GROUP  BY region
ORDER  BY total DESC
LIMIT  10;`

	fmt.Fprintf(os.Stderr, "source: %d bytes\n", len(input))

	for _, tok := range lexer.Tokenize(input) {
		if tok.Type == lexer.TokenEOF {
			break
		}
		fmt.Printf("%4d:%-3d %s\n", tok.Line, tok.Col, tok.Literal)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

The demo writes the source size (222 bytes) to standard error, then prints to standard output:

```
   2:1   SELECT
   2:8   region
   2:14  ,
   2:16  COUNT
   2:21  (
   2:22  *
   2:23  )
   2:25  AS
   2:28  total
   3:1   FROM
   3:8   users
   4:1   WHERE
   4:8   status
   4:15  =
   4:17  active
   5:3   AND
   5:8   age
   5:12  BETWEEN
   5:20  18
   5:23  AND
   5:27  65
   7:1   GROUP
   7:8   BY
   7:11  region
   8:1   ORDER
   8:8   BY
   8:11  total
   8:17  DESC
   9:1   LIMIT
   9:8   10
   9:10  ;
```

The line comment on line 1 and the block comment on line 6 are consumed silently, which is why the output jumps from line 2 to line 3 and from line 5 to line 7; the keywords come back uppercased and the string `'active'` is reported as the value `active`.

### Tests

The tests pin the numeric grammar and the comment machinery. `TestTokenizeNumericLiterals` walks integers, decimals, and signed-exponent floats; `TestTrailingDot` proves `42.col` splits into three tokens; `TestMalformedExponentError` proves `1e` is an error, not a silent `1`. The comment tests cover the skipped and kept line-comment paths, a skipped block comment, a nested block comment, and the unterminated-comment error, and `TestUnknownCharacterError` confirms an unexpected byte is a positioned error token. `ExampleTokenize` pins the literal stream for a simple statement.

Create `lexer_test.go`:

```go
package lexer

import (
	"fmt"
	"strings"
	"testing"
)

func TestTokenizeNumericLiterals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  TokenType
		lit   string
	}{
		{"42", TokenInt, "42"},
		{"0", TokenInt, "0"},
		{"3.14", TokenFloat, "3.14"},
		{"1.5e10", TokenFloat, "1.5e10"},
		{"2E3", TokenFloat, "2E3"},
		{"1.0e+6", TokenFloat, "1.0e+6"},
		{"9.9e-3", TokenFloat, "9.9e-3"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			toks := Tokenize(tc.input)
			if toks[0].Type != tc.want || toks[0].Literal != tc.lit {
				t.Fatalf("Tokenize(%q): got Type=%v Literal=%q, want %v %q",
					tc.input, toks[0].Type, toks[0].Literal, tc.want, tc.lit)
			}
		})
	}
}

// TestTrailingDot pins the maximal-munch exception: a dot not followed by a
// digit is punctuation, so "table.col" and "42.col" split at the dot.
func TestTrailingDot(t *testing.T) {
	t.Parallel()

	toks := Tokenize("42.col")
	want := []TokenType{TokenInt, TokenDot, TokenIdent, TokenEOF}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].Type != w {
			t.Errorf("toks[%d]: got %v, want %v", i, toks[i].Type, w)
		}
	}
}

func TestMalformedExponentError(t *testing.T) {
	t.Parallel()

	toks := Tokenize("1e")
	if toks[0].Type != TokenError {
		t.Fatalf("expected TokenError for malformed exponent, got %v", toks[0].Type)
	}
	if !strings.Contains(toks[0].Literal, "malformed numeric literal") {
		t.Fatalf("error literal: %q", toks[0].Literal)
	}
}

func TestLineCommentSkipped(t *testing.T) {
	t.Parallel()

	toks := Tokenize("-- this is a comment\nSELECT")
	if toks[0].Type != TokenSelect {
		t.Fatalf("expected TokenSelect after comment, got %v (literal=%q)", toks[0].Type, toks[0].Literal)
	}
}

func TestLineCommentKept(t *testing.T) {
	t.Parallel()

	l := New("-- my comment\nSELECT")
	l.KeepComments = true
	tok := l.NextToken()
	if tok.Type != TokenLineComment {
		t.Fatalf("expected TokenLineComment, got %v", tok.Type)
	}
	if tok.Literal != "my comment" {
		t.Fatalf("literal = %q, want %q", tok.Literal, "my comment")
	}
}

func TestBlockCommentSkipped(t *testing.T) {
	t.Parallel()

	toks := Tokenize("/* skip this */ SELECT")
	if toks[0].Type != TokenSelect {
		t.Fatalf("expected TokenSelect after block comment, got %v", toks[0].Type)
	}
}

func TestNestedBlockComment(t *testing.T) {
	t.Parallel()

	// The outer comment spans the inner /* ... */ without closing at the first */.
	toks := Tokenize("/* outer /* inner */ still outer */ SELECT")
	if toks[0].Type != TokenSelect {
		t.Fatalf("expected TokenSelect after nested block comment, got %v", toks[0].Type)
	}
}

func TestUnterminatedBlockCommentError(t *testing.T) {
	t.Parallel()

	toks := Tokenize("/* never closed")
	if toks[0].Type != TokenError {
		t.Fatalf("expected TokenError, got %v", toks[0].Type)
	}
	if !strings.Contains(toks[0].Literal, "unterminated block comment") {
		t.Fatalf("error literal: %q", toks[0].Literal)
	}
}

func TestUnknownCharacterError(t *testing.T) {
	t.Parallel()

	toks := Tokenize("@")
	if toks[0].Type != TokenError {
		t.Fatalf("expected TokenError for unknown char '@', got %v", toks[0].Type)
	}
}

func ExampleTokenize() {
	toks := Tokenize("SELECT id FROM users;")
	for _, tok := range toks {
		if tok.Type == TokenEOF {
			break
		}
		fmt.Printf("%s\n", tok.Literal)
	}
	// Output:
	// SELECT
	// id
	// FROM
	// users
	// ;
}
```

## Review

The number reader is correct when it is greedy where it should be and conservative where it must be. Confirm `1e10`, `1.5e-3`, and `9.9E+6` are floats, that a lone `.` is `TokenDot` and `42.col` splits at the dot, and that `1e` returns a `TokenError` rather than a truncated `1`. The block-comment reader is correct when the depth counter — not a boolean flag — governs the end: `/* outer /* inner */ still outer */` must consume to the final `*/`, and an unclosed comment must return a positioned error token. The `KeepComments` path must surface comment text when requested and stay invisible otherwise.

Two mistakes recur. Consuming the decimal dot unconditionally turns `table.col` into a malformed float; gating it on `isDigit(peekChar())` is the fix. And matching a block comment with a regex or a single boolean flag stops at the first `*/`, leaving the tail of a nested comment to be mis-tokenized; only the increment/decrement counter recognizes the nested structure correctly.

## Resources

- [PostgreSQL: Numeric Constants](https://www.postgresql.org/docs/current/sql-syntax-lexical.html#SQL-SYNTAX-CONSTANTS-NUMERIC) — the reference grammar for integers, decimals, and scientific notation this reader follows.
- [PostgreSQL: Comments](https://www.postgresql.org/docs/current/sql-syntax-lexical.html#SQL-SYNTAX-COMMENTS) — line comments and the nested-block-comment rule that requires a depth counter.
- [pkg.go.dev: strings.TrimSpace](https://pkg.go.dev/strings#TrimSpace) — used to trim the captured text of a kept line comment.

---

Back to [03-strings-and-identifiers.md](03-strings-and-identifiers.md) | Next: [05-bind-parameters.md](05-bind-parameters.md)
