# 5. SQL Parser — Concepts

A recursive descent parser converts a flat token stream into a typed Abstract Syntax Tree (AST) that exposes the grammatical structure of a SQL statement. The hard problems are not the happy path: they are operator precedence in expressions (Pratt parsing), the syntactic overlap between the `AND` in `BETWEEN lo AND hi` and the logical `AND` operator, the three-way distinction between `IS NULL`, `IS NOT NULL`, and ordinary comparisons, and producing error messages that carry the exact source location. This file is the conceptual foundation: read it once and you will have everything you need to reason through each exercise. The exercises build the parser as independent, self-contained Go modules — each one bundles its own minimal lexer, so it compiles and tests standalone with no cross-module dependency.

## Recursive Descent: Grammar Rules Become Functions

Recursive descent assigns one parsing function to each grammar rule. The top-level call inspects the first token and delegates: `SELECT` goes to a select parser, `INSERT` to an insert parser, and so on. Each function consumes exactly the tokens it owns and returns the AST node. The call stack mirrors the grammar tree, which makes bugs straightforward to isolate: if the select parser returns a wrong result, the error is inside the select parser, not somewhere else.

The parser holds two tokens at all times: `cur` (the token being examined) and `peek` (one token of lookahead). Two tokens suffice for SQL because the language is almost LL(1) — the only cases requiring a second token are `LEFT [OUTER] JOIN`, `IS [NOT] NULL`, `[NOT] BETWEEN`, `[NOT] IN`, `[NOT] LIKE`, and the keyword pairs `GROUP BY` / `ORDER BY`. A `nextToken` helper advances both tokens by one; an `expect` helper asserts the type of the current token before advancing.

## Pratt Parsing for Operator Precedence

A naive recursive descent parser encodes each precedence level as a separate grammar rule (`parseOr` calls `parseAnd` calls `parseComparison` calls `parseAddition` and so on). It works, but it produces a cascade of tiny functions that are tedious to extend. Pratt parsing consolidates all expression parsing into a single `parseExpression(minBP int)` function driven by a binding-power table.

Each infix operator has a left binding power (lBP) and a right binding power (rBP). The invariant: an operator is absorbed into the current expression only when its `lBP` exceeds `minBP`. The precedence table used throughout these exercises:

```
                          lBP   rBP   associativity
OR          (OR)           1     2    left
AND         (AND)          3     4    left
Comparison  (= != < > <= >=) 5   6    left
Addition    (+ -)          7     8    left
Multiply    (* /)          9    10    left
```

### Precedence and associativity, encoded in two integers

The two binding powers carry both pieces of information that the cascade of one-rule-per-level functions encodes structurally:

- Precedence is the magnitude. `*` (lBP=9) outranks `+` (lBP=7), so in `a + b * c` the recursive call for the right side of `+` uses `minBP=8`, which keeps `*` (9 > 8) but stops a second `+` (7 is not > 8). The result is `a + (b * c)`. Gaps of two between levels (1, 3, 5, 7, 9) leave room for each right binding power to sit between adjacent levels without colliding.

- Associativity is the sign of the asymmetry between lBP and rBP. For a left-associative operator, `rBP = lBP + 1`, so the recursive call on the right uses a `minBP` one higher than the operator's own lBP; a second operator of the same precedence has `lBP <= minBP` and stops, forcing it to attach to the left: `a - b - c` parses as `(a - b) - c`. For a right-associative operator, `rBP = lBP - 1`, so a second operator of equal precedence is absorbed on the right instead. Every operator in this SQL parser is left-associative, so every rBP is one above its lBP. matklad's article frames the same idea as a single number split into a `(left, right)` pair; the parity trick (`2*level` versus `2*level+1`) is one common encoding.

This is exactly Vaughan Pratt's "top-down operator precedence": each token carries a binding power and a parsing action, and one loop drives them. Crockford's JavaScript adaptation names the two actions `nud` (null denotation: a token with no left operand — a prefix or atom) and `led` (left denotation: a token that operates on an already-parsed left operand). Splitting `parseExpression` along precisely that nud/led seam lets the led loop be reused by other entry points (an ORDER BY clause parser, an EXISTS predicate parser) without duplicating the operator machinery.

### Special SQL infix forms

`IS [NOT] NULL`, `[NOT] BETWEEN lo AND hi`, `[NOT] IN (...)`, and `[NOT] LIKE p` sit at comparison binding power (lBP=5) but cannot be table-driven like `+` or `=`, because their right side is not a single expression: `IS NULL` has no right operand at all, `BETWEEN` takes two bounds joined by a keyword, `IN` takes a parenthesized list or a subquery, and each carries an optional leading `NOT`. They are handled by dedicated functions inside the led loop, guarded by the same `cmpBP > minBP` test that a table-driven operator at lBP=5 would face. Keeping them at comparison precedence is what makes `a = 1 AND b BETWEEN 2 AND 3` group as `(a = 1) AND (b BETWEEN 2 AND 3)`: the `AND` (lBP=3) is too weak to pull `BETWEEN` (lBP=5) leftward, and `BETWEEN` is too weak to cross the `AND`. The `[NOT]` prefix is recognized by a two-token peek (`NOT` followed by `BETWEEN`/`IN`/`LIKE`) so that a bare `NOT` elsewhere still parses as the unary prefix operator.

## The AND Ambiguity in BETWEEN

`x BETWEEN lo AND hi` uses `AND` as a syntactic separator, not as logical conjunction. If `lo` is parsed with `parseExpression(0)`, the Pratt loop absorbs the `AND` as a `BinaryExpr`, leaving the BETWEEN parser with no separator to consume and the high bound unparsed.

The fix is to parse both `lo` and `hi` with `minBP=5` (comparison binding power). At `minBP=5`, the `AND` operator (lBP=3, not greater than 5) stops the sub-expression immediately, leaving `AND` as the current token for the BETWEEN parser to consume explicitly. At the same `minBP=5`, comparison operators (lBP=5, not greater than 5) also stop, which prevents a stray `=` or `<` from being pulled into the bounds, while arithmetic (lBP=7) inside a bound still binds: `score BETWEEN base + 1 AND base + 10` keeps each `+`.

This is the one place where the same keyword token, `AND`, has two grammatical roles: a left-associative logical operator and an inert separator inside `BETWEEN`. Grammars that keep `AND` purely as an operator and treat `BETWEEN ... AND ...` as a single production avoid the clash by construction; the PostgreSQL grammar (`gram.y`) resolves it with an explicit precedence declaration that gives `BETWEEN` higher precedence than `AND`, and SQLite's hand-written parser threads a flag through expression parsing. The binding-power approach reaches the same outcome without a generator: raise `minBP` for the bounds to exactly the level that fences out both `AND` and comparison, and the separator survives for the BETWEEN parser to consume. The mirror image is `NOT`: as a prefix it is a unary operator, but immediately before `BETWEEN`/`IN`/`LIKE` it is part of the special form, disambiguated by a one-token peek.

## IS NULL, IN, LIKE, BETWEEN: One Binding Power, Four Shapes

All four special predicates share comparison precedence, but each has a distinct right-hand shape, and getting the shapes right is what keeps the AST faithful:

- `IS [NOT] NULL` has no right operand. After consuming `IS`, the parser peeks for an optional `NOT`, then requires `NULL`, and emits an `IsNullExpr` carrying only the left operand and the `IsNot` flag. There is no expression to parse on the right.
- `[NOT] IN (...)` takes either a parenthesized value list or a parenthesized subquery, and the two are mutually exclusive. The parser opens the paren, and if the next token is `SELECT` it parses a subquery; otherwise it reads a comma-separated value list. This is why a single `InExpr` node carries both a `Values` slice and a `Subquery` pointer.
- `[NOT] LIKE pattern` takes a single right operand — the pattern — parsed at comparison binding power so a trailing comparison does not get absorbed into it.
- `[NOT] BETWEEN lo AND hi` is the two-bound form described above.

Because all four sit at lBP=5, they compose with the logical operators exactly the way comparisons do: `a IS NULL OR b > 1` groups as `(a IS NULL) OR (b > 1)`, since `OR` (lBP=1) is weaker than the predicate's effective level.

## AST Design: Marker Methods and the String Contract

The `Statement` and `Expression` interfaces use unexported marker methods (`stmtNode()`, `exprNode()`) to prevent accidental implementation. Without the marker, any struct with a `String()` method would satisfy the interface. The standard library's `go/ast` package uses the same technique.

Every AST node implements `String() string`, which regenerates valid SQL. The regenerated SQL wraps every `BinaryExpr` in parentheses, making precedence explicit and making round-trip tests reliable: parsing a statement and printing it produces unambiguous SQL that re-parses to an identical structure. A round-trip test does not need to compare AST trees field by field; it compares the second print against the first, and any precedence bug shows up as a different parenthesization.

## Why SQL Is Almost LL(1)

A grammar is LL(1) when one token of lookahead is enough to choose the next production at every decision point. SQL is almost LL(1): the statement dispatcher branches on a single leading keyword, and most clause boundaries are marked by a unique keyword (`FROM`, `WHERE`, `GROUP`, `HAVING`, `LIMIT`). The parser still keeps a second token, `peek`, because a small, enumerable set of constructs needs two tokens to decide which production applies:

- `GROUP BY` / `ORDER BY`: the keyword `GROUP` (or `ORDER`) alone does not commit to the clause; the parser confirms `peek == BY` before consuming, so a column named `group` in some dialects would not derail it.
- `LEFT [OUTER] JOIN` and `RIGHT [OUTER] JOIN`: `LEFT` could begin other constructs, so the join path consumes the optional `OUTER` and confirms a following `JOIN`.
- `IS [NOT] NULL`: after `IS`, the parser peeks for an optional `NOT` before requiring `NULL`.
- `[NOT] BETWEEN`, `[NOT] IN`, `[NOT] LIKE`: a leading `NOT` is committed to the special form only when `peek` is `BETWEEN`, `IN`, or `LIKE`; otherwise `NOT` is the unary prefix operator.
- `IF NOT EXISTS` / `IF EXISTS`: `IF` is not a lexer keyword (it arrives as an identifier), so these are recognized by matching the literal `IF` and peeking for `NOT`/`EXISTS`.
- `table.column` versus `table.*`: distinguishing a qualified column from a wildcard needs the token after the dot, which is the one case the two-token buffer cannot see directly. The select-item parser handles it by consuming the identifier and the dot, then inspecting the current token — a controlled one-token over-read rather than a third buffered token.

Everything else is a single-token decision, which is why a hand-written recursive-descent parser stays compact: there is no need for backtracking or a parser generator's conflict resolution. The same near-LL(1) property is why keywords that are not in the lexer's keyword set (`IF`, `DEFAULT`, `NULLS`, `FIRST`, `LAST`, `UNIQUE`, `ALTER`, `ADD`, `COLUMN`) can still be recognized: the parser matches them by their uppercased identifier literal at exactly the position the grammar expects them.

## Error Accumulation, Source Locations, and Recovery

A `ParseError` carries a line number, a column number, and a message. It wraps a sentinel `ErrSyntax` via `Unwrap()`, so callers can test `errors.Is(err, ErrSyntax)` without pattern-matching on strings, and `errors.As(err, &pe)` recovers the location for an editor underline. Locations come from the lexer: each token already carries line and column, so the parser reports the position of the offending token rather than a byte offset into a re-scanned string.

The parser accumulates errors in a slice and returns the first one. Returning the first error keeps a single failure from cascading into a flood of meaningless follow-on errors, which is what happens when a parser keeps going in a confused state. To report several real errors in one pass, the standard technique is panic-mode recovery with a synchronization point: on error, discard tokens until one that reliably begins a new unit — a `;` (statement boundary) or the next top-level statement keyword — then resume. SQL's semicolon makes this clean: a syntax error in one statement need not abandon the whole script. The trade-off to watch is over-synchronizing: skipping too aggressively swallows a second genuine error, while skipping too little re-reports the same one.

## Common Mistakes

### Absorbing the BETWEEN AND into an expression

Wrong: parsing the BETWEEN bounds with `parseExpression(0)`. The Pratt loop sees `AND` (lBP=3) and absorbs it as a logical conjunction, producing a bound of `(lo AND hi)` with no separator left for the BETWEEN parser, and then a syntax error on the missing high bound.

Fix: parse both bounds with `parseExpression(cmpBP)` where `cmpBP=5`. At `minBP=5`, `AND` (lBP=3) stops the sub-expression immediately, leaving `AND` as the current token for the BETWEEN parser to consume explicitly.

### Handling NOT IN / NOT BETWEEN as two independent tokens

Wrong: treating `NOT` as a prefix operator in every position. In infix position the parser then produces a malformed `(NOT IN)` or fails because `NOT` has no infix binding power.

Fix: in the led loop, when the current token is `NOT`, peek at the next token. If it is `BETWEEN`, `IN`, or `LIKE`, consume the `NOT` and delegate to the dedicated parsing function with the negation flag set. This must happen inside the special-form block, which is guarded by `cmpBP > minBP`.

### Letting special forms bind inside arithmetic

Wrong: entering the special-form block unconditionally, so the right-hand side of `a + b NOT IN (...)` absorbs `NOT IN` and groups as `a + (b NOT IN (...))`, giving the wrong precedence.

Fix: guard the whole special-form block with `if cmpBP > minBP`. Inside the right side of a `+` (parsed at `minBP=8`), `cmpBP` (5) is not greater than 8, so the block is skipped; the `NOT` stops the sub-expression and the outer call absorbs `NOT IN` at the correct level.

### Comparing error message strings instead of sentinel values

Wrong: `if err != nil && strings.Contains(err.Error(), "syntax error")`. String matching is fragile and breaks the moment a message is reworded.

Fix: use `errors.Is(err, ErrSyntax)`. `ParseError.Unwrap()` returns `ErrSyntax`, so `errors.Is` works through the wrapping chain regardless of the message text.

### Not priming the lookahead buffer

Wrong: constructing the parser and immediately examining `cur` — it is the zero-value token because `nextToken` has not run yet.

Fix: the constructor calls `nextToken` twice. The first call loads the lexer's first token into `peek`; the second moves it into `cur` and reads the next token into `peek`. After construction, `cur` is the first real token and `peek` is the second.

---

Next: [01-core-sql-parser.md](01-core-sql-parser.md)
