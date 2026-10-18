// Text helpers shared by the calculator, the symbolic engine and the unit
// converter: name suggestions for "unknown name" errors, and one number
// formatter so a value is never printed with more digits than it has.

// closeMatches returns up to `limit` names that look like a typo of `name`:
// prefix, substring, or a shared prefix of at least two characters. An error
// that offers the near miss is worth ten that say "unknown".
export function closeMatches(name, candidates, limit) {
  const wanted = String(name).toLowerCase();
  const found = [];
  const push = function (candidate) {
    if (found.indexOf(candidate) < 0) found.push(candidate);
  };
  for (let index = 0; index < candidates.length; index += 1) {
    const candidate = String(candidates[index]);
    const text = candidate.toLowerCase();
    if (text === wanted) continue;
    if (text.indexOf(wanted) === 0 || wanted.indexOf(text) === 0 || text.indexOf(wanted) >= 0) {
      push(candidate);
    }
  }
  if (found.length === 0) {
    for (let index = 0; index < candidates.length; index += 1) {
      const candidate = String(candidates[index]);
      const text = candidate.toLowerCase();
      let shared = 0;
      while (shared < text.length && shared < wanted.length && text.charAt(shared) === wanted.charAt(shared)) {
        shared += 1;
      }
      if (shared >= 2) push(candidate);
    }
  }
  return found.slice(0, limit || 6);
}

// significantDigits counts the significant digits of a decimal string, which is
// what decides whether printing a float64 at N digits is exact or a rounding.
export function significantDigits(text) {
  let mantissa = String(text);
  const exponentAt = mantissa.indexOf("e");
  if (exponentAt < 0) {
    const upper = mantissa.indexOf("E");
    if (upper >= 0) mantissa = mantissa.slice(0, upper);
  } else {
    mantissa = mantissa.slice(0, exponentAt);
  }
  let digits = mantissa.replace("-", "").replace(".", "");
  digits = digits.replace(/^0+/, "").replace(/0+$/, "");
  return digits.length;
}

// formatNumber prints a float64 with `digits` significant digits, without an
// exponent for anything a person would write out (up to 1e15) and with one
// beyond that. It is the only place that decides how a float looks.
export function formatNumber(value, digits) {
  if (typeof value !== "number" || !isFinite(value)) {
    if (value === Infinity) return "inf";
    if (value === -Infinity) return "-inf";
    return "nan";
  }
  if (value === 0) return "0";
  const precision = Math.max(1, Math.min(17, Math.floor(digits)));
  const magnitude = Math.abs(value);
  if (magnitude >= 1e15 || magnitude < 1e-4) {
    const text = value.toExponential(precision - 1);
    const parts = text.split("e");
    const mantissa = parts[0].indexOf(".") >= 0 ? parts[0].replace(/0+$/, "").replace(/\.$/, "") : parts[0];
    return mantissa + "e" + parts[1];
  }
  const exponent = Math.floor(Math.log10(magnitude));
  const factor = Math.pow(10, precision - 1 - exponent);
  if (!isFinite(factor)) return String(value);
  const rounded = Math.round(value * factor) / factor;
  if (rounded === 0) return "0";
  return String(rounded);
}

// isApproximate reports whether printing `value` at `digits` significant digits
// loses information. A float64 whose shortest representation fits in `digits` is
// exact when printed; one that does not is a rounding, and the caller marks it
// with an approximation sign.
export function isApproximate(value, digits) {
  if (typeof value !== "number" || !isFinite(value)) return false;
  if (value === 0) return false;
  return significantDigits(String(value)) > Math.max(1, Math.floor(digits));
}

// plural picks a word form; a single helper keeps messages uniform.
export function plural(count, singular, multiple) {
  return count === 1 ? singular : multiple;
}
