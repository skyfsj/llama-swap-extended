// Math Tools - offline arithmetic and algebra for the model.
//
// calculator keeps integers exact: an operation whose operands are all integers
// is computed with BigInt, so 2^200 and 100! come back digit for digit instead of
// as a rounded float64 (which carries about 15.95 significant digits). Anything
// that leaves the integers is printed with an approximation sign, because more
// digits would be a lie.
//
// symbolic_math is a small CAS with a documented rule set: it names the rule it
// lacks instead of returning a wrong answer. See lib/symbolic.js.
//
// Both tools are pure computation: the manifest declares no permissions, so this
// extension cannot reach the network or the file system.

import { ANGLE_UNITS, angleNote, functionNames, runProgram } from "./lib/evaluate.js";
import { OPERATIONS, runSymbolic } from "./lib/symbolic.js";

const FORMAT_TEXT = "text";
const FORMAT_LATEX = "latex";
const RADIANS = "rad";
const DEGREES = "deg";
const GRADIANS = "grad";

// An expression longer than this is not a question, it is a paste; the parser
// and the exact arithmetic both degrade on it.
const MAX_EXPRESSION_CHARS = 4000;

export const settings = {
  angleUnit: {
    type: "select",
    label: "Angle unit",
    hint: "Unit for sin/cos/tan arguments and asin/acos/atan results. Radians matches mathematics; degrees is what a model usually means when it writes sin(90).",
    options: [RADIANS, DEGREES, GRADIANS],
    default: RADIANS,
    section: "Calculator"
  },
  precision: {
    type: "number",
    label: "Significant digits",
    hint: "How many significant digits a float result is rounded to. 12 keeps a double's useful digits without printing its noise; raise it to 17 to see the full float64.",
    default: 12,
    min: 1,
    max: 17,
    section: "Calculator"
  },
  exactIntegers: {
    type: "boolean",
    label: "Exact integers",
    hint: "Compute integer arithmetic with arbitrary-precision integers (2^200 exactly) instead of float64. Turn it off only to reproduce a float calculator's rounding.",
    default: true,
    section: "Calculator"
  },
  maxIntegerDigits: {
    type: "number",
    label: "Exact integer digit limit",
    hint: "Largest exact integer this tool will print. A result over the limit is reported as a float with an approximation sign and the digit count, never as a truncated exact number.",
    default: 5000,
    min: 1,
    max: 100000,
    section: "Calculator"
  },
  format: {
    type: "select",
    label: "Symbolic output format",
    hint: "text uses ^ and an explicit *, for example x^2 + 2*x; latex returns a formula for a document. A tool call may ask for the other one.",
    options: [FORMAT_TEXT, FORMAT_LATEX],
    default: FORMAT_TEXT,
    section: "Symbolic math"
  },
  maxDegree: {
    type: "number",
    label: "Max degree",
    hint: "Largest polynomial degree that expand, factor, solve and the derivative order will handle. Raising it raises the time an answer can take.",
    default: 8,
    min: 1,
    max: 20,
    section: "Symbolic math"
  },
  decimalPlaces: {
    type: "number",
    label: "Decimal places",
    hint: "Digits after the point for results that have no exact form (numeric roots, a rational rendered as a decimal).",
    default: 6,
    min: 1,
    max: 15,
    section: "Symbolic math"
  },
  numericSearchRange: {
    type: "number",
    label: "Numeric root search range",
    hint: "solve scans [-range, +range] for a sign change when a polynomial root cannot be found exactly. A root outside this window is not found; widen it for a large root.",
    default: 20,
    min: 1,
    max: 1000,
    section: "Symbolic math"
  }
};

function numberSetting(value, fallback, low, high) {
  const parsed = Number(value);
  if (!isFinite(parsed)) return fallback;
  const floored = Math.floor(parsed);
  if (floored < low) return low;
  return floored > high ? high : floored;
}

function angleUnit(config, requested) {
  if (requested !== undefined && requested !== null) {
    const wanted = String(requested).trim().toLowerCase();
    if (ANGLE_UNITS.indexOf(wanted) < 0) {
      throw new Error("angles must be one of " + ANGLE_UNITS.join(", ") + " (got " + JSON.stringify(requested) + ")");
    }
    return { unit: wanted, overridden: true };
  }
  const configured = String(config.angleUnit === undefined ? RADIANS : config.angleUnit).trim().toLowerCase();
  return { unit: ANGLE_UNITS.indexOf(configured) >= 0 ? configured : RADIANS, overridden: false };
}

function expressionArgument(value) {
  const text = String(value === undefined || value === null ? "" : value);
  if (text.trim() === "") {
    throw new Error("expression must not be empty");
  }
  if (text.length > MAX_EXPRESSION_CHARS) {
    throw new Error("expression is " + text.length + " characters, above the " + MAX_EXPRESSION_CHARS + " character limit; split it into separate calls");
  }
  return text;
}

function runCalculator(ctx, args) {
  const angles = angleUnit(ctx.config, args.angles);
  const precision = numberSetting(
    args.precision === undefined ? ctx.config.precision : args.precision,
    12, 1, 17
  );
  const source = expressionArgument(args.expression);
  const result = runProgram(source, {
    angles: angles.unit,
    precision: precision,
    exactIntegers: ctx.config.exactIntegers !== false,
    maxIntegerDigits: numberSetting(ctx.config.maxIntegerDigits, 5000, 1, 100000),
    variables: args.variables,
  });
  const lines = [];
  const note = angleNote(angles.unit, angles.overridden);
  if (note !== "") lines.push(note);
  for (let index = 0; index < result.lines.length; index += 1) lines.push(result.lines[index]);
  for (let index = 0; index < result.notes.length; index += 1) lines.push("note: " + result.notes[index]);
  ctx.log.info("calculator: " + result.lines.length + " statement(s), angles " + angles.unit);
  return { content: lines.join("\n") };
}

function formatArgument(config, requested) {
  const wanted = String(requested === undefined || requested === null ? config.format : requested).trim().toLowerCase();
  if (wanted !== FORMAT_TEXT && wanted !== FORMAT_LATEX) {
    throw new Error("format must be \"" + FORMAT_TEXT + "\" or \"" + FORMAT_LATEX + "\" (got " + JSON.stringify(requested) + ")");
  }
  return wanted;
}

function runSymbolicMath(ctx, args) {
  const operation = String(args.operation === undefined || args.operation === null ? "" : args.operation).trim().toLowerCase();
  const expression = expressionArgument(args.expression);
  const variable = args.variable === undefined || args.variable === null ? "" : String(args.variable).trim();
  if (variable !== "" && !/^[A-Za-z_][A-Za-z0-9_]*$/.test(variable)) {
    throw new Error("variable must be a name (got " + JSON.stringify(args.variable) + ")");
  }
  const order = args.order === undefined || args.order === null ? 1 : Number(args.order);
  if (!isFinite(order)) throw new Error("order must be a number");
  const result = runSymbolic({
    operation: operation,
    expression: expression,
    variable: variable === "" ? undefined : variable,
    order: Math.floor(order),
    values: args.values,
    format: formatArgument(ctx.config, args.format),
    maxDegree: numberSetting(ctx.config.maxDegree, 8, 1, 20),
    decimalPlaces: numberSetting(ctx.config.decimalPlaces, 6, 1, 15),
    range: numberSetting(ctx.config.numericSearchRange, 20, 1, 1000),
    from: args.from === undefined || args.from === null ? undefined : Number(args.from),
    to: args.to === undefined || args.to === null ? undefined : Number(args.to),
  });
  const lines = [result.header, result.body];
  for (let index = 0; index < result.notes.length; index += 1) lines.push("note: " + result.notes[index]);
  ctx.log.info("symbolic_math: " + operation + " over " + expression.length + " characters");
  return { content: lines.join("\n") };
}

const CALCULATOR_FUNCTIONS = functionNames().filter(function (name) {
  return name !== "pow" && name !== "mod";
});

export default {
  tools: [
    {
      type: "function",
      execution: "server",
      function: {
        name: "calculator",
        description: "Evaluate an arithmetic expression or a short program of statements offline and return the value. Use it for any calculation that must be right: percentages, roots, logarithms, factorials, gcd and lcm, large powers, trigonometry. " +
          "Exactness: when every operand of an operation is an integer the result is computed with arbitrary-precision integers, so 2^200 and 100! come back digit for digit and are reported as an exact integer with its digit count; any result that is not exact is marked ≈ and printed with the configured significant digits (12 by default). " +
          "Syntax: numbers (1, 2.5, 1.5e-3), + - * / % (modulo), ^ or ** (power), unary +/-, postfix ! (factorial), parentheses, implicit multiplication (2pi, 3(4+5), 2sin(x)), constants pi e tau phi inf, and the functions " + CALCULATOR_FUNCTIONS.join(" ") + ". " +
          "Precedence: ^ and ** are right-associative and bind tighter than unary minus, so -2^2 = -4 and 2^3^2 = 512; % keeps the sign of the left operand, so -7 % 3 = -1; round() rounds a tie upward. " +
          "A program is statements separated by ; or a newline: name = expr defines a variable for the statements that follow, and the value of the last statement is the result. Variables do not survive the call, so pass them again in variables (or repeat the definitions). " +
          "Angles follow the angleUnit setting (radians by default); the angles argument overrides it for this call, for example {\"expression\": \"sin(x)\", \"variables\": {\"x\": 90}, \"angles\": \"deg\"} gives 1. " +
          "Cannot: complex numbers (symbolic_math solve returns those), unit or currency conversion (install the unit-convert preset), matrices, and symbolic manipulation (use the symbolic_math tool from this same preset).",
        parameters: {
          type: "object",
          properties: {
            expression: {
              type: "string",
              description: "The expression to evaluate, or a program of statements separated by ; or newlines. Example: \"2^200\" or \"r = 7; 3.14159*r^2\"."
            },
            angles: {
              type: "string",
              enum: ANGLE_UNITS,
              description: "Angle unit for this call: rad, deg or grad. Overrides the angleUnit setting."
            },
            precision: {
              type: "number",
              description: "Significant digits for a float result (1-17). Overrides the precision setting for this call."
            },
            variables: {
              type: "object",
              description: "Numbers to bind before the program runs, for example {\"x\": 90}. Bindings made by the program do not survive the call."
            }
          },
          required: ["expression"]
        }
      }
    },
    {
      type: "function",
      execution: "server",
      function: {
        name: "symbolic_math",
        description: "Simplify, expand, factor, differentiate, integrate or solve an expression or equation in one variable, exactly where an exact answer exists. Use it to check algebra, find roots, or rewrite an equation into a readable form. " +
          "Operations: simplify (constant folding, x+0, x*1, x^0, like terms, x*(y+z) kept as written); expand (distribute products and integer powers); factor (common factor, difference of squares, rational quadratics, pair grouping of four terms, and a clear statement when it cannot factor further); derivative (power, product, quotient and chain rules for sqrt cbrt exp ln log sin cos tan asin acos atan sinh cosh tanh asinh acosh atanh abs, nth order via order); integrate (indefinite for powers, 1/x, k*x exponentials, sin/cos of a linear argument, sqrt, ln and constant factors, plus a definite integral over from..to evaluated exactly when an antiderivative exists and numerically otherwise); solve (linear and quadratic exactly, including complex roots when the discriminant is negative; higher degrees through their rational roots; anything else numerically over a sign-change scan of the configured range); evaluate (an exact rational value from values); substitute (rewrite with values and simplify, staying symbolic). " +
          "Write an equation as lhs = rhs; it is solved as lhs - rhs = 0, and an expression with no = is taken as equal to zero. variable defaults to the only free variable (else x). values maps a variable to a number or to an expression. order is the derivative order. from and to are numeric limits for a definite integral. format is text (^ and an explicit *, for example x^2 + 2*x) or latex. " +
          "Exactness: coefficients are exact rationals, so 1/3 stays 1/3 and evaluate of 1/4 + 1/6 prints 5/12 (0.416667). A result with no exact rational value is marked ≈ and rounded to the configured decimal places. Every answer says which operation ran and which variable it used. " +
          "Cannot: systems of equations, inequalities, limits, series, integrals outside the rule set above, polynomial degrees above maxDegree, and expressions with more than one free variable.",
        parameters: {
          type: "object",
          properties: {
            expression: {
              type: "string",
              description: "The expression or equation, for example \"x^2 - 5*x + 6\", \"x^2*sin(x)\" or \"2*x + 3 = 11\"."
            },
            operation: {
              type: "string",
              enum: OPERATIONS,
              description: "simplify, expand, factor, derivative, integrate, solve, evaluate or substitute."
            },
            variable: {
              type: "string",
              description: "The variable to work in, for example x. Defaults to the only free variable in the expression."
            },
            order: {
              type: "number",
              description: "Derivative order, at least 1 (default 1). Order n differentiates n times."
            },
            values: {
              type: "object",
              description: "Values for evaluate, substitute and derivative-free use, as numbers or expression strings, for example {\"x\": 2} or {\"x\": \"y + 1\"}."
            },
            format: {
              type: "string",
              enum: [FORMAT_TEXT, FORMAT_LATEX],
              description: "text (default) for plain formulas, latex for a typeset formula."
            },
            from: {
              type: "number",
              description: "Lower limit of a definite integral. Needs to with it; both are numbers, so write pi as 3.141592653589793."
            },
            to: {
              type: "number",
              description: "Upper limit of a definite integral."
            }
          },
          required: ["expression", "operation"]
        }
      }
    }
  ],

  async onToolCall(ctx, call) {
    if (call.name !== "calculator" && call.name !== "symbolic_math") return undefined;
    const args = call.arguments || {};
    try {
      if (call.name === "calculator") return runCalculator(ctx, args);
      return runSymbolicMath(ctx, args);
    } catch (error) {
      const message = error && error.message ? error.message : String(error);
      // The runtime reports a failed tool call with this text; naming the tool
      // and the position is what lets the model fix its own input.
      throw new Error(call.name + ": " + message);
    }
  }
};
