// The calculator: exact evaluation with a float64 fallback.
//
// A value is either an exact integer ({exact: true, big: BigInt}) or a float64
// ({exact: false, num: number}). An operation whose operands are all exact
// integers stays exact - + - * / % ^ ! gcd lcm nCr nPr - and only leaves the
// integers when the result has no exact integer form (7/2) or when the caller
// switched exactIntegers off. Every place that cannot stay exact says so, either
// with an "≈" in the result or in a note, because a silently rounded integer is
// the one failure mode a calculator must not have.

import {
  biAbs, biChoose, biFactorial, biGcd, biIcbrt, biIsqrt, biLcm, biPermutation, biPow,
  biSign, estimatePowDigits, factorialDigits, ratFromNumber, bigToNumber } from "./bignum.js";
import { parseAssignment, parseExpression, splitStatements, tokenize } from "./parse.js";
import { closeMatches, formatNumber, isApproximate, plural } from "./util.js";

export const ANGLE_UNITS = ["rad", "deg", "grad"];

const CONSTANTS = {
  pi: Math.PI,
  e: Math.E,
  tau: Math.PI * 2,
  phi: (1 + Math.sqrt(5)) / 2,
  inf: Infinity,
};

// Every function the calculator knows, with its arity. The tool description is
// generated from this list, so the two cannot drift apart.
export const FUNCTIONS = {
  sqrt: [1, 1], cbrt: [1, 1], root: [2, 2], abs: [1, 1], sign: [1, 1],
  floor: [1, 1], ceil: [1, 1], round: [1, 1], trunc: [1, 1], fract: [1, 1],
  exp: [1, 1], ln: [1, 1], log: [1, 2], log2: [1, 1], log10: [1, 1],
  sin: [1, 1], cos: [1, 1], tan: [1, 1], asin: [1, 1], acos: [1, 1], atan: [1, 1], atan2: [2, 2],
  sinh: [1, 1], cosh: [1, 1], tanh: [1, 1], asinh: [1, 1], acosh: [1, 1], atanh: [1, 1],
  pow: [2, 2], min: [1, 8], max: [1, 8], hypot: [1, 8], gcd: [2, 8], lcm: [2, 8],
  mod: [2, 2], nCr: [2, 2], nPr: [2, 2],
};

export function functionNames() {
  return Object.keys(FUNCTIONS).sort();
}

function intValue(big) {
  return { exact: true, big: big };
}

function floatValue(num) {
  return { exact: false, num: num };
}

function fail(message, node) {
  const column = node && node.at ? node.at : 1;
  throw new Error(message + " at column " + column);
}

function arity(name, args, node) {
  const limits = FUNCTIONS[name];
  const count = args.length;
  if (count < limits[0] || count > limits[1]) {
    const expected = limits[0] === limits[1] ? String(limits[0]) : limits[0] + "-" + limits[1];
    fail(name + " takes " + expected + " " + plural(limits[1], "argument", "arguments") + ", got " + count, node);
  }
}

function isInteger(value) {
  return value.exact === true;
}

function toNumber(value) {
  return value.exact ? bigToNumber(value.big) : value.num;
}

function requireInteger(value, name, node) {
  if (!isInteger(value)) {
    fail(name + " needs integer arguments (got " + formatNumber(toNumber(value), 12) + ")", node);
  }
  return value.big;
}

function angleToRadians(value, unit) {
  if (unit === "deg") return value * Math.PI / 180;
  if (unit === "grad") return value * Math.PI / 200;
  return value;
}

function radiansToAngle(value, unit) {
  if (unit === "deg") return value * 180 / Math.PI;
  if (unit === "grad") return value * 200 / Math.PI;
  return value;
}

// capExact keeps an exact result inside the caller's digit limit. Over the limit
// the value is reported as a float with an approximation sign and an explanation
// rather than as a truncated exact number, which would be wrong in the last
// digits and impossible to spot.
function capExact(big, state, node, what) {
  if (biAbs(big).toString().length <= state.maxIntegerDigits) {
    return intValue(big);
  }
  if (state.fallback === "") {
    state.fallback = what + " has " + biAbs(big).toString().length + " digits, above the " +
      state.maxIntegerDigits + "-digit exact limit, so it is shown as a float";
  }
  return floatValue(bigToNumber(big));
}

function exactPow(base, exponent, state, node) {
  if (biAbs(base) === 1n) {
    if (base === 1n) return intValue(1n);
    return intValue(exponent % 2n === 0n ? 1n : -1n);
  }
  if (exponent < 0n) {
    // Only ±1 has an exact integer form for a negative exponent; the rest is a
    // fraction, which is a float here.
    return null;
  }
  if (exponent === 0n) return intValue(1n);
  if (base === 0n) return intValue(0n);
  const digits = estimatePowDigits(base, bigToNumber(exponent));
  if (!isFinite(digits) || digits > state.maxIntegerDigits) {
    state.powerDigits = isFinite(digits) ? Math.ceil(digits) : Infinity;
    if (state.fallback === "") {
      state.fallback = "the exact value of this power has about " +
        (isFinite(digits) ? Math.ceil(digits) : "more than " + state.maxIntegerDigits) +
        " digits, above the " + state.maxIntegerDigits + "-digit exact limit, so it is shown as a float";
    }
    return null;
  }
  return intValue(biPow(base, bigToNumber(exponent)));
}

function exactNthRoot(magnitude, degree) {
  if (degree === 2) {
    const root = biIsqrt(magnitude);
    return root * root === magnitude ? root : null;
  }
  if (degree === 3) {
    const root = biIcbrt(magnitude);
    return root * root * root === magnitude ? root : null;
  }
  if (magnitude.toString().length > 30) return null;
  let guess = BigInt(Math.max(1, Math.round(Math.pow(bigToNumber(magnitude), 1 / degree))));
  if (biPow(guess, degree) === magnitude) return guess;
  for (let step = 1; step <= 2; step += 1) {
    if (biPow(guess + BigInt(step), degree) === magnitude) return guess + BigInt(step);
    if (guess > BigInt(step) && biPow(guess - BigInt(step), degree) === magnitude) return guess - BigInt(step);
  }
  return null;
}

// exactLog finds an integer k with base^k === value, which is what makes
// log(8, 2) print 3 instead of 2.9999999999999996.
function exactLog(value, base) {
  if (value < 1n || base < 2n) return null;
  let rest = value;
  let power = 0;
  while (rest > 1n) {
    if (rest % base !== 0n) return null;
    rest /= base;
    power += 1;
    if (power > 20000) return null;
  }
  return power;
}

function factorialApprox(count) {
  const n = Number(count);
  if (n < 2) return 1;
  return Math.exp(n * Math.log(n) - n + 0.5 * Math.log(2 * Math.PI * n) + 1 / (12 * n));
}

function applyFunction(node, args, state) {
  const name = node.name;
  arity(name, args, node);
  const first = args[0];
  const asNumber = function (value) {
    return toNumber(value);
  };
  switch (name) {
    case "sqrt": {
      if (isInteger(first)) {
        if (first.big < 0n) {
          fail("sqrt(" + first.big.toString() + ") is not a real number; symbolic_math returns complex roots", node);
        }
        const root = biIsqrt(first.big);
        if (root * root === first.big) return intValue(root);
        return floatValue(Math.sqrt(toNumber(first)));
      }
      if (toNumber(first) < 0) {
        fail("sqrt of a negative number is not real; symbolic_math returns complex roots", node);
      }
      return floatValue(Math.sqrt(toNumber(first)));
    }
    case "cbrt": {
      if (isInteger(first)) {
        const root = biIcbrt(first.big);
        if (root * root * root === first.big) return intValue(root);
      }
      return floatValue(Math.cbrt(toNumber(first)));
    }
    case "root": {
      const degreeBig = requireInteger(args[1], "root", node);
      if (degreeBig < 1n) {
        fail("root needs a positive integer degree", node);
      }
      // A degree is an exponent: an enormous one cannot be searched for an
      // exact root (the intermediate would have hundreds of millions of
      // digits), so it is refused with the limit named rather than attempted.
      // The comparison is on the BigInt, never on a Number() of it.
      if (degreeBig > BigInt(state.maxIntegerDigits)) {
        fail("root degree " + degreeBig.toString() + " is above the " + state.maxIntegerDigits +
          "-digit limit for an exact root; raise maxIntegerDigits if the answer is really wanted", node);
      }
      const degree = bigToNumber(degreeBig);
      if (!isFinite(degree) || degree < 1 || Math.floor(degree) !== degree) {
        fail("root needs a positive integer degree", node);
      }
      const value = asNumber(first);
      if (isInteger(first) && degree >= 1) {
        const negative = first.big < 0n;
        if (negative && degree % 2 === 0) {
          fail("an even root of a negative number is not real; symbolic_math returns complex roots", node);
        }
        const root = exactNthRoot(biAbs(first.big), degree);
        if (root !== null) return intValue(negative ? -root : root);
      }
      if (value < 0 && degree % 2 === 0) {
        fail("an even root of a negative number is not real; symbolic_math returns complex roots", node);
      }
      return floatValue(value < 0 ? -Math.pow(-value, 1 / degree) : Math.pow(value, 1 / degree));
    }
    case "abs":
      return isInteger(first) ? intValue(biAbs(first.big)) : floatValue(Math.abs(asNumber(first)));
    case "sign":
      return isInteger(first) ? intValue(BigInt(biSign(first.big))) : floatValue(Math.sign(asNumber(first)));
    case "floor":
      return isInteger(first) ? first : floatValue(Math.floor(asNumber(first)));
    case "ceil":
      return isInteger(first) ? first : floatValue(Math.ceil(asNumber(first)));
    case "round":
      // Math.round breaks ties toward +Infinity, which is the common
      // "round half up" reading; state it in the description rather than guess.
      return isInteger(first) ? first : floatValue(Math.round(asNumber(first)));
    case "trunc":
      return isInteger(first) ? first : floatValue(Math.trunc(asNumber(first)));
    case "fract": {
      if (isInteger(first)) return intValue(0n);
      const value = asNumber(first);
      return floatValue(value - Math.trunc(value));
    }
    case "exp":
      return floatValue(Math.exp(asNumber(first)));
    case "ln": {
      const value = asNumber(first);
      if (value <= 0) fail("ln needs a positive argument (got " + formatNumber(value, 12) + ")", node);
      return floatValue(Math.log(value));
    }
    case "log2": {
      const value = asNumber(first);
      if (value <= 0) fail("log2 needs a positive argument (got " + formatNumber(value, 12) + ")", node);
      if (isInteger(first)) {
        let rest = first.big;
        let power = 0;
        while (rest > 1n && rest % 2n === 0n) {
          rest /= 2n;
          power += 1;
        }
        if (rest === 1n && power > 0) return intValue(BigInt(power));
      }
      return floatValue(Math.log2(value));
    }
    case "log10": {
      const value = asNumber(first);
      if (value <= 0) fail("log10 needs a positive argument (got " + formatNumber(value, 12) + ")", node);
      if (isInteger(first)) {
        const power = exactLog(first.big, 10n);
        if (power !== null) return intValue(BigInt(power));
      }
      return floatValue(Math.log10(value));
    }
    case "log": {
      const value = asNumber(first);
      if (value <= 0) fail("log needs a positive argument (got " + formatNumber(value, 12) + ")", node);
      if (args.length === 1) return floatValue(Math.log(value));
      const base = asNumber(args[1]);
      if (base <= 0 || base === 1) fail("log needs a base greater than 0 and not equal to 1", node);
      if (isInteger(first) && isInteger(args[1]) && args[1].big >= 2n) {
        const power = exactLog(first.big, args[1].big);
        if (power !== null) return intValue(BigInt(power));
      }
      return floatValue(Math.log(value) / Math.log(base));
    }
    case "sin":
      return floatValue(Math.sin(angleToRadians(asNumber(first), state.angles)));
    case "cos":
      return floatValue(Math.cos(angleToRadians(asNumber(first), state.angles)));
    case "tan":
      return floatValue(Math.tan(angleToRadians(asNumber(first), state.angles)));
    case "asin": {
      const value = asNumber(first);
      if (value < -1 || value > 1) fail("asin needs an argument in [-1, 1] (got " + formatNumber(value, 12) + ")", node);
      return floatValue(radiansToAngle(Math.asin(value), state.angles));
    }
    case "acos": {
      const value = asNumber(first);
      if (value < -1 || value > 1) fail("acos needs an argument in [-1, 1] (got " + formatNumber(value, 12) + ")", node);
      return floatValue(radiansToAngle(Math.acos(value), state.angles));
    }
    case "atan":
      return floatValue(radiansToAngle(Math.atan(asNumber(first)), state.angles));
    case "atan2":
      return floatValue(radiansToAngle(Math.atan2(asNumber(first), asNumber(args[1])), state.angles));
    case "sinh":
      return floatValue(Math.sinh(asNumber(first)));
    case "cosh":
      return floatValue(Math.cosh(asNumber(first)));
    case "tanh":
      return floatValue(Math.tanh(asNumber(first)));
    case "asinh":
      return floatValue(Math.asinh(asNumber(first)));
    case "acosh": {
      const value = asNumber(first);
      if (value < 1) fail("acosh needs an argument of at least 1 (got " + formatNumber(value, 12) + ")", node);
      return floatValue(Math.acosh(value));
    }
    case "atanh": {
      const value = asNumber(first);
      if (value <= -1 || value >= 1) fail("atanh needs an argument in (-1, 1) (got " + formatNumber(value, 12) + ")", node);
      return floatValue(Math.atanh(value));
    }
    case "pow":
      return power(first, args[1], state, node);
    case "min":
    case "max": {
      const wantSmallest = name === "min";
      let best = first;
      for (let index = 1; index < args.length; index += 1) {
        const candidate = args[index];
        const comparison = toNumber(candidate) - toNumber(best);
        if (wantSmallest ? comparison < 0 : comparison > 0) best = candidate;
      }
      return best;
    }
    case "hypot": {
      let sum = 0n;
      let allIntegers = true;
      for (let index = 0; index < args.length; index += 1) {
        if (isInteger(args[index])) sum += args[index].big * args[index].big;
        else allIntegers = false;
      }
      if (allIntegers) {
        const root = biIsqrt(sum);
        if (root * root === sum) return intValue(root);
      }
      let total = 0;
      for (let index = 0; index < args.length; index += 1) total += Math.pow(asNumber(args[index]), 2);
      return floatValue(Math.sqrt(total));
    }
    case "gcd": {
      let result = requireInteger(first, "gcd", node);
      for (let index = 1; index < args.length; index += 1) {
        result = biGcd(result, requireInteger(args[index], "gcd", node));
      }
      return intValue(result);
    }
    case "lcm": {
      let result = requireInteger(first, "lcm", node);
      for (let index = 1; index < args.length; index += 1) {
        result = biLcm(result, requireInteger(args[index], "lcm", node));
      }
      return intValue(result);
    }
    case "mod": {
      const divisor = requireInteger(args[1], "mod", node);
      if (divisor === 0n) fail("modulo by zero", node);
      if (isInteger(first)) return intValue(first.big % divisor);
      return floatValue(asNumber(first) % asNumber(args[1]));
    }
    case "nCr":
    case "nPr": {
      const total = requireInteger(first, name, node);
      const taken = requireInteger(args[1], name, node);
      const totalNumber = Number(total);
      const takenNumber = Number(taken);
      if (!isFinite(totalNumber) || totalNumber > 100000) {
        fail(name + " needs a total of at most 100000 (got " + total.toString() + ")", node);
      }
      if (takenNumber < 0 || takenNumber > totalNumber) {
        fail(name + " needs 0 <= r <= n (got n = " + total.toString() + ", r = " + taken.toString() + ")", node);
      }
      const estimate = takenNumber * Math.log10(Math.max(2, totalNumber));
      if (estimate > state.maxIntegerDigits) {
        if (state.fallback === "") {
          state.fallback = name + "(" + total.toString() + ", " + taken.toString() + ") has about " +
            Math.ceil(estimate) + " digits, above the " + state.maxIntegerDigits +
            "-digit exact limit, so it is not computed exactly";
        }
        return floatValue(name === "nCr" ? stirlingChoose(totalNumber, takenNumber) : stirlingPermutation(totalNumber, takenNumber));
      }
      return intValue(name === "nCr" ? biChoose(totalNumber, takenNumber) : biPermutation(totalNumber, takenNumber));
    }
    default:
      return undefined;
  }
}

// The over-limit fallbacks for the combinatorial functions: exact combinatorics
// on numbers this large is pointless, and a float with an approximation sign and
// a stated reason is honest.
function logFactorial(value) {
  if (value < 2) return 0;
  return value * Math.log(value) - value + 0.5 * Math.log(2 * Math.PI * value);
}

function stirlingChoose(total, taken) {
  return Math.exp(logFactorial(total) - logFactorial(taken) - logFactorial(total - taken));
}

function stirlingPermutation(total, taken) {
  return Math.exp(logFactorial(total) - logFactorial(total - taken));
}

function addValues(left, right, state, node) {
  return sumValues(left, right, 1, state, node);
}

function subValues(left, right, state, node) {
  return sumValues(left, right, -1, state, node);
}

function sumValues(left, right, sign, state, node) {
  if (left.exact && right.exact && state.exactIntegers) {
    const combined = sign > 0 ? left.big + right.big : left.big - right.big;
    return capExact(combined, state, node, "the sum");
  }
  return floatValue(toNumber(left) + sign * toNumber(right));
}

function multiplyValues(left, right, state, node) {
  if (left.exact && right.exact && state.exactIntegers) {
    return capExact(left.big * right.big, state, node, "the product");
  }
  return floatValue(toNumber(left) * toNumber(right));
}

function divideValues(left, right, state, node) {
  if (right.exact && right.big === 0n) fail("division by zero", node);
  if (left.exact && right.exact && state.exactIntegers) {
    if (right.big !== 0n && left.big % right.big === 0n) return intValue(left.big / right.big);
    return floatValue(bigToNumber(left.big) / bigToNumber(right.big));
  }
  return floatValue(toNumber(left) / toNumber(right));
}

function moduloValues(left, right, state, node) {
  if (right.exact && right.big === 0n) fail("modulo by zero", node);
  if (left.exact && right.exact && state.exactIntegers) return intValue(left.big % right.big);
  return floatValue(toNumber(left) % toNumber(right));
}

function power(base, exponent, state, node) {
  state.powerDigits = 0;
  if (base.exact && exponent.exact && state.exactIntegers) {
    const exact = exactPow(base.big, exponent.big, state, node);
    if (exact !== null) return exact;
  }
  const value = Math.pow(toNumber(base), toNumber(exponent));
  if (!isFinite(value) && isFinite(toNumber(base)) && isFinite(toNumber(exponent))) {
    // An over-limit exact power is far past 1.8e308, so the float fallback
    // overflows too: saying how many digits it has beats naming an overflow.
    if (state.powerDigits > 0) {
      fail("the exact value has about " + state.powerDigits + " digits (about 1e" +
        (state.powerDigits - 1) + "), above the " + state.maxIntegerDigits +
        "-digit exact limit, and no float64 can hold it either; raise the exact integer digit limit to get it", node);
    }
    fail("the power has no float64 value; it overflows the largest float64 (about 1.8e308)", node);
  }
  return floatValue(value);
}

function factorialValue(operand, state, node) {
  if (!isInteger(operand)) {
    fail("factorial needs a non-negative integer (got " + formatNumber(toNumber(operand), 12) + ")", node);
  }
  if (operand.big < 0n) fail("factorial needs a non-negative integer", node);
  const count = bigToNumber(operand.big);
  if (!isFinite(count) || count > 1000000) fail("factorial is limited to 1000000", node);
  const digits = factorialDigits(count);
  if (digits > state.maxIntegerDigits) {
    if (state.fallback === "") {
      state.fallback = count + "! has " + digits + " digits, above the " + state.maxIntegerDigits +
        "-digit exact limit, so it is shown as a Stirling approximation";
    }
    return floatValue(factorialApprox(count));
  }
  return intValue(biFactorial(count));
}

function evalNode(node, environment, state) {
  switch (node.k) {
    case "num": {
      // A literal such as 1e308 is held exactly, as a BigInt over a power of
      // ten; converting it has to go through bigToNumber for the same reason
      // every other float fallback does.
      if (node.approx || !state.exactIntegers) return floatValue(bigToNumber(node.n) / bigToNumber(node.d));
      if (node.d === 1n) return intValue(node.n);
      return floatValue(bigToNumber(node.n) / bigToNumber(node.d));
    }
    case "sym": {
      const value = environment[node.name];
      if (value !== undefined) return value;
      if (Object.prototype.hasOwnProperty.call(CONSTANTS, node.name)) return floatValue(CONSTANTS[node.name]);
      const known = Object.keys(environment).concat(Object.keys(CONSTANTS));
      const near = closeMatches(node.name, known, 4);
      fail("unknown name \"" + node.name + "\"" + (near.length > 0 ? "; did you mean " + near.map(JSON.stringify).join(", ") + "?" : "; define it first with " + node.name + " = ..."), node);
      return undefined;
    }
    case "add":
      return addValues(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "sub":
      return subValues(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "mul":
      return multiplyValues(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "div":
      return divideValues(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "mod":
      return moduloValues(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "pow":
      return power(evalNode(node.a, environment, state), evalNode(node.b, environment, state), state, node);
    case "neg": {
      const inner = evalNode(node.a, environment, state);
      return inner.exact ? intValue(-inner.big) : floatValue(-inner.num);
    }
    case "fact":
      return factorialValue(evalNode(node.a, environment, state), state, node);
    case "call": {
      const args = [];
      for (let index = 0; index < node.args.length; index += 1) {
        args.push(evalNode(node.args[index], environment, state));
      }
      if (Object.prototype.hasOwnProperty.call(FUNCTIONS, node.name)) {
        return applyFunction(node, args, state);
      }
      // A defined variable followed by parentheses is a multiplication, which is
      // what x(2) means when x is a number.
      const bound = environment[node.name];
      if (bound !== undefined) {
        if (args.length !== 1) {
          fail("\"" + node.name + "\" is a variable, not a function; " + node.name + "(...) takes one factor", node);
        }
        return multiplyValues(bound, args[0], state, node);
      }
      const near = closeMatches(node.name, functionNames().concat(Object.keys(CONSTANTS)).concat(Object.keys(environment)), 4);
      fail("unknown function \"" + node.name + "\"" + (near.length > 0 ? "; did you mean " + near.map(JSON.stringify).join(", ") + "?" : ""), node);
      return undefined;
    }
    default:
      fail("unsupported expression", node);
      return undefined;
  }
}

function describeValue(value, state) {
  if (value.exact) {
    const text = value.big.toString();
    const digits = text.replace("-", "").length;
    return {
      text: text,
      approximate: false,
      note: digits > 15 ? "exact integer, " + digits + " digits" : "",
    };
  }
  return {
    text: formatNumber(value.num, state.precision),
    approximate: isApproximate(value.num, state.precision),
    note: "",
  };
}

// runProgram evaluates every statement and renders one line per statement. The
// statement's own text is echoed back, so a wrong result can be traced to the
// line that produced it.
export function runProgram(source, options) {
  const state = {
    angles: options.angles,
    precision: options.precision,
    exactIntegers: options.exactIntegers,
    maxIntegerDigits: options.maxIntegerDigits,
    fallback: "",
    powerDigits: 0,
  };
  const environment = Object.create(null);
  const names = options.variables || {};
  const provided = Object.keys(names);
  for (let index = 0; index < provided.length; index += 1) {
    const key = provided[index];
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      throw new Error("variables key " + JSON.stringify(key) + " is not a name");
    }
    const numeric = Number(names[key]);
    if (typeof names[key] !== "number" && !isFinite(numeric)) {
      throw new Error("variables." + key + " must be a number (got " + JSON.stringify(names[key]) + ")");
    }
    if (!isFinite(numeric)) throw new Error("variables." + key + " must be finite");
    const exact = ratFromNumber(numeric);
    environment[key] = exact.d === 1n ? intValue(exact.n) : floatValue(numeric);
  }

  const tokens = tokenize(source);
  const statements = splitStatements(tokens);
  if (statements.length === 0) {
    throw new Error("the expression is empty");
  }

  const lines = [];
  const notes = [];
  for (let index = 0; index < statements.length; index += 1) {
    const statement = statements[index];
    const first = statement[0];
    const last = statement[statement.length - 1];
    const endColumn = last.column + last.text.length;
    const assignment = parseAssignment(statement);
    let node;
    let label;
    if (assignment !== null) {
      node = parseExpression(assignment.body, endColumn);
      label = assignment.name;
    } else {
      node = parseExpression(statement, endColumn);
      label = source.slice(first.column - 1, last.column - 1 + last.text.length).replace(/\s+/g, " ").trim();
    }
    state.fallback = "";
    const value = evalNode(node, environment, state);
    if (value.exact === false && !isFinite(value.num)) {
      throw new Error("the result is " + (isNaN(value.num)
        ? "not a number (a domain error, a 0/0, or an overflow)"
        : "infinite: either a division by zero or ln(0), or a value past the largest float64 (about 1.8e308)") + " at column " + node.at);
    }
    const described = describeValue(value, state);
    if (assignment !== null) environment[assignment.name] = value;
    lines.push(label + " " + (described.approximate ? "≈" : "=") + " " + described.text);
    if (described.note !== "") notes.push(described.note + ".");
    if (state.fallback !== "") notes.push(state.fallback + ".");
  }
  return { lines: lines, notes: notes };
}

// angleNote explains which unit the trigonometry used, which is the one setting
// a wrong answer is usually blamed on.
export function angleNote(angles, overridden) {
  if (angles === "rad") return "";
  return "angles: " + angles + (overridden ? " (from the angles argument)" : " (from the angleUnit setting)");
}
