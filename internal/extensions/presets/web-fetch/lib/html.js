// HTML to readable Markdown.
//
// A page is mostly not content: navigation, cookie banners, comment threads,
// related-post rails and script payloads often weigh more than the article.
// This module does what a reader-mode does, in three passes:
//
//   1. tokenize  - one linear scan over the tags, with raw elements (script,
//                  style, title, svg) consumed whole so their contents are
//                  never mistaken for markup;
//   2. analyze   - a stack walk that measures each element's own text and how
//                  much of it is link text, which is what tells an article from
//                  a navigation block;
//   3. convert   - walk the chosen fragment again and emit Markdown.
//
// The result degrades instead of giving up: if the strict pass would return
// almost nothing (a page whose layout wrappers all look like boilerplate), the
// next pass strips less, and the last one only removes scripts and styles.

import { decodeEntities } from "./entities.js";
import { resolveUrl } from "./url.js";

// One tag: name, attributes, and whether it closes or self-closes. The
// attribute group accepts quoted values, so a ">" inside an attribute does not
// end the tag.
const TAG_PATTERN = "<(\\/?)([a-zA-Z][a-zA-Z0-9:-]*)((?:\"[^\"]*\"|'[^']*'|[^>\"'])*)(\\/?)>";

// Elements with no end tag.
const VOID_TAGS = {
  area: 1, base: 1, br: 1, col: 1, embed: 1, hr: 1, img: 1, input: 1,
  link: 1, meta: 1, param: 1, source: 1, track: 1, wbr: 1,
};

// Elements consumed whole: their contents are text, not markup.
const RAW_TAGS = { script: 1, style: 1, textarea: 1, title: 1, svg: 1 };

// Elements that never hold content.
const DROP_TAGS = {
  script: 1, style: 1, noscript: 1, template: 1, svg: 1, canvas: 1, iframe: 1,
  object: 1, embed: 1, applet: 1, audio: 1, video: 1, source: 1, track: 1,
  form: 1, button: 1, select: 1, option: 1, optgroup: 1, textarea: 1, label: 1,
  input: 1, meta: 1, link: 1, base: 1, title: 1, head: 1, math: 1, dialog: 1,
};

// Elements that are page furniture by definition, wherever they appear.
const STRUCTURAL_TAGS = { nav: 1, header: 1, footer: 1, aside: 1, menu: 1 };

// Containers that are dropped only when their class or id says so: a div is
// content or furniture depending on how it is named.
const KEYWORD_TAGS = { div: 1, section: 1, ul: 1, ol: 1, dl: 1, table: 1, span: 1, center: 1, figure: 1 };

// Furniture names. Word boundaries keep "ads" from matching "downloads".
const BOILERPLATE = /\b(?:nav|navigation|navbar|menu|sidebar|side|footer|header|masthead|banner|breadcrumbs?|crumbs?|comments?|disqus|share|sharing|social|related|recommend(?:ed|ations)?|promo|advert|ads?|sponsor(?:ed)?|cookie|consent|gdpr|popup|overlay|modal|newsletter|subscribe|signup|signin|login|pagination|pager|paging|tags?|toolbar|skip|survey|paywall|outbrain|taboola|backtotop|print)\b/i;

// Content names, which earn a candidate a better score.
const CONTENT = /\b(?:article|content|post|entry|main|body|text|story|read|prose|markdown|documentation|docs?)\b/i;

const HEADINGS = { h1: 1, h2: 2, h3: 3, h4: 4, h5: 5, h6: 6 };
const BLOCKS = {
  p: 1, div: 1, section: 1, article: 1, main: 1, aside: 1, figure: 1, figcaption: 1,
  blockquote: 1, pre: 1, table: 1, ul: 1, ol: 1, dl: 1, dd: 1, dt: 1, header: 1,
  footer: 1, nav: 1, form: 1, details: 1, summary: 1, address: 1, center: 1,
};

const MAX_TAGS = 60000;
const MIN_CANDIDATE_TEXT = 200;
const MIN_KEPT_TEXT = 200;
const MAX_CODE_BLOCK = 8000;

function newline(count) {
  return count <= 1 ? "\n" : count === 2 ? "\n\n" : "\n\n\n";
}

// attributeOf reads one attribute out of a raw attribute string. The value is
// entity-decoded, because href="a&amp;b" means a&b.
function attributeOf(attrs, name) {
  const pattern = new RegExp("(?:^|\\s)" + name + "\\s*=\\s*(?:\"([^\"]*)\"|'([^']*)'|([^\\s>]+))", "i");
  const match = pattern.exec(attrs || "");
  if (!match) {
    return "";
  }
  const value = match[1] !== undefined ? match[1] : match[2] !== undefined ? match[2] : match[3] || "";
  return decodeEntities(value);
}

// nameOf collects class and id, which is how a block says what it is.
function nameOf(attrs) {
  const cls = attributeOf(attrs, "class");
  const id = attributeOf(attrs, "id");
  return (cls + " " + id).trim();
}

// findRawClose locates the end tag of a raw element. A browser ends a script at
// the first "</script>", and so does this.
function findRawClose(html, name, from) {
  const pattern = new RegExp("</" + name + "(?:\\s[^>]*)?>", "gi");
  pattern.lastIndex = from;
  const match = pattern.exec(html);
  if (!match) {
    return null;
  }
  return { start: match.index, end: match.index + match[0].length };
}

// scanTags returns the tag tokens of a fragment in document order, with raw
// elements collapsed into a single open token carrying its text.
export function scanTags(html) {
  const tokens = [];
  const pattern = new RegExp(TAG_PATTERN, "g");
  let match;
  while ((match = pattern.exec(html)) !== null) {
    const token = {
      close: match[1] === "/",
      name: match[2].toLowerCase(),
      attrs: match[3] || "",
      selfClosing: match[4] === "/",
      start: match.index,
      end: match.index + match[0].length,
    };
    tokens.push(token);
    if (tokens.length >= MAX_TAGS) {
      break;
    }
    if (!token.close && !token.selfClosing && RAW_TAGS[token.name]) {
      const close = findRawClose(html, token.name, token.end);
      if (close) {
        token.rawText = html.slice(token.end, close.start);
        token.rawEnd = close.end;
        tokens.push({
          close: true, name: token.name, attrs: "", selfClosing: false,
          start: close.start, end: close.end,
        });
        pattern.lastIndex = close.end;
      }
    }
  }
  return tokens;
}

// blankTags removes tags from a fragment, for measuring how much text is left.
function textLength(fragment) {
  return decodeEntities(fragment.replace(/<[^>]*>/g, " ")).replace(/\s+/g, " ").trim().length;
}

// analyze walks the tokens once and records every element that has an end tag:
// where it starts and ends, how much text it holds, and how much of that text
// is link text.
function analyze(html) {
  const tokens = scanTags(html);
  const elements = [];
  const stack = [{ name: "#root", textLen: 0, linkLen: 0 }];
  let cursor = 0;
  let links = 0;

  const closeFrame = function (frame, end) {
    frame.end = end;
    elements.push(frame);
    const parent = stack[stack.length - 1];
    if (parent) {
      parent.textLen += frame.textLen;
      parent.linkLen += frame.linkLen;
    }
  };

  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token.start > cursor) {
      const top = stack[stack.length - 1];
      const length = token.start - cursor;
      top.textLen += length;
      if (links > 0) {
        top.linkLen += length;
      }
    }
    cursor = token.end;
    if (token.close) {
      let position = stack.length - 1;
      while (position > 0 && stack[position].name !== token.name) {
        position -= 1;
      }
      while (stack.length - 1 > position) {
        closeFrame(stack.pop(), token.start);
      }
      if (stack.length > 1) {
        const frame = stack.pop();
        closeFrame(frame, token.end);
        if (frame.name === "a") {
          links -= 1;
        }
      }
      continue;
    }
    if (token.rawEnd) {
      // Skip the raw element's text: it is either dropped or read separately.
      cursor = token.rawEnd;
    }
    const frame = {
      name: token.name, attrs: token.attrs, start: token.start, end: token.end,
      textLen: 0, linkLen: 0, raw: token.rawText,
    };
    if (token.selfClosing || VOID_TAGS[token.name]) {
      elements.push(frame);
      continue;
    }
    stack.push(frame);
    if (token.name === "a") {
      links += 1;
    }
  }
  while (stack.length > 1) {
    closeFrame(stack.pop(), html.length);
  }
  return { tokens: tokens, elements: elements, totalText: stack[0].textLen };
}

// score ranks one element as the page's main content. Link density is the
// signal: a block that is half links is a list of other pages, not this one.
function score(element, totalText) {
  if (element.textLen < MIN_CANDIDATE_TEXT || element.textLen < totalText * 0.1) {
    return 0;
  }
  const named = element.name === "article" || element.name === "main";
  if (!named && (element.name !== "div" && element.name !== "section" && element.name !== "td")) {
    return 0;
  }
  const label = nameOf(element.attrs);
  if (!named && !CONTENT.test(label)) {
    return 0;
  }
  const density = element.textLen > 0 ? element.linkLen / element.textLen : 1;
  let value = element.textLen * (1 - Math.min(1, density));
  if (named) {
    value *= 1.5;
  } else if (CONTENT.test(label)) {
    value *= 1.3;
  }
  if (BOILERPLATE.test(label)) {
    value *= 0.4;
  }
  return value;
}

// pickContent returns the element that best looks like the page's content, or
// null when nothing does (a link list, a search result page).
function pickContent(elements, totalText) {
  let best = null;
  let bestScore = 0;
  for (let index = 0; index < elements.length; index += 1) {
    const value = score(elements[index], totalText);
    if (value > bestScore) {
      bestScore = value;
      best = elements[index];
    }
  }
  return best;
}

// removals lists the index ranges to cut. The aggressive pass also drops
// containers whose class or id names furniture; the cautious one only takes
// scripts, styles and the structural elements.
function removals(elements, totalText, aggressive) {
  const ranges = [];
  for (let index = 0; index < elements.length; index += 1) {
    const element = elements[index];
    if (DROP_TAGS[element.name] || STRUCTURAL_TAGS[element.name]) {
      ranges.push([element.start, element.end]);
      continue;
    }
    if (!aggressive || !KEYWORD_TAGS[element.name]) {
      continue;
    }
    const label = nameOf(element.attrs);
    if (label === "" || !BOILERPLATE.test(label)) {
      continue;
    }
    // A block holding most of the page's text is the page: a site whose main
    // wrapper is called "content-header" must not lose its article.
    if (element.textLen > totalText * 0.5) {
      continue;
    }
    ranges.push([element.start, element.end]);
  }
  return ranges;
}

// assemble copies the kept ranges, skipping everything inside a removal.
function assemble(html, ranges, cuts) {
  const sorted = cuts.slice().sort(function (left, right) { return left[0] - right[0]; });
  const out = [];
  for (let index = 0; index < ranges.length; index += 1) {
    const range = ranges[index];
    let cursor = range[0];
    for (let cut = 0; cut < sorted.length; cut += 1) {
      const start = Math.max(sorted[cut][0], range[0]);
      const end = Math.min(sorted[cut][1], range[1]);
      if (end <= cursor || start >= range[1]) {
        continue;
      }
      if (start > cursor) {
        out.push(html.slice(cursor, start));
      }
      cursor = Math.max(cursor, end);
    }
    if (cursor < range[1]) {
      out.push(html.slice(cursor, range[1]));
    }
  }
  return out.join("");
}

// extractContent returns the fragment worth converting. It never returns
// nothing: each attempt strips less than the one before.
export function extractContent(html, mode) {
  const analysis = analyze(html);
  const strict = removals(analysis.elements, analysis.totalText, true);
  const light = removals(analysis.elements, analysis.totalText, false);
  const floor = Math.max(MIN_KEPT_TEXT, analysis.totalText * 0.15);
  const attempts = [];
  if (mode !== "full") {
    const candidate = pickContent(analysis.elements, analysis.totalText);
    if (candidate) {
      attempts.push({ ranges: [[candidate.start, candidate.end]], cuts: strict });
    }
  }
  attempts.push({ ranges: [[0, html.length]], cuts: strict });
  attempts.push({ ranges: [[0, html.length]], cuts: light });
  for (let index = 0; index < attempts.length; index += 1) {
    const fragment = assemble(html, attempts[index].ranges, attempts[index].cuts);
    if (textLength(fragment) >= floor) {
      return fragment;
    }
  }
  return assemble(html, [[0, html.length]], light);
}

// documentInfo pulls the metadata the result needs: the title, the declared
// charset, the page language and any <base href>, which relative links resolve
// against.
export function documentInfo(html) {
  const titleMatch = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(html);
  const ogMatch = /<meta[^>]+property\s*=\s*["']og:title["'][^>]*>/i.exec(html);
  const h1Match = /<h1[^>]*>([\s\S]*?)<\/h1>/i.exec(html);
  const charsetMatch = /<meta[^>]+charset\s*=\s*["']?\s*([a-zA-Z0-9_:.-]+)/i.exec(html);
  const baseMatch = /<base[^>]*>/i.exec(html);
  const htmlMatch = /<html[^>]*>/i.exec(html);
  const candidates = [
    ogMatch ? attributeOf(ogMatch[0], "content") : "",
    titleMatch ? decodeEntities(titleMatch[1].replace(/<[^>]*>/g, " ")) : "",
    h1Match ? decodeEntities(h1Match[1].replace(/<[^>]*>/g, " ")) : "",
  ];
  let title = "";
  for (let index = 0; index < candidates.length; index += 1) {
    const candidate = collapse(candidates[index]);
    if (candidate !== "" && candidate.length <= 300) {
      title = cleanTitle(candidate);
      break;
    }
  }
  return {
    title: title,
    charset: charsetMatch ? charsetMatch[1].toLowerCase() : "",
    base: baseMatch ? attributeOf(baseMatch[0], "href") : "",
    lang: htmlMatch ? attributeOf(htmlMatch[0], "lang") : "",
  };
}

// cleanTitle drops the site name a <title> usually appends: "Title | Site".
function cleanTitle(value) {
  const match = /^([\s\S]{8,}?)\s+[|·—–-]\s+([^|·—–-]{1,40})$/.exec(value);
  if (match) {
    return match[1].trim();
  }
  return value;
}

function collapse(value) {
  return String(value || "").replace(/\s+/g, " ").trim();
}

// toMarkdown converts a fragment. With plain set, markup is omitted and links
// become "text (url)", which is what a text-only consumer wants.
export function toMarkdown(fragment, baseUrl, plain) {
  const tokens = scanTags(fragment);
  const out = [];
  const links = [];
  const lists = [];
  let cursor = 0;
  let pending = 0;
  let quote = 0;
  let cells = 0;
  let space = false;

  const push = function (value) {
    if (value === "") {
      return;
    }
    if (links.length > 0) {
      const link = links[links.length - 1];
      // A gap before the link stays pending so it lands outside the brackets;
      // a gap inside the link text belongs to the text itself.
      if (space && link.text !== "") {
        link.text += " ";
        space = false;
      }
      link.text += value;
      return;
    }
    if (pending > 0) {
      out.push(newline(pending));
      pending = 0;
      space = false;
    } else if (space) {
      out.push(" ");
      space = false;
    }
    if (quote > 0 && !plain) {
      out.push("> ");
    }
    out.push(value);
  };
  const block = function () {
    if (pending < 2) {
      pending = 2;
    }
  };
  // tail is the last thing written, whichever buffer is currently receiving.
  const tail = function () {
    if (links.length > 0) {
      return links[links.length - 1].text;
    }
    return out.length > 0 ? out[out.length - 1] : "";
  };
  // gap remembers that the source had whitespace here. Inline chunks would
  // otherwise run together: "Spin the <b>crank</b>" is not "Spin the<crank>".
  // A line break supersedes the space, so a gap before a block costs nothing.
  const gap = function () {
    if (space || pending > 0) {
      return;
    }
    const last = tail();
    if (last === "" || /\s$/.test(last)) {
      return;
    }
    space = true;
  };
  const write = function (value) {
    const decoded = decodeEntities(value);
    const text = collapse(decoded);
    if (text === "") {
      if (/\s/.test(decoded)) {
        gap();
      }
      return;
    }
    if (/^\s/.test(decoded)) {
      gap();
    }
    push(text);
    if (/\s$/.test(decoded)) {
      gap();
    }
  };

  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (token.start > cursor) {
      write(fragment.slice(cursor, token.start));
    }
    cursor = token.end;
    const name = token.name;
    if (name === "pre") {
      // Take the block whole: code keeps its line structure, and the tokens
      // inside it are never converted. Inner tags are removed before entities
      // are decoded, so "&lt;div&gt;" still comes back as "<div>".
      let depth = 0;
      let end = fragment.length;
      for (let scan = index + 1; scan < tokens.length; scan += 1) {
        if (tokens[scan].name !== "pre") {
          continue;
        }
        depth += tokens[scan].close ? -1 : 1;
        if (depth < 0) {
          end = tokens[scan].start;
          cursor = tokens[scan].end;
          index = scan;
          break;
        }
      }
      let content = decodeEntities(fragment.slice(token.end, end).replace(/<[^>]*>/g, "")).replace(/[ \t]+$/gm, "").trim();
      if (content.length > MAX_CODE_BLOCK) {
        content = content.slice(0, MAX_CODE_BLOCK) + "\n[... code truncated ...]";
      }
      block();
      if (content !== "") {
        push(plain ? content : "```\n" + content + "\n```");
      }
      block();
      continue;
    }
    if (HEADINGS[name]) {
      if (token.close) {
        block();
      } else {
        block();
        if (!plain) {
          let marks = "";
          for (let level = 0; level < HEADINGS[name]; level += 1) {
            marks += "#";
          }
          push(marks + " ");
        }
      }
      continue;
    }
    if (name === "br") {
      pending = 1;
      continue;
    }
    if (name === "hr") {
      block();
      if (!plain) {
        push("---");
      }
      block();
      continue;
    }
    if (name === "li") {
      if (token.close) {
        pending = 1;
      } else {
        pending = 1;
        push(lists.length > 0 && lists[lists.length - 1] === "ol" ? "1. " : "- ");
      }
      continue;
    }
    if (name === "ul" || name === "ol") {
      if (token.close) {
        lists.pop();
        block();
      } else {
        block();
        lists.push(name);
      }
      continue;
    }
    if (name === "blockquote") {
      if (token.close) {
        quote -= 1;
        block();
      } else {
        block();
        quote += 1;
      }
      continue;
    }
    if (name === "tr") {
      cells = 0;
      pending = 1;
      continue;
    }
    if (name === "td" || name === "th") {
      if (!token.close) {
        if (cells > 0) {
          push(" | ");
        }
        cells += 1;
      }
      continue;
    }
    if (name === "a") {
      if (token.close) {
        const link = links.length > 0 ? links.pop() : null;
        if (link) {
          const text = collapse(link.text);
          if (link.href === "" || link.href === text) {
            push(text);
          } else if (plain) {
            push(text + " (" + link.href + ") ");
          } else {
            push("[" + text + "](" + link.href + ") ");
          }
        }
      } else {
        const href = attributeOf(token.attrs, "href");
        const skipped = href === "" || /^(?:javascript|mailto|tel|data|about):/i.test(href);
        links.push({ href: skipped ? "" : resolveUrl(baseUrl, href), text: "" });
      }
      continue;
    }
    if (name === "img") {
      if (!token.close && !plain) {
        const src = attributeOf(token.attrs, "src");
        if (src !== "") {
          const alt = collapse(attributeOf(token.attrs, "alt"));
          push("![" + alt + "](" + resolveUrl(baseUrl, src) + ") ");
        }
      }
      continue;
    }
    if (plain) {
      continue;
    }
    if (name === "strong" || name === "b") {
      push("**");
      continue;
    }
    if (name === "em" || name === "i") {
      push("*");
      continue;
    }
    if (name === "code") {
      push("`");
      continue;
    }
    if (name === "del" || name === "s" || name === "strike") {
      push("~~");
      continue;
    }
    if (BLOCKS[name]) {
      if (token.close) {
        block();
      } else if (pending < 2) {
        block();
      }
    }
  }
  if (cursor < fragment.length) {
    write(fragment.slice(cursor));
  }
  return tidy(out.join(""));
}

// tidy normalizes spacing without touching fenced code, where blank lines and
// indentation carry meaning. The segments between fences are trimmed of trailing
// spaces and runs of blank lines, but their edges are left alone: trimming them
// would glue a fence onto the paragraph before it.
function tidy(markdown) {
  const parts = markdown.split("```");
  for (let index = 0; index < parts.length; index += 2) {
    parts[index] = parts[index].replace(/[ \t]+$/gm, "").replace(/\n{3,}/g, "\n\n");
  }
  return parts.join("```").replace(/^[\s\n]+/, "").replace(/[\s\n]+$/, "");
}
