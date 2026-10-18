// A small computer-algebra engine over the shared AST.
//
// The representation is a canonical sum of monomials: an expression becomes a
// list of terms, each one a rational coefficient times a product of
// base^exponent factors, with equal bases merged and equal terms collected. That
// one form covers simplify, expand (distribute instead of collect), polynomial
// extraction for solve/factor, and the term-at-a-time rules of integration, so
// each operation is a short walk over a single structure rather than its own
// rewrite system.
//
// Everything is exact: coefficients are BigInt rationals (see bignum.js). A
// decimal answer appears only where a rule has no exact form, and it is marked
// "≈". Limits (the integration rule set, which polynomials factor, the numeric
// root search) are stated in the tool description and next to a result, because
// a CAS that quietly returns nothing is worse than one that names the rule it
// lacks.

import {
  biAbs, biGcd, biIsqrt, biLcm, biPow, rat, ratAbs, ratAdd, ratCmp, ratDiv, ratEquals,
  ratFromNumber, ratInt, ratIsInt, ratIsOne, ratIsZero, ratMul, ratNeg, ratOne, ratSign,
  ratSub, ratToDecimal, ratToNumber, ratToText, ratZero, bigToNumber,
} from "./bignum.js";
import { collectSymbols, nodeKey, nodeText, parseExpression, splitEquation, tokenize } from "./parse.js";
import { closeMatches, formatNumber, plural } from "./util.js";

const ONE = ratOne();
const ZERO = ratZero();
const CONSTANT_NAMES = ["pi", "e", "tau", "phi"];

export const OPERATIONS = ["simplify", "expand", "factor", "derivative", "integrate", "solve", "evaluate", "substitute"];

function numNode(value) {
  return { k: "num", n: value.n, d: value.d, approx: false, at: 1 };
}

function oneNode() {
  return numNode(ONE);
}

function symNode(name) {
  return { k: "sym", name: name, at: 1 };
}

function callNode(name, args) {
  return { k: "call", name: name, args: args, at: 1 };
}

function addNode(left, right) {
  return { k: "add", a: left, b: right, at: 1 };
}

function subNode(left, right) {
  return { k: "sub", a: left, b: right, at: 1 };
}

function mulNode(left, right) {
  return { k: "mul", a: left, b: right, at: 1 };
}

function rationalOf(node) {
  if (node === undefined || node === null || node.k !== "num") return null;
  return rat(node.n, node.d);
}

function bad(message) {
  throw new Error(message);
}

// ---- term form --------------------------------------------------------------

// A term is {coeff: Rational, factors: [{base: Node, exp: Node}]} with a non-zero
// coefficient and at most one factor per base.
function makeTerm(coeff, factors) {
  const merged = [];
  for (let index = 0; index < factors.length; index += 1) {
    const factor = factors[index];
    let placed = false;
    for (let other = 0; other < merged.length; other += 1) {
      if (nodeKey(merged[other].base) === nodeKey(factor.base)) {
        merged[other] = { base: merged[other].base, exp: addExponents(merged[other].exp, factor.exp) };
        placed = true;
        break;
      }
    }
    if (!placed) merged.push({ base: factor.base, exp: factor.exp });
  }
  const kept = [];
  for (let index = 0; index < merged.length; index += 1) {
    const exponent = rationalOf(merged[index].exp);
    if (exponent !== null && ratIsZero(exponent)) continue;
    kept.push(merged[index]);
  }
  return { coeff: coeff, factors: kept };
}

function addExponents(left, right) {
  const a = rationalOf(left);
  const b = rationalOf(right);
  if (a !== null && b !== null) return numNode(ratAdd(a, b));
  return addNode(left, right);
}

function termKey(term) {
  const parts = [];
  for (let index = 0; index < term.factors.length; index += 1) {
    parts.push(nodeKey(term.factors[index].base) + "^" + nodeKey(term.factors[index].exp));
  }
  parts.sort();
  return parts.join("*");
}

function foldTerms(terms) {
  const collected = [];
  for (let index = 0; index < terms.length; index += 1) {
    const term = terms[index];
    if (ratIsZero(term.coeff)) continue;
    const key = termKey(term);
    let found = -1;
    for (let other = 0; other < collected.length; other += 1) {
      if (collected[other].key === key) {
        found = other;
        break;
      }
    }
    if (found >= 0) {
      collected[found] = {
        key: key,
        term: { coeff: ratAdd(collected[found].term.coeff, term.coeff), factors: collected[found].term.factors },
      };
      if (ratIsZero(collected[found].term.coeff)) collected.splice(found, 1);
    } else {
      collected.push({ key: key, term: term });
    }
  }
  return collected.map(function (entry) {
    return entry.term;
  });
}

// toTerms lowers a node into the sum-of-terms form. expand=false keeps a sum
// inside a product intact (so simplify does not distribute); expand=true
// distributes it.
function toTerms(node, expand) {
  switch (node.k) {
    case "num": {
      const value = rationalOf(node);
      return value === null || ratIsZero(value) ? [] : [{ coeff: value, factors: [] }];
    }
    case "sym":
      return [{ coeff: ONE, factors: [{ base: node, exp: oneNode() }] }];
    case "neg":
      return toTerms(node.a, expand).map(function (term) {
        return { coeff: ratNeg(term.coeff), factors: term.factors };
      });
    case "add":
    case "sub": {
      const left = toTerms(node.a, expand);
      const right = toTerms(node.b, expand);
      const flipped = node.k === "add" ? right : right.map(function (term) {
        return { coeff: ratNeg(term.coeff), factors: term.factors };
      });
      return foldTerms(left.concat(flipped));
    }
    case "mul":
      return multiplyTerms(toTerms(node.a, expand), toTerms(node.b, expand), expand);
    case "div": {
      const numerator = toTerms(node.a, expand);
      const denominator = rationalOf(node.b);
      if (denominator !== null) {
        if (ratIsZero(denominator)) bad("division by zero");
        return numerator.map(function (term) {
          return { coeff: ratDiv(term.coeff, denominator), factors: term.factors };
        });
      }
      const inverse = makeTerm(ONE, [{
        base: fromTerms(toTerms(node.b, expand), expand, {}),
        exp: numNode(ratInt(-1n)),
      }]);
      return multiplyTerms(numerator, [inverse], expand);
    }
    case "mod":
      return [makeTerm(ONE, [{ base: node, exp: oneNode() }])];
    case "pow": {
      const exponent = rationalOf(node.b);
      const baseTerms = toTerms(node.a, expand);
      if (exponent !== null && baseTerms.length === 1) {
        const inner = baseTerms[0];
        const coefficient = ratPow(inner.coeff, exponent);
        if (coefficient !== null) {
          const factors = [];
          for (let index = 0; index < inner.factors.length; index += 1) {
            const current = rationalOf(inner.factors[index].exp);
            if (current === null) {
              factors.length = 0;
              break;
            }
            factors.push({ base: inner.factors[index].base, exp: numNode(ratMul(current, exponent)) });
          }
          if (factors.length === inner.factors.length) {
            // makeTerm drops a factor raised to the power zero (x^0 is 1).
            const term = makeTerm(coefficient, factors);
            return ratIsZero(term.coeff) ? [] : [term];
          }
        }
      }
      if (exponent !== null && ratIsInt(exponent) && exponent.n >= 0n && baseTerms.length > 1) {
        if (expand) {
          // An expansion is a repeated multiply; an exponent past a small bound
          // is not an answer anyone can use, and Number(bigint) would have
          // turned 2^64 into 0 and returned a silently wrong "1".
          if (exponent.n > 64n) {
            return [makeTerm(ONE, [{ base: fromTerms(baseTerms, false, {}), exp: numNode(exponent) }])];
          }
          return powerTerms(baseTerms, bigToNumber(exponent.n));
        }
        return [makeTerm(ONE, [{ base: fromTerms(baseTerms, false, {}), exp: numNode(exponent) }])];
      }
      return [makeTerm(ONE, [{
        base: fromTerms(baseTerms, expand, {}),
        exp: exponent !== null ? numNode(exponent) : node.b,
      }])];
    }
    case "fact":
      return [makeTerm(ONE, [{ base: node, exp: oneNode() }])];
    case "call":
      return [makeTerm(ONE, [{ base: canonicalise(node, expand), exp: oneNode() }])];
    default:
      return [makeTerm(ONE, [{ base: node, exp: oneNode() }])];
  }
}

function multiplyTerms(left, right, expand) {
  if (left.length === 0 || right.length === 0) return [];
  if (left.length === 1 && right.length === 1) {
    return [makeTerm(ratMul(left[0].coeff, right[0].coeff), left[0].factors.concat(right[0].factors))];
  }
  if (expand) {
    const product = [];
    for (let i = 0; i < left.length; i += 1) {
      for (let j = 0; j < right.length; j += 1) {
        product.push(makeTerm(ratMul(left[i].coeff, right[j].coeff), left[i].factors.concat(right[j].factors)));
      }
    }
    return foldTerms(product);
  }
  // A side with more than one term stays a single opaque factor: simplify does
  // not distribute, so x*(y+z) comes back as x*(y+z).
  const coefficient = ratMul(left.length === 1 ? left[0].coeff : ONE, right.length === 1 ? right[0].coeff : ONE);
  let factors = [];
  if (left.length === 1) factors = factors.concat(left[0].factors);
  else factors.push({ base: fromTerms(left, false, {}), exp: oneNode() });
  if (right.length === 1) factors = factors.concat(right[0].factors);
  else factors.push({ base: fromTerms(right, false, {}), exp: oneNode() });
  return [makeTerm(coefficient, factors)];
}

function powerTerms(terms, exponent) {
  let result = [{ coeff: ONE, factors: [] }];
  for (let step = 0; step < exponent; step += 1) {
    result = multiplyTerms(result, terms, true);
  }
  return result;
}

function canonicalise(node, expand) {
  const args = [];
  for (let index = 0; index < node.args.length; index += 1) {
    args.push(fromTerms(toTerms(node.args[index], expand), expand, {}));
  }
  return callNode(node.name, args);
}

// ratPow is exact or null: a fractional power folds only when the result is
// rational again, and an exponent too large to be worth computing is refused
// rather than run.
function ratPow(base, exponent) {
  if (ratIsZero(exponent)) return ONE;
  if (ratIsInt(exponent)) {
    if (exponent.n > 5000n || exponent.n < -5000n) return null;
    const power = exponent.n < 0n ? -exponent.n : exponent.n;
    const numerator = biPow(base.n, Number(power));
    const denominator = biPow(base.d, Number(power));
    return exponent.n < 0n ? ratDiv(ONE, rat(numerator, denominator)) : rat(numerator, denominator);
  }
  const powered = ratPow(base, ratInt(exponent.n));
  if (powered === null) return null;
  return exactRationalRoot(powered, exponent.d);
}

function exactRationalRoot(value, degree) {
  if (degree === 1n) return value;
  const degreeNumber = Number(degree);
  const negative = value.n < 0n;
  if (negative && degreeNumber % 2 === 0) return null;
  const numeratorRoot = exactIntegerRoot(negative ? -value.n : value.n, degreeNumber);
  const denominatorRoot = exactIntegerRoot(value.d, degreeNumber);
  if (numeratorRoot === null || denominatorRoot === null) return null;
  const root = rat(numeratorRoot, denominatorRoot);
  return negative ? ratNeg(root) : root;
}

function exactIntegerRoot(value, degree) {
  if (value < 0n) return null;
  if (value < 2n) return value;
  if (degree === 2) {
    const root = biIsqrt(value);
    return root * root === value ? root : null;
  }
  const digits = value.toString().length;
  const base = 10n ** BigInt(Math.max(0, Math.floor(digits / degree) - 1));
  // The digit-count guess is within about a factor of ten of the root.
  for (let offset = 0; offset <= 30; offset += 1) {
    const candidate = base * BigInt(offset + 1);
    const power = biPow(candidate, degree);
    if (power === value) return candidate;
    if (power > value && offset > 0) break;
  }
  return null;
}

// fromTerms builds the canonical AST back out of terms, ordered highest degree
// first so a polynomial reads the way it is written by hand.
function fromTerms(terms, expand, context) {
  const ordered = terms.slice().sort(function (left, right) {
    return termOrder(left, right, context);
  });
  let result = undefined;
  for (let index = 0; index < ordered.length; index += 1) {
    const term = ordered[index];
    const negative = ratSign(term.coeff) < 0;
    const body = termBody(ratAbs(term.coeff), term.factors, context);
    if (result === undefined) {
      result = negative ? { k: "neg", a: body, at: 1 } : body;
      continue;
    }
    result = negative ? subNode(result, body) : addNode(result, body);
  }
  return result === undefined ? numNode(ZERO) : result;
}

function buildProduct(nodes) {
  if (nodes.length === 0) return undefined;
  let result = nodes[0];
  for (let index = 1; index < nodes.length; index += 1) {
    result = mulNode(result, nodes[index]);
  }
  return result;
}

function termBody(coeff, factors, context) {
  const variable = context && context.variable;
  const ordered = factors.slice().sort(function (left, right) {
    const leftMain = variable !== undefined && left.base.k === "sym" && left.base.name === variable;
    const rightMain = variable !== undefined && right.base.k === "sym" && right.base.name === variable;
    if (leftMain !== rightMain) return leftMain ? -1 : 1;
    if (leftMain && rightMain) {
      const leftDegree = rationalOf(left.exp);
      const rightDegree = rationalOf(right.exp);
      if (leftDegree !== null && rightDegree !== null) {
        const difference = ratCmp(rightDegree, leftDegree);
        if (difference !== 0) return difference;
      }
    }
    const leftKey = nodeKey(left.base);
    const rightKey = nodeKey(right.base);
    return leftKey < rightKey ? -1 : (leftKey > rightKey ? 1 : 0);
  });
  const numerator = coeff.n === 1n ? [] : [numNode(ratInt(coeff.n))];
  const denominator = coeff.d === 1n ? [] : [numNode(ratInt(coeff.d))];
  for (let index = 0; index < ordered.length; index += 1) {
    const factor = ordered[index];
    const exponent = rationalOf(factor.exp);
    if (exponent !== null && ratSign(exponent) < 0) {
      denominator.push(poweredNode(factor.base, ratNeg(exponent), undefined));
    } else {
      numerator.push(poweredNode(factor.base, exponent, factor.exp));
    }
  }
  const top = buildProduct(numerator);
  if (denominator.length === 0) return top === undefined ? numNode(ONE) : top;
  const bottom = buildProduct(denominator);
  return { k: "div", a: top === undefined ? numNode(ONE) : top, b: bottom, at: 1 };
}

function poweredNode(base, exponent, fallback) {
  if (exponent !== null && ratIsOne(exponent)) return base;
  return { k: "pow", a: base, b: exponent !== null ? numNode(exponent) : fallback, at: 1 };
}

function termDegree(term) {
  let degree = 0;
  for (let index = 0; index < term.factors.length; index += 1) {
    const exponent = rationalOf(term.factors[index].exp);
    degree += exponent === null ? 1 : Math.max(0, ratToNumber(exponent));
  }
  return degree;
}

function termOrder(left, right, context) {
  const variable = context && context.variable;
  if (variable !== undefined) {
    const leftDegree = variableDegree(left, variable);
    const rightDegree = variableDegree(right, variable);
    if (leftDegree !== rightDegree) return rightDegree - leftDegree;
  }
  const leftTotal = termDegree(left);
  const rightTotal = termDegree(right);
  if (leftTotal !== rightTotal) return rightTotal - leftTotal;
  const leftKey = termKey(left);
  const rightKey = termKey(right);
  return leftKey < rightKey ? -1 : (leftKey > rightKey ? 1 : 0);
}

function variableDegree(term, variable) {
  for (let index = 0; index < term.factors.length; index += 1) {
    const factor = term.factors[index];
    if (factor.base.k === "sym" && factor.base.name === variable) {
      const exponent = rationalOf(factor.exp);
      return exponent === null ? 1 : ratToNumber(exponent);
    }
  }
  return 0;
}

// ---- canonical form ---------------------------------------------------------

function simplifyDeep(node, context, mode) {
  const rebuilt = mapChildren(node, function (child) {
    return simplifyDeep(child, context, mode);
  });
  if (rebuilt.k === "call") {
    const folded = evalExact(rebuilt, context.bound || {});
    if (folded !== null && isNumericallyBound(rebuilt, context.bound || {})) return numNode(folded);
  } else if (rebuilt.k === "num" || rebuilt.k === "sym") {
    return rebuilt;
  }
  if (collectSymbols(rebuilt).length === 0) {
    const folded = evalExact(rebuilt, {});
    if (folded !== null) return numNode(folded);
  }
  return fromTerms(toTerms(rebuilt, mode === "expand"), mode === "expand", context);
}

function mapChildren(node, visit) {
  switch (node.k) {
    case "num":
    case "sym":
      return node;
    case "neg":
    case "fact":
      return { k: node.k, a: visit(node.a), at: node.at };
    case "call": {
      const args = [];
      for (let index = 0; index < node.args.length; index += 1) args.push(visit(node.args[index]));
      return callNode(node.name, args);
    }
    default:
      return { k: node.k, a: visit(node.a), b: visit(node.b), at: node.at };
  }
}

function isNumericallyBound(node, bound) {
  const names = collectSymbols(node);
  for (let index = 0; index < names.length; index += 1) {
    if (CONSTANT_NAMES.indexOf(names[index]) >= 0) continue;
    if (!Object.prototype.hasOwnProperty.call(bound, names[index])) return false;
  }
  return true;
}

// ---- exact evaluation -------------------------------------------------------

// evalExact returns an exact rational or null. null means "no rational value",
// which is what sends an answer to the float path with an "≈".
export function evalExact(node, environment) {
  const env = environment || {};
  switch (node.k) {
    case "num":
      return rat(node.n, node.d);
    case "sym":
      return Object.prototype.hasOwnProperty.call(env, node.name) ? env[node.name] : null;
    case "neg": {
      const inner = evalExact(node.a, env);
      return inner === null ? null : ratNeg(inner);
    }
    case "add":
    case "sub":
    case "mul":
    case "div": {
      const left = evalExact(node.a, env);
      const right = evalExact(node.b, env);
      if (left === null || right === null) return null;
      if (node.k === "add") return ratAdd(left, right);
      if (node.k === "sub") return ratSub(left, right);
      if (node.k === "mul") return ratMul(left, right);
      if (ratIsZero(right)) return null;
      return ratDiv(left, right);
    }
    case "pow": {
      const base = evalExact(node.a, env);
      const exponent = evalExact(node.b, env);
      if (base === null || exponent === null) return null;
      if (!ratIsInt(exponent) && !ratEquals(base, ZERO) && ratSign(base) < 0) return null;
      const powered = ratPow(base, ratInt(exponent.n));
      if (powered === null) return null;
      if (ratIsInt(exponent)) return powered;
      return exactRationalRoot(powered, exponent.d);
    }
    case "fact": {
      const inner = evalExact(node.a, env);
      if (inner === null || !ratIsInt(inner) || inner.n < 0n || inner.n > 200n) return null;
      let result = ONE;
      for (let index = 2n; index <= inner.n; index += 1n) result = ratMul(result, ratInt(index));
      return result;
    }
    case "call":
      return evalExactCall(node, env);
    default:
      return null;
  }
}

// piMultiple returns k when the argument is exactly k*pi, which is what makes
// sin(pi) come back as 0 instead of 1.2246e-16.
function piMultiple(node) {
  const terms = toTerms(node, true);
  if (terms.length === 0) return ZERO;
  if (terms.length !== 1) return null;
  const term = terms[0];
  if (term.factors.length !== 1) return null;
  const factor = term.factors[0];
  if (factor.base.k !== "sym" || factor.base.name !== "pi") return null;
  const exponent = rationalOf(factor.exp);
  if (exponent === null || !ratIsOne(exponent)) return null;
  return term.coeff;
}

// quarterTurn reduces k*pi to its quadrant index 0..3.
function quarterTurn(turns) {
  const whole = turns.n / turns.d;
  const doubled = ratMul(ratSub(turns, ratInt(whole)), ratInt(2n));
  if (!ratIsInt(doubled)) return null;
  const index = bigToNumber(doubled.n) % 4;
  return index < 0 ? index + 4 : index;
}

const SIN_TABLE = [ZERO, ONE, ZERO, ratInt(-1n)];
const COS_TABLE = [ONE, ZERO, ratInt(-1n), ZERO];

function evalExactCall(node, env) {
  const first = node.args.length > 0 ? evalExact(node.args[0], env) : null;
  switch (node.name) {
    case "sqrt":
      return first === null || first.n < 0n ? null : exactRationalRoot(first, 2n);
    case "cbrt":
      return first === null ? null : exactRationalRoot(first, 3n);
    case "root": {
      const degree = node.args.length > 1 ? evalExact(node.args[1], env) : null;
      if (first === null || degree === null || !ratIsInt(degree) || degree.n < 1n) return null;
      return exactRationalRoot(first, degree.n);
    }
    case "abs":
      return first === null ? null : ratAbs(first);
    case "sign":
      return first === null ? null : ratInt(BigInt(ratSign(first)));
    case "floor":
      return first === null ? null : ratInt(floorRational(first));
    case "ceil": {
      if (first === null) return null;
      const floor = floorRational(first);
      return ratInt(ratIsInt(first) ? floor : floor + 1n);
    }
    case "trunc":
      return first === null ? null : ratInt(truncRational(first));
    case "round":
      return first === null ? null : ratInt(floorRational(ratAdd(first, rat(1n, 2n))));
    case "fract":
      return first === null ? null : ratSub(first, ratInt(truncRational(first)));
    case "exp":
      return first !== null && ratIsZero(first) ? ONE : null;
    case "ln":
      return first !== null && ratIsOne(first) ? ZERO : null;
    case "log": {
      if (node.args.length === 1) return first !== null && ratIsOne(first) ? ZERO : null;
      const base = evalExact(node.args[1], env);
      if (first === null || base === null) return null;
      if (ratIsOne(first)) return ZERO;
      if (ratEquals(first, base)) return ONE;
      return exactLog(first, base);
    }
    case "log2":
    case "log10": {
      if (first === null) return null;
      if (ratIsOne(first)) return ZERO;
      return exactLog(first, node.name === "log2" ? ratInt(2n) : ratInt(10n));
    }
    case "sin":
    case "cos":
    case "tan": {
      const turns = node.args.length === 1 ? piMultiple(node.args[0]) : null;
      if (turns === null) return null;
      const index = quarterTurn(turns);
      if (index === null) return null;
      if (node.name === "sin") return SIN_TABLE[index];
      if (node.name === "cos") return COS_TABLE[index];
      return index % 2 === 0 ? ZERO : null;
    }
    default:
      return null;
  }
}

function exactLog(value, base) {
  if (!ratIsInt(value) || !ratIsInt(base) || value.n < 1n || base.n < 2n) return null;
  let rest = value.n;
  let power = 0;
  while (rest > 1n) {
    if (rest % base.n !== 0n) return null;
    rest /= base.n;
    power += 1;
    if (power > 20000) return null;
  }
  return power === 0 ? null : ratInt(BigInt(power));
}

function floorRational(value) {
  const quotient = value.n / value.d;
  return value.n < 0n && quotient * value.d !== value.n ? quotient - 1n : quotient;
}

function truncRational(value) {
  return value.n / value.d;
}

// evalFloat is the numeric fallback: the root finder and the Simpson fallback
// for a definite integral. Unsupported subterms become NaN, which a caller
// reports as "no root found" rather than as an answer.
export function evalFloat(node, environment) {
  const env = environment || {};
  switch (node.k) {
    case "num":
      return bigToNumber(node.n) / bigToNumber(node.d);
    case "sym": {
      if (Object.prototype.hasOwnProperty.call(env, node.name)) return env[node.name];
      if (node.name === "pi") return Math.PI;
      if (node.name === "e") return Math.E;
      if (node.name === "tau") return Math.PI * 2;
      if (node.name === "phi") return (1 + Math.sqrt(5)) / 2;
      return NaN;
    }
    case "add":
      return evalFloat(node.a, env) + evalFloat(node.b, env);
    case "sub":
      return evalFloat(node.a, env) - evalFloat(node.b, env);
    case "mul":
      return evalFloat(node.a, env) * evalFloat(node.b, env);
    case "div":
      return evalFloat(node.a, env) / evalFloat(node.b, env);
    case "mod":
      return evalFloat(node.a, env) % evalFloat(node.b, env);
    case "neg":
      return -evalFloat(node.a, env);
    case "pow":
      return Math.pow(evalFloat(node.a, env), evalFloat(node.b, env));
    case "fact": {
      const argument = evalFloat(node.a, env);
      if (!isFinite(argument) || argument < 0) return NaN;
      if (argument > 170) {
        return Math.exp(argument * Math.log(argument) - argument + 0.5 * Math.log(2 * Math.PI * argument));
      }
      let result = 1;
      for (let index = 2; index <= argument; index += 1) result *= index;
      return result;
    }
    case "call":
      return evalFloatCall(node, env);
    default:
      return NaN;
  }
}

function evalFloatCall(node, env) {
  const args = [];
  for (let index = 0; index < node.args.length; index += 1) {
    args.push(evalFloat(node.args[index], env));
  }
  const first = args[0];
  switch (node.name) {
    case "sqrt": return Math.sqrt(first);
    case "cbrt": return Math.cbrt(first);
    case "root": return first < 0 ? -Math.pow(-first, 1 / args[1]) : Math.pow(first, 1 / args[1]);
    case "abs": return Math.abs(first);
    case "sign": return Math.sign(first);
    case "floor": return Math.floor(first);
    case "ceil": return Math.ceil(first);
    case "round": return Math.round(first);
    case "trunc": return Math.trunc(first);
    case "fract": return first - Math.trunc(first);
    case "exp": return Math.exp(first);
    case "ln": return Math.log(first);
    case "log2": return Math.log2(first);
    case "log10": return Math.log10(first);
    case "log": return args.length === 1 ? Math.log(first) : Math.log(first) / Math.log(args[1]);
    case "sin": return Math.sin(first);
    case "cos": return Math.cos(first);
    case "tan": return Math.tan(first);
    case "asin": return Math.asin(first);
    case "acos": return Math.acos(first);
    case "atan": return Math.atan(first);
    case "atan2": return Math.atan2(first, args[1]);
    case "sinh": return Math.sinh(first);
    case "cosh": return Math.cosh(first);
    case "tanh": return Math.tanh(first);
    case "asinh": return Math.asinh(first);
    case "acosh": return Math.acosh(first);
    case "atanh": return Math.atanh(first);
    case "pow": return Math.pow(first, args[1]);
    case "min": return Math.min.apply(null, args);
    case "max": return Math.max.apply(null, args);
    case "hypot": return Math.hypot.apply(null, args);
    default: return NaN;
  }
}

// ---- polynomials ------------------------------------------------------------

// polyCoeffs returns [c0, c1, ...] for an expression that is polynomial in the
// variable with rational coefficients, or null when it is not (a denominator, a
// function of the variable, or a second free symbol).
function polyCoeffs(node, variable) {
  let terms;
  try {
    terms = toTerms(node, true);
  } catch (error) {
    return null;
  }
  const coefficients = [];
  for (let index = 0; index < terms.length; index += 1) {
    const term = terms[index];
    let degree = 0;
    for (let position = 0; position < term.factors.length; position += 1) {
      const factor = term.factors[position];
      if (factor.base.k !== "sym" || factor.base.name !== variable) return null;
      const exponent = rationalOf(factor.exp);
      if (exponent === null || !ratIsInt(exponent) || exponent.n < 0n) return null;
      // 2^63 and above are not degrees any polynomial here can hold, and
      // Number(bigint) reported them as negative or zero.
      if (exponent.n > 64n) return null;
      degree = bigToNumber(exponent.n);
    }
    while (coefficients.length <= degree) coefficients.push(ZERO);
    coefficients[degree] = ratAdd(coefficients[degree], term.coeff);
  }
  while (coefficients.length > 0 && ratIsZero(coefficients[coefficients.length - 1])) coefficients.pop();
  return coefficients.length === 0 ? [ZERO] : coefficients;
}

function polynomialNode(coefficients, variable) {
  const terms = [];
  for (let degree = 0; degree < coefficients.length; degree += 1) {
    if (ratIsZero(coefficients[degree])) continue;
    terms.push({
      coeff: coefficients[degree],
      factors: degree === 0 ? [] : [{ base: symNode(variable), exp: numNode(ratInt(BigInt(degree))) }],
    });
  }
  return fromTerms(terms, false, { variable: variable });
}

function evaluatePolynomial(coefficients, value) {
  let result = ZERO;
  for (let index = coefficients.length - 1; index >= 0; index -= 1) {
    result = ratAdd(ratMul(result, value), coefficients[index]);
  }
  return result;
}

function dividePolynomial(coefficients, root) {
  const quotient = [];
  let carry = ZERO;
  for (let index = coefficients.length - 1; index >= 1; index -= 1) {
    carry = ratAdd(coefficients[index], carry);
    quotient.unshift(carry);
    carry = ratMul(carry, root);
  }
  return quotient;
}

function quadraticDiscriminant(coefficients) {
  return ratSub(ratMul(coefficients[1], coefficients[1]), ratMul(ratInt(4n), ratMul(coefficients[0], coefficients[2])));
}

function divisorsOf(value) {
  const divisors = [];
  if (value === 0n) return divisors;
  for (let candidate = 1n; candidate * candidate <= value; candidate += 1n) {
    if (value % candidate === 0n) {
      divisors.push(candidate);
      if (candidate * candidate !== value) divisors.push(value / candidate);
      if (divisors.length > 60) break;
    }
  }
  divisors.sort(function (left, right) {
    return left < right ? -1 : (left > right ? 1 : 0);
  });
  return divisors;
}

function rationalRootCandidates(coefficients) {
  const constant = coefficients[0];
  const leading = coefficients[coefficients.length - 1];
  if (!ratIsInt(constant) || !ratIsInt(leading) || ratIsZero(constant)) return [];
  const numerators = divisorsOf(biAbs(constant.n));
  const denominators = divisorsOf(biAbs(leading.n));
  const candidates = [];
  for (let index = 0; index < numerators.length; index += 1) {
    for (let position = 0; position < denominators.length; position += 1) {
      const value = rat(numerators[index], denominators[position]);
      candidates.push(value);
      candidates.push(ratNeg(value));
    }
  }
  candidates.push(ZERO);
  return candidates;
}

// ---- factor -----------------------------------------------------------------

function commonFactor(terms) {
  if (terms.length === 0) return null;
  let coefficient = ratAbs(terms[0].coeff);
  for (let index = 1; index < terms.length; index += 1) {
    coefficient = ratGcd(coefficient, ratAbs(terms[index].coeff));
  }
  const factors = [];
  const first = terms[0];
  for (let index = 0; index < first.factors.length; index += 1) {
    const candidate = first.factors[index];
    const exponent = rationalOf(candidate.exp);
    if (exponent === null || !ratIsInt(exponent) || exponent.n < 1n) continue;
    let smallest = exponent;
    let shared = true;
    for (let other = 1; other < terms.length; other += 1) {
      let found = null;
      for (let position = 0; position < terms[other].factors.length; position += 1) {
        if (nodeKey(terms[other].factors[position].base) === nodeKey(candidate.base)) {
          found = terms[other].factors[position];
          break;
        }
      }
      const otherExponent = found === null ? null : rationalOf(found.exp);
      if (otherExponent === null || !ratIsInt(otherExponent) || otherExponent.n < 1n) {
        shared = false;
        break;
      }
      if (ratCmp(otherExponent, smallest) < 0) smallest = otherExponent;
    }
    if (shared) factors.push({ base: candidate.base, exp: numNode(smallest) });
  }
  if (ratIsOne(coefficient) && factors.length === 0) return null;
  return makeTerm(coefficient, factors);
}

function ratGcd(left, right) {
  if (ratIsZero(left)) return ratAbs(right);
  if (ratIsZero(right)) return ratAbs(left);
  return rat(biGcd(left.n, right.n), biLcm(left.d, right.d));
}

function divideTerm(term, divisor) {
  const factors = term.factors.slice();
  for (let index = 0; index < divisor.factors.length; index += 1) {
    const wanted = divisor.factors[index];
    for (let position = 0; position < factors.length; position += 1) {
      if (nodeKey(factors[position].base) === nodeKey(wanted.base)) {
        factors[position] = {
          base: factors[position].base,
          exp: numNode(ratSub(rationalOf(factors[position].exp), rationalOf(wanted.exp))),
        };
        break;
      }
    }
  }
  return makeTerm(ratDiv(term.coeff, divisor.coeff), factors);
}

function squareRootOfTerm(term) {
  const numeratorRoot = exactIntegerRoot(biAbs(term.coeff.n), 2);
  const denominatorRoot = exactIntegerRoot(term.coeff.d, 2);
  if (numeratorRoot === null || denominatorRoot === null) return null;
  const factors = [];
  for (let index = 0; index < term.factors.length; index += 1) {
    const exponent = rationalOf(term.factors[index].exp);
    if (exponent === null || !ratIsInt(exponent) || exponent.n % 2n !== 0n) return null;
    factors.push({ base: term.factors[index].base, exp: numNode(ratInt(exponent.n / 2n)) });
  }
  return makeTerm(rat(numeratorRoot, denominatorRoot), factors);
}

function factorQuadratic(coefficients, variable, context) {
  const discriminant = quadraticDiscriminant(coefficients);
  if (ratSign(discriminant) < 0) return null;
  const root = exactRationalRoot(discriminant, 2n);
  if (root === null) return null;
  const denominator = ratMul(ratInt(2n), coefficients[2]);
  const roots = [
    ratDiv(ratAdd(ratNeg(coefficients[1]), root), denominator),
    ratDiv(ratSub(ratNeg(coefficients[1]), root), denominator),
  ];
  const parts = [];
  for (let index = 0; index < roots.length; index += 1) {
    parts.push(fromTerms([
      makeTerm(ratInt(roots[index].d), [{ base: symNode(variable), exp: oneNode() }]),
      makeTerm(ratInt(-roots[index].n), []),
    ], false, context));
  }
  // a*(x - p/q)*(x - r/s) is written with integer coefficients as
  // (a/(q*s)) * (q*x - p) * (s*x - r).
  const leading = ratDiv(coefficients[2], ratInt(roots[0].d * roots[1].d));
  let result = mulNode(parts[0], parts[1]);
  if (!ratIsOne(leading)) result = mulNode(numNode(leading), result);
  return sameExpression(result, polynomialNode(coefficients, variable), context) ? result : null;
}

// sameExpression compares two nodes through the canonical term form, which is
// what makes the integer-coefficient rewrite above safe to emit.
function sameExpression(left, right, context) {
  let leftTerms;
  let rightTerms;
  try {
    leftTerms = foldTerms(toTerms(simplifyDeep(left, context, "expand"), true));
    rightTerms = foldTerms(toTerms(simplifyDeep(right, context, "expand"), true));
  } catch (error) {
    return false;
  }
  if (leftTerms.length !== rightTerms.length) return false;
  const index = {};
  for (let position = 0; position < rightTerms.length; position += 1) {
    index[termKey(rightTerms[position])] = rightTerms[position].coeff;
  }
  for (let position = 0; position < leftTerms.length; position += 1) {
    const key = termKey(leftTerms[position]);
    if (index[key] === undefined || !ratEquals(index[key], leftTerms[position].coeff)) return false;
  }
  return true;
}

function factorByGrouping(terms, context) {
  const pairings = [[0, 1, 2, 3], [0, 2, 1, 3], [0, 3, 1, 2]];
  for (let index = 0; index < pairings.length; index += 1) {
    const order = pairings[index];
    const firstPair = [terms[order[0]], terms[order[1]]];
    const secondPair = [terms[order[2]], terms[order[3]]];
    const firstCommon = commonFactor(firstPair);
    const secondCommon = commonFactor(secondPair);
    if (firstCommon === null || secondCommon === null) continue;
    const firstRest = firstPair.map(function (term) {
      return divideTerm(term, firstCommon);
    });
    const secondRest = secondPair.map(function (term) {
      return divideTerm(term, secondCommon);
    });
    const firstKeys = firstRest.map(termKey).sort();
    const secondKeys = secondRest.map(termKey).sort();
    if (firstKeys.length !== 2 || firstKeys[0] !== secondKeys[0] || firstKeys[1] !== secondKeys[1]) continue;
    const firstCoefficients = coefficientMap(firstRest);
    const secondCoefficients = coefficientMap(secondRest);
    if (!ratEquals(firstCoefficients[firstKeys[0]], secondCoefficients[secondKeys[0]])) continue;
    if (!ratEquals(firstCoefficients[firstKeys[1]], secondCoefficients[secondKeys[1]])) continue;
    const factor = fromTerms([firstCommon, secondCommon], false, context);
    const residue = fromTerms(firstRest, false, context);
    return mulNode(factor, residue);
  }
  return null;
}

function coefficientMap(terms) {
  const map = {};
  for (let index = 0; index < terms.length; index += 1) {
    map[termKey(terms[index])] = terms[index].coeff;
  }
  return map;
}

function factorNode(node, context, notes, inner) {
  const quiet = inner ? [] : notes;
  const variable = context.variable;
  const expanded = simplifyDeep(node, context, "expand");
  const terms = toTerms(expanded, true);
  if (terms.length <= 1) return expanded;

  const common = commonFactor(terms);
  if (common !== null) {
    const rest = terms.map(function (term) {
      return divideTerm(term, common);
    });
    const restNode = factorNode(fromTerms(rest, false, context), context, notes, true);
    const commonNode = fromTerms([common], false, context);
    quiet.push("common factor " + nodeText(commonNode) + " taken out.");
    return mulNode(commonNode, restNode);
  }

  if (terms.length === 2 && ratSign(terms[0].coeff) !== ratSign(terms[1].coeff)) {
    const leftRoot = squareRootOfTerm(terms[0]);
    const rightRoot = squareRootOfTerm(terms[1]);
    if (leftRoot !== null && rightRoot !== null) {
      const positive = ratSign(terms[0].coeff) > 0 ? leftRoot : rightRoot;
      const negative = ratSign(terms[0].coeff) > 0 ? rightRoot : leftRoot;
      const firstFactor = fromTerms([
        makeTerm(positive.coeff, positive.factors),
        makeTerm(ratNeg(negative.coeff), negative.factors),
      ], false, context);
      const secondFactor = fromTerms([
        makeTerm(positive.coeff, positive.factors),
        makeTerm(negative.coeff, negative.factors),
      ], false, context);
      quiet.push("difference of squares.");
      return mulNode(firstFactor, secondFactor);
    }
  }

  const coefficients = polyCoeffs(expanded, variable);
  if (coefficients !== null && coefficients.length === 3) {
    const factored = factorQuadratic(coefficients, variable, context);
    if (factored !== null) {
      quiet.push("quadratic with rational roots.");
      return factored;
    }
    const discriminant = quadraticDiscriminant(coefficients);
    quiet.push("no further rational factorisation: the discriminant " + ratToText(discriminant) +
      (ratSign(discriminant) < 0
        ? " is negative, so the roots are complex (solve returns them)."
        : " is not a perfect square, so the roots are irrational (solve returns them)."));
    return expanded;
  }

  if (terms.length === 4) {
    const grouped = factorByGrouping(terms, context);
    if (grouped !== null) {
      quiet.push("grouping: each pair shares a factor.");
      return grouped;
    }
  }

  if (!inner) {
    quiet.push("left as it is: no common factor, difference of squares, rational quadratic or pair grouping applies.");
  }
  return expanded;
}

// ---- derivative -------------------------------------------------------------

function derivativeOfCall(node) {
  const u = node.args[0];
  const one = oneNode();
  const two = numNode(ratInt(2n));
  const square = function (value) {
    return { k: "pow", a: value, b: two, at: 1 };
  };
  const inverse = function (value) {
    return { k: "div", a: one, b: value, at: 1 };
  };
  switch (node.name) {
    case "sqrt":
      return inverse(mulNode(two, callNode("sqrt", [u])));
    case "cbrt":
      return inverse(mulNode(numNode(ratInt(3n)), { k: "pow", a: u, b: numNode(rat(2n, 3n)), at: 1 }));
    case "exp":
      return callNode("exp", [u]);
    case "ln":
      return inverse(u);
    case "log":
      return inverse(mulNode(u, callNode("ln", [node.args.length > 1 ? node.args[1] : numNode(Math.E)])));
    case "sin":
      return callNode("cos", [u]);
    case "cos":
      return { k: "neg", a: callNode("sin", [u]), at: 1 };
    case "tan":
      return inverse(square(callNode("cos", [u])));
    case "asin":
      return inverse(callNode("sqrt", [subNode(one, square(u))]));
    case "acos":
      return { k: "neg", a: inverse(callNode("sqrt", [subNode(one, square(u))])), at: 1 };
    case "atan":
      return inverse(addNode(one, square(u)));
    case "sinh":
      return callNode("cosh", [u]);
    case "cosh":
      return callNode("sinh", [u]);
    case "tanh":
      return inverse(square(callNode("cosh", [u])));
    case "asinh":
      return inverse(callNode("sqrt", [addNode(square(u), one)]));
    case "acosh":
      return inverse(callNode("sqrt", [subNode(square(u), one)]));
    case "atanh":
      return inverse(subNode(one, square(u)));
    case "abs":
      return callNode("sign", [u]);
    default:
      bad("derivative: no rule for " + node.name + "()");
      return undefined;
  }
}

// tidyDerivative expands a quotient's numerator, so a quotient-rule result reads
// as (x^2 - 2*x - 1)/(x - 1)^2 rather than as nested products, while the
// denominator stays factored.
function tidyDerivative(node, context) {
  if (node === undefined || node === null || typeof node.k !== "string") return node;
  if (node.k === "div") {
    return {
      k: "div",
      a: tidyDerivative(simplifyDeep(node.a, context, "expand"), context),
      b: tidyDerivative(node.b, context),
      at: 1,
    };
  }
  if (node.k === "add" || node.k === "sub" || node.k === "mul") {
    return { k: node.k, a: tidyDerivative(node.a, context), b: tidyDerivative(node.b, context), at: 1 };
  }
  if (node.k === "neg") return { k: "neg", a: tidyDerivative(node.a, context), at: 1 };
  return node;
}

function derivativeNode(node, variable, context) {
  switch (node.k) {
    case "num":
      return numNode(ZERO);
    case "sym":
      return node.name === variable ? numNode(ONE) : numNode(ZERO);
    case "neg":
      return { k: "neg", a: derivativeNode(node.a, variable, context), at: 1 };
    case "add":
      return addNode(derivativeNode(node.a, variable, context), derivativeNode(node.b, variable, context));
    case "sub":
      return subNode(derivativeNode(node.a, variable, context), derivativeNode(node.b, variable, context));
    case "mul":
      return addNode(
        mulNode(derivativeNode(node.a, variable, context), node.b),
        mulNode(node.a, derivativeNode(node.b, variable, context))
      );
    case "div":
      return {
        k: "div",
        a: subNode(mulNode(derivativeNode(node.a, variable, context), node.b), mulNode(node.a, derivativeNode(node.b, variable, context))),
        b: { k: "pow", a: node.b, b: numNode(ratInt(2n)), at: 1 },
        at: 1,
      };
    case "pow": {
      const exponentIsConstant = collectSymbols(node.b).indexOf(variable) < 0;
      const baseIsConstant = collectSymbols(node.a).indexOf(variable) < 0;
      if (exponentIsConstant) {
        const reduced = { k: "pow", a: node.a, b: subNode(node.b, oneNode()), at: 1 };
        return mulNode(mulNode(node.b, reduced), derivativeNode(node.a, variable, context));
      }
      if (baseIsConstant) {
        return mulNode(mulNode(node, callNode("ln", [node.a])), derivativeNode(node.b, variable, context));
      }
      const first = mulNode(derivativeNode(node.b, variable, context), callNode("ln", [node.a]));
      const second = mulNode(node.b, { k: "div", a: derivativeNode(node.a, variable, context), b: node.a, at: 1 });
      return mulNode(node, addNode(first, second));
    }
    case "mod":
      bad("derivative does not support the modulo operator");
      return undefined;
    case "fact":
      bad("derivative does not support factorial");
      return undefined;
    case "call":
      return mulNode(derivativeOfCall(node), derivativeNode(node.args[0], variable, context));
    default:
      bad("derivative: unsupported expression");
      return undefined;
  }
}

// ---- integrate --------------------------------------------------------------

function linearCoefficients(node, variable) {
  const coefficients = polyCoeffs(node, variable);
  if (coefficients === null || coefficients.length > 2) return null;
  return {
    intercept: coefficients.length > 0 ? coefficients[0] : ZERO,
    slope: coefficients.length > 1 ? coefficients[1] : ZERO,
  };
}

function freeOfVariable(node, variable) {
  return collectSymbols(node).indexOf(variable) < 0;
}

function integerPowerNode(base, exponent) {
  return { k: "pow", a: base, b: numNode(ratInt(BigInt(exponent))), at: 1 };
}

function integrateTerm(term, variable, context) {
  const x = symNode(variable);
  const whole = nodeText(fromTerms([term], false, context));
  const variableTimesCoefficient = function (coeff, factors) {
    return fromTerms([makeTerm(coeff, [{ base: x, exp: oneNode() }].concat(factors))], false, context);
  };
  if (term.factors.length === 0) {
    return variableTimesCoefficient(term.coeff, []);
  }
  if (term.factors.length > 1) {
    bad("integrate: no rule for " + whole + " (a product of two variable parts; powers, exponentials, sin/cos, sqrt and ln of the variable are covered)");
  }
  const factor = term.factors[0];
  const exponent = rationalOf(factor.exp);
  const base = factor.base;
  if (base.k === "sym" && base.name === variable) {
    if (exponent === null) bad("integrate: no rule for " + whole + " (a symbolic exponent)");
    if (ratEquals(exponent, ratInt(-1n))) {
      return mulNode(numNode(term.coeff), callNode("ln", [callNode("abs", [x])]));
    }
    const raised = ratAdd(exponent, ONE);
    return fromTerms([makeTerm(ratDiv(term.coeff, raised), [{ base: x, exp: numNode(raised) }])], false, context);
  }
  if (freeOfVariable(fromTerms([term], false, context), variable)) {
    return variableTimesCoefficient(term.coeff, term.factors);
  }
  if (base.k === "num" && term.factors.length === 1) {
    const linear = linearCoefficients(factor.exp, variable);
    if (linear === null || ratIsZero(linear.slope)) {
      bad("integrate: no rule for " + whole + " (the exponent must be linear in " + variable + ")");
    }
    const value = { k: "pow", a: base, b: fromTerms(toTerms(factor.exp, false), false, context), at: 1 };
    return fromTerms([makeTerm(ratDiv(term.coeff, linear.slope), [
      { base: value, exp: oneNode() },
      { base: callNode("ln", [base]), exp: numNode(ratInt(-1n)) },
    ])], false, context);
  }
  if (base.k === "call" && ratIsOne(exponent)) {
    const argument = base.args[0];
    const linear = linearCoefficients(argument, variable);
    if (base.name === "exp" && linear !== null && !ratIsZero(linear.slope)) {
      return fromTerms([makeTerm(ratDiv(term.coeff, linear.slope), [{ base: base, exp: oneNode() }])], false, context);
    }
    if (base.name === "sin" && linear !== null && !ratIsZero(linear.slope)) {
      return fromTerms([makeTerm(ratDiv(ratNeg(term.coeff), linear.slope), [{
        base: callNode("cos", [argument]), exp: oneNode(),
      }])], false, context);
    }
    if (base.name === "cos" && linear !== null && !ratIsZero(linear.slope)) {
      return fromTerms([makeTerm(ratDiv(term.coeff, linear.slope), [{
        base: callNode("sin", [argument]), exp: oneNode(),
      }])], false, context);
    }
    if (base.name === "ln" && argument.k === "sym" && argument.name === variable) {
      // x*ln(x) - x
      return fromTerms([
        makeTerm(term.coeff, [{ base: x, exp: oneNode() }, { base: base, exp: oneNode() }]),
        makeTerm(ratNeg(term.coeff), [{ base: x, exp: oneNode() }]),
      ], false, context);
    }
    if (base.name === "sqrt" && argument.k === "sym" && argument.name === variable) {
      return fromTerms([makeTerm(ratMul(term.coeff, rat(2n, 3n)), [
        { base: x, exp: numNode(rat(3n, 2n)) },
      ])], false, context);
    }
  }
  bad("integrate: no rule for " + whole + " (only powers, k*x exponentials, sin/cos of a linear argument, sqrt, ln and constant factors are integrated)");
  return undefined;
}

function antiderivative(node, variable, context) {
  const terms = toTerms(node, true);
  const pieces = [];
  for (let index = 0; index < terms.length; index += 1) {
    pieces.push(integrateTerm(terms[index], variable, context));
  }
  if (pieces.length === 0) return numNode(ZERO);
  let result = pieces[0];
  for (let index = 1; index < pieces.length; index += 1) {
    result = addNode(result, pieces[index]);
  }
  return simplifyDeep(result, context, "simplify");
}

function adaptiveSimpson(f, a, b, tolerance, depth) {
  const midpoint = (a + b) / 2;
  const left = f(a);
  const middle = f(midpoint);
  const right = f(b);
  if (!isFinite(left) || !isFinite(middle) || !isFinite(right)) return NaN;
  const whole = (b - a) / 6 * (left + 4 * middle + right);
  const leftHalf = (midpoint - a) / 6 * (left + 4 * f((a + midpoint) / 2) + middle);
  const rightHalf = (b - midpoint) / 6 * (middle + 4 * f((midpoint + b) / 2) + right);
  if (depth <= 0 || Math.abs(leftHalf + rightHalf - whole) <= 15 * tolerance) {
    return leftHalf + rightHalf + (leftHalf + rightHalf - whole) / 15;
  }
  return adaptiveSimpson(f, a, midpoint, tolerance / 2, depth - 1) +
    adaptiveSimpson(f, midpoint, b, tolerance / 2, depth - 1);
}

// ---- solve ------------------------------------------------------------------

function numericRoots(node, variable, range, notes) {
  const samples = 400;
  const at = function (x) {
    const environment = {};
    environment[variable] = x;
    return evalFloat(node, environment);
  };
  const roots = [];
  const push = function (value) {
    for (let index = 0; index < roots.length; index += 1) {
      if (Math.abs(roots[index] - value) < 1e-9) return;
    }
    roots.push(value);
  };
  let previousX = -range;
  let previousY = at(previousX);
  for (let index = 1; index <= samples; index += 1) {
    const x = -range + (2 * range * index) / samples;
    const y = at(x);
    if (isFinite(previousY) && isFinite(y)) {
      if (previousY === 0) push(previousX);
      if (y === 0) push(x);
      if (previousY * y < 0) {
        let low = previousX;
        let high = x;
        let lowValue = previousY;
        for (let step = 0; step < 200; step += 1) {
          const middle = (low + high) / 2;
          const value = at(middle);
          if (!isFinite(value)) break;
          if (value === 0 || high - low < 1e-14) {
            low = middle;
            high = middle;
            break;
          }
          if (lowValue * value < 0) high = middle;
          else {
            low = middle;
            lowValue = value;
          }
        }
        push((low + high) / 2);
      }
    }
    previousX = x;
    previousY = y;
  }
  if (roots.length > 0) {
    notes.push("numeric roots come from a sign-change scan over [-" + range + ", " + range + "] refined by bisection; they are not exact.");
  } else {
    notes.push("no real root was found numerically in [-" + range + ", " + range + "] (a root outside that range, or a double root, is not ruled out).");
  }
  roots.sort(function (left, right) {
    return left - right;
  });
  return roots;
}

function formatRational(value, context) {
  return context.format === "latex" ? latexOf(numNode(value)) : ratToText(value);
}

// exactSeries factors out every rational root, then finishes with the quadratic
// formula; a complex pair is reported with its exact form when there is one.
// radicalTerm rewrites sqrt(value)*factor/divisor with the square factors of
// value taken out, which is what turns (0 ± sqrt(8))/2 into ± sqrt(2).
function radicalTerm(value, factor, divisor, context) {
  let square = 1n;
  let rest = value;
  for (let candidate = 2n; candidate * candidate <= rest && candidate < 100000n; candidate += 1n) {
    while (rest % (candidate * candidate) === 0n) {
      rest /= candidate * candidate;
      square *= candidate;
    }
  }
  let top = square * factor;
  let bottom = divisor < 0n ? -divisor : divisor;
  const shared = bigGcd(top === 0n ? 1n : top, bottom);
  top /= shared;
  bottom /= shared;
  const root = context.format === "latex"
    ? "\\sqrt{" + rest.toString() + "}"
    : "sqrt(" + rest.toString() + ")";
  if (rest === 1n) return ratToText(rat(top, bottom));
  const topText = top === 1n ? root : top.toString() + "*" + root;
  return bottom === 1n ? topText : topText + "/" + bottom.toString();
}

function bigGcd(left, right) {
  let a = left < 0n ? -left : left;
  let b = right < 0n ? -right : right;
  while (b !== 0n) {
    const remainder = a % b;
    a = b;
    b = remainder;
  }
  return a;
}

// highestPower reports the largest exponent of the variable in the node, or null
// when every occurrence is at degree 0. The AST uses a/b for binary operators and
// args for calls, which is why both are walked.
function highestPower(node, variable) {
  let highest = null;
  const record = function (power) {
    highest = highest === null ? power : Math.max(highest, power);
  };
  const visit = function (current, power) {
    if (!current || typeof current !== "object") {
      return;
    }
    if (current.k === "sym") {
      if (current.name === variable) {
        record(power);
      }
      return;
    }
    if (current.k === "num") {
      return;
    }
    if (current.k === "pow") {
      const exponent = rationalOf(current.b);
      if (exponent !== null && ratIsInt(exponent) && exponent.n >= 0n && exponent.n <= 10000n) {
        visit(current.a, power * bigToNumber(exponent.n));
        return;
      }
      // A symbolic exponent: the degree is not known, so both operands are
      // walked at the current degree.
      visit(current.a, power);
      visit(current.b, 0);
      return;
    }
    if (current.a) {
      visit(current.a, power);
    }
    if (current.b) {
      visit(current.b, power);
    }
    if (current.args) {
      for (let index = 0; index < current.args.length; index += 1) {
        visit(current.args[index], power);
      }
    }
  };
  visit(node, 1);
  return highest;
}

function exactSeries(coefficients, variable, context) {
  const lines = [];
  let remaining = coefficients.slice();
  const roots = [];
  const line = function (root, suffix) {
    return variable + " = " + formatRational(root, context) + "  (" + suffix + ")";
  };
  while (remaining.length > 1) {
    if (remaining.length === 2) {
      roots.push(ratDiv(ratNeg(remaining[0]), remaining[1]));
      break;
    }
    if (remaining.length === 3) break;
    let found = null;
    const candidates = rationalRootCandidates(remaining);
    for (let index = 0; index < candidates.length; index += 1) {
      if (ratIsZero(evaluatePolynomial(remaining, candidates[index]))) {
        found = candidates[index];
        break;
      }
    }
    if (found === null) break;
    roots.push(found);
    remaining = dividePolynomial(remaining, found);
  }
  for (let index = 0; index < roots.length; index += 1) {
    lines.push(line(roots[index], "exact"));
  }
  if (remaining.length <= 1) return { lines: lines, complete: true };
  // After the rational roots are factored out, the remainder is solved in closed
  // form only when it is linear or quadratic. Treating a longer remainder as a
  // quadratic (the first three coefficients of a degree-4+ polynomial) produced
  // roots of a *different* polynomial and claimed the set was complete: x^5-1
  // reported the roots of x^2+x+1, and x^3-2 threw a division by zero.
  if (remaining.length > 3) {
    return { lines: lines, complete: false };
  }
  const discriminant = quadraticDiscriminant(remaining);
  const denominator = ratMul(ratInt(2n), remaining[2]);
  const realPart = ratDiv(ratNeg(remaining[1]), denominator);
  if (ratSign(discriminant) >= 0) {
    const root = exactRationalRoot(discriminant, 2n);
    if (root !== null) {
      lines.push(line(ratDiv(ratAdd(ratNeg(remaining[1]), root), denominator), "exact"));
      lines.push(line(ratDiv(ratSub(ratNeg(remaining[1]), root), denominator), "exact"));
      return { lines: lines, complete: true };
    }
    // sqrt(D)/denominator, with D's square factors taken out when D is an
    // integer (the usual case), else stated as written.
    const radical = discriminant.d === 1n
      ? radicalTerm(biAbs(discriminant.n), denominator.d, denominator.n, context)
      : (context.format === "latex"
        ? "\\frac{\\sqrt{" + latexOf(numNode(discriminant)) + "}}{" + latexOf(numNode(denominator)) + "}"
        : "sqrt(" + ratToText(discriminant) + ")/" + ratToText(denominator));
    lines.push(variable + " = " + (ratIsZero(realPart) ? "± " : formatRational(realPart, context) + " ± ") + radical + "  (exact)");
    const offset = Math.sqrt(ratToNumber(discriminant));
    const midpoint = ratToNumber(realPart);
    const scale = ratToNumber(denominator);
    lines.push(variable + " ≈ " + formatNumber(midpoint + offset / scale, context.decimalPlaces) + " and " +
      formatNumber(midpoint - offset / scale, context.decimalPlaces) + "  (numeric)");
    return { lines: lines, complete: true };
  }
  const imaginaryRoot = exactRationalRoot(ratNeg(discriminant), 2n);
  const magnitude = Math.sqrt(ratToNumber(ratNeg(discriminant))) / ratToNumber(denominator);
  if (imaginaryRoot !== null) {
    const imaginary = ratDiv(imaginaryRoot, denominator);
    const imaginaryText = ratIsOne(imaginary) ? "i" : formatRational(imaginary, context) + "*i";
    const exactText = ratIsZero(realPart)
      ? "± " + imaginaryText
      : formatRational(realPart, context) + " ± " + imaginaryText;
    lines.push(variable + " = " + exactText + "  (exact, complex)");
  } else if (ratNeg(discriminant).d === 1n) {
    const radical = radicalTerm(biAbs(discriminant.n), denominator.d, denominator.n, context);
    const imaginaryText = radical === "1" ? "i" : radical + "*i";
    const exactText = ratIsZero(realPart)
      ? "± " + imaginaryText
      : formatRational(realPart, context) + " ± " + imaginaryText;
    lines.push(variable + " = " + exactText + "  (exact, complex)");
  }
  lines.push(variable + " = " + formatNumber(ratToNumber(realPart), context.decimalPlaces) + " ± " +
    formatNumber(magnitude, context.decimalPlaces) + " i  (numeric, complex: the discriminant " +
    ratToText(discriminant) + " is negative)");
  return { lines: lines, complete: true };
}

function solveNode(node, variable, context, notes) {
  const coefficients = polyCoeffs(node, variable);
  if (coefficients !== null) {
    if (coefficients.length === 2) {
      return {
        lines: [variable + " = " + formatRational(ratDiv(ratNeg(coefficients[0]), coefficients[1]), context) + "  (exact)"],
        complete: true,
      };
    }
    if (coefficients.length === 3) {
      const discriminant = quadraticDiscriminant(coefficients);
      if (ratSign(discriminant) < 0) {
        notes.push("the discriminant " + ratToText(discriminant) + " is negative, so there is no real solution.");
      }
      return exactSeries(coefficients, variable, context);
    }
    const result = exactSeries(coefficients, variable, context);
    if (result.complete) return result;
    notes.push("a factor of degree " + (coefficients.length - 1) + " or more is left, so the remaining roots were found numerically over the search range.");
    const roots = numericRoots(node, variable, context.range, notes);
    return {
      lines: result.lines.concat(roots.map(function (root) {
        return variable + " = " + formatNumber(root, context.decimalPlaces) + "  (numeric)";
      })),
      complete: true,
    };
  }
  const roots = numericRoots(node, variable, context.range, notes);
  return {
    lines: roots.map(function (root) {
      return variable + " = " + formatNumber(root, context.decimalPlaces) + "  (numeric)";
    }),
    complete: roots.length > 0,
  };
}

// ---- laTeX ------------------------------------------------------------------

export function latexOf(node) {
  switch (node.k) {
    case "num": {
      const value = rationalOf(node);
      if (value !== null && value.d !== 1n) {
        const sign = ratSign(value) < 0 ? "-" : "";
        return sign + "\\frac{" + biAbs(value.n).toString() + "}{" + value.d.toString() + "}";
      }
      return nodeText(node);
    }
    case "sym":
      if (node.name === "pi") return "\\pi";
      if (node.name === "tau") return "\\tau";
      if (node.name === "phi") return "\\phi";
      if (node.name.length > 1) return "\\mathrm{" + node.name + "}";
      return node.name;
    case "add":
      return latexOf(node.a) + " + " + latexOf(node.b);
    case "sub":
      return latexOf(node.a) + " - " + latexOf(node.b);
    case "neg":
      return "-" + latexOf(node.a);
    case "mul":
      return latexOf(node.a) + " \\cdot " + latexOf(node.b);
    case "div":
      return "\\frac{" + latexOf(node.a) + "}{" + latexOf(node.b) + "}";
    case "mod":
      return latexOf(node.a) + " \\bmod " + latexOf(node.b);
    case "pow":
      return latexOf(node.a) + "^{" + latexOf(node.b) + "}";
    case "fact":
      return latexOf(node.a) + "!";
    case "call": {
      const args = node.args.map(latexOf).join(", ");
      switch (node.name) {
        case "sqrt": return "\\sqrt{" + args + "}";
        case "cbrt": return "\\sqrt[3]{" + args + "}";
        case "abs": return "\\left|" + args + "\\right|";
        case "exp": return "e^{" + args + "}";
        case "ln": return "\\ln\\left(" + args + "\\right)";
        case "log":
          return node.args.length < 2
            ? "\\ln\\left(" + args + "\\right)"
            : "\\log_{" + latexOf(node.args[1]) + "}\\left(" + latexOf(node.args[0]) + "\\right)";
        case "sin":
        case "cos":
        case "tan":
        case "sinh":
        case "cosh":
        case "tanh":
        case "asin":
        case "acos":
        case "atan":
          return "\\" + node.name + "\\left(" + args + "\\right)";
        default:
          return "\\mathrm{" + node.name + "}\\left(" + args + "\\right)";
      }
    }
    default:
      return nodeText(node);
  }
}

// ---- driver -----------------------------------------------------------------

function symbolicNames(node) {
  return collectSymbols(node).filter(function (name) {
    return CONSTANT_NAMES.indexOf(name) < 0;
  });
}

function chooseVariable(node, requested) {
  const names = symbolicNames(node);
  if (requested) return { variable: requested, note: "" };
  if (names.length === 0) return { variable: "x", note: "" };
  if (names.length === 1) return { variable: names[0], note: "" };
  return {
    variable: names[0],
    note: "the expression has " + names.length + " variables (" + names.join(", ") + "); used \"" + names[0] + "\", pass variable to choose another",
  };
}

function parseValues(values) {
  const bound = {};
  const symbolic = {};
  if (values === undefined || values === null) return { bound: bound, symbolic: symbolic };
  const keys = Object.keys(values);
  for (let index = 0; index < keys.length; index += 1) {
    const key = keys[index];
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) bad("values key " + JSON.stringify(key) + " is not a name");
    const raw = values[key];
    if (typeof raw === "number") {
      if (!isFinite(raw)) bad("values." + key + " must be finite");
      bound[key] = ratFromNumber(raw);
      symbolic[key] = numNode(bound[key]);
      continue;
    }
    if (typeof raw === "string") {
      const parsed = parseExpression(tokenize(raw), raw.length + 1);
      if (collectSymbols(parsed).length === 0) {
        const exact = evalExact(parsed, {});
        if (exact !== null) bound[key] = exact;
      }
      symbolic[key] = parsed;
      continue;
    }
    bad("values." + key + " must be a number or an expression string");
  }
  return { bound: bound, symbolic: symbolic };
}

function substituteNode(node, replacements) {
  if (node.k === "sym" && Object.prototype.hasOwnProperty.call(replacements, node.name)) {
    return replacements[node.name];
  }
  switch (node.k) {
    case "num":
    case "sym":
      return node;
    case "neg":
    case "fact":
      return { k: node.k, a: substituteNode(node.a, replacements), at: node.at };
    case "call": {
      const args = [];
      for (let index = 0; index < node.args.length; index += 1) args.push(substituteNode(node.args[index], replacements));
      return callNode(node.name, args);
    }
    default:
      return { k: node.k, a: substituteNode(node.a, replacements), b: substituteNode(node.b, replacements), at: node.at };
  }
}

function render(node, context) {
  return context.format === "latex" ? latexOf(node) : nodeText(node);
}

function containsCall(node, name) {
  let found = false;
  const visit = function (current) {
    if (current === undefined || current === null || typeof current.k !== "string") return;
    if (current.k === "call") {
      if (current.name === name) found = true;
      for (let index = 0; index < current.args.length; index += 1) visit(current.args[index]);
      return;
    }
    visit(current.a);
    visit(current.b);
  };
  visit(node);
  return found;
}

function describeBindings(values) {
  const keys = Object.keys(values.symbolic);
  const parts = [];
  for (let index = 0; index < keys.length; index += 1) {
    parts.push(keys[index] + " = " + nodeText(values.symbolic[keys[index]]));
  }
  return parts.join(", ");
}

export function suggestOperations(name) {
  return closeMatches(name, OPERATIONS, 4);
}

// runSymbolic is the whole tool: parse the expression, run one operation, and
// return the header, the result and the notes the model reads.
export function runSymbolic(options) {
  const source = String(options.expression === undefined || options.expression === null ? "" : options.expression).trim();
  if (source === "") bad("expression must not be empty");
  if (OPERATIONS.indexOf(options.operation) < 0) {
    const near = suggestOperations(String(options.operation));
    bad("unknown operation \"" + options.operation + "\"" +
      (near.length > 0 ? "; did you mean " + near.map(JSON.stringify).join(", ") + "?" : "; use " + OPERATIONS.join(", ")));
  }
  const tokens = tokenize(source);
  const endColumn = source.length + 1;
  const equation = splitEquation(tokens);
  let node;
  if (equation !== null) {
    const left = parseExpression(equation.left, endColumn);
    const right = parseExpression(equation.right, endColumn);
    node = subNode(left, right);
  } else {
    node = parseExpression(tokens, endColumn);
  }

  const values = parseValues(options.values);
  const context = {
    variable: "x",
    format: options.format,
    decimalPlaces: options.decimalPlaces,
    maxDegree: options.maxDegree,
    range: options.range,
    bound: values.bound,
  };
  const notes = [];
  const operation = options.operation;
  const chosen = chooseVariable(node, options.variable);
  // simplify and expand do not act on a particular variable, so the "which
  // variable" note would be noise there; every other operation needs one.
  if (chosen.note !== "" && operation !== "simplify" && operation !== "expand") notes.push(chosen.note + ".");
  context.variable = chosen.variable;

  if (operation === "simplify" || operation === "expand") {
    if (operation === "expand") {
      // An expansion's cost is its degree: (x+1)^64 has 65 terms, and the
      // setting exists to bound exactly that. It applied to factor and the
      // derivative order only, so a documented cost control did not apply to
      // the operation most likely to blow up.
      const degree = highestPower(node, chosen.variable);
      if (degree !== null && degree > context.maxDegree) {
        bad("expand is limited to degree " + context.maxDegree + " in " + chosen.variable + "; this is degree " + degree + ". Raise maxDegree in the settings to expand it.");
      }
    }
    const result = simplifyDeep(node, context, operation === "expand" ? "expand" : "simplify");
    return { header: operation + ": " + chosen.variable, body: "= " + render(result, context), notes: notes };
  }

  if (operation === "factor") {
    const degrees = polyCoeffs(node, chosen.variable);
    if (degrees !== null && degrees.length - 1 > context.maxDegree) {
      bad("factor is limited to degree " + context.maxDegree + " in " + chosen.variable + "; this is degree " + (degrees.length - 1));
    }
    return { header: "factor: " + chosen.variable, body: "= " + render(factorNode(node, context, notes), context), notes: notes };
  }

  if (operation === "derivative") {
    const order = options.order;
    if (Math.floor(order) !== order || order < 1) bad("order must be a whole number of at least 1 (got " + order + ")");
    if (order > context.maxDegree) bad("order is limited to " + context.maxDegree + " (got " + order + ")");
    let result = simplifyDeep(node, context, "simplify");
    for (let step = 0; step < order; step += 1) {
      result = simplifyDeep(derivativeNode(result, chosen.variable, context), context, "simplify");
    }
    result = tidyDerivative(result, context);
    if (containsCall(result, "sign")) {
      notes.push("abs() differentiates to sign(), which is undefined where its argument is zero.");
    }
    return {
      header: "derivative: d/d" + chosen.variable + (order > 1 ? ", order " + order : ""),
      body: "= " + render(result, context),
      notes: notes,
    };
  }

  if (operation === "integrate") {
    const hasLower = options.from !== undefined && options.from !== null;
    const hasUpper = options.to !== undefined && options.to !== null;
    if (hasLower !== hasUpper) bad("a definite integral needs both from and to");
    if (!hasLower) {
      const result = antiderivative(node, chosen.variable, context);
      return {
        header: "integrate: d" + chosen.variable,
        body: "= " + render(result, context) + " + C",
        notes: notes,
      };
    }
    const lower = options.from;
    const upper = options.to;
    if (!isFinite(lower) || !isFinite(upper)) bad("from and to must be finite numbers");
    let symbolic = null;
    try {
      symbolic = antiderivative(node, chosen.variable, context);
    } catch (error) {
      notes.push("no antiderivative rule applies (" + String(error && error.message ? error.message : error).replace(/^[a-z_]+: /, "") + "), so the value comes from numeric quadrature.");
    }
    let body = "";
    if (symbolic !== null) {
      const environment = {};
      environment[chosen.variable] = ratFromNumber(upper);
      const atUpper = evalExact(symbolic, environment);
      environment[chosen.variable] = ratFromNumber(lower);
      const atLower = evalExact(symbolic, environment);
      if (atUpper !== null && atLower !== null) {
        const value = ratSub(atUpper, atLower);
        body = "= " + render(numNode(value), context);
        if (value.d !== 1n) body += " (" + ratToDecimal(value, context.decimalPlaces) + ")";
        body += "  (exact)";
      } else {
        // The antiderivative exists but a limit is not an exact rational (a
        // decimal approximation of pi, a sine of a number): evaluate it in
        // floating point, which is more accurate than quadrature.
        const upperEnvironment = {};
        upperEnvironment[chosen.variable] = upper;
        const lowerEnvironment = {};
        lowerEnvironment[chosen.variable] = lower;
        const upperValue = evalFloat(symbolic, upperEnvironment);
        const lowerValue = evalFloat(symbolic, lowerEnvironment);
        if (isFinite(upperValue) && isFinite(lowerValue)) {
          body = "≈ " + formatNumber(upperValue - lowerValue, context.decimalPlaces) +
            "  (numeric: the antiderivative evaluated at the limits)";
          notes.push("the limits are not exact rationals, so the antiderivative was evaluated in floating point.");
        }
      }
    }
    if (body === "") {
      const value = adaptiveSimpson(function (x) {
        const environment = {};
        environment[chosen.variable] = x;
        return evalFloat(node, environment);
      }, lower, upper, 1e-9, 40);
      if (!isFinite(value)) {
        bad("the definite integral is not finite on [" + formatNumber(lower, 12) + ", " + formatNumber(upper, 12) + "]: the integrand may be singular there");
      }
      body = "≈ " + formatNumber(value, context.decimalPlaces) + "  (numeric: adaptive Simpson)";
    }
    return {
      header: "integrate: d" + chosen.variable + " from " + formatNumber(lower, 12) + " to " + formatNumber(upper, 12),
      body: body,
      notes: notes,
    };
  }

  if (operation === "solve") {
    const solveDegree = highestPower(node, chosen.variable);
    if (solveDegree !== null && solveDegree > context.maxDegree) {
      // Above the configured degree the exact path is not attempted, but the
      // roots a numeric scan can find are still reported, with the cap stated:
      // refusing outright lost the real root of x^63-1 and made a documented
      // operation unavailable.
      notes.push("the equation is degree " + solveDegree + ", above the maxDegree setting of " + context.maxDegree +
        ", so no exact factorisation was attempted; the roots below come from a numeric scan.");
      const numeric = numericRoots(node, chosen.variable, context.range, notes);
      if (numeric.length === 0) {
        bad("no real root was found for " + chosen.variable + " over the search range, and degree " + solveDegree +
          " is above the maxDegree setting of " + context.maxDegree + "; raise maxDegree to attempt an exact solution");
      }
      return {
        header: "solve: " + chosen.variable,
        body: numeric.map(function (root) {
          return chosen.variable + " = " + formatNumber(root, context.decimalPlaces) + "  (numeric)";
        }).join("\n"),
        notes: notes,
      };
    }
    const names = symbolicNames(node);
    if (names.length > 1) {
      bad("solve needs one variable, but the expression has " + names.length + " (" + names.join(", ") + "); systems of equations are not supported");
    }
    const simplified = simplifyDeep(node, context, "simplify");
    const result = solveNode(simplified, chosen.variable, context, notes);
    if (!result.complete || result.lines.length === 0) {
      bad("no solution was found for " + chosen.variable + " over the search range [-" + context.range + ", " + context.range +
        "]; solve covers linear and quadratic equations exactly, higher degrees through their rational roots, and numeric roots otherwise");
    }
    return {
      header: "solve: " + chosen.variable + (equation !== null ? " (equation set to zero)" : " (expression set to zero)"),
      body: result.lines.join("\n"),
      notes: notes,
    };
  }

  if (operation === "evaluate" || operation === "substitute") {
    if (operation === "substitute" && Object.keys(values.symbolic).length === 0) {
      bad("substitute needs values, for example \"values\": {\"" + chosen.variable + "\": \"y + 1\"}");
    }
    const substituted = substituteNode(node, values.symbolic);
    const bindings = describeBindings(values);
    if (operation === "substitute") {
      const result = simplifyDeep(substituted, context, "simplify");
      return {
        header: "substitute: " + bindings,
        body: "= " + render(result, context),
        notes: notes,
      };
    }
    const remaining = symbolicNames(substituted);
    if (remaining.length > 0) {
      bad("evaluate: " + remaining.join(", ") + " " + plural(remaining.length, "is", "are") + " still unbound; pass a value for every variable");
    }
    const header = bindings === "" ? "evaluate" : "evaluate: " + bindings;
    const exact = evalExact(substituted, values.bound);
    if (exact !== null) {
      let text = ratToText(exact);
      if (exact.d !== 1n) text += " (" + ratToDecimal(exact, context.decimalPlaces) + ")";
      return { header: header, body: "= " + text + "  (exact)", notes: notes };
    }
    const numeric = evalFloat(substituted, {});
    if (!isFinite(numeric)) bad("evaluate: the value is not a finite number (a domain error, or a division by zero)");
    return {
      header: header,
      body: "≈ " + formatNumber(numeric, context.decimalPlaces) + "  (numeric: this expression has no exact rational value)",
      notes: notes,
    };
  }

  bad("unknown operation \"" + operation + "\"");
  return undefined;
}
