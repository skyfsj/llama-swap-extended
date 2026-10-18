package extensions

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
)

// Image processing for extension host calls.
//
// Why the pixels are handled here and not in the script: the sandbox runs
// JavaScript in goja, where a tight loop over a Uint8Array manages roughly six
// million element touches a second (measured: 1M fill + 1M sum in 160 ms). A PNG
// decode plus a resample is dozens of passes over the pixels, so a 1.4 MP image
// costs seconds there, against about 0.1 s for the same work here (measured
// through the worker: analyze 115 ms, crop 166 ms, 4x downscale 125 ms on a
// 2.25 MiB base64 payload). image/png and image/jpeg also read more of the
// format than a hand-written decoder should try to (progressive and CMYK JPEG,
// interlaced PNG, every bit depth). So an extension owns the tool surface -
// parameters, settings, wording - and asks for the pixels in one call.
//
// The call takes and returns base64 because that is the only shape binary data
// survives the worker protocol: it is JSON lines, and every host-call argument is
// a JSON string. The result travels back on the same channel, which is why the
// encoded image has a hard size ceiling (MaxImageResultBytes) rather than only a
// pixel one.
//
// Resource ceilings are enforced here, not trusted from the request: an
// extension may ask for less headroom but never for more than the daemon is
// willing to allocate in its own process.

const (
	// MaxImageInputBytes caps the decoded payload of one call.
	MaxImageInputBytes = 32 << 20
	// MaxImagePixels caps what one call may decode.
	MaxImagePixels = 25_000_000
	// MaxImageOutputBytes caps the encoded result. A host-call result is one
	// protocol line with an 8 MiB ceiling (maxWorkerLine); base64 expands by 4/3
	// and the summary shares the line, so the image itself gets 5 MiB.
	MaxImageOutputBytes = 5 << 20
	// MaxImageOutputPixels caps what one call may produce.
	MaxImageOutputPixels = 25_000_000
)

// imageRequest is the payload of one images.apply call.
type imageRequest struct {
	Operation string `json:"operation"`
	Image     string `json:"image"`
	// AutoOrient applies a JPEG's EXIF orientation before anything else; it is
	// on unless the caller says otherwise, because every other tool the user
	// meets shows the photo the right way up.
	AutoOrient *bool `json:"autoOrient,omitempty"`
	// MaxPixels bounds how many pixels this call may decode. The daemon clamps
	// it; the extension sends its own, smaller, setting.
	MaxPixels       int             `json:"maxPixels,omitempty"`
	MaxOutputPixels int             `json:"maxOutputPixels,omitempty"`
	MaxInputBytes   int             `json:"maxInputBytes,omitempty"`
	Crop            *imageRectangle `json:"crop,omitempty"`
	Resize          *imageResize    `json:"resize,omitempty"`
	Rotate          int             `json:"rotate,omitempty"`
	Flip            string          `json:"flip,omitempty"`
	Colors          int             `json:"colors,omitempty"`
	Samples         int             `json:"samples,omitempty"`
	Format          string          `json:"format,omitempty"`
	Quality         int             `json:"quality,omitempty"`
}

type imageRectangle struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type imageResize struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Filter string `json:"filter"`
}

// imageResponse is what the extension receives back. It carries the metadata a
// tool result needs, the encoded image when the operation produced one, and the
// notes that explain what was adjusted.
type imageResponse struct {
	Operation          string          `json:"operation"`
	Format             string          `json:"format"`
	Width              int             `json:"width"`
	Height             int             `json:"height"`
	SourceWidth        int             `json:"sourceWidth"`
	SourceHeight       int             `json:"sourceHeight"`
	BitDepth           int             `json:"bitDepth"`
	ColorType          string          `json:"colorType"`
	HasAlpha           bool            `json:"hasAlpha"`
	Interlaced         bool            `json:"interlaced,omitempty"`
	Progressive        bool            `json:"progressive,omitempty"`
	Subsampling        string          `json:"subsampling,omitempty"`
	Orientation        int             `json:"orientation"`
	OrientationApplied bool            `json:"orientationApplied,omitempty"`
	Cropped            *imageRectangle `json:"cropped,omitempty"`
	Adjusted           []string        `json:"adjusted,omitempty"`
	Dpi                int             `json:"dpi,omitempty"`
	DidResize          bool            `json:"didResize,omitempty"`
	Filter             string          `json:"filter,omitempty"`
	Rotated            int             `json:"rotated,omitempty"`
	Flipped            string          `json:"flipped,omitempty"`
	MediaType          string          `json:"mediaType,omitempty"`
	Bytes              int             `json:"bytes,omitempty"`
	Image              string          `json:"image,omitempty"`
	Analysis           *imageStats     `json:"analysis,omitempty"`
	Notes              []string        `json:"notes,omitempty"`
}

// applyImagePanicHook is test-only: it lets a test prove the recover below is
// installed, which is otherwise unobservable without a real decoder bug.
var applyImagePanicHook func()

// ImageHostCall serves the images.* host functions. It is the single entry point
// the daemon dispatch and the tests share, so a test exercises the same code a
// running daemon does.
func ImageHostCall(fn string, args json.RawMessage) (any, error) {
	switch fn {
	case "images.apply":
		return ApplyImage(args)
	default:
		return nil, fmt.Errorf("host function %s is not supported", fn)
	}
}

// ApplyImage runs one image request.
//
// The recover is the safety net for the whole host capability: this runs inside
// the daemon's process on a goroutine that has no other recover, so a panic in
// any decoder or operation would take every model on the machine down with it.
// The input is a model-authored string, which makes that a realistic accident
// rather than a theoretical one.
func ApplyImage(args json.RawMessage) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("the image operation failed on malformed input: %v", recovered)
		}
	}()
	if applyImagePanicHook != nil {
		applyImagePanicHook()
	}
	var request imageRequest
	if len(args) == 0 {
		return nil, errors.New("images.apply: a request is required")
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return nil, fmt.Errorf("images.apply: %w", err)
	}
	return runImageRequest(request)
}

func runImageRequest(request imageRequest) (*imageResponse, error) {
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	switch operation {
	case "info", "analyze", "crop", "resize":
	default:
		return nil, fmt.Errorf("images.apply: %q is not an image operation (info, analyze, crop, resize)", request.Operation)
	}
	maxInput := clampInt(request.MaxInputBytes, 1<<20, MaxImageInputBytes, MaxImageInputBytes)
	maxPixels := clampInt(request.MaxPixels, 256, MaxImagePixels, MaxImagePixels)
	maxOutputPixels := clampInt(request.MaxOutputPixels, 256, MaxImageOutputPixels, MaxImageOutputPixels)
	payload, err := decodeImagePayload(request.Image, maxInput)
	if err != nil {
		return nil, err
	}
	header, err := inspectImage(payload)
	if err != nil {
		return nil, err
	}
	response := &imageResponse{
		Operation:    operation,
		Format:       header.format,
		Width:        header.width,
		Height:       header.height,
		SourceWidth:  header.width,
		SourceHeight: header.height,
		BitDepth:     header.bitDepth,
		ColorType:    header.colorType,
		Interlaced:   header.interlaced,
		Progressive:  header.progressive,
		Subsampling:  header.subsampling,
		Orientation:  header.orientation,
		Dpi:          header.dpi,
		// HasAlpha means "the file carries an alpha channel or a transparency
		// key", not "some pixel is transparent". The analysis reports the second
		// separately, and the preset words the two differently so they do not
		// read as a contradiction.
		HasAlpha: header.transparency || header.colorType == "RGBA" ||
			header.colorType == "greyscale with alpha",
	}
	if operation == "info" {
		return response, nil
	}
	// The product is what overflows: on a 64-bit build two dimensions near
	// 2^32 multiply to 2^64, which wraps to zero and passes an int check.
	if pixels := int64(header.width) * int64(header.height); pixels > int64(maxPixels) {
		return nil, fmt.Errorf("the image is %dx%d (%s pixels), over this tool's %s pixel budget; raise it in the extension settings, or send a smaller image",
			header.width, header.height, thousands64(pixels), thousands(maxPixels))
	}
	decoded, err := decodeImage(payload, header)
	if err != nil {
		return nil, err
	}
	result := toRGBA(decoded)
	autoOrient := request.AutoOrient == nil || *request.AutoOrient
	if autoOrient && header.orientation > 1 {
		result = applyExifOrientation(result, header.orientation)
		response.OrientationApplied = true
		response.Notes = append(response.Notes, fmt.Sprintf("EXIF orientation %d was applied, so the result is %dx%d", header.orientation, result.Bounds().Dx(), result.Bounds().Dy()))
	}
	if hasTransparency(result) {
		response.HasAlpha = true
	} else if !header.transparency {
		// Keep what the header declared: an RGBA file carries an alpha channel
		// even when every byte happens to be 255. Matching the substring
		// "alpha" missed "RGBA", so analyze reported no alpha for the most
		// common screenshot format while info reported one.
		colorType := strings.ToLower(header.colorType)
		response.HasAlpha = colorType == "rgba" || strings.Contains(colorType, "alpha")
	}
	// Rotate and flip run before crop so coordinates are in the picture as the
	// caller sees it after their own transform request.
	if request.Rotate%90 != 0 {
		return nil, errors.New("rotate must be 0, 90, 180 or 270 degrees")
	}
	if turn := ((request.Rotate % 360) + 360) % 360; turn != 0 {
		result = rotateImage(result, turn)
		response.Rotated = turn
	}
	flipMode := strings.ToLower(strings.TrimSpace(request.Flip))
	switch flipMode {
	case "", "none":
	case "horizontal":
		result = flipImage(result, true)
		response.Flipped = "horizontal"
	case "vertical":
		result = flipImage(result, false)
		response.Flipped = "vertical"
	default:
		return nil, fmt.Errorf("flip must be \"none\", \"horizontal\" or \"vertical\" (got %q)", request.Flip)
	}

	switch operation {
	case "crop":
		if request.Crop == nil {
			return nil, errors.New("a crop needs the crop rectangle")
		}
		if request.Crop.Width <= 0 || request.Crop.Height <= 0 {
			return nil, fmt.Errorf("the crop rectangle is %dx%d: width and height must be positive",
				request.Crop.Width, request.Crop.Height)
		}
		cropped, adjusted := cropImage(result, *request.Crop)
		response.Cropped = &cropped
		response.Adjusted = adjusted
		result = cropTo(result, cropped)
		// A crop may also ask for the result to be resized; the preset exposes
		// outputWidth/outputHeight for exactly that, and ignoring them silently
		// was a bug rather than a simplification.
		if request.Resize != nil && (request.Resize.Width > 0 || request.Resize.Height > 0) {
			width, height, err := resolveTargetSize(result.Bounds().Dx(), result.Bounds().Dy(), request.Resize.Width, request.Resize.Height)
			if err != nil {
				return nil, err
			}
			if width > maxOutputPixels || height > maxOutputPixels {
				return nil, fmt.Errorf("the result would be %dx%d, with a side over the %s pixel output budget",
					width, height, thousands(maxOutputPixels))
			}
			if pixels := int64(width) * int64(height); pixels > int64(maxOutputPixels) {
				return nil, fmt.Errorf("the result would be %dx%d (%s pixels), over the %s pixel output budget",
					width, height, thousands64(pixels), thousands(maxOutputPixels))
			}
			filter := strings.ToLower(strings.TrimSpace(request.Resize.Filter))
			if filter != "box" && filter != "nearest" {
				// A crop is usually enlarged for inspection, and bilinear is the
				// right default there; the preset sends "auto".
				filter = "bilinear"
			}
			if width != result.Bounds().Dx() || height != result.Bounds().Dy() {
				result = resizeImage(result, width, height, filter)
				response.DidResize = true
				response.Filter = filter
			}
		}
	case "resize":
		if request.Resize == nil {
			// A rotate or a flip on its own is a resize request without a size.
			if response.Rotated == 0 && response.Flipped == "" {
				return nil, errors.New("give a width, a height, a rotation or a flip")
			}
			break
		}
		width, height, err := resolveTargetSize(result.Bounds().Dx(), result.Bounds().Dy(), request.Resize.Width, request.Resize.Height)
		if err != nil {
			return nil, err
		}
		// NewRGBA is what panics on an impossible size, so the check has to be
		// exact: bounding each side first makes the product safe to compute (two
		// values near 2^32 multiply to 2^64, which overflows int64 just as it
		// overflows int).
		if width > maxOutputPixels || height > maxOutputPixels {
			return nil, fmt.Errorf("the result would be %dx%d, with a side over the %s pixel output budget; ask for something smaller, or raise it in the settings",
				width, height, thousands(maxOutputPixels))
		}
		if pixels := int64(width) * int64(height); pixels > int64(maxOutputPixels) {
			return nil, fmt.Errorf("the result would be %dx%d (%s pixels), over the %s pixel output budget; ask for something smaller, or raise it in the settings",
				width, height, thousands64(pixels), thousands(maxOutputPixels))
		}
		filter := strings.ToLower(strings.TrimSpace(request.Resize.Filter))
		switch filter {
		case "", "auto":
			if width*height < result.Bounds().Dx()*result.Bounds().Dy() {
				filter = "box"
			} else {
				filter = "bilinear"
			}
		case "box", "bilinear", "nearest":
		default:
			return nil, fmt.Errorf("filter must be auto, box, bilinear or nearest (got %q)", request.Resize.Filter)
		}
		if width != result.Bounds().Dx() || height != result.Bounds().Dy() {
			result = resizeImage(result, width, height, filter)
			response.DidResize = true
			response.Filter = filter
		}
	case "analyze":
		response.Analysis = analyzeImage(result, clampInt(request.Samples, 1024, 4_000_000, 262144), clampInt(request.Colors, 1, 16, 6))
	}
	response.Width = result.Bounds().Dx()
	response.Height = result.Bounds().Dy()
	if operation == "analyze" {
		return response, nil
	}
	encoded, mediaType, err := encodeImage(result, request.Format, request.Quality)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxImageOutputBytes {
		return nil, fmt.Errorf("the result is %s as %s, over the %s a tool result can carry; ask for a smaller size, or a JPEG when the picture is a photograph",
			humanBytes(len(encoded)), mediaType, humanBytes(MaxImageOutputBytes))
	}
	response.MediaType = mediaType
	response.Bytes = len(encoded)
	response.Image = base64.StdEncoding.EncodeToString(encoded)
	return response, nil
}

// imageHeader is what the format's own header says, before any pixel is decoded.
type imageHeader struct {
	format      string
	width       int
	height      int
	bitDepth    int
	colorType   string
	interlaced  bool
	progressive bool
	subsampling string
	orientation int
	dpi         int
	// transparency is set when the file declares any, which is how a PNG's tRNS
	// chunk (with no alpha channel) is reported.
	transparency bool
}

// decodeImagePayload accepts a bare base64 payload or a data URL, and tolerates
// the line breaks a wrapped payload carries.
func decodeImagePayload(raw string, limit int) ([]byte, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, errors.New("the image payload is empty")
	}
	if strings.HasPrefix(strings.ToLower(text), "data:") {
		comma := strings.Index(text, ",")
		if comma < 0 {
			return nil, errors.New("the data URL has no comma before its payload")
		}
		if !strings.Contains(strings.ToLower(text[:comma]), "base64") {
			return nil, errors.New("only base64 data URLs are supported (this one is percent-encoded text)")
		}
		text = text[comma+1:]
	}
	// A data URL keeps its newlines when it was wrapped for transport; the
	// decoder ignores them, but the size check has to allow for them.
	stripped := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, text)
	if stripped == "" {
		return nil, errors.New("the image payload is empty")
	}
	if (len(stripped)*3)/4 > limit {
		return nil, fmt.Errorf("the image is larger than the %s this tool accepts; raise the limit in the settings, or send a smaller image", humanBytes(limit))
	}
	// URL-safe base64 appears in the wild (a payload copied out of a URL), and
	// the alphabets differ in exactly two characters.
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(stripped)
	decoded, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(strings.TrimRight(normalized, "="))
	if err != nil {
		return nil, fmt.Errorf("the payload is not valid base64: %w", err)
	}
	if len(decoded) > limit {
		return nil, fmt.Errorf("the image is larger than the %s this tool accepts", humanBytes(limit))
	}
	if len(decoded) == 0 {
		return nil, errors.New("the image payload is empty")
	}
	return decoded, nil
}

// inspectImage reads the format header: enough to refuse an oversized decode and
// to describe the file, without touching the pixels.
func inspectImage(data []byte) (imageHeader, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		header, err := inspectPNG(data)
		if err != nil {
			return imageHeader{}, err
		}
		return header, nil
	case len(data) > 3 && data[0] == 0xff && data[1] == 0xd8:
		header, err := inspectJPEG(data)
		if err != nil {
			return imageHeader{}, err
		}
		return header, nil
	default:
		// Staticcheck objects to a sentence ending in a period, so the
		// unsupported formats are named last without one.
		return imageHeader{}, errors.New("the payload is not a PNG or a JPEG - PNG covers every colour type a normal encoder writes, JPEG is read in every form image/jpeg understands (baseline, progressive, greyscale, CMYK); WebP, HEIC and GIF are not supported")
	}
}

func inspectPNG(data []byte) (imageHeader, error) {
	header := imageHeader{format: "png", bitDepth: 8, colorType: "unknown"}
	if len(data) < 33 || string(data[12:16]) != "IHDR" {
		return header, errors.New("the PNG has no readable IHDR chunk")
	}
	width := int(be32(data[16:20]))
	height := int(be32(data[20:24]))
	bitDepth := int(data[24])
	colorType := data[25]
	interlace := data[28]
	if width <= 0 || height <= 0 {
		return header, fmt.Errorf("the PNG header reports %dx%d", width, height)
	}
	header.width = width
	header.height = height
	header.bitDepth = bitDepth
	header.interlaced = interlace != 0
	switch colorType {
	case 0:
		header.colorType = "greyscale"
	case 2:
		header.colorType = "RGB"
	case 3:
		header.colorType = "palette"
	case 4:
		header.colorType = "greyscale with alpha"
	case 6:
		header.colorType = "RGBA"
	default:
		header.colorType = fmt.Sprintf("colour type %d", colorType)
	}
	// Walk the chunks for the pieces a description needs: tRNS carries
	// transparency, pHYs the intended resolution.
	position := 8
	for position+8 <= len(data) {
		length := int(be32(data[position : position+4]))
		kind := string(data[position+4 : position+8])
		end := position + 8 + length
		if end+4 > len(data) {
			break
		}
		if kind == "tRNS" {
			header.colorType += " with transparency"
			header.transparency = true
		} else if kind == "pHYs" && length >= 9 && data[position+8+8] == 1 {
			perMeter := be32(data[position+8 : position+8+4])
			header.dpi = int(math.Round(float64(perMeter) * 0.0254))
		}
		position = end + 4
		if kind == "IEND" {
			break
		}
	}
	return header, nil
}

func inspectJPEG(data []byte) (imageHeader, error) {
	header := imageHeader{format: "jpeg", bitDepth: 8, colorType: "YCbCr", orientation: 1}
	position := 2
	for position+4 <= len(data) {
		if data[position] != 0xff {
			position++
			continue
		}
		marker := data[position+1]
		position += 2
		for marker == 0xff && position < len(data) {
			marker = data[position]
			position++
		}
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if marker == 0xd9 || marker == 0xda {
			break
		}
		if position+2 > len(data) {
			break
		}
		length := int(data[position])<<8 | int(data[position+1])
		start := position + 2
		end := position + length
		if length < 2 || end > len(data) {
			break
		}
		switch marker {
		case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
			// The frame header is a fixed part plus three bytes per component.
			// A segment shorter than that is malformed, and reading past it is
			// how a six-byte SOF took the whole process down.
			if length < 8 || start+6 > end {
				return header, fmt.Errorf("the JPEG frame header is %d bytes, too short to describe an image", length)
			}
			components := int(data[start+5])
			if components < 1 || start+6+components*3 > end {
				return header, fmt.Errorf("the JPEG frame header claims %d components but is only %d bytes", components, length)
			}
			header.progressive = marker == 0xc2
			header.bitDepth = int(data[start])
			header.height = int(data[start+1])<<8 | int(data[start+2])
			header.width = int(data[start+3])<<8 | int(data[start+4])
			header.subsampling = describeJPEGSubsampling(data[start+6:], components)
			if components == 1 {
				header.colorType = "greyscale"
			} else if components == 4 {
				header.colorType = "CMYK"
			}
		case 0xe1:
			if orientation := exifOrientation(data[start:end]); orientation != 1 {
				header.orientation = orientation
			}
		}
		position = end
		if marker == 0xc0 || marker == 0xc1 || marker == 0xc2 || marker == 0xc3 || marker == 0xc5 || marker == 0xc6 || marker == 0xc7 ||
			marker == 0xc9 || marker == 0xca || marker == 0xcb || marker == 0xcd || marker == 0xce || marker == 0xcf {
			break
		}
	}
	if header.width <= 0 || header.height <= 0 {
		return header, errors.New("the JPEG has no frame header with a size")
	}
	return header, nil
}

func describeJPEGSubsampling(components []byte, count int) string {
	if count < 3 {
		return ""
	}
	lumaH := int(components[1] >> 4)
	lumaV := int(components[1] & 15)
	chromaH := int(components[4] >> 4)
	chromaV := int(components[4] & 15)
	if lumaH == 0 || chromaH == 0 {
		return ""
	}
	horizontal := lumaH / chromaH
	vertical := lumaV / chromaV
	switch {
	case horizontal == 1 && vertical == 1:
		return "4:4:4 (no chroma subsampling)"
	case horizontal == 2 && vertical == 1:
		return "4:2:2 (chroma halved horizontally)"
	case horizontal == 2 && vertical == 2:
		return "4:2:0 (chroma halved both ways)"
	default:
		return fmt.Sprintf("luma %dx%d, chroma %dx%d", lumaH, lumaV, chromaH, chromaV)
	}
}

// exifOrientation reads tag 0x0112 out of an APP1 Exif block.
func exifOrientation(segment []byte) int {
	if len(segment) < 14 || string(segment[0:6]) != "Exif\x00\x00" {
		return 1
	}
	block := segment[6:]
	little := block[0] == 'I' && block[1] == 'I'
	if !little && !(block[0] == 'M' && block[1] == 'M') {
		return 1
	}
	read16 := func(at int) int {
		if little {
			return int(block[at]) | int(block[at+1])<<8
		}
		return int(block[at])<<8 | int(block[at+1])
	}
	read32 := func(at int) int {
		if little {
			return int(block[at]) | int(block[at+1])<<8 | int(block[at+2])<<16 | int(block[at+3])<<24
		}
		return int(block[at])<<24 | int(block[at+1])<<16 | int(block[at+2])<<8 | int(block[at+3])
	}
	if read16(2) != 0x2a {
		return 1
	}
	directory := read32(4)
	if directory+2 > len(block) {
		return 1
	}
	count := read16(directory)
	for index := 0; index < count; index++ {
		entry := directory + 2 + index*12
		if entry+12 > len(block) {
			break
		}
		if read16(entry) == 0x0112 {
			value := read16(entry + 8)
			if value >= 1 && value <= 8 {
				return value
			}
			return 1
		}
	}
	return 1
}

// decodeImage decodes the payload. image/png and image/jpeg handle every variant
// they document; the operations convert to RGBA, which normalises the colour
// model so nothing downstream branches on the source format.
func decodeImage(data []byte, header imageHeader) (image.Image, error) {
	var (
		decoded image.Image
		err     error
	)
	switch header.format {
	case "png":
		decoded, err = png.Decode(bytes.NewReader(data))
	case "jpeg":
		decoded, err = jpeg.Decode(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("%s is not a supported image format", header.format)
	}
	if err != nil {
		return nil, fmt.Errorf("the %s could not be decoded: %w", strings.ToUpper(header.format), err)
	}
	return decoded, nil
}

func rotateImage(source *image.RGBA, degrees int) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	switch degrees {
	case 90, 270:
		target := image.NewRGBA(image.Rect(0, 0, height, width))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				pixel := source.At(bounds.Min.X+x, bounds.Min.Y+y)
				if degrees == 90 {
					target.Set(height-1-y, x, pixel)
				} else {
					target.Set(y, width-1-x, pixel)
				}
			}
		}
		return target
	case 180:
		target := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				target.Set(width-1-x, height-1-y, source.At(bounds.Min.X+x, bounds.Min.Y+y))
			}
		}
		return target
	default:
		return source
	}
}

func flipImage(source *image.RGBA, horizontal bool) *image.RGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := source.At(bounds.Min.X+x, bounds.Min.Y+y)
			if horizontal {
				target.Set(width-1-x, y, pixel)
			} else {
				target.Set(x, height-1-y, pixel)
			}
		}
	}
	return target
}

// be32 reads a big-endian unsigned 32-bit value. PNG headers are big-endian and
// unsign, so the conversion has to go through uint32 first: a 0xFFFF width must
// not come back negative.
func be32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func clampInt(value, low, high, fallback int) int {
	if value <= 0 {
		return fallback
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// thousands64 is thousands for a value that had to be widened to avoid overflow.
func thousands64(value int64) string {
	return thousands(int(min(value, math.MaxInt32)))
}

// thousand separates digits so a pixel count is readable in a message.
func thousands(value int) string {
	digits := fmt.Sprintf("%d", value)
	var out strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

func humanBytes(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%d bytes", bytes)
	case bytes < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.2f MiB", float64(bytes)/(1<<20))
	}
}
