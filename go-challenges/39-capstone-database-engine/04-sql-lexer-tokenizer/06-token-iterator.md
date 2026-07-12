# Exercise 6: A Range-Over-Func Token Iterator

`Tokenize` returns a slice, which forces the entire token stream into memory and gives the caller no way to stop early. Go 1.23's range-over-func iterators offer an additive alternative: a `func(yield func(Token) bool)` that a consumer drives with a plain `for range` loop and can abandon with `break`. This exercise adds an `iter.Seq[Token]` over the same `Lexer`, proves it agrees with the batch API token-for-token, and layers a filtered variant on top.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
token.go             the baseline token model
keywords.go          the baseline keyword map
lexer.go             the baseline lexer, including Tokenize
iter.go              Tokens (iter.Seq[Token]) and TokensFiltered
cmd/
  demo/
    main.go          range over Tokens, then over a comma-filtered stream
iter_test.go         iterator agrees with Tokenize, early break, filtering
```

- Files: `token.go`, `keywords.go`, `lexer.go`, `iter.go`, `cmd/demo/main.go`, `iter_test.go`.
- Implement: `Tokens(input string) iter.Seq[Token]` and `TokensFiltered(input string, keep func(Token) bool) iter.Seq[Token]`.
- Test: `iter_test.go` proves `Tokens` matches `Tokenize` over a corpus, that a `break` stops iteration early, and that `TokensFiltered` drops the filtered tokens while keeping the terminator.
- Verify: `go test -run 'TestTokens|ExampleTokens' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/06-token-iterator/cmd/demo && cd go-solutions/39-capstone-database-engine/04-sql-lexer-tokenizer/06-token-iterator
```

### Why an iterator, and why it must agree with the slice

The slice API is fine for a whole statement but wrong for two cases: a multi-megabyte script, where materializing every token at once wastes memory, and a consumer that only needs a prefix — a `LIMIT`-clause sniffer, say — which still pays to tokenize the entire input. A range-over-func iterator fixes both. `Tokens` returns a `func(yield func(Token) bool)`: the runtime calls the function, which drives the same `Lexer` one `NextToken` at a time and hands each token to `yield`. If `yield` returns false — which is what `break` in the consumer's `for range` loop causes — the iterator returns immediately, so a consumer that stops after two tokens lexes only as far as it needs. There is no slice, no preallocation, and laziness is automatic.

The load-bearing property is that `Tokens` and `Tokenize` must agree token-for-token, because the rest of the engine should be able to choose either without changing behavior. They agree by construction: both drive `New(input)` and loop on `NextToken` with the identical termination test (`TokenEOF` or `TokenError` ends the stream). The iterator yields each token first and then checks for termination, so the terminating `TokenEOF`/`TokenError` is itself yielded — exactly the element `Tokenize` puts last in its slice. The agreement test in this exercise makes that concrete by comparing the two outputs element by element over a corpus that includes the empty string, comments, escapes, an unterminated string, and a float.

`TokensFiltered` shows how cheaply iterators compose: it wraps `Tokens` and forwards only the tokens for which `keep` returns true, but always forwards the terminating `TokenEOF`/`TokenError` regardless of `keep`, so the consumer keeps its single termination test. Composing iterators this way — one driving another — is the idiom that makes range-over-func worth having; a filtered stream is a three-line wrapper rather than a reimplemented scan.

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

Create `iter.go`:

```go
package lexer

import "iter"

// Tokens returns a range-over-func iterator (Go 1.23) over the tokens of input.
// It yields each token in order, including the terminating TokenEOF or
// TokenError, then stops. Tokens drives the same Lexer as Tokenize, so the two
// always agree token-for-token; Tokens simply avoids materializing the slice
// and lets the caller stop early with break.
func Tokens(input string) iter.Seq[Token] {
	return func(yield func(Token) bool) {
		l := New(input)
		for {
			tok := l.NextToken()
			if !yield(tok) {
				return
			}
			if tok.Type == TokenEOF || tok.Type == TokenError {
				return
			}
		}
	}
}

// TokensFiltered wraps Tokens and yields only the tokens for which keep returns
// true. The terminating TokenEOF or TokenError is always yielded so the consumer
// still has a single termination test, regardless of what keep decides.
func TokensFiltered(input string, keep func(Token) bool) iter.Seq[Token] {
	return func(yield func(Token) bool) {
		for tok := range Tokens(input) {
			if tok.Type == TokenEOF || tok.Type == TokenError || keep(tok) {
				if !yield(tok) {
					return
				}
			}
		}
	}
}
```

The base lexer ships unchanged; `iter.go` adds the two iterators and reuses `New`, `NextToken`, and `Tokenize` from the baseline.

### The runnable demo

The demo ranges over `Tokens` for a small expression, then over `TokensFiltered` with a predicate that drops commas, so the lazy iteration and the composition are both visible. Coalescing the two shows that the filtered stream yields exactly the non-comma literals.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	lexer "example.com/token-iterator"
)

func main() {
	fmt.Println("all tokens:")
	for tok := range lexer.Tokens("SELECT 1 + 2") {
		if tok.Type == lexer.TokenEOF {
			break
		}
		fmt.Println(" ", tok.Literal)
	}

	fmt.Println("commas filtered out:")
	keep := func(tok lexer.Token) bool { return tok.Type != lexer.TokenComma }
	for tok := range lexer.TokensFiltered("a, b, c", keep) {
		if tok.Type == lexer.TokenEOF {
			break
		}
		fmt.Println(" ", tok.Literal)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all tokens:
  SELECT
  1
  +
  2
commas filtered out:
  a
  b
  c
```

The first loop yields `SELECT 1 + 2`; the second yields `a b c`, the commas removed by the predicate while the terminating EOF (which the loop breaks on) is still delivered.

### Tests

The tests pin agreement, laziness, and filtering. `TestTokensMatchesBatch` compares `Tokens` against `Tokenize` element by element over a varied corpus; `TestTokensEarlyBreak` proves a `break` after two tokens stops the iterator (and therefore the underlying scan) early; `TestTokensFiltered` drops commas from `a, b, c` and asserts the surviving literals are `a`, `b`, `c` plus the terminating empty-literal EOF. `ExampleTokens` pins the literal stream for a trivial input.

Create `iter_test.go`:

```go
package lexer

import (
	"fmt"
	"testing"
)

func TestTokensMatchesBatch(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"",
		"SELECT id FROM users;",
		"SELECT * FROM t WHERE a >= 1 AND b <> 2;",
		"-- comment\nSELECT 'x''y' FROM z",
		"'unterminated",
		"1.5e-3 + 42",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			want := Tokenize(in)
			var got []Token
			for tok := range Tokens(in) {
				got = append(got, tok)
			}
			if len(got) != len(want) {
				t.Fatalf("Tokens(%q) yielded %d tokens, Tokenize yielded %d", in, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("token %d: Tokens=%v, Tokenize=%v", i, got[i], want[i])
				}
			}
		})
	}
}

func TestTokensEarlyBreak(t *testing.T) {
	t.Parallel()

	var seen []string
	for tok := range Tokens("SELECT id FROM users") {
		seen = append(seen, tok.Literal)
		if len(seen) == 2 {
			break
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected to stop after 2 tokens, got %d: %v", len(seen), seen)
	}
	if seen[0] != "SELECT" || seen[1] != "id" {
		t.Fatalf("unexpected tokens: %v", seen)
	}
}

func TestTokensFiltered(t *testing.T) {
	t.Parallel()

	keep := func(tok Token) bool { return tok.Type != TokenComma }
	var lits []string
	for tok := range TokensFiltered("a, b, c", keep) {
		lits = append(lits, tok.Literal)
	}
	// The commas are dropped; the terminating EOF (empty literal) remains.
	want := []string{"a", "b", "c", ""}
	if len(lits) != len(want) {
		t.Fatalf("got %d literals %q, want %d %q", len(lits), lits, len(want), want)
	}
	for i, w := range want {
		if lits[i] != w {
			t.Fatalf("literal %d: got %q, want %q", i, lits[i], w)
		}
	}
}

func ExampleTokens() {
	for tok := range Tokens("SELECT 1") {
		if tok.Type == TokenEOF {
			break
		}
		fmt.Println(tok.Literal)
	}
	// Output:
	// SELECT
	// 1
}
```

## Review

The iterator is correct when it is indistinguishable from the slice except in laziness. Confirm `Tokens` and `Tokenize` produce the same tokens in the same order, terminator included, over inputs ranging from empty to malformed; confirm a consumer that `break`s mid-stream causes the iterator to return rather than run to EOF; and confirm `TokensFiltered` removes only what `keep` rejects while always delivering the terminating token so downstream loops keep one exit test. Run it under `-race` to be sure the iterator holds no shared state across calls.

The mistake to avoid is checking for termination before yielding, which would drop the final `TokenEOF`/`TokenError` and make the iterator disagree with the slice on its last element. Yield first, then test. The second is forgetting to honor `yield`'s false return: ignoring it defeats early `break` and, worse, can let the consumer's loop body run after it asked to stop.

## Resources

- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) — how `for range` drives a `func(yield func(V) bool)` and what `yield`'s return value means.
- [pkg.go.dev: iter](https://pkg.go.dev/iter) — the `iter.Seq` type alias these iterators return.
- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the language rule that lets a function value be ranged over.

---

Back to [05-bind-parameters.md](05-bind-parameters.md) | Next: [07-fuzz-and-properties.md](07-fuzz-and-properties.md)
