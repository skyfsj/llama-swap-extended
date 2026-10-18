// Expression parser shared by the calculator and the symbolic engine.
//
// One grammar, one AST, two evaluators. Every node carries `at`, the 1-based
// column where it started, because an error the model cannot locate in its own
// input is an error it cannot fix.
//
// Grammar (lowest precedence first):
//   expression := term (('+' | '-') term)*
//   term       := unary (('*' | '/' | '%') unary | unary)*     // the bare unary is implicit multiplication
//   unary      := ('+' | '-') unary | power
//   power      := postfix (('^' | '**') unary)?                // right-associative
//   postfix    := primary '!'*
//   primary    := number | '(' expression ')' | name [ '(' arguments ')' ]
//
// `^` sits above unary minus, so -2^2 is -4 while 2^3^2 is 512.

import { biGcd } from "./bignum.js";

function isDigit(ch) {
  return ch >= "0" && ch <= "9";
}

function isNameStart(ch) {
  return (ch >= "a" && ch <= "z") || (ch >= "A" && ch <= "Z") || ch === "_";
}

function isNamePart(ch) {
  return isNameStart(ch) || isDigit(ch);
}

// numberNode keeps a literal as an exact rational: 2 is 2/1, 2.0 reduces to 2/1
// (so 2.0^100 is computed exactly), and 0.5 stays 1/2, which is what makes the
// calculator fall back to float64 for a non-integer operand.
function numberNode(text, column) {
  const match = /^(\d*)(?:\.(\d*))?(?:[eE]([+-]?\d+))?$/.exec(text);
  const whole = match[1] || "";
  const fraction = match[2] || "";
  const exponent = match[3] ? parseInt(match[3], 10) : 0;
  const digits = (whole + fraction) || "0";
  const scale = exponent - fraction.length;
  // A literal longer than this stays exact in principle but would cost more than
  // it is worth in a one-second budget; it is flagged so callers fall back to a
  // float rather than pretending.
  const approx = digits.replace(/^0+/, "").length > 60 || Math.abs(scale) > 300;
  let n = BigInt(digits);
  let d = 1n;
  if (scale > 0) n *= 10n ** BigInt(Math.min(scale, 400));
  else if (scale < 0) d = 10n ** BigInt(Math.min(-scale, 400));
  const divisor = biGcd(n, d);
  if (divisor > 1n) {
    n /= divisor;
    d /= divisor;
  }
  return { k: "num", n: n, d: d, approx: approx, at: column };
}

// tokenize turns source text into tokens. `sep` marks a statement boundary
// (`;` or a newline), which the calculator's program loop splits on.
export function tokenize(source) {
  const tokens = [];
  let index = 0;
  while (index < source.length) {
    const ch = source.charAt(index);
    const column = index + 1;
    if (ch === " " || ch === "\t" || ch === "\r") {
      index += 1;
      continue;
    }
    if (ch === "\n" || ch === ";") {
      tokens.push({ kind: "sep", text: ch, column: column });
      index += 1;
      continue;
    }
    if (isDigit(ch) || (ch === "." && isDigit(source.charAt(index + 1)))) {
      let text = "";
      while (index < source.length && isDigit(source.charAt(index))) {
        text += source.charAt(index);
        index += 1;
      }
      if (source.charAt(index) === ".") {
        text += ".";
        index += 1;
        while (index < source.length && isDigit(source.charAt(index))) {
          text += source.charAt(index);
          index += 1;
        }
      }
      if (source.charAt(index) === "e" || source.charAt(index) === "E") {
        let cursor = index + 1;
        if (source.charAt(cursor) === "+" || source.charAt(cursor) === "-") cursor += 1;
        if (isDigit(source.charAt(cursor))) {
          text += source.slice(index, cursor);
          index = cursor;
          while (index < source.length && isDigit(source.charAt(index))) {
            text += source.charAt(index);
            index += 1;
          }
        }
      }
      tokens.push({ kind: "number", text: text, column: column });
      continue;
    }
    if (isNameStart(ch)) {
      let text = "";
      while (index < source.length && isNamePart(source.charAt(index))) {
        text += source.charAt(index);
        index += 1;
      }
      tokens.push({ kind: "name", text: text, column: column });
      continue;
    }
    const pair = source.substr(index, 2);
    if (pair === "**") {
      tokens.push({ kind: "op", text: "**", column: column });
      index += 2;
      continue;
    }
    if ("+-*/%^!(),=".indexOf(ch) >= 0) {
      tokens.push({ kind: "op", text: ch, column: column });
      index += 1;
      continue;
    }
    throw new Error("unexpected character " + JSON.stringify(ch) + " at column " + column);
  }
  return tokens;
}

// splitStatements cuts a token list at top-level separators, which is how a
// multi-statement program is read. Parentheses hide a separator, so a future
// function with a multi-argument form stays intact.
export function splitStatements(tokens) {
  const statements = [];
  let current = [];
  let depth = 0;
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token.kind === "op" && token.text === "(") depth += 1;
    if (token.kind === "op" && token.text === ")") depth -= 1;
    if (token.kind === "sep" && depth <= 0) {
      if (current.length > 0) statements.push(current);
      current = [];
      continue;
    }
    current.push(token);
  }
  if (current.length > 0) statements.push(current);
  return statements;
}

function parseList(tokens, endColumn) {
  let index = 0;
  const peek = function () {
    return tokens[index];
  };
  const next = function () {
    const token = tokens[index];
    index += 1;
    return token;
  };
  const isOp = function (text) {
    const token = tokens[index];
    return token !== undefined && token.kind === "op" && token.text === text;
  };
  const fail = function (message, token) {
    const column = token === undefined ? endColumn : token.column;
    throw new Error(message + " at column " + column);
  };
  const describe = function (token) {
    if (token === undefined) return "end of expression";
    if (token.kind === "number") return JSON.stringify(token.text);
    if (token.kind === "name") return JSON.stringify(token.text);
    return JSON.stringify(token.text);
  };

  const parseExpression = function () {
    let node = parseTerm();
    while (isOp("+") || isOp("-")) {
      const operator = next();
      const right = parseTerm();
      node = operator.text === "+"
        ? { k: "add", a: node, b: right, at: node.at }
        : { k: "sub", a: node, b: right, at: node.at };
    }
    return node;
  };

  const startsOperand = function () {
    const token = peek();
    if (token === undefined) return false;
    return token.kind === "number" || token.kind === "name" || (token.kind === "op" && token.text === "(");
  };

  const parseTerm = function () {
    let node = parseUnary();
    for (;;) {
      if (isOp("*") || isOp("/") || isOp("%")) {
        const operator = next();
        const right = parseUnary();
        const kind = operator.text === "*" ? "mul" : (operator.text === "/" ? "div" : "mod");
        node = { k: kind, a: node, b: right, at: node.at };
        continue;
      }
      // Implicit multiplication: 2pi, 3(4+5), 2sin(x), (a)(b).
      if (startsOperand()) {
        const right = parseUnary();
        node = { k: "mul", a: node, b: right, at: node.at };
        continue;
      }
      return node;
    }
  };

  const parseUnary = function () {
    if (isOp("+")) {
      next();
      return parseUnary();
    }
    if (isOp("-")) {
      const token = next();
      const operand = parseUnary();
      return { k: "neg", a: operand, at: token.column };
    }
    return parsePower();
  };

  const parsePower = function () {
    const base = parsePostfix();
    if (isOp("^") || isOp("**")) {
      next();
      const exponent = parseUnary();
      return { k: "pow", a: base, b: exponent, at: base.at };
    }
    return base;
  };

  const parsePostfix = function () {
    let node = parsePrimary();
    while (isOp("!")) {
      next();
      node = { k: "fact", a: node, at: node.at };
    }
    return node;
  };

  const parsePrimary = function () {
    const token = peek();
    if (token === undefined) {
      fail("unexpected end of expression", token);
    }
    if (token.kind === "number") {
      next();
      return numberNode(token.text, token.column);
    }
    if (isOp("(")) {
      next();
      const inner = parseExpression();
      if (!isOp(")")) fail("unexpected " + describe(peek()) + ", expected \")\"", peek());
      next();
      return inner;
    }
    if (token.kind === "name") {
      next();
      if (isOp("(")) {
        next();
        const args = [];
        if (!isOp(")")) {
          args.push(parseExpression());
          while (isOp(",")) {
            next();
            args.push(parseExpression());
          }
        }
        if (!isOp(")")) fail("unexpected " + describe(peek()) + ", expected \")\"", peek());
        next();
        return { k: "call", name: token.text, args: args, at: token.column };
      }
      return { k: "sym", name: token.text, at: token.column };
    }
    fail("unexpected " + describe(token), token);
    return undefined;
  };

  const node = parseExpression();
  if (index < tokens.length) {
    const leftover = tokens[index];
    fail("unexpected " + describe(leftover), leftover);
  }
  return node;
}

// parseExpression parses one complete expression. endColumn is where "end of
// expression" errors point, which is the end of the caller's own text.
export function parseExpression(tokens, endColumn) {
  if (tokens.length === 0) {
    throw new Error("unexpected end of expression at column " + (endColumn || 1));
  }
  return parseList(tokens, endColumn || 1);
}

// parseAssignment reads `name = expression`. It returns null when the tokens are
// not an assignment, which lets the calculator fall through to a plain
// expression and the symbolic engine treat a top-level `=` as an equation.
export function parseAssignment(tokens) {
  if (tokens.length >= 3 && tokens[0].kind === "name" && tokens[1].kind === "op" && tokens[1].text === "=") {
    return { name: tokens[0].text, body: tokens.slice(2), at: tokens[0].column };
  }
  if (tokens.length >= 2 && tokens[0].kind === "op" && tokens[0].text === "=") {
    throw new Error("unexpected \"=\" at column " + tokens[0].column + ": a statement is name = expression or an expression");
  }
  return null;
}

// splitEquation cuts `lhs = rhs` at the single top-level `=`. A second one is an
// error, because "a = b = c" has no meaning here.
export function splitEquation(tokens) {
  let depth = 0;
  let at = -1;
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token.kind === "op" && token.text === "(") depth += 1;
    else if (token.kind === "op" && token.text === ")") depth -= 1;
    else if (token.kind === "op" && token.text === "=" && depth === 0) {
      if (at >= 0) throw new Error("two \"=\" signs at column " + token.column + ": use one equation, or one expression equal to zero");
      at = index;
    }
  }
  if (at < 0) return null;
  if (at === 0 || at === tokens.length - 1) {
    throw new Error("one side of \"=\" is empty at column " + tokens[at].column);
  }
  return { left: tokens.slice(0, at), right: tokens.slice(at + 1) };
}

// ---- canonical text ---------------------------------------------------------

function wrap(text, needed) {
  return needed ? "(" + text + ")" : text;
}

// nodeText renders a node as canonical text: explicit `*`, `^` for powers, and
// parentheses only where the precedence requires them. The symbolic engine uses
// it as its node identity, so two equal expressions must render identically.
export function nodeText(node) {
  if (node === undefined || node === null) return "";
  switch (node.k) {
    case "num":
      return node.d === 1n ? node.n.toString() : node.n.toString() + "/" + node.d.toString();
    case "sym":
      return node.name;
    case "add":
      // Addition is associative, so nested sums need no parentheses.
      return nodeText(node.a) + " + " + nodeText(node.b);
    case "sub": {
      // A left-nested subtraction needs no parentheses: (a - b) - c == a - b - c.
      const right = nodeText(node.b);
      const negated = node.b.k === "add" || node.b.k === "sub" ? "(" + right + ")" : right;
      return nodeText(node.a) + " - " + negated;
    }
    case "neg":
      return "-" + wrap(nodeText(node.a), node.a.k === "num" || node.a.k === "sym" || node.a.k === "fnum");
    case "mul": {
      const left = wrap(nodeText(node.a), node.a.k === "add" || node.a.k === "sub");
      const right = wrap(nodeText(node.b), node.b.k === "add" || node.b.k === "sub");
      return left + "*" + right;
    }
    case "div":
      return wrap(nodeText(node.a), node.a.k === "add" || node.a.k === "sub") +
        "/" + wrap(nodeText(node.b), node.b.k === "add" || node.b.k === "sub" || node.b.k === "mul" || node.b.k === "div");
    case "mod":
      return nodeText(node.a) + " % " + nodeText(node.b);
    case "pow":
      return wrap(nodeText(node.a), node.a.k !== "sym" && node.a.k !== "call" && node.a.k !== "pow") +
        "^" + wrap(nodeText(node.b), node.b.k === "mul" || node.b.k === "div" || node.b.k === "add" ||
          node.b.k === "sub" || (node.b.k === "num" && node.b.d !== 1n));
    case "fact":
      return wrap(nodeText(node.a), node.a.k !== "sym" && node.a.k !== "num") + "!";
    case "call":
      return node.name + "(" + node.args.map(nodeText).join(", ") + ")";
    default:
      return "?";
  }
}

// collectSymbols lists the free variable names of a node, in first-seen order.
export function collectSymbols(node, into) {
  const names = into || [];
  const visit = function (current) {
    if (current === undefined || current === null) return;
    switch (current.k) {
      case "sym":
        if (names.indexOf(current.name) < 0) names.push(current.name);
        return;
      case "num":
        return;
      case "call":
        for (let index = 0; index < current.args.length; index += 1) visit(current.args[index]);
        return;
      case "add":
      case "sub":
      case "mul":
      case "div":
      case "mod":
      case "pow":
        visit(current.a);
        visit(current.b);
        return;
      case "neg":
      case "fact":
        visit(current.a);
        return;
      default:
        return;
    }
  };
  visit(node);
  return names;
}

// nodeKey is a canonical identity string: same key means same expression.
export function nodeKey(node) {
  return nodeText(node).replace(/\s+/g, "");
}
