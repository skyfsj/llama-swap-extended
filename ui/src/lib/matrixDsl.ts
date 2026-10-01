// Graphical editing support for the swap matrix. The DSL is the storage format;
// this module converts between it and a table of "combinations", the form a
// graphic editor can drive. A combination is one set of models that may run
// together, and a set is the OR of its combinations: "a & b | c" means the two
// combinations {a, b} and {c}.
//
// The conversion is exact for the leaf/AND/OR subset the table covers. AND is
// distributed over OR so "(a | b) & c" becomes the two combinations {a, c} and
// {b, c} — the same sets, written flat. Expressions that reference another set
// (+name) or that the parser cannot read are not representable, and callers
// must keep the DSL editor open for them instead of silently dropping meaning.

export interface MatrixCombination {
  members: string[];
}

export interface MatrixSetDraft {
  name: string;
  /** The edited form. Empty means the operator has not entered a combination. */
  combinations: MatrixCombination[];
  /** The DSL the table was read from, when it is not representable as a table. */
  rawDSL: string;
  /** True when rawDSL must be used because the table cannot express the DSL. */
  unrepresentable: boolean;
}

export interface MatrixVarsDraft {
  name: string;
  model: string;
}

export interface MatrixEvictCostDraft {
  key: string;
  cost: string;
}

export interface MatrixDraft {
  vars: MatrixVarsDraft[];
  evictCosts: MatrixEvictCostDraft[];
  sets: MatrixSetDraft[];
}

export interface MatrixConfig {
  vars?: Record<string, string>;
  evict_costs?: Record<string, number>;
  sets?: Record<string, string>;
}

type Node =
  | { kind: "leaf"; name: string }
  | { kind: "and" | "or"; children: Node[] }
  | { kind: "ref"; name: string };

// tokenize splits a DSL expression into its tokens. A leading '+' marks a
// reference to another set and is kept as part of the token so the parser can
// tell it from a model leaf.
function tokenize(input: string): string[] {
  const tokens: string[] = [];
  let current = "";
  let reference = false;
  const flush = () => {
    if (current === "") return;
    tokens.push(reference ? `+${current}` : current);
    current = "";
    reference = false;
  };
  for (const char of input) {
    if (char === "+") {
      flush();
      reference = true;
      continue;
    }
    if (char === "&" || char === "|" || char === "(" || char === ")") {
      flush();
      tokens.push(char);
      continue;
    }
    if (/\s/.test(char)) {
      flush();
      continue;
    }
    current += char;
  }
  flush();
  return tokens;
}

// parse reads the flat OR/AND grammar. AND binds tighter than OR, which matches
// the engine's parser, so "a & b | c" is (a & b) | c.
function parse(tokens: string[]): Node | null {
  let index = 0;

  function peek(): string | undefined {
    return tokens[index];
  }

  function parseOr(): Node | null {
    const children: Node[] = [];
    for (;;) {
      const child = parseAnd();
      if (child === null) return null;
      children.push(child);
      if (peek() === "|") {
        index += 1;
        continue;
      }
      break;
    }
    return children.length === 1 ? children[0] : { kind: "or", children };
  }

  function parseAnd(): Node | null {
    const children: Node[] = [];
    for (;;) {
      const child = parseAtom();
      if (child === null) return null;
      children.push(child);
      if (peek() === "&") {
        index += 1;
        continue;
      }
      break;
    }
    return children.length === 1 ? children[0] : { kind: "and", children };
  }

  function parseAtom(): Node | null {
    const token = peek();
    if (token === undefined) return null;
    if (token === "(") {
      index += 1;
      const inner = parseOr();
      if (inner === null || peek() !== ")") return null;
      index += 1;
      return inner;
    }
    if (token === ")" || token === "&" || token === "|") return null;
    if (token.startsWith("+")) return { kind: "ref", name: token.slice(1) };
    index += 1;
    return { kind: "leaf", name: token };
  }

  const node = parseOr();
  if (node === null || index !== tokens.length) return null;
  return node;
}

// toDNF distributes AND over OR into the list of combinations a table row
// represents. A reference to another set is not distributed: the reference keeps
// its meaning, and inlining it would freeze the referenced set's combinations.
function toDNF(node: Node): MatrixCombination[] | null {
  if (node.kind === "leaf") return [{ members: [node.name] }];
  if (node.kind === "ref") return null;
  if (node.kind === "or") {
    const rows: MatrixCombination[] = [];
    for (const child of node.children) {
      const expanded = toDNF(child);
      if (expanded === null) return null;
      rows.push(...expanded);
    }
    return rows;
  }
  let rows: MatrixCombination[] = [{ members: [] }];
  for (const child of node.children) {
    const expanded = toDNF(child);
    if (expanded === null) return null;
    const next: MatrixCombination[] = [];
    for (const left of rows) {
      for (const right of expanded) {
        const members = [...left.members];
        for (const member of right.members) {
          if (!members.includes(member)) members.push(member);
        }
        next.push({ members });
      }
    }
    rows = next;
  }
  return rows;
}

// combinationsToDsl renders the table back into the DSL: every row is an AND of
// its members, and the rows are alternatives.
export function combinationsToDsl(combinations: MatrixCombination[]): string {
  const parts = combinations
    .filter((combination) => combination.members.length > 0)
    .map((combination) => {
      const joined = combination.members.join(" & ");
      return combination.members.length > 1 ? `(${joined})` : joined;
    });
  return parts.join(" | ");
}

// dslToCombinations parses a DSL expression into table rows, or returns null
// when the expression cannot be shown as a table.
export function dslToCombinations(dsl: string): MatrixCombination[] | null {
  const trimmed = dsl.trim();
  if (trimmed === "") return [];
  const node = parse(tokenize(trimmed));
  if (node === null) return null;
  const rows = toDNF(node);
  if (rows === null) return null;
  return rows;
}

// matrixFromConfig reads a matrix configuration block into the editor draft.
// Both the canonical routing tree and the legacy top-level key are accepted.
export function matrixFromConfig(value: unknown): MatrixDraft {
  const matrix = (value && typeof value === "object" && !Array.isArray(value) ? value : {}) as MatrixConfig;
  const vars = Object.entries(matrix.vars ?? {}).map(([name, model]) => ({ name, model }));
  const evictCosts = Object.entries(matrix.evict_costs ?? {}).map(([key, cost]) => ({ key, cost: String(cost) }));
  const sets = Object.entries(matrix.sets ?? {}).map(([name, dsl]) => {
    const combinations = dslToCombinations(String(dsl));
    return combinations === null
      ? { name, combinations: [], rawDSL: String(dsl), unrepresentable: true }
      : { name, combinations, rawDSL: String(dsl), unrepresentable: false };
  });
  return { vars, evictCosts, sets };
}

// matrixToConfig renders the draft back into the configuration block. Vars whose
// name or model is empty are dropped, a evict cost must be a positive integer,
// and a set with no combination is dropped so the matrix never stores an
// expression that matches nothing.
export function matrixToConfig(draft: MatrixDraft): MatrixConfig {
  const vars: Record<string, string> = {};
  for (const entry of draft.vars) {
    const name = entry.name.trim();
    const model = entry.model.trim();
    if (name === "" || model === "") continue;
    vars[name] = model;
  }
  const evictCosts: Record<string, number> = {};
  for (const entry of draft.evictCosts) {
    const key = entry.key.trim();
    const cost = Number.parseInt(entry.cost.trim(), 10);
    if (key === "" || !Number.isInteger(cost) || cost < 1) continue;
    evictCosts[key] = cost;
  }
  const sets: Record<string, string> = {};
  for (const entry of draft.sets) {
    const name = entry.name.trim();
    if (name === "") continue;
    const dsl = entry.unrepresentable ? entry.rawDSL.trim() : combinationsToDsl(entry.combinations);
    if (dsl === "") continue;
    sets[name] = dsl;
  }
  const config: MatrixConfig = {};
  if (Object.keys(vars).length > 0) config.vars = vars;
  if (Object.keys(evictCosts).length > 0) config.evict_costs = evictCosts;
  if (Object.keys(sets).length > 0) config.sets = sets;
  return config;
}

// matrixIdentifiers lists every name a set expression may use: the vars plus
// the configured model IDs.
export function matrixIdentifiers(draft: MatrixDraft, modelOptions: string[]): string[] {
  const names = [
    ...draft.vars.map((entry) => entry.name.trim()).filter((name) => name !== ""),
    ...modelOptions,
  ];
  return [...new Set(names)];
}
