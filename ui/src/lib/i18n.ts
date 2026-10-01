import { derived, get } from "svelte/store";
import { persistentStore } from "../stores/persistent";
import en from "../locales/en.json";
import zhCN from "../locales/zh-CN.json";
import zhTW from "../locales/zh-TW.json";

export const supportedLocales = ["en", "zh-CN", "zh-TW"] as const;
export type Locale = (typeof supportedLocales)[number];
export type MessageParams = Record<string, string | number>;
export type Translate = (key: string, params?: MessageParams) => string;

export const localeNames: Record<Locale, string> = {
  en: "English",
  "zh-CN": "简体中文",
  "zh-TW": "繁體中文"
};

type Catalog = Record<string, unknown>;

const catalogs: Record<Locale, Catalog> = {
  en: en as Catalog,
  "zh-CN": zhCN as Catalog,
  "zh-TW": zhTW as Catalog
};

function isLocale(value: unknown): value is Locale {
  return typeof value === "string" && (supportedLocales as readonly string[]).includes(value);
}

function detectLocale(): Locale {
  if (typeof navigator === "undefined") return "en";

  const candidates = [navigator.language, ...(navigator.languages ?? [])];
  for (const candidate of candidates) {
    if (/^zh-(tw|hk|mo|hant)/i.test(candidate)) return "zh-TW";
    if (/^zh/i.test(candidate)) return "zh-CN";
  }
  return "en";
}

export const locale = persistentStore<Locale>("locale", detectLocale());

export function setLocale(next: Locale): void {
  if (isLocale(next)) locale.set(next);
}

export function localeToIntl(value: Locale): string {
  return value === "en" ? "en-US" : value;
}

function resolve(catalog: Catalog, key: string): string | undefined {
  let current: unknown = catalog;
  for (const segment of key.split(".")) {
    if (!current || typeof current !== "object" || !(segment in current)) return undefined;
    current = (current as Record<string, unknown>)[segment];
  }
  return typeof current === "string" ? current : undefined;
}

function interpolate(message: string, params?: MessageParams): string {
  if (!params) return message;
  return message.replace(/\{(\w+)\}/g, (match, name: string) => {
    const value = params[name];
    return value === undefined ? match : String(value);
  });
}

export function translateFor(value: Locale, key: string, params?: MessageParams): string {
  const message = resolve(catalogs[value], key) ?? resolve(catalogs.en, key) ?? key;
  return interpolate(message, params);
}

export function t(key: string, params?: MessageParams): string {
  const current = get(locale);
  return translateFor(isLocale(current) ? current : "en", key, params);
}

export const translate = derived< typeof locale, Translate>(locale, (current) => {
  const active = isLocale(current) ? current : "en";
  return (key: string, params?: MessageParams) => translateFor(active, key, params);
});

