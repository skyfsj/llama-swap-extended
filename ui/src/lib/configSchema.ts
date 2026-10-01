import schemaDocument from "../../../config-schema.json";

export type SchemaNode = boolean | Record<string, unknown>;
export type LabelTranslator = (key: string) => string;

const rootSchema = schemaDocument as unknown as Record<string, unknown>;

export function resolveSchema(schema: SchemaNode | undefined): Record<string, unknown> {
  if (schema === undefined || schema === true) return {};
  if (schema === false) return { type: "never" };
  const reference = typeof schema.$ref === "string" ? schema.$ref : "";
  let resolved = schema;
  if (reference.startsWith("#/")) {
    let current: unknown = rootSchema;
    for (const token of reference.slice(2).split("/")) {
      if (current === null || typeof current !== "object") return schema;
      current = (current as Record<string, unknown>)[token.replaceAll("~1", "/").replaceAll("~0", "~")];
    }
    if (current !== null && typeof current === "object" && !Array.isArray(current)) {
      resolved = { ...(current as Record<string, unknown>), ...schema, $ref: undefined };
    }
  }
  return mergeVariants(resolved);
}

function mergeVariants(schema: Record<string, unknown>): Record<string, unknown> {
  let merged = { ...schema };
  for (const keyword of ["allOf", "oneOf", "anyOf"] as const) {
    const variants = Array.isArray(schema[keyword]) ? schema[keyword] as SchemaNode[] : [];
    for (const variant of variants) {
      const resolved = resolveSchema(variant);
      merged = {
        ...resolved,
        ...merged,
        properties: {
          ...((resolved.properties as Record<string, unknown> | undefined) ?? {}),
          ...((merged.properties as Record<string, unknown> | undefined) ?? {}),
        },
      };
    }
  }
  if (merged.const !== undefined && merged.enum === undefined) merged.enum = [merged.const];
  return merged;
}

export function topLevelSchema(key: string): SchemaNode {
  const properties = rootSchema.properties as Record<string, SchemaNode> | undefined;
  return properties?.[key] ?? true;
}

export function childSchema(schema: SchemaNode | undefined, key: string): SchemaNode {
  const resolved = resolveSchema(schema);
  const properties = resolved.properties as Record<string, SchemaNode> | undefined;
  if (properties?.[key] !== undefined) return properties[key];
  const additional = resolved.additionalProperties;
  return additional === false ? false : (additional as SchemaNode | undefined) ?? true;
}

export function modelFieldSchema(key: string): SchemaNode {
  const models = resolveSchema(topLevelSchema("models"));
  const model = resolveSchema(models.additionalProperties as SchemaNode | undefined);
  return childSchema(model, key);
}

export function schemaFieldLabel(key: string, translate: LabelTranslator): string {
  const translationKey = `controlPlane.schemaFieldLabels.${key}`;
  const translated = translate(translationKey);
  if (translated !== translationKey) return translated;
  return key
    .replaceAll("_", " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/^./, (character) => character.toUpperCase());
}

export function defaultForSchema(schema: SchemaNode | undefined): unknown {
  const resolved = resolveSchema(schema);
  const type = resolved.type;
  if (type === "object" || resolved.properties || resolved.additionalProperties) {
    const initial = resolved.default;
    const value: Record<string, unknown> = initial !== null && typeof initial === "object" && !Array.isArray(initial)
      ? structuredClone(initial as Record<string, unknown>)
      : {};
    const properties = (resolved.properties as Record<string, SchemaNode> | undefined) ?? {};
    const required = Array.isArray(resolved.required)
      ? resolved.required.filter((key): key is string => typeof key === "string")
      : [];
    for (const key of required) {
      if (properties[key] !== undefined) value[key] = defaultForSchema(properties[key]);
    }
    for (const [key, property] of Object.entries(properties)) {
      if (Object.prototype.hasOwnProperty.call(value, key)) continue;
      if (resolveSchema(property).default !== undefined) value[key] = defaultForSchema(property);
    }
    return value;
  }
  if (resolved.default !== undefined) return structuredClone(resolved.default);
  if (type === "array" || resolved.items) return [];
  if (type === "boolean") return false;
  if (type === "integer" || type === "number") return 0;
  return "";
}
