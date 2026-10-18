// Image Tools - crop, resize, rotate and inspect images.
//
// The pixels are processed by the daemon, not here. That is a performance
// decision with measured numbers behind it: a tight loop in the sandbox VM
// manages about six million Uint8Array element touches a second (1M fill + 1M sum
// in 160 ms), and a decode plus a resample is dozens of passes over the pixels.
// Through one `ctx.images.apply` call the same operations take about a tenth of a
// second on a 1.4 MP image. The codec it reaches also reads more of the formats
// than a hand-written decoder should attempt (progressive and CMYK JPEG,
// interlaced PNG, every bit depth). Nothing leaves the machine.
//
// This file therefore owns the tool surface - parameter validation, endpoint
// shaping, wording - and nothing else.
//
// Images travel as base64 text: a data URL or a bare payload. The sandbox cannot
// carry binary bytes (ctx.http.fetch and ctx.files.read both decode to a UTF-8
// string, and bytes above 0x7F do not survive), which is also why a URL or a path
// is refused with an explanation instead of fetched.
//
// Results are shaped by the endpoint, because the protocols differ. Anthropic
// carries an image inside a tool result natively; Chat Completions and Responses
// render a tool result as text, where a base64 payload would cost far more
// context than the picture is worth. `output: "auto"` attaches the image on
// anthropic.messages and answers with a summary elsewhere; a caller that needs the
// bytes asks for `output: "dataUrl"`, which the console renders as an image.

const OUTPUT_AUTO = "auto";
const OUTPUT_DATA_URL = "dataUrl";
const OUTPUT_SUMMARY = "summary";
const OUTPUT_MODES = [OUTPUT_AUTO, OUTPUT_DATA_URL, OUTPUT_SUMMARY];

// A hook call is one 8 MiB protocol line shared with the extension's own bundle,
// so the reachable ceiling is about 5.9 MiB of decoded image. The default is set
// there rather than at the daemon's 32 MiB ceiling, which a tool call can never
// reach: a default that cannot be honoured is a default that fails.
const DEFAULT_MAX_INPUT_KIB = 5120;
const DEFAULT_MAX_PIXELS = 4000000;
const DEFAULT_MAX_OUTPUT_PIXELS = 4000000;
const DEFAULT_ATTACH_LIMIT_KIB = 2048;
const DEFAULT_COLOR_COUNT = 6;
const DEFAULT_ANALYSIS_SAMPLES = 262144;

// A tool result joins the conversation, so the summary is capped: what a model
// needs is the geometry, what was done, and how large the result is.
const MAX_SUMMARY_CHARS = 1800;

export const settings = {
  maxInputKiB: {
    type: "number",
    label: "Largest input (KiB)",
    hint: "An image payload larger than this is refused before it is decoded. A tool call carries about 5.9 MiB of image at most (one 8 MiB protocol line shared with the extension's own code), so values above that cannot be reached; the daemon's own ceiling is 32 MiB.",
    default: DEFAULT_MAX_INPUT_KIB,
    min: 64,
    max: 6144,
    section: "Limits",
  },
  maxPixels: {
    type: "number",
    label: "Decode budget (pixels)",
    hint: "How many pixels one call may decode - the guard against a 100 MP image eating the daemon. A PNG over it is refused; raise it for very large screenshots.",
    default: DEFAULT_MAX_PIXELS,
    min: 65536,
    max: 25000000,
    section: "Limits",
  },
  maxOutputPixels: {
    type: "number",
    label: "Output budget (pixels)",
    hint: "The largest image these tools will produce; a resize past it is refused. A result is also capped at 5 MiB once encoded.",
    default: DEFAULT_MAX_OUTPUT_PIXELS,
    min: 256,
    max: 25000000,
    section: "Limits",
  },
  autoOrient: {
    type: "boolean",
    label: "Apply EXIF orientation",
    hint: "Rotate a JPEG according to its EXIF orientation before cropping, resizing or analysing, so a phone photo is not sideways. Crop coordinates are then in the rotated image, which the result states.",
    default: true,
    section: "Behaviour",
  },
  imageOutput: {
    type: "select",
    label: "Image results",
    hint: "The default for tools that produce an image. auto attaches it on Anthropic endpoints and summarizes elsewhere; dataUrl always returns the encoded image (displayed in the console, but large for a model); summary never returns bytes.",
    options: OUTPUT_MODES,
    default: OUTPUT_AUTO,
    section: "Behaviour",
  },
  outputFormat: {
    type: "select",
    label: "Output format",
    hint: "png is lossless, which is right for screenshots, charts and text. jpeg is several times smaller for a photograph and matters when a result would otherwise hit the 5 MiB ceiling; it has no transparency, so transparent areas are flattened onto white.",
    options: ["png", "jpeg"],
    default: "png",
    section: "Behaviour",
  },
  jpegQuality: {
    type: "number",
    label: "JPEG quality",
    hint: "Used when the output format is jpeg. 85 is visually lossless for most photographs; lower saves size, higher preserves detail.",
    default: 85,
    min: 1,
    max: 100,
    section: "Behaviour",
  },
  attachLimitKiB: {
    type: "number",
    label: "Attach limit (KiB)",
    hint: "In auto mode an image larger than this is not attached to an Anthropic result; the summary reports the size instead and the caller can ask for a smaller resize.",
    default: DEFAULT_ATTACH_LIMIT_KIB,
    min: 16,
    max: 5120,
    section: "Behaviour",
  },
  colorCount: {
    type: "number",
    label: "Dominant colours",
    hint: "How many colours image_analyze reports, most common first.",
    default: DEFAULT_COLOR_COUNT,
    min: 1,
    max: 16,
    section: "Analysis",
  },
  analysisSamples: {
    type: "number",
    label: "Analysis sample size",
    hint: "How many pixels image_analyze looks at. Sampling keeps the cost flat for a 12 MP photo; the report always states how many pixels were counted.",
    default: DEFAULT_ANALYSIS_SAMPLES,
    min: 1024,
    max: 4000000,
    section: "Analysis",
  },
};

function number(value, fallback) {
  const parsed = Number(value);
  return isFinite(parsed) ? parsed : fallback;
}

function integer(value, fallback, low, high) {
  const parsed = Math.floor(number(value, fallback));
  if (!isFinite(parsed)) {
    return fallback;
  }
  return Math.max(low, Math.min(high, parsed));
}

// apply runs one operation through the daemon and turns a host failure into a
// message the model can act on. The host's own errors already name the limit
// that was hit, so they are passed through rather than rewritten.
async function apply(ctx, request) {
  try {
    return await ctx.images.apply(request);
  } catch (error) {
    const message = String(error === undefined || error === null ? "" : error);
    if (message.indexOf("not a PNG or a JPEG") >= 0) {
      throw new Error("the payload is not a PNG or a JPEG. PNG covers every colour type a normal encoder writes; JPEG is read in every form image/jpeg understands, so a progressive or CMYK file is fine. WebP, HEIC and GIF are not supported - convert them first.");
    }
    throw new Error(message || "the image operation failed");
  }
}

// assertUsableImage refuses a URL or a filesystem path before anything else, with
// an explanation rather than a base64 error. The check has to leave base64 alone:
// a JPEG payload always begins "/9j/", so "starts with a slash" is not the test.
// What a path has and base64 does not is a character outside the base64 alphabet
// (or a scheme).
function assertUsableImage(image) {
  const text = String(image === undefined || image === null ? "" : image).trim();
  if (text === "") {
    throw new Error("image data is required: pass the image as base64 or a data URL");
  }
  const looksLikePath = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(text) || /^[a-zA-Z]:[\\/]/.test(text) ||
    (/^[/~.]/.test(text) && !/^[A-Za-z0-9+/=_-]+$/.test(text));
  if (looksLikePath && !/^data:/i.test(text)) {
    throw new Error("this tool cannot read a URL or a file path: the sandbox would corrupt the pixels passing them as text. Pass the image itself as base64 or a data URL.");
  }
}

// requestBase assembles the call from the settings, with two per-call overrides:
// the output format and the JPEG quality. A result over the size ceiling is a
// real possibility (a noisy PNG is large), and the message tells the caller to
// try JPEG, so the caller has to be able to.
function requestBase(ctx, args) {
  assertUsableImage(args.image);
  const requestedFormat = String(args.format || "").toLowerCase();
  const format = requestedFormat === "jpeg" || requestedFormat === "jpg"
    ? "jpeg"
    : requestedFormat === "png"
      ? "png"
      : ctx.config.outputFormat === "jpeg" ? "jpeg" : "png";
  return {
    image: args.image,
    autoOrient: ctx.config.autoOrient !== false,
    maxInputBytes: integer(ctx.config.maxInputKiB, DEFAULT_MAX_INPUT_KIB, 64, 32768) * 1024,
    maxPixels: integer(ctx.config.maxPixels, DEFAULT_MAX_PIXELS, 65536, 25000000),
    maxOutputPixels: integer(ctx.config.maxOutputPixels, DEFAULT_MAX_OUTPUT_PIXELS, 256, 25000000),
    colors: integer(args.colors === undefined ? ctx.config.colorCount : args.colors, DEFAULT_COLOR_COUNT, 1, 16),
    samples: integer(ctx.config.analysisSamples, DEFAULT_ANALYSIS_SAMPLES, 1024, 4000000),
    format: format,
    quality: integer(args.quality === undefined ? ctx.config.jpegQuality : args.quality, 85, 1, 100),
  };
}

function formatSize(result) {
  return result.width + "x" + result.height;
}

function byteSize(bytes) {
  if (bytes < 1024) {
    return bytes + " bytes";
  }
  if (bytes < 1024 * 1024) {
    return (Math.round((bytes / 1024) * 10) / 10) + " KiB";
  }
  return (Math.round((bytes / (1024 * 1024)) * 100) / 100) + " MiB";
}

function sourceLine(result) {
  const parts = [result.format.toUpperCase() + " " + result.sourceWidth + "x" + result.sourceHeight];
  if (result.format === "png") {
    parts.push(result.bitDepth + "-bit " + result.colorType);
  }
  if (result.subsampling) {
    parts.push(result.subsampling);
  }
  if (result.interlaced) {
    parts.push("interlaced");
  }
  if (result.dpi) {
    parts.push(result.dpi + " dpi");
  }
  if (result.hasAlpha) {
    // The channel exists; whether any pixel uses it is the analysis line's job.
    parts.push("has an alpha channel");
  }
  return "source: " + parts.join(", ");
}

function truncateSummary(lines) {
  const text = lines.join("\n");
  if (text.length <= MAX_SUMMARY_CHARS) {
    return text;
  }
  return text.slice(0, MAX_SUMMARY_CHARS) + "\n[summary truncated]";
}

// encodeResult renders the produced image for the endpoint and the caller's
// output mode. The content is always an array of typed parts: Anthropic requires
// that shape, the console reads it, and the other endpoints flatten it to text.
function encodeResult(ctx, call, result, summaryLines) {
  const requested = call.arguments && call.arguments.output ? String(call.arguments.output) : "";
  const configured = String(ctx.config.imageOutput || OUTPUT_AUTO);
  const mode = OUTPUT_MODES.indexOf(requested) >= 0
    ? requested
    : OUTPUT_MODES.indexOf(configured) >= 0 ? configured : OUTPUT_AUTO;
  const limit = integer(ctx.config.attachLimitKiB, DEFAULT_ATTACH_LIMIT_KIB, 16, 5120) * 1024;
  const lines = summaryLines.slice();
  lines.push("output: " + result.mediaType.replace("image/", "").toUpperCase() + " " + formatSize(result) + ", " + byteSize(result.bytes));
  const imagePart = { type: "image", source: { type: "base64", media_type: result.mediaType, data: result.image } };
  const native = ctx.endpoint === "anthropic.messages";
  if (mode === OUTPUT_DATA_URL) {
    if (!native) {
      lines.push("the image follows as an image part: it is large text, so keep it out of the conversation unless the caller needs the pixels.");
    }
    return { content: [{ type: "text", text: truncateSummary(lines) }, imagePart] };
  }
  if (mode === OUTPUT_AUTO && native) {
    if (result.bytes > limit) {
      lines.push("the image is larger than the " + Math.round(limit / 1024) + " KiB attach limit, so it was not attached; ask for a smaller resize, or raise attachLimitKiB.");
      return { content: truncateSummary(lines) };
    }
    return { content: [{ type: "text", text: truncateSummary(lines) }, imagePart] };
  }
  lines.push('the image was produced but not returned: call again with output:"dataUrl" to get it.');
  return { content: truncateSummary(lines) };
}

export default {
  tools: [
    {
      type: "function",
      execution: "server",
      function: {
        name: "image_crop",
        description: "Crop a rectangle out of an image and return it. Give the image as base64 or a data URL (data:image/png;base64,...); a URL or a file path cannot be used, and the payload has to fit in one call (roughly 5 MiB of image), so downscale a very large picture first. Coordinates are pixels, origin at the top-left of the image as the caller sees it (a JPEG's EXIF rotation is applied first when autoOrient is on, and the result says so). A rectangle that runs past an edge is clamped to the picture and the adjustment is reported. Use it to look closely at a region: a chart's axis, a dialog in a screenshot, part of a photo. On Anthropic endpoints the crop comes back as an image; on the other endpoints it is a text summary unless output:\"dataUrl\" is passed.",
        parameters: {
          type: "object",
          properties: {
            image: { type: "string", description: "The image as base64 or a data URL. Required." },
            x: { type: "number", description: "Left edge of the crop in pixels from the left of the image." },
            y: { type: "number", description: "Top edge of the crop in pixels from the top of the image." },
            width: { type: "number", description: "Width of the crop in pixels. Required." },
            height: { type: "number", description: "Height of the crop in pixels. Required." },
            outputWidth: { type: "number", description: "Resize the crop to this width afterwards, keeping the aspect ratio unless outputHeight is given too." },
            outputHeight: { type: "number", description: "Resize the crop to this height afterwards." },
            output: { type: "string", enum: OUTPUT_MODES, description: "auto (default) attaches the image on Anthropic endpoints and summarizes elsewhere; dataUrl always returns the image; summary returns text only." },
            format: { type: "string", enum: ["png", "jpeg"], description: "Output format. png is lossless and right for screenshots, charts and text; jpeg is several times smaller for a photograph and is the way out if a result would otherwise be too large to return. Defaults to the extension setting." },
            quality: { type: "number", description: "JPEG quality 1-100 (default 85), used when the output format is jpeg." },
          },
          required: ["image", "x", "y", "width", "height"],
        },
      },
    },
    {
      type: "function",
      execution: "server",
      function: {
        name: "image_resize",
        description: "Resize, rotate or mirror an image and return the result. Give the image as base64 or a data URL (a URL or file path cannot be used, and the payload has to fit in one call: roughly 5 MiB of image). Give width, height, or both: with one of them the aspect ratio is kept, with neither only rotate and flip are applied. The default filter is a box average when shrinking - it averages every pixel it covers, so nothing aliases - and bilinear when growing; nearest is there for pixel art. A rotation must be 0, 90, 180 or 270 degrees. Use it to shrink a large screenshot to something cheap to look at, or to rotate a photo that came in sideways.",
        parameters: {
          type: "object",
          properties: {
            image: { type: "string", description: "The image as base64 or a data URL. Required." },
            width: { type: "number", description: "Target width in pixels. Omit it to derive the width from the height." },
            height: { type: "number", description: "Target height in pixels. Omit it to derive the height from the width." },
            filter: { type: "string", enum: ["auto", "box", "bilinear", "nearest"], description: "auto (default) uses box when shrinking and bilinear when growing; box averages the covered pixels, bilinear interpolates, nearest copies source pixels." },
            rotate: { type: "number", description: "Rotate clockwise by 0, 90, 180 or 270 degrees before resizing." },
            flip: { type: "string", enum: ["none", "horizontal", "vertical"], description: "Mirror the image after rotating." },
            output: { type: "string", enum: OUTPUT_MODES, description: "auto (default) attaches the image on Anthropic endpoints and summarizes elsewhere; dataUrl always returns the image; summary returns text only." },
            format: { type: "string", enum: ["png", "jpeg"], description: "Output format. png is lossless and right for screenshots, charts and text; jpeg is several times smaller for a photograph and is the way out if a result would otherwise be too large to return. Defaults to the extension setting." },
            quality: { type: "number", description: "JPEG quality 1-100 (default 85), used when the output format is jpeg." },
          },
          required: ["image"],
        },
      },
    },
    {
      type: "function",
      execution: "server",
      function: {
        name: "image_analyze",
        description: "Describe an image without returning it: exact pixel dimensions, format and colour type, aspect ratio, resolution, whether it has transparency and how much, mean colour, brightness, contrast, the tonal split, a 3x3 grid of average brightness, how detailed it is, and the dominant colours as hex values with their share. Give the image as base64 or a data URL (it has to fit in one call: roughly 5 MiB). Use it to answer how large an image is, whether it is mostly dark, what colour dominates, or whether a crop is worth making. Statistics come from a sample of at most analysisSamples pixels, and the report says how many were counted.",
        parameters: {
          type: "object",
          properties: {
            image: { type: "string", description: "The image as base64 or a data URL. Required." },
            colors: { type: "number", description: "How many dominant colours to report (1-16). Defaults to the extension setting." },
          },
          required: ["image"],
        },
      },
    },
  ],

  async onToolCall(ctx, call) {
    if (call.name === "image_crop") {
      return cropTool(ctx, call);
    }
    if (call.name === "image_resize") {
      return resizeTool(ctx, call);
    }
    if (call.name === "image_analyze") {
      return analyzeTool(ctx, call);
    }
    return undefined;
  },
};

async function cropTool(ctx, call) {
  const args = call.arguments || {};
  const width = Math.floor(number(args.width, 0));
  const height = Math.floor(number(args.height, 0));
  if (!(width > 0) || !(height > 0)) {
    throw new Error("image_crop: width and height must be positive pixel counts");
  }
  const request = requestBase(ctx, args);
  request.operation = "crop";
  request.crop = { x: Math.floor(number(args.x, 0)), y: Math.floor(number(args.y, 0)), width: width, height: height };
  const outputWidth = Math.floor(number(args.outputWidth, 0));
  const outputHeight = Math.floor(number(args.outputHeight, 0));
  if (outputWidth > 0 || outputHeight > 0) {
    request.resize = { width: outputWidth, height: outputHeight, filter: "auto" };
  }
  const result = await apply(ctx, request);
  const lines = [sourceLine(result)];
  if (result.orientationApplied) {
    lines.push("orientation: EXIF rotation applied, so coordinates are in the rotated image");
  }
  lines.push("crop: " + result.cropped.x + "," + result.cropped.y + " " + result.cropped.width + "x" + result.cropped.height);
  for (const note of result.adjusted || []) {
    lines.push("note: " + note);
  }
  if (result.didResize) {
    lines.push("resize: -> " + formatSize(result));
  }
  return encodeResult(ctx, call, result, lines);
}

async function resizeTool(ctx, call) {
  const args = call.arguments || {};
  const rotate = Math.floor(number(args.rotate, 0));
  if (rotate % 90 !== 0) {
    throw new Error("image_resize: rotate must be 0, 90, 180 or 270");
  }
  const flip = String(args.flip === undefined ? "none" : args.flip).toLowerCase();
  if (["none", "horizontal", "vertical"].indexOf(flip) < 0) {
    throw new Error('image_resize: flip must be "none", "horizontal" or "vertical"');
  }
  const width = Math.floor(number(args.width, 0));
  const height = Math.floor(number(args.height, 0));
  if (width <= 0 && height <= 0 && ((rotate % 360) + 360) % 360 === 0 && flip === "none") {
    throw new Error("image_resize: give a width, a height, a rotation or a flip");
  }
  const request = requestBase(ctx, args);
  request.operation = "resize";
  request.rotate = rotate;
  request.flip = flip;
  if (width > 0 || height > 0) {
    request.resize = {
      width: width,
      height: height,
      filter: ["box", "bilinear", "nearest"].indexOf(String(args.filter || "").toLowerCase()) >= 0
        ? String(args.filter).toLowerCase()
        : "auto",
    };
  }
  const result = await apply(ctx, request);
  const lines = [sourceLine(result)];
  if (result.orientationApplied) {
    lines.push("orientation: EXIF rotation applied");
  }
  if (result.rotated) {
    lines.push("rotation: " + result.rotated + " degrees clockwise");
  }
  if (result.flipped) {
    lines.push("flip: " + result.flipped);
  }
  if (result.didResize) {
    lines.push("resize: -> " + formatSize(result) + " (" + result.filter + ")");
  } else if (!result.rotated && !result.flipped) {
    lines.push("geometry: unchanged (the requested size is the source size)");
  }
  return encodeResult(ctx, call, result, lines);
}

async function analyzeTool(ctx, call) {
  const args = call.arguments || {};
  const request = requestBase(ctx, args);
  request.operation = "analyze";
  const result = await apply(ctx, request);
  const stats = result.analysis || {};
  const lines = [sourceLine(result)];
  lines.push("size: " + formatSize(result) + " (" + megapixels(result.width, result.height) + " MP, " + aspectRatio(result.width, result.height) + ")");
  if (result.orientation > 1) {
    lines.push("orientation: EXIF " + result.orientation + " (a viewer rotates this image; image_crop and image_resize apply it automatically)");
  }
  lines.push("colour: mean rgb(" + stats.meanRed + ", " + stats.meanGreen + ", " + stats.meanBlue + "), " + transparencyLine(stats));
  lines.push("tone: brightness " + stats.brightness + "/255, contrast " + stats.contrast + ", range " + stats.minLuma + "-" + stats.maxLuma + ", detail " + stats.detail + "/255");
  lines.push("tone split: " + toneSplit(stats));
  lines.push("brightness grid (top-left to bottom-right): " + gridLine(stats.lumaGrid));
  lines.push("dominant colours: " + (stats.dominant || []).map((color) => color.hex + " (" + (Math.round(color.share * 1000) / 10) + "%)").join(", "));
  lines.push("sampled " + stats.sampled + " of " + stats.total + " pixels" + (stats.stride > 1 ? " (every " + stats.stride + "th pixel in each direction)" : ""));
  return { content: truncateSummary(lines) };
}

function megapixels(width, height) {
  return Math.round((width * height) / 10000) / 100;
}

function transparencyLine(stats) {
  if (!stats.transparentSamples) {
    return "no transparency";
  }
  return "transparency covers " + (Math.round((stats.transparentSamples / stats.sampled) * 1000) / 10) + "% of the sampled pixels";
}

// The histogram is eight luma bands; naming them in the groups a photographer
// would use says more than the eight numbers.
function toneSplit(stats) {
  const histogram = stats.histogram || [];
  const total = stats.sampled || 1;
  const share = (from, to) => {
    let sum = 0;
    for (let index = from; index <= to; index += 1) {
      sum += histogram[index] || 0;
    }
    return Math.round((sum / total) * 1000) / 10;
  };
  return "black " + share(0, 0) + "%, shadows " + share(1, 2) + "%, midtones " + share(3, 4) + "%, highlights " + share(5, 5) + "%, blown " + share(6, 7) + "%";
}

function gridLine(cells) {
  const grid = cells || [];
  return "[" + grid.slice(0, 3).join(", ") + "] [" + grid.slice(3, 6).join(", ") + "] [" + grid.slice(6, 9).join(", ") + "]";
}

// aspectRatio names the shape when it reduces to something short, and gives a
// decimal ratio otherwise.
function aspectRatio(width, height) {
  let a = width;
  let b = height;
  while (b !== 0) {
    const next = a % b;
    a = b;
    b = next;
  }
  const divisor = a || 1;
  const left = width / divisor;
  const right = height / divisor;
  if (left <= 64 && right <= 64) {
    return left + ":" + right;
  }
  return (Math.round((width / height) * 100) / 100) + ":1";
}
