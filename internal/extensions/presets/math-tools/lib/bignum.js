// Exact integer and rational arithmetic.
//
// BigInt is the only exact numeric type the sandbox has. A float64 carries at
// most 15.95 significant digits, so 2^200 or 100! computed with `**` and `*` is
// simply wrong; every path that stays inside the integers goes through BigInt
// instead. Rationals are {n, d} pairs of normalized BigInts with d > 0, which is
// what the symbolic engine uses for coefficients: 1/2 + 1/3 is exactly 5/6.

// bigToNumber converts a BigInt to float64 the way Number() is specified to:
// the *value* is converted, rounded to the nearest representable double.
//
// Number(bigint) cannot be used for that here. The production runtime is goja,
// whose implementation narrows through the low 64 bits of the integer, so
// Number(2n ** 100n) is 0 and Number(18446744073709551617n) is 1. Every float
// fallback in this preset (a logarithm, a division, the value shown when an
// exact result exceeds the digit limit) goes through here, and with the naive
// conversion those answers were silently wrong with no approximation warning.
//
// Converting the decimal text is what a spec-compliant engine effectively does,
// and it is exact for the shortest representation.
export function bigToNumber(value) {
  if (typeof value === "bigint") {
    return Number(value.toString());
  }
  return Number(value);
}

export function biAbs(value) {
  return value < 0n ? -value : value;
}

export function biGcd(left, right) {
  // A non-BigInt here would compare false against 0n forever, which is a hang
  // rather than an error; the check turns a silent loop into a loud failure.
  if (typeof left !== "bigint" || typeof right !== "bigint") {
    throw new Error("internal arithmetic error: gcd needs two BigInts");
  }
  let a = biAbs(left);
  let b = biAbs(right);
  while (b !== 0n) {
    const remainder = a % b;
    a = b;
    b = remainder;
  }
  return a;
}

export function biLcm(left, right) {
  if (left === 0n || right === 0n) return 0n;
  return biAbs(left / biGcd(left, right) * right);
}

export function biSign(value) {
  if (value === 0n) return 0;
  return value < 0n ? -1 : 1;
}

export function biDigitCount(value) {
  return biAbs(value).toString().length;
}

// biPow takes the exponent as a Number because BigInt ** requires a BigInt
// exponent and a Number is what the call sites have. The loop is binary
// exponentiation, so 2^200000 would be 18 multiplications - the digit estimate
// in estimatePowDigits is what keeps the result from being computed at all when
// it is larger than the caller's limit.
export function biPow(base, exponent) {
  let result = 1n;
  let factor = base;
  let power = BigInt(exponent);
  while (power > 0n) {
    if (power % 2n !== 0n) result *= factor;
    power /= 2n;
    if (power > 0n) factor *= factor;
  }
  return result;
}

// estimatePowDigits returns log10(|base^exponent|), the number of digits the
// exact result would have. It is a float estimate and is only ever compared
// against a digit limit, where being off by one does not matter.
export function estimatePowDigits(base, exponent) {
  const magnitude = bigToNumber(biAbs(base));
  let log10Base;
  if (isFinite(magnitude) && magnitude > 0) {
    log10Base = Math.log10(magnitude);
  } else {
    const text = biAbs(base).toString();
    const lead = Number(text.slice(0, 15)) / Math.pow(10, Math.min(14, text.length - 1));
    log10Base = text.length - 1 + Math.log10(lead);
  }
  return Math.abs(exponent) * log10Base;
}

// factorialDigits is Stirling's series for log10(n!). A factorial is only
// computed when its digit count is under the caller's limit, so this has to be
// cheap: a loop to 100000 in BigInt would not be.
export function factorialDigits(count) {
  if (count < 2) return 1;
  const n = Number(count);
  const log10 = (n * Math.log(n) - n + 0.5 * Math.log(2 * Math.PI * n)) / Math.LN10;
  return Math.floor(log10 + 1 / (12 * n * Math.LN10)) + 1;
}

export function biFactorial(count) {
  let result = 1n;
  for (let index = 2; index <= count; index += 1) {
    result *= BigInt(index);
  }
  return result;
}

export function biChoose(total, taken) {
  if (taken < 0 || taken > total) return 0n;
  const k = Math.min(taken, total - taken);
  let result = 1n;
  for (let index = 1; index <= k; index += 1) {
    result = result * BigInt(total - k + index) / BigInt(index);
  }
  return result;
}

export function biPermutation(total, taken) {
  if (taken < 0 || taken > total) return 0n;
  let result = 1n;
  for (let index = 0; index < taken; index += 1) {
    result *= BigInt(total - index);
  }
  return result;
}

// Integer square and cube roots. The initial guess is a power of ten from the
// digit count: Number(value) overflows to Infinity long before a 5000-digit
// value, and BigInt(Infinity) throws.
export function biIsqrt(value) {
  if (value < 0n) throw new Error("square root of a negative integer");
  if (value < 2n) return value;
  let guess = 10n ** BigInt(Math.ceil(value.toString().length / 2));
  let next = (guess + value / guess) / 2n;
  while (next < guess) {
    guess = next;
    next = (guess + value / guess) / 2n;
  }
  return guess;
}

export function biIcbrt(value) {
  const negative = value < 0n;
  const magnitude = biAbs(value);
  if (magnitude < 2n) return value;
  let guess = 10n ** BigInt(Math.ceil(magnitude.toString().length / 3));
  for (let step = 0; step < 100; step += 1) {
    const next = (2n * guess + magnitude / (guess * guess)) / 3n;
    if (next === guess) break;
    guess = next;
  }
  // Newton's iteration converges from above but can land one off the floor.
  while (guess > 1n && guess * guess * guess > magnitude) guess -= 1n;
  while ((guess + 1n) * (guess + 1n) * (guess + 1n) <= magnitude) guess += 1n;
  return negative ? -guess : guess;
}

// ---- rationals --------------------------------------------------------------

export function rat(n, d) {
  let num = n;
  let den = d;
  if (den === 0n) throw new Error("a rational cannot have zero as its denominator");
  if (den < 0n) {
    num = -num;
    den = -den;
  }
  const divisor = biGcd(num, den);
  if (divisor > 1n) {
    num /= divisor;
    den /= divisor;
  }
  return { n: num, d: den };
}

export function ratInt(value) {
  return { n: value, d: 1n };
}

export function ratZero() {
  return { n: 0n, d: 1n };
}

export function ratOne() {
  return { n: 1n, d: 1n };
}

export function ratAdd(left, right) {
  return rat(left.n * right.d + right.n * left.d, left.d * right.d);
}

export function ratSub(left, right) {
  return rat(left.n * right.d - right.n * left.d, left.d * right.d);
}

export function ratMul(left, right) {
  return rat(left.n * right.n, left.d * right.d);
}

export function ratDiv(left, right) {
  if (right.n === 0n) throw new Error("division by zero");
  return rat(left.n * right.d, left.d * right.n);
}

export function ratNeg(value) {
  return { n: -value.n, d: value.d };
}

export function ratAbs(value) {
  return { n: biAbs(value.n), d: value.d };
}

export function ratSign(value) {
  return biSign(value.n);
}

export function ratIsZero(value) {
  return value.n === 0n;
}

export function ratIsInt(value) {
  return value.d === 1n;
}

export function ratIsOne(value) {
  return value.n === 1n && value.d === 1n;
}

export function ratEquals(left, right) {
  return left.n === right.n && left.d === right.d;
}

export function ratCmp(left, right) {
  const delta = left.n * right.d - right.n * left.d;
  if (delta === 0n) return 0;
  return delta < 0n ? -1 : 1;
}

export function ratToNumber(value) {
  const top = bigToNumber(value.n);
  const bottom = bigToNumber(value.d);
  if (isFinite(top) && isFinite(bottom)) return top / bottom;
  return biSign(value.n) * Infinity;
}

// ratFromNumber is exact for every number a caller can write down: the shortest
// decimal representation of a float64 is a decimal fraction, and BigInt can hold
// it. 0.5 becomes 1/2; 0.1 becomes 3602879701896397/36028797018963968.
export function ratFromNumber(value) {
  if (!isFinite(value)) throw new Error("the value must be finite");
  const text = String(value);
  const negative = text.charAt(0) === "-";
  const unsigned = negative ? text.slice(1) : text;
  const exponentAt = unsigned.indexOf("e");
  const mantissa = exponentAt >= 0 ? unsigned.slice(0, exponentAt) : unsigned;
  const exponent = exponentAt >= 0 ? Number(unsigned.slice(exponentAt + 1)) : 0;
  const pointAt = mantissa.indexOf(".");
  const whole = pointAt >= 0 ? mantissa.slice(0, pointAt) : mantissa;
  const fraction = pointAt >= 0 ? mantissa.slice(pointAt + 1) : "";
  const digits = (whole + fraction) || "0";
  const scale = exponent - fraction.length;
  let num = BigInt(digits);
  let den = 1n;
  if (scale > 0) num *= 10n ** BigInt(scale);
  else if (scale < 0) den = 10n ** BigInt(-scale);
  const result = rat(num, den);
  return negative ? ratNeg(result) : result;
}

export function ratToText(value) {
  if (value.d === 1n) return value.n.toString();
  return value.n.toString() + "/" + value.d.toString();
}

// ratToDecimal renders an exact rational as a decimal with `places` digits
// after the point, rounding half away from zero. It is BigInt arithmetic all the
// way down, so 1/3 prints as 0.333333 rather than as a float64's noise.
export function ratToDecimal(value, places) {
  const negative = value.n < 0n;
  const num = biAbs(value.n);
  const den = value.d;
  const scale = 10n ** BigInt(places);
  const scaled = (num * scale * 2n + den) / (den * 2n);
  let text = scaled.toString();
  if (places > 0) {
    while (text.length <= places) text = "0" + text;
    const cut = text.length - places;
    const whole = text.slice(0, cut).replace(/^0+(?=\d)/, "");
    let fraction = text.slice(cut);
    if (places > 1) fraction = fraction.replace(/0+$/, "");
    text = fraction === "" ? whole : whole + "." + fraction;
  }
  return (negative && scaled !== 0n ? "-" : "") + text;
}
