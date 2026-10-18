// HTML character references.
//
// A page's text is full of entities — &amp;, &#8217;, &nbsp; — and a reader
// that leaves them alone hands the model "don&#8217;t" instead of "don't".
// The named table below covers the references that actually appear in pages
// (ASCII, Latin-1, and the typographic marks); anything else is left as it was
// written rather than guessed at.

const NAMED = {
  amp: "&", lt: "<", gt: ">", quot: '"', apos: "'",
  nbsp: "\u00a0", iexcl: "\u00a1", cent: "\u00a2", pound: "\u00a3", curren: "\u00a4",
  yen: "\u00a5", brvbar: "\u00a6", sect: "\u00a7", uml: "\u00a8", copy: "\u00a9",
  ordf: "\u00aa", laquo: "\u00ab", not: "\u00ac", shy: "\u00ad", reg: "\u00ae",
  macr: "\u00af", deg: "\u00b0", plusmn: "\u00b1", sup2: "\u00b2", sup3: "\u00b3",
  acute: "\u00b4", micro: "\u00b5", para: "\u00b6", middot: "\u00b7", cedil: "\u00b8",
  sup1: "\u00b9", ordm: "\u00ba", raquo: "\u00bb", frac14: "\u00bc", frac12: "\u00bd",
  frac34: "\u00be", iquest: "\u00bf", times: "\u00d7", divide: "\u00f7",
  Agrave: "\u00c0", Aacute: "\u00c1", Auml: "\u00c4", Aring: "\u00c5", AElig: "\u00c6",
  Ccedil: "\u00c7", Eacute: "\u00c9", Egrave: "\u00c8", Euml: "\u00cb",
  Iacute: "\u00cd", Iuml: "\u00cf", Ntilde: "\u00d1",
  Oacute: "\u00d3", Ouml: "\u00d6", Oslash: "\u00d8", Uacute: "\u00da", Uuml: "\u00dc",
  Yacute: "\u00dd", szlig: "\u00df",
  agrave: "\u00e0", aacute: "\u00e1", acirc: "\u00e2", atilde: "\u00e3", auml: "\u00e4",
  aring: "\u00e5", aelig: "\u00e6", ccedil: "\u00e7", egrave: "\u00e8", eacute: "\u00e9",
  ecirc: "\u00ea", euml: "\u00eb", igrave: "\u00ec", iacute: "\u00ed", icirc: "\u00ee",
  iuml: "\u00ef", ntilde: "\u00f1", ograve: "\u00f2", oacute: "\u00f3", ocirc: "\u00f4",
  otilde: "\u00f5", ouml: "\u00f6", oslash: "\u00f8", ugrave: "\u00f9", uacute: "\u00fa",
  ucirc: "\u00fb", uuml: "\u00fc", yacute: "\u00fd", yuml: "\u00ff",
  OElig: "\u0152", oelig: "\u0153", Scaron: "\u0160", scaron: "\u0161", Yuml: "\u0178",
  fnof: "\u0192", circ: "\u02c6", tilde: "\u02dc",
  // Spacing references decode to a plain space: the converter collapses runs of
  // whitespace anyway, and a literal U+2002 in the output helps nobody.
  ensp: " ", emsp: " ", thinsp: " ", zwnj: "", zwj: "",
  ndash: "\u2013", mdash: "\u2014", horbar: "\u2015",
  lsquo: "\u2018", rsquo: "\u2019", sbquo: "\u201a", ldquo: "\u201c", rdquo: "\u201d",
  bdquo: "\u201e", dagger: "\u2020", Dagger: "\u2021", bull: "\u2022",
  hellip: "\u2026", permil: "\u2030", prime: "\u2032", Prime: "\u2033",
  lsaquo: "\u2039", rsaquo: "\u203a", oline: "\u203e", frasl: "\u2044",
  euro: "\u20ac", trade: "\u2122", minus: "\u2212", lowast: "\u2217",
  radic: "\u221a", infin: "\u221e", ne: "\u2260", le: "\u2264", ge: "\u2265",
  larr: "\u2190", uarr: "\u2191", rarr: "\u2192", darr: "\u2193", harr: "\u2194",
  spades: "\u2660", clubs: "\u2663", hearts: "\u2665", diams: "\u2666",
  check: "\u2713", cross: "\u2717", star: "\u2606",
};

// Character references: named, decimal, and hexadecimal.
const REFERENCE = /&(#x[0-9a-fA-F]+|#[0-9]+|[a-zA-Z][a-zA-Z0-9]{1,31});/g;

// fromCodePoint builds the string for a code point. Astral planes are encoded
// as a surrogate pair by hand: String.fromCodePoint is newer than the sandbox's
// JavaScript.
function fromCodePoint(code) {
  if (!isFinite(code) || code < 1 || code > 0x10ffff) {
    return "";
  }
  if (code >= 0xd800 && code <= 0xdfff) {
    return "";
  }
  if (code <= 0xffff) {
    return String.fromCharCode(code);
  }
  const offset = code - 0x10000;
  return String.fromCharCode(0xd800 + (offset >> 10), 0xdc00 + (offset & 0x3ff));
}

// decodeEntities replaces references in text and attribute values. An unknown
// name is left alone: a page that writes &foo; means it.
export function decodeEntities(text) {
  if (!text || text.indexOf("&") === -1) {
    return text || "";
  }
  return text.replace(REFERENCE, function (match, body) {
    if (body.charAt(0) === "#") {
      const hex = body.charAt(1) === "x" || body.charAt(1) === "X";
      const code = parseInt(hex ? body.slice(2) : body.slice(1), hex ? 16 : 10);
      const decoded = fromCodePoint(code);
      return decoded === "" ? "" : decoded;
    }
    const named = NAMED[body];
    return named === undefined ? match : named;
  });
}
