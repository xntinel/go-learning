# Numeric Precision and Overflow in Production Money and Counter Code — Concepts

Numeric bugs in backend code are the quietest data-corrupting bugs there are.
There is no panic, no error, no log line. An `int64` ledger total wraps past
`math.MaxInt64` and becomes a large negative number that still looks like a
plausible balance. A `float64` turns `$10.10` into `10.099999999999999` and an
invoice is off by a cent that nobody can reconcile. A narrowing `int64->int32`
conversion truncates a valid record ID before it reaches a downstream gRPC field,
and the request silently addresses the wrong row. A `uint64` metrics counter laps
`2^64` under load and reports a byte rate that is off by exabytes. Every one of
these is a *representation* mistake: the wrong type was chosen for the invariant,
or a value crossed a trust boundary without being range-checked, and the hardware
did exactly what the language says it must — wrap, truncate, or round — with no
signal.

A senior engineer treats numeric representation as a design decision with an
audit trail. The through-line of this lesson: choose the representation that makes
the invariant *checkable* (integer minor units for money you add and subtract,
`math/bits` for wide accumulators, `math/big` for exact rational tax and
proration), validate at every trust boundary (JSON decode, DB read, config parse,
type-narrowing conversion), and fail closed *before* the operation that would
corrupt state rather than inspecting a value that has already wrapped. Read this
file once; each of the nine independent exercises that follow is a production
artifact that applies one facet of it.

## Concepts

### Runtime integer overflow wraps silently

Go distinguishes two overflows. A *constant* overflow is a compile error:
`const x int64 = 9223372036854775808` does not build, because the compiler
evaluates constants in arbitrary precision and rejects a value that does not fit.
A *runtime* overflow — arithmetic on values parsed from JSON, read from a database
row, or supplied on an HTTP form — is defined by the spec to wrap modulo `2^N` for
an `N`-bit integer, with no panic and no signal. `math.MaxInt64 + 1` evaluated at
runtime is `math.MinInt64`. The wrapped value is a legal bit pattern that looks
like an ordinary number, which is exactly why it corrupts a ledger undetected: the
next read sees a valid-looking total, not a fault. The compiler protects you from
literals; it does nothing for the number a user just POSTed.

### Check overflow before the operation, never after

Once `a + b` or `a * b` has wrapped, the evidence is gone: the result is a
truncated low-order slice of the true mathematical value, and some wrapped results
even land on plausible-looking numbers, so "does the result look wrong?" is not a
test you can write. The check must happen against the type's bounds *before* you
operate. For signed addition the two guards are: when `b > 0`, overflow iff
`a > math.MaxInt64 - b`; when `b < 0`, underflow iff `a < math.MinInt64 - b`. Both
right-hand sides are computed without overflowing because subtracting a
same-signed operand moves toward zero. For multiplication, handle zero and sign
first, then check `a > math.MaxInt64 / b` before multiplying. The division-based
guard is the canonical one because `math.MaxInt64 / b` is the largest multiplicand
that still fits.

### float64 cannot represent most decimal fractions

`float64` is IEEE-754 binary64: a sign bit, an 11-bit exponent, and a 52-bit
mantissa. It represents sums of powers of two, so any decimal fraction whose value
is not a finite sum of negative powers of two is stored as the nearest
representable neighbor. `0.1` is stored as `0.1000000000000000055511151231...`;
`0.1 + 0.2` is `0.30000000000000004`, which is not equal to the stored `0.3`.
Above `2^53` the gaps between representable values exceed 1, so not every integer
can be represented — a 64-bit ID cast through `float64` can come back as a
different number. The rule is categorical: never use `float64` for money, IDs, or
counters. Use it only for measurements and ratios where a small relative error is
acceptable, and even then compare with an explicit epsilon.

### Match the representation to the invariant

There is no single "correct" numeric type; there is the type whose arithmetic
preserves the property you must guarantee. Money you add and subtract wants integer
*minor units* (cents), because integer addition is exact and overflow is checkable.
Money you *divide* — tax, interest, a proration with a remainder to distribute —
wants `math/big.Rat` or `big.Int`, because the exact quotient and remainder must
be recoverable before you round. A `uint64` accumulator that legitimately exceeds
63 bits (bytes served, aggregated event counts) wants `math/bits` primitives that
detect the carry, or a 128-bit `(hi, lo)` pair. `float64` is correct only for
inherently approximate measurements. Choosing the type *is* the design.

### math/bits gives portable wide-integer primitives

`math/bits` exposes the CPU's carry-aware operations as portable functions.
`bits.Add64(x, y, carry uint64) (sum, carryOut uint64)` returns the low 64 bits of
`x + y + carry` and a `carryOut` of 0 or 1; a nonzero `carryOut` is the overflow
signal. `bits.Sub64` is its borrow-aware mirror. `bits.Mul64(x, y uint64) (hi, lo
uint64)` returns the full 128-bit product split into high and low words; `hi != 0`
means the product did not fit in 64 bits. These are constant-time and are the
idiomatic way to build checked `uint64` math or a 128-bit accumulator that never
wraps — the low word absorbs each add and the carry ripples into the high word.

### Narrowing conversions truncate silently

Go conversions are explicit — you must write `int32(n)` — but an *explicit*
conversion of an out-of-range value still truncates without error. `int32(n)` keeps
only the low 32 bits; `uint16(n)` keeps the low 16; `int(n)` on a 32-bit platform
can lose the high half of an `int64`. A record ID, a port number, a slice length,
or a protobuf field width that is narrowed without a bounds check is a silent
corruption waiting for the first value that exceeds the destination range. Guard
every narrowing at a trust boundary by comparing against the destination's
`math.MaxIntNN` / `math.MinIntNN` constants and returning an error when the value
does not fit, rather than letting the conversion mangle it.

### JSON numbers decode to lossy float64 by default

`encoding/json` decodes a JSON number into an `interface{}` (or a `map` value) as
`float64`. A 19-digit ID or any money decimal loses precision the instant it is
unmarshaled into `any`. The fix is to preserve the exact literal: `json.Number` is
a `string` alias with `Int64()`, `Float64()`, and `String()` methods, and
`Decoder.UseNumber()` makes a decoder produce `json.Number` instead of `float64`
for numbers decoded into `interface{}`. Better still for a money field, give the
type a custom `UnmarshalJSON` that reads the raw literal and parses it into integer
minor units yourself, so a `float64` never touches the value.

### float64 has poisoning special states

Binary64 has three non-finite states that break arithmetic and comparison: `NaN`,
`+Inf`, and `-Inf`. `NaN` compares false to everything including itself, so
`x == x` is `false` when `x` is `NaN` — the one place in Go where a value is not
equal to itself. A single `NaN` folded into a running sum makes every subsequent
value `NaN`, and every downstream comparison meaningless. Ingested feed data must
be screened with `math.IsNaN` and `math.IsInf` *before* it enters an aggregation,
because there is no way to recover a finite total once a `NaN` has propagated
through it.

### Exact decimal math with math/big

`big.Rat` holds an exact rational number: a numerator and denominator in lowest
terms, with no rounding until you explicitly render it. A tax computed as
`amount * 725 / 10000` stays exact through the multiplication and the division; you
choose when and how to round by calling `FloatString(n)` or by converting to minor
units with an explicit rule. `big.Int.QuoRem` gives you the truncated quotient and
the remainder together, and `big.Int.DivMod` gives the Euclidean (floor) quotient
and a non-negative remainder — the exact leftover you need to distribute cents
deterministically. The cost is allocation and speed; the payoff is an auditable
number.

### Deterministic remainder distribution

When a total does not divide evenly across N shares, the leftover minor units must
be allocated by a fixed, reproducible rule, or the parts will not sum back to the
whole. The largest-remainder (Hamilton) method gives each share its floor and then
hands the leftover units, one at a time, to the shares with the largest fractional
remainders, breaking ties by a stable order (e.g. lowest index). The result
guarantees `sum(parts) == total` exactly and is reproducible bit-for-bit, so two
services computing the same split from the same inputs agree to the cent.

### Currency minor-unit scale is not universally 2

The number of decimal places in a currency is an ISO 4217 property, not a constant.
JPY and KRW have 0 minor-unit digits, most currencies have 2, and BHD, KWD, and OMR
have 3. A money type that hard-codes `/100` formats JPY with two phantom decimals
and rounds BHD to the wrong scale. A correct type carries the currency's minor-unit
exponent and derives its scale as `10^exponent`, so parsing, formatting, and
rounding all use the right power of ten.

### Signed money needs the mirror-image guard

Subtraction and negative balances are as dangerous as addition. A debit that
underflows past `math.MinInt64` corrupts a balance exactly as a credit that
overflows past `math.MaxInt64` does, and the guard is the mirror image: check the
lower bound before subtracting. Whether negative balances are even *allowed* is a
policy decision that must be enforced explicitly with a bound check, never assumed
by omission.

## Common Mistakes

### Using float64 for money

Wrong: parse `"12.99"` with `strconv.ParseFloat`, multiply by a quantity, round at
the end. Accumulated binary error and non-representable decimals produce invoices
off by a cent. Fix: parse the decimal string into integer minor units and keep the
arithmetic integral.

### Checking overflow after the operation

Wrong: compute `a + b` (or `a * b`) and then test whether the result "looks wrong"
or is smaller than an operand. The wrap already happened and some wrapped values
look plausible. Fix: compare against `math.MaxInt64` / `math.MinInt64` before
operating.

### Assuming compile-time constant checks cover runtime data

Wrong: trust that Go protects you because `const x int64 = 9223372036854775808`
fails to build. The compiler does nothing for a quantity parsed from JSON, a DB
row, or an HTTP form. Fix: every runtime numeric input needs an explicit runtime
bounds check.

### Silent truncation on narrowing conversion

Wrong: write `int32(n)` or `uint16(n)` on a value that may exceed the target range.
Go truncates with no error, corrupting IDs, ports, and lengths. Fix: guard with the
destination's `math.MaxInt32` / `math.MaxUint16` constants and return an error on
out-of-range.

### Letting encoding/json decode amounts to float64

Wrong: unmarshal a money field into `float64` or `interface{}`, silently losing
precision on large or decimal values. Fix: use `json.Number` / `UseNumber()` or a
custom `UnmarshalJSON` that parses the exact literal into minor units.

### Trusting float comparisons and ignoring special values

Wrong: use `==` on floats, or sum feed values that may contain `NaN`/`Inf`.
`NaN == NaN` is false and one `NaN` poisons the whole aggregate. Fix: screen with
`math.IsNaN` / `math.IsInf` and compare with an explicit epsilon where floats are
unavoidable.

### Dividing money with integer division and dropping the remainder

Wrong: compute `total / n` and multiply back, silently losing the leftover minor
units so the parts do not sum to the whole. Fix: distribute the remainder
deterministically (largest-remainder) so `sum(parts) == total` exactly.

### Hard-coding a 2-decimal, 100-unit scale

Wrong: format JPY as `X.XX` or round BHD to 2 places. Fix: carry the currency's
minor-unit exponent and derive the scale from it.

### Rounding your way out of float money

Wrong: use `math.Round(x*100) / 100` to "fix" float money. This compounds the
original binary error and still cannot represent the target decimal. Fix: never
round out of float money; represent it exactly.

### Handling only the positive overflow direction

Wrong: guard `Add` against `MaxInt64` but not `MinInt64` on subtraction and
negative operands, so debits underflow silently. Fix: implement the symmetric
lower-bound guard.

Next: [01-parse-cents-decimal-parser.md](01-parse-cents-decimal-parser.md)
