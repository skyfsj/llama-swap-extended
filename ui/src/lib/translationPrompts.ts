/**
 * HY-MT2 (Tencent Hunyuan) translation prompt presets and language table.
 *
 * Templates come from the official Hy-MT2 README
 * (https://github.com/Tencent-Hunyuan/Hy-MT2). Hy-MT2 has no default system
 * prompt: the instruction is a single user message. Per the README, both the
 * source and target language placeholders should use full language names, and
 * Chinese prompts should use Chinese language names while English prompts use
 * English ones — hence the zh/en template variants.
 *
 * Recommended inference params: temperature 0.7, top_p 0.6, top_k 20,
 * repetition_penalty 1.05, max_tokens 4096, max_context 8192.
 */

export interface TranslationLanguage {
  code: string;
  name: string;
  nameZh: string;
}

export const translationLanguages: TranslationLanguage[] = [
  { code: "zh", name: "Chinese", nameZh: "中文" },
  { code: "en", name: "English", nameZh: "英语" },
  { code: "fr", name: "French", nameZh: "法语" },
  { code: "pt", name: "Portuguese", nameZh: "葡萄牙语" },
  { code: "es", name: "Spanish", nameZh: "西班牙语" },
  { code: "ja", name: "Japanese", nameZh: "日语" },
  { code: "tr", name: "Turkish", nameZh: "土耳其语" },
  { code: "ru", name: "Russian", nameZh: "俄语" },
  { code: "ar", name: "Arabic", nameZh: "阿拉伯语" },
  { code: "ko", name: "Korean", nameZh: "韩语" },
  { code: "th", name: "Thai", nameZh: "泰语" },
  { code: "it", name: "Italian", nameZh: "意大利语" },
  { code: "de", name: "German", nameZh: "德语" },
  { code: "vi", name: "Vietnamese", nameZh: "越南语" },
  { code: "ms", name: "Malay", nameZh: "马来语" },
  { code: "id", name: "Indonesian", nameZh: "印尼语" },
  { code: "tl", name: "Filipino", nameZh: "菲律宾语" },
  { code: "hi", name: "Hindi", nameZh: "印地语" },
  { code: "zh-Hant", name: "Traditional Chinese", nameZh: "繁体中文" },
  { code: "pl", name: "Polish", nameZh: "波兰语" },
  { code: "cs", name: "Czech", nameZh: "捷克语" },
  { code: "nl", name: "Dutch", nameZh: "荷兰语" },
  { code: "km", name: "Khmer", nameZh: "高棉语" },
  { code: "my", name: "Burmese", nameZh: "缅甸语" },
  { code: "fa", name: "Persian", nameZh: "波斯语" },
  { code: "gu", name: "Gujarati", nameZh: "古吉拉特语" },
  { code: "ur", name: "Urdu", nameZh: "乌尔都语" },
  { code: "te", name: "Telugu", nameZh: "泰卢固语" },
  { code: "mr", name: "Marathi", nameZh: "马拉地语" },
  { code: "he", name: "Hebrew", nameZh: "希伯来语" },
  { code: "bn", name: "Bengali", nameZh: "孟加拉语" },
  { code: "ta", name: "Tamil", nameZh: "泰米尔语" },
  { code: "uk", name: "Ukrainian", nameZh: "乌克兰语" },
  { code: "bo", name: "Tibetan", nameZh: "藏语" },
  { code: "kk", name: "Kazakh", nameZh: "哈萨克语" },
  { code: "mn", name: "Mongolian", nameZh: "蒙古语" },
  { code: "ug", name: "Uyghur", nameZh: "维吾尔语" },
  { code: "yue", name: "Cantonese", nameZh: "粤语" },
];

export interface TranslationPreset {
  id: string;
  /** i18n key under playground.translation.presets */
  labelKey: string;
  /** Chinese variant of the template (use when the UI locale is Chinese). */
  zh: string;
  /** English variant of the template. */
  en: string;
}

export const translationPresets: TranslationPreset[] = [
  {
    id: "default",
    labelKey: "playground.translation.presets.default",
    zh: "将以下文本翻译为{target_lang}，注意只需要输出翻译后的结果，不要额外解释：\n\n{source_text}",
    en: "Translate the following text into {target_lang}. Note that you should only output the translated result without any additional explanation:\n\n{source_text}",
  },
  {
    id: "terminology",
    labelKey: "playground.translation.presets.terminology",
    zh: "参考下面的翻译：\n{原文} 翻译成 {译文}\n{原文} 翻译成 {译文}\n{原文} 翻译成 {译文}\n将以下文本翻译为{target_lang}，注意只需要输出翻译后的结果，不要额外解释：\n\n{source_text}",
    en: "Reference the following translations:\n{source} translates to {target}\n{source} translates to {target}\n{source} translates to {target}\n\nTranslate the following text into {target_lang}. Note that you must ONLY output the translated result without any additional explanation:\n\n{source_text}",
  },
  {
    id: "style",
    labelKey: "playground.translation.presets.style",
    zh: "请将以下文本翻译为{target_lang}。\n注意翻译的风格要严格符合【{target_style}】\n\n{source_text}",
    en: "Please translate the following text into {target_lang}. Note that the translation style must strictly conform to [{target_style}]:\n\n{source_text}",
  },
  {
    id: "personalization",
    labelKey: "playground.translation.presets.personalization",
    zh: "【待翻译文本】\n{source_text}\n\n【翻译任务】\n1、{user_preferences}\n2、{user_preferences}\n3、……\n4、将【待翻译文本】翻译为{target_lang}。",
    en: "[Source Text]\n{source_text}\n\n[Translation Tasks]\n1. {user_preferences}\n2. {user_preferences}\n3. ...\n4. Translate the [Source Text] into {target_lang}.",
  },
  {
    id: "delimiters",
    labelKey: "playground.translation.presets.delimiters",
    zh: "请将以下文本准确翻译为{target_lang}。\n你必须在译文中保留等量的分隔符，绝对不可遗漏、转义或翻译该符号，并注意分隔符的位置。\n\n{source_text}",
    en: "Please accurately translate the following text into {target_lang}.\nYou must retain the exact same number of delimiters in the translation. Strictly do not omit, escape, or translate these symbols, and pay close attention to their placement.\n\n{source_text}",
  },
  {
    id: "structured",
    labelKey: "playground.translation.presets.structured",
    zh: "# 任务目标\n将下方 {source_text} 中的 {format_type} 格式数据翻译为{target_lang}。\n\n# 严格约束\n1. 结构锁定：绝对保持原有的 {format_type} 数据结构、缩进和层级完全不变。\n2. 选择性翻译：仅翻译面向用户展示的可见文本内容。\n3. 禁止修改：严禁翻译或更改任何代码标签、键名 (Key)、变量占位符（如 {{var}}、${var}、%s、%d 等）或代码属性。\n\n# 数据输入\n{source_text}",
    en: "### Task\nTranslate the user-facing text within the following {format_type} data into {target_lang}.\n\n### Strict Rules\n1. Structure Preservation: You MUST preserve the original {format_type} data structure, nesting, hierarchy, and indentation exactly as they are.\n2. Selective Translation: Translate ONLY the visible, user-facing text content/values.\n3. Strict Non-Translation: NEVER translate or alter code tags, keys, properties, object names, or variable placeholders. Leave them exactly in their original English/code form.\n\n### Source Data\n{source_text}",
  },
];

export function templateFor(preset: TranslationPreset, locale: string): string {
  return locale.startsWith("zh") ? preset.zh : preset.en;
}

export function defaultPresetTemplate(locale: string): string {
  return templateFor(translationPresets[0], locale);
}

export function languageName(lang: TranslationLanguage, locale: string): string {
  return locale.startsWith("zh") ? lang.nameZh : lang.name;
}

/**
 * Fills the Hy-MT2 prompt template. Unknown placeholders (e.g. {target_style})
 * are left untouched so users can edit the template for advanced presets.
 * `sourceLanguage` only fills {source_lang} when a concrete language is given
 * (Hy-MT2 auto-detects, so "auto" is omitted from official templates).
 */
export function buildTranslationPrompt(
  template: string,
  targetLanguage: string,
  sourceText: string,
  sourceLanguage?: string
): string {
  let prompt = template
    .replaceAll("{target_lang}", targetLanguage)
    .replaceAll("{source_text}", sourceText);
  if (sourceLanguage) {
    prompt = prompt.replaceAll("{source_lang}", sourceLanguage);
  }
  return prompt;
}
