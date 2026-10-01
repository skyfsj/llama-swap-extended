// PDF text extraction, pure JavaScript.
//
// The sandbox has no PDF library and no zlib, so this extractor reads the
// text-showing operators (Tj, TJ, ', ") directly out of uncompressed content
// streams. LIMITATION, stated plainly: streams that declare /FlateDecode
// (or DCTDecode/LZW) are skipped and counted; PDFs produced by most modern
// tools compress every stream, so they will yield little or no text here.
// Feed it an uncompressed PDF (for example produced by
// qpdf --stream-data=uncompress) for full text.
//
// The sandbox transports HTTP bodies as UTF-8 text, so fetching binary PDFs
// by URL can corrupt bytes above 0x7F; ASCII-only PDFs survive intact.
// Preferred input is the content parameter with a base64-encoded PDF, which
// avoids that transport entirely.
//
// Supported: literal strings with escapes and nested parens, hex strings,
// UTF-16BE strings (BOM FEFF), line-break heuristics via Td/TD/T*/BT/ET.
// Not supported: compressed streams, font encodings and CMap subsets beyond
// UTF-16BE, ToUnicode maps, and PDF object streams (cross-reference
// streams). Scanned image PDFs contain no text at all.

export const settings = {
  maxChars: {
    type: "number",
    label: "Max characters",
    hint: "Truncate the extracted text to this many characters.",
    default: 20000,
    min: 1000,
    max: 200000
  }
};

const BASE64_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
const MAX_PDF_BYTES = 6 * 1024 * 1024;

let logWarn = function (message) { /* no-op until a hook installs ctx.log */ };

// ---- base64 ----------------------------------------------------------------

function base64ToByteString(text) {
  const clean = text.replace(/[^A-Za-z0-9+/=]/g, "").replace(/=+$/, "");
  let bits = 0;
  let value = 0;
  let out = "";
  for (let index = 0; index < clean.length; index += 1) {
    const digit = BASE64_ALPHABET.indexOf(clean.charAt(index));
    if (digit < 0) continue;
    value = (value << 6) | digit;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out += String.fromCharCode((value >> bits) & 0xff);
    }
  }
  return out;
}

function decodeUtf16Bytes(bytes, offset) {
  let text = "";
  for (let index = offset || 0; index + 1 < bytes.length; index += 2) {
    text += String.fromCharCode((bytes[index] << 8) | bytes[index + 1]);
  }
  return text;
}

// ---- PDF structure ---------------------------------------------------------

// Walk every stream ... endstream pair. The callback receives the raw stream
// bytes plus whether its dictionary declares a filter we cannot decode.
function eachStream(pdf, callback) {
  let index = pdf.indexOf("stream");
  while (index !== -1) {
    const before = index > 0 ? pdf.charAt(index - 1) : "\n";
    const isKeyword = before === "\n" || before === "\r" || before === " " || before === "\t";
    if (!isKeyword) {
      index = pdf.indexOf("stream", index + 6);
      continue;
    }
    let start = index + 6;
    if (pdf.substr(start, 2) === "\r\n") start += 2;
    else if (pdf.charAt(start) === "\n" || pdf.charAt(start) === "\r") start += 1;
    const end = pdf.indexOf("endstream", start);
    if (end === -1) return;
    let dataEnd = end;
    if (pdf.charAt(dataEnd - 1) === "\n") dataEnd -= 1;
    if (pdf.charAt(dataEnd - 1) === "\r") dataEnd -= 1;
    // The dictionary owning this stream sits before the keyword.
    const dictArea = pdf.slice(Math.max(0, index - 2048), index);
    const dictStart = dictArea.lastIndexOf("<<");
    const dict = dictStart >= 0 ? dictArea.slice(dictStart) : "";
    const compressed = dict.indexOf("/FlateDecode") !== -1 || dict.indexOf("/DCTDecode") !== -1 || dict.indexOf("/LZWDecode") !== -1;
    callback(pdf.slice(start, dataEnd), compressed);
    index = pdf.indexOf("stream", end + 9);
  }
}

// ---- content-stream lexer ---------------------------------------------------

function newlineInto(out) {
  if (out.length > 0 && out.charAt(out.length - 1) !== "\n") return out + "\n";
  return out;
}

// Read a (...) literal string starting at data.charAt(open) === "(". Bytes
// are collected first so a UTF-16BE BOM can switch the decoding.
function readLiteralString(data, open) {
  let depth = 1;
  let index = open + 1;
  const bytes = [];
  const pushByte = function (byte) { bytes.push(byte & 0xff); };
  while (index < data.length && depth > 0) {
    const c = data.charAt(index);
    if (c === "\\") {
      const next = data.charAt(index + 1);
      if (next >= "0" && next <= "7") {
        let octal = "";
        let cursor = index + 1;
        while (cursor < data.length && octal.length < 3 && data.charAt(cursor) >= "0" && data.charAt(cursor) <= "7") {
          octal += data.charAt(cursor);
          cursor += 1;
        }
        pushByte(parseInt(octal, 8));
        index = cursor;
        continue;
      }
      if (next === "n") pushByte(10);
      else if (next === "r") pushByte(13);
      else if (next === "t") pushByte(9);
      else if (next === "b") pushByte(8);
      else if (next === "f") pushByte(12);
      else if (next === "\n" || next === "\r") { /* line continuation: emit nothing */ }
      else pushByte(next.charCodeAt(0));
      index += 2;
      continue;
    }
    if (c === "(") {
      depth += 1;
      pushByte(40);
      index += 1;
      continue;
    }
    if (c === ")") {
      depth -= 1;
      index += 1;
      if (depth === 0) break;
      pushByte(41);
      continue;
    }
    pushByte(c.charCodeAt(0));
    index += 1;
  }
  let text = "";
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff) {
    text = decodeUtf16Bytes(bytes, 2);
  } else {
    for (let position = 0; position < bytes.length; position += 1) {
      text += String.fromCharCode(bytes[position]);
    }
  }
  return { text: text, next: index };
}

// Read a <hex> string starting at data.charAt(open) === "<".
function readHexString(data, open) {
  let digits = "";
  let index = open + 1;
  while (index < data.length && data.charAt(index) !== ">") {
    const c = data.charAt(index);
    if (/[0-9A-Fa-f]/.test(c)) digits += c;
    index += 1;
  }
  if (digits.length % 2 === 1) digits += "0";
  const bytes = [];
  for (let position = 0; position < digits.length; position += 2) {
    bytes.push(parseInt(digits.substr(position, 2), 16));
  }
  let text = "";
  if (bytes.length >= 2 && bytes[0] === 0xfe && bytes[1] === 0xff) {
    text = decodeUtf16Bytes(bytes, 2);
  } else {
    for (let position = 0; position < bytes.length; position += 1) {
      text += String.fromCharCode(bytes[position]);
    }
  }
  return { text: text, next: index + 1 };
}

// Skip a << ... >> dictionary, honoring nested strings and dictionaries.
function skipDictionary(data, start) {
  let depth = 0;
  let index = start;
  while (index < data.length) {
    const c = data.charAt(index);
    if (c === "(") {
      const parsed = readLiteralString(data, index);
      index = parsed.next;
      continue;
    }
    if (c === "<" && data.charAt(index + 1) === "<") {
      depth += 1;
      index += 2;
      continue;
    }
    if (c === ">" && data.charAt(index + 1) === ">") {
      depth -= 1;
      index += 2;
      if (depth <= 0) return index;
      continue;
    }
    index += 1;
  }
  return index;
}

function isWhitespace(c) {
  return c === " " || c === "\n" || c === "\r" || c === "\t" || c === "\f" || c === "\0";
}

function isDelimiter(c) {
  return c === "(" || c === ")" || c === "<" || c === ">" || c === "[" || c === "]" || c === "{" || c === "}" || c === "/" || c === "%";
}

// Tokenize one content stream and return the text it draws.
function lexContentStream(data) {
  let out = "";
  const operands = [];
  let index = 0;

  const lastString = function () {
    for (let position = operands.length - 1; position >= 0; position -= 1) {
      if (typeof operands[position] === "string") return operands[position];
    }
    return "";
  };
  const joinArray = function () {
    let text = "";
    let seen = false;
    for (let position = operands.length - 1; position >= 0; position -= 1) {
      const item = operands[position];
      if (item === "[") break;
      if (typeof item === "string") text = item + text;
      seen = true;
    }
    return seen ? text : lastString();
  };
  const tailNumbers = function (howMany) {
    const numbers = [];
    for (let position = operands.length - 1; position >= 0 && numbers.length < howMany; position -= 1) {
      if (typeof operands[position] === "number") numbers.unshift(operands[position]);
    }
    return numbers;
  };

  while (index < data.length) {
    const c = data.charAt(index);
    if (isWhitespace(c)) {
      index += 1;
      continue;
    }
    if (c === "%") {
      while (index < data.length && data.charAt(index) !== "\n" && data.charAt(index) !== "\r") index += 1;
      continue;
    }
    if (c === "(") {
      const parsed = readLiteralString(data, index);
      operands.push(parsed.text);
      index = parsed.next;
      continue;
    }
    if (c === "<") {
      if (data.charAt(index + 1) === "<") {
        index = skipDictionary(data, index);
        operands.push("<<");
        continue;
      }
      const parsed = readHexString(data, index);
      operands.push(parsed.text);
      index = parsed.next;
      continue;
    }
    if (c === "[") {
      operands.push("[");
      index += 1;
      continue;
    }
    if (c === "]" || c === "{" || c === "}") {
      index += 1;
      continue;
    }
    if (c === "/") {
      index += 1;
      while (index < data.length && !isWhitespace(data.charAt(index)) && !isDelimiter(data.charAt(index))) index += 1;
      continue;
    }
    if ((c >= "0" && c <= "9") || c === "+" || c === "-" || c === ".") {
      let number = "";
      while (index < data.length && /[0-9+\-.]/.test(data.charAt(index))) {
        number += data.charAt(index);
        index += 1;
      }
      const value = parseFloat(number);
      if (!isNaN(value)) operands.push(value);
      continue;
    }
    // A run of regular characters: an operator or keyword.
    let word = "";
    while (index < data.length && !isWhitespace(data.charAt(index)) && !isDelimiter(data.charAt(index))) {
      word += data.charAt(index);
      index += 1;
    }
    if (word === "") {
      index += 1;
      continue;
    }
    if (word === "Tj" || word === "TJ") {
      const text = word === "TJ" ? joinArray() : lastString();
      if (text) out += text;
    } else if (word === "'" || word === '"') {
      out = newlineInto(out);
      const text = lastString();
      if (text) out += text;
    } else if (word === "Td" || word === "TD") {
      const numbers = tailNumbers(2);
      // Td operands: tx ty; a non-zero ty means a new line.
      if (numbers.length === 2 && numbers[1] !== 0) out = newlineInto(out);
    } else if (word === "Tm") {
      const numbers = tailNumbers(6);
      // Tm operands: a b c d e f; d is the vertical translation.
      if (numbers.length === 6 && numbers[3] !== 0) out = newlineInto(out);
    } else if (word === "T*" || word === "BT" || word === "ET") {
      out = newlineInto(out);
    }
    // Every keyword ends the current operand list.
    operands.length = 0;
  }
  return out;
}

function cleanText(text) {
  let cleaned = "";
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index);
    if (code === 10 || code === 9) cleaned += text.charAt(index);
    else if (code < 32) cleaned += " ";
    else cleaned += text.charAt(index);
  }
  return cleaned.replace(/\n{3,}/g, "\n\n").trim();
}

// ---- driver -----------------------------------------------------------------

function extractPdfText(pdf) {
  let text = "";
  let total = 0;
  let skipped = 0;
  eachStream(pdf, function (data, compressed) {
    total += 1;
    if (compressed) {
      skipped += 1;
      return;
    }
    if (data.indexOf("BT") === -1 && data.indexOf("Tj") === -1 && data.indexOf("TJ") === -1) return;
    try {
      text += lexContentStream(data) + "\n";
    } catch (error) {
      logWarn("content stream " + total + " failed to parse: " + (error && error.message ? error.message : error));
    }
  });
  return { text: cleanText(text), total: total, skipped: skipped };
}

function emptyMessage(source, result) {
  let message = "No text could be extracted from the " + source + ".";
  if (result.skipped > 0) {
    message += " All " + result.total + " content streams use a compression filter this extractor cannot read (FlateDecode). Uncompress the PDF first (qpdf --stream-data=uncompress) and retry.";
  } else if (result.total === 0) {
    message += " The document contains no content streams; it may be a scanned image PDF, which needs OCR.";
  }
  return message;
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "extract_pdf_text",
      description: "Extract the text of a PDF document. Only uncompressed PDFs are supported (FlateDecode streams are skipped and reported). Pass the PDF as url (its host must be listed in the manifest's networkHosts) or as content (base64-encoded PDF, or raw PDF text).",
      parameters: {
        type: "object",
        properties: {
          url: {
            type: "string",
            description: "http(s) URL of the PDF. Its host must appear in the manifest networkHosts; binary bytes above 0x7F can be mangled by the sandbox transport, so ASCII PDFs are most reliable."
          },
          content: {
            type: "string",
            description: "The PDF itself, base64-encoded (preferred) or as raw PDF text. Used when url is absent."
          }
        }
      }
    }
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "extract_pdf_text") return undefined;
    logWarn = ctx.log.warn;

    let pdf = "";
    let source = "";
    const url = String(call.arguments.url || "").trim();
    const content = call.arguments.content ? String(call.arguments.content) : "";

    if (content.trim().length > 0) {
      source = "content";
      const trimmed = content.trim();
      if (trimmed.indexOf("%PDF") === 0) {
        pdf = trimmed;
      } else if (/^[A-Za-z0-9+/=\s]+$/.test(trimmed)) {
        const decoded = base64ToByteString(trimmed);
        if (decoded.indexOf("%PDF") !== 0) {
          throw new Error("content is not a PDF: it looks like base64 but decodes without a %PDF header");
        }
        pdf = decoded;
      } else {
        throw new Error("content is not a PDF: raw text has no %PDF header and it is not base64");
      }
    } else if (url) {
      source = "url";
      const response = await ctx.http.fetch(url);
      if (!response.ok) {
        throw new Error("fetching the PDF failed with HTTP " + response.status);
      }
      pdf = await response.text();
      if (pdf.indexOf("%PDF") === -1) {
        throw new Error("the fetched document is not a PDF (no %PDF header)");
      }
    } else {
      throw new Error("provide either url or content");
    }

    if (pdf.length > MAX_PDF_BYTES) {
      throw new Error("PDF is larger than " + Math.floor(MAX_PDF_BYTES / 1048576) + " MiB; this extractor is best-effort for small documents");
    }

    const result = extractPdfText(pdf);
    logWarn = function (message) { /* restored */ };
    if (result.text.length === 0) {
      return { content: emptyMessage(source, result) };
    }

    const requested = Number(ctx.config.maxChars);
    const maxChars = requested > 0 ? Math.floor(requested) : 20000;
    let text = result.text;
    let truncated = "";
    if (text.length > maxChars) {
      text = text.slice(0, maxChars);
      truncated = "\n\n[pdf-extract] output truncated at " + maxChars + " characters";
    }
    let note = "";
    if (result.skipped > 0) {
      note = "\n\n[pdf-extract] " + result.skipped + " of " + result.total + " content streams use a compression filter and were skipped; only uncompressed text operators were extracted.";
    }
    return { content: text + truncated + note };
  }
};
