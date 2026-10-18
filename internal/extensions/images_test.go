package extensions

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callImage runs one images.apply request through the same entry point the
// daemon uses, so these tests exercise the real path.
func callImage(t *testing.T, request map[string]any) *imageResponse {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ImageHostCall("images.apply", encoded)
	if err != nil {
		t.Fatalf("images.apply: %v", err)
	}
	response, ok := result.(*imageResponse)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	return response
}

func callImageError(t *testing.T, request map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ImageHostCall("images.apply", encoded)
	if err == nil {
		t.Fatal("the request was accepted but should have been refused")
	}
	return err.Error()
}

// imageFixture is a deterministic picture: a smooth red/green gradient with a
// white square in the top-left quadrant and a transparent corner, so crop,
// rotation, filtering, tone and transparency all have something to measure.
func imageFixture(width, height int) *image.RGBA {
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := color.RGBA{
				R: uint8((x * 255) / maxInt(1, width-1)),
				G: uint8((y * 255) / maxInt(1, height-1)),
				B: 40,
				A: 255,
			}
			switch {
			case x < width/4 && y < height/4:
				pixel = color.RGBA{R: 255, G: 255, B: 255, A: 255}
			case x >= width-5 && y >= height-5:
				pixel = color.RGBA{R: 0, G: 0, B: 0, A: 0}
			}
			result.SetRGBA(x, y, pixel)
		}
	}
	return result
}

func encodePNGPayload(t *testing.T, img image.Image) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func encodeJPEGPayload(t *testing.T, img image.Image, quality int) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

// decodeResponsePNG reads the encoded result back, which is also how the
// extension's callers see it.
func decodeResponsePNG(t *testing.T, response *imageResponse) *image.RGBA {
	t.Helper()
	if response.Image == "" {
		t.Fatal("the response carries no image")
	}
	data, err := base64.StdEncoding.DecodeString(response.Image)
	if err != nil {
		t.Fatalf("result is not base64: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("result is not a PNG: %v", err)
	}
	return toRGBA(decoded)
}

func TestImages_InfoReadsTheHeaderWithoutDecoding(t *testing.T) {
	fixture := imageFixture(64, 32)
	response := callImage(t, map[string]any{"operation": "info", "image": encodePNGPayload(t, fixture)})
	if response.Format != "png" || response.Width != 64 || response.Height != 32 {
		t.Fatalf("info = %+v", response)
	}
	if response.Image != "" {
		t.Fatal("info should not encode an image")
	}
	if response.BitDepth != 8 || response.ColorType != "RGBA" {
		t.Fatalf("colour description = %d/%s", response.BitDepth, response.ColorType)
	}
}

func TestImages_CropClampsAndCopiesPixels(t *testing.T) {
	fixture := imageFixture(100, 80)
	// Ask for a rectangle that runs past the right and bottom edges.
	response := callImage(t, map[string]any{
		"operation": "crop", "image": encodePNGPayload(t, fixture),
		"crop": map[string]any{"x": 60, "y": 40, "width": 80, "height": 80},
	})
	if response.Cropped == nil || response.Cropped.Width != 40 || response.Cropped.Height != 40 {
		t.Fatalf("crop = %+v", response.Cropped)
	}
	if len(response.Adjusted) == 0 {
		t.Fatal("the clamping was not reported")
	}
	result := decodeResponsePNG(t, response)
	if result.Bounds().Dx() != 40 || result.Bounds().Dy() != 40 {
		t.Fatalf("crop result = %v", result.Bounds())
	}
	// The white square ends at 25,20, so nothing of it may appear here.
	want := fixture.RGBAAt(60, 40)
	got := result.RGBAAt(0, 0)
	if got != want {
		t.Fatalf("crop took the wrong origin: got %+v want %+v", got, want)
	}
}

func TestImages_ResizeFiltersKeepTheGradientAndTheEndpoints(t *testing.T) {
	fixture := imageFixture(100, 80)
	// Shrinking defaults to a box average, which preserves the endpoints of a
	// linear gradient.
	response := callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture),
		"resize": map[string]any{"width": 50},
	})
	if response.Filter != "box" || !response.DidResize {
		t.Fatalf("shrink filter = %q (didResize %v)", response.Filter, response.DidResize)
	}
	if response.Width != 50 || response.Height != 40 {
		t.Fatalf("aspect ratio was not kept: %dx%d", response.Width, response.Height)
	}
	result := decodeResponsePNG(t, response)
	// Row 25 is below the white square, so the red ramp runs 0..255 there.
	if red := result.RGBAAt(0, 25).R; red > 8 {
		t.Fatalf("box average lost the dark end: %d", red)
	}
	if red := result.RGBAAt(49, 25).R; red < 246 {
		t.Fatalf("box average lost the bright end: %d", red)
	}
	// Growing defaults to bilinear.
	response = callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture),
		"resize": map[string]any{"width": 200, "height": 160},
	})
	if response.Filter != "bilinear" {
		t.Fatalf("grow filter = %q", response.Filter)
	}
	// Nearest copies source pixels rather than blending: every output pixel must
	// be one of the source pixels its cell covers.
	response = callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture),
		"resize": map[string]any{"width": 50, "height": 40, "filter": "nearest"},
	})
	result = decodeResponsePNG(t, response)
	for _, point := range [][2]int{{0, 25}, {10, 30}, {49, 39}, {25, 12}} {
		got := result.RGBAAt(point[0], point[1])
		sourceX := 2*point[0] + 1
		sourceY := 2*point[1] + 1
		if want := fixture.RGBAAt(sourceX, sourceY); got != want {
			t.Fatalf("nearest at %v: got %+v want the source pixel %+v", point, got, want)
		}
	}
}

func TestImages_ResizeRejectsOversizedOutput(t *testing.T) {
	fixture := imageFixture(64, 64)
	message := callImageError(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture),
		"resize": map[string]any{"width": 3000, "height": 3000}, "maxOutputPixels": 100000,
	})
	if !strings.Contains(message, "output budget") {
		t.Fatalf("error = %q", message)
	}
}

func TestImages_RotateAndFlipMoveTheRightPixel(t *testing.T) {
	fixture := imageFixture(3, 2)
	// A marker pixel in the top-left corner, so a rotation is unambiguous.
	fixture.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	response := callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture), "rotate": 90,
	})
	if response.Rotated != 90 || response.Width != 2 || response.Height != 3 {
		t.Fatalf("rotate report = %+v", response)
	}
	result := decodeResponsePNG(t, response)
	if got := result.RGBAAt(1, 0); got.R != 255 {
		t.Fatalf("clockwise rotation put the marker at the wrong corner: %+v", got)
	}
	response = callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture), "flip": "horizontal",
	})
	result = decodeResponsePNG(t, response)
	if got := result.RGBAAt(2, 0); got.R != 255 {
		t.Fatalf("flip did not mirror: %+v", got)
	}
	message := callImageError(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture), "rotate": 45,
	})
	if !strings.Contains(message, "90, 180 or 270") {
		t.Fatalf("rotate error = %q", message)
	}
	message = callImageError(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture), "flip": "sideways",
	})
	if !strings.Contains(message, "flip must be") {
		t.Fatalf("flip error = %q", message)
	}
}

// TestImages_ExifOrientationIsApplied pins all eight EXIF orientations against
// the pixel a viewer would show in the top-left corner.
func TestImages_ExifOrientationIsApplied(t *testing.T) {
	// A 4x2 picture whose four left pixels differ, so every transform is
	// distinguishable.
	build := func() *image.RGBA {
		fixture := image.NewRGBA(image.Rect(0, 0, 4, 2))
		draw.Draw(fixture, fixture.Bounds(), image.NewUniform(color.RGBA{R: 10, G: 10, B: 10, A: 255}), image.Point{}, draw.Src)
		fixture.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
		return fixture
	}
	// The orientation a JPEG would declare, the tool applied without it, and the
	// shape of the result.
	// The marker sits at the source's top-left (0,0). Orientation 6 is a quarter
	// turn clockwise, which moves it to the top-right of a 2x4 result; orientation
	// 8 turns the other way, so its top-left comes from the source's top-right.
	grey := color.RGBA{R: 10, G: 10, B: 10, A: 255}
	for _, entry := range []struct {
		orientation int
		width       int
		height      int
		topLeft     color.RGBA
		topRight    color.RGBA
	}{
		{1, 4, 2, color.RGBA{R: 255, A: 255}, grey},
		{3, 4, 2, grey, grey},
		{6, 2, 4, grey, color.RGBA{R: 255, A: 255}},
		{8, 2, 4, grey, grey},
	} {
		applied := applyExifOrientation(build(), entry.orientation)
		if applied.Bounds().Dx() != entry.width || applied.Bounds().Dy() != entry.height {
			t.Fatalf("orientation %d produced %v", entry.orientation, applied.Bounds())
		}
		if got := applied.RGBAAt(0, 0); got != entry.topLeft {
			t.Fatalf("orientation %d top-left = %+v want %+v", entry.orientation, got, entry.topLeft)
		}
		if got := applied.RGBAAt(entry.width-1, 0); got != entry.topRight {
			t.Fatalf("orientation %d top-right = %+v want %+v", entry.orientation, got, entry.topRight)
		}
	}
}

func TestImages_AnalyzeMeasuresToneAndTransparency(t *testing.T) {
	fixture := imageFixture(120, 60)
	response := callImage(t, map[string]any{
		"operation": "analyze", "image": encodePNGPayload(t, fixture), "colors": 4,
	})
	stats := response.Analysis
	if stats == nil {
		t.Fatal("no analysis in the response")
	}
	if stats.Total != 120*60 || stats.Sampled != stats.Total || stats.Stride != 1 {
		t.Fatalf("sampling = %+v", stats)
	}
	if stats.TransparentSamples != 25 {
		t.Fatalf("transparent samples = %d, want the 5x5 corner", stats.TransparentSamples)
	}
	if len(stats.Dominant) != 4 {
		t.Fatalf("dominant colours = %d", len(stats.Dominant))
	}
	if stats.Dominant[0].Hex == "" || stats.Dominant[0].Share <= 0 {
		t.Fatalf("dominant entry = %+v", stats.Dominant[0])
	}
	if stats.MinLuma >= stats.MaxLuma {
		t.Fatalf("luma range = %d..%d", stats.MinLuma, stats.MaxLuma)
	}
	for cell, value := range stats.LumaGrid {
		if value < 0 || value > 255 {
			t.Fatalf("grid cell %d is out of range: %v", cell, stats.LumaGrid)
		}
	}
	// The fixture's ramps both peak at the bottom-right, so that cell is the
	// brightest; the white square lifts the top-left above the top-right.
	if stats.LumaGrid[8] < stats.LumaGrid[2] || stats.LumaGrid[8] < stats.LumaGrid[6] {
		t.Fatalf("the ramps do not show up in the grid: %v", stats.LumaGrid)
	}
	if stats.LumaGrid[0] <= stats.LumaGrid[2] {
		t.Fatalf("the bright square is not visible in the grid: %v", stats.LumaGrid)
	}
	// A sampled analysis must stay representative of the whole picture.
	sampled := callImage(t, map[string]any{
		"operation": "analyze", "image": encodePNGPayload(t, fixture), "samples": 2000,
	})
	if sampled.Analysis.Stride < 2 || sampled.Analysis.Sampled >= sampled.Analysis.Total {
		t.Fatalf("sampling did not kick in: %+v", sampled.Analysis)
	}
	if math.Abs(float64(sampled.Analysis.MeanLuma-stats.MeanLuma)) > 6 {
		t.Fatalf("sampled mean %d is far from the full mean %d", sampled.Analysis.MeanLuma, stats.MeanLuma)
	}
}

func TestImages_JPEGRoundTripAndMetadata(t *testing.T) {
	fixture := imageFixture(64, 48)
	payload := encodeJPEGPayload(t, fixture, 92)
	response := callImage(t, map[string]any{"operation": "analyze", "image": payload})
	if response.Format != "jpeg" || response.Width != 64 || response.Height != 48 {
		t.Fatalf("jpeg info = %+v", response)
	}
	if !strings.Contains(response.Subsampling, "4:2:0") {
		t.Fatalf("subsampling = %q", response.Subsampling)
	}
	// The mean colour of the fixture (a red ramp) must survive the round trip.
	if response.Analysis.MeanRed < 100 || response.Analysis.MeanRed > 150 {
		t.Fatalf("JPEG mean red = %d, want roughly the ramp's midpoint", response.Analysis.MeanRed)
	}
	// Cropping a JPEG must work on the decoded pixels, and the result is a PNG.
	cropped := callImage(t, map[string]any{
		"operation": "crop", "image": payload,
		"crop": map[string]any{"x": 32, "y": 24, "width": 16, "height": 16},
	})
	if cropped.Width != 16 || cropped.MediaType != "image/png" {
		t.Fatalf("jpeg crop = %+v", cropped)
	}
}

func TestImages_ProgressiveJPEGIsRead(t *testing.T) {
	// Go's encoder only writes baseline, so a progressive fixture has to come
	// from the testdata directory.
	data := progressiveFixture(t)
	response := callImage(t, map[string]any{"operation": "info", "image": base64.StdEncoding.EncodeToString(data)})
	if !response.Progressive {
		t.Fatalf("the fixture was not recognised as progressive: %+v", response)
	}
	// And it must decode: unlike a hand-written decoder, image/jpeg reads it.
	analyzed := callImage(t, map[string]any{"operation": "analyze", "image": base64.StdEncoding.EncodeToString(data)})
	if analyzed.Analysis == nil || analyzed.Width != response.Width {
		t.Fatalf("progressive analyse = %+v", analyzed)
	}
}

func progressiveFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "progressive.jpg"))
	if err != nil {
		t.Skipf("no progressive fixture: %v", err)
	}
	return data
}

func TestImages_JPEGOutputFlattensTransparency(t *testing.T) {
	fixture := imageFixture(32, 32)
	response := callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, fixture),
		"resize": map[string]any{"width": 32, "height": 32},
		"format": "jpeg", "quality": 90,
	})
	if response.MediaType != "image/jpeg" {
		t.Fatalf("media type = %q", response.MediaType)
	}
	data, err := base64.StdEncoding.DecodeString(response.Image)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the result is not a JPEG: %v", err)
	}
	if decoded.Bounds().Dx() != 32 {
		t.Fatalf("jpeg result = %v", decoded.Bounds())
	}
	// The transparent corner is composited on white, not black: JPEG has no
	// alpha, and a black corner is what a naive flattening produces.
	corner := toRGBA(decoded).RGBAAt(31, 31)
	if corner.R < 200 || corner.G < 200 || corner.B < 200 {
		t.Fatalf("transparency was flattened onto the wrong colour: %+v", corner)
	}
}

func TestImages_RefusesUnusableInput(t *testing.T) {
	fixture := imageFixture(20, 20)
	pngPayload := encodePNGPayload(t, fixture)
	for _, entry := range []struct {
		name    string
		request map[string]any
		want    string
	}{
		{"no image", map[string]any{"operation": "info"}, "payload is empty"},
		{"bad base64", map[string]any{"operation": "info", "image": "!!!!"}, "not valid base64"},
		{"not an image", map[string]any{"operation": "info", "image": base64.StdEncoding.EncodeToString([]byte("hello world"))}, "not a PNG or a JPEG"},
		{"percent data URL", map[string]any{"operation": "info", "image": "data:image/png,abc"}, "base64 data URLs"},
		{"unknown operation", map[string]any{"operation": "sharpen", "image": pngPayload}, "is not an image operation"},
		{"crop without a rectangle", map[string]any{"operation": "crop", "image": pngPayload}, "needs the crop rectangle"},
		{"resize without a size", map[string]any{"operation": "resize", "image": pngPayload, "resize": map[string]any{}}, "give a width"},
		{"unknown filter", map[string]any{"operation": "resize", "image": pngPayload, "resize": map[string]any{"width": 10, "filter": "lanczos"}}, "filter must be"},
		{"over the pixel budget", map[string]any{"operation": "analyze", "image": pngPayload, "maxPixels": 100}, "pixel budget"},
	} {
		got := callImageError(t, entry.request)
		if !strings.Contains(got, entry.want) {
			t.Fatalf("%s: error %q does not mention %q", entry.name, got, entry.want)
		}
	}
	// The request's own limit is clamped up to the floor, so the size check is
	// exercised directly.
	if _, err := decodeImagePayload(pngPayload, 64); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("the payload limit was not enforced: %v", err)
	}
}

// TestImages_DataURLAndWhitespaceAreAccepted pins the two shapes a caller
// actually sends: a data URL copied out of a page, and a base64 payload wrapped
// across lines.
func TestImages_DataURLAndWhitespaceAreAccepted(t *testing.T) {
	fixture := imageFixture(16, 16)
	payload := encodePNGPayload(t, fixture)
	response := callImage(t, map[string]any{"operation": "info", "image": "data:image/png;base64," + payload})
	if response.Width != 16 {
		t.Fatalf("data URL was not decoded: %+v", response)
	}
	wrapped := payload[:20] + "\n" + payload[20:40] + "\r\n" + payload[40:]
	response = callImage(t, map[string]any{"operation": "info", "image": wrapped})
	if response.Width != 16 {
		t.Fatalf("wrapped base64 was not decoded: %+v", response)
	}
	urlSafe := strings.NewReplacer("+", "-", "/", "_").Replace(payload)
	response = callImage(t, map[string]any{"operation": "info", "image": urlSafe})
	if response.Width != 16 {
		t.Fatalf("URL-safe base64 was not decoded: %+v", response)
	}
}

// TestImages_GreyscaleAndPalettedSourcesAreNormalised covers the colour models
// image/png can produce: an operation must not care which one it got.
func TestImages_GreyscaleAndPalettedSourcesAreNormalised(t *testing.T) {
	grey := image.NewGray(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			grey.SetGray(x, y, color.Gray{Y: uint8(x * 8)})
		}
	}
	response := callImage(t, map[string]any{"operation": "resize", "image": encodePNGPayload(t, grey), "resize": map[string]any{"width": 16}})
	if response.Width != 16 || response.ColorType != "greyscale" {
		t.Fatalf("greyscale resize = %+v", response)
	}
	result := decodeResponsePNG(t, response)
	if red, _, _, _ := result.At(0, 0).RGBA(); red>>8 > 4 {
		t.Fatalf("greyscale ramp lost its dark end: %d", red>>8)
	}

	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{G: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	paletted := image.NewPaletted(image.Rect(0, 0, 30, 30), palette)
	for y := 0; y < 30; y++ {
		for x := 0; x < 30; x++ {
			paletted.SetColorIndex(x, y, uint8((x+y)%3))
		}
	}
	response = callImage(t, map[string]any{"operation": "crop", "image": encodePNGPayload(t, paletted),
		"crop": map[string]any{"x": 0, "y": 0, "width": 10, "height": 10}})
	if response.ColorType != "palette" {
		t.Fatalf("palette header = %q", response.ColorType)
	}
	result = decodeResponsePNG(t, response)
	got := result.RGBAAt(0, 0)
	if got.R != 255 || got.A != 255 {
		t.Fatalf("palette pixel = %+v", got)
	}
}

// TestImages_OutputCeilingMatchesTheProtocol pins that a result cannot exceed
// what one host-call line can carry, or the extension would receive a truncated
// response instead of an error.
func TestImages_OutputCeilingMatchesTheProtocol(t *testing.T) {
	if MaxImageOutputBytes*4/3+4096 > maxWorkerLine {
		t.Fatalf("an output of %d bytes becomes %d base64 characters, over the %d line limit",
			MaxImageOutputBytes, MaxImageOutputBytes*4/3, maxWorkerLine)
	}
	// A noisy picture is the worst case for PNG: it must be refused with the
	// size named rather than silently truncated.
	noisy := image.NewRGBA(image.Rect(0, 0, 2000, 1500))
	seed := uint32(1)
	for index := 0; index < len(noisy.Pix); index += 4 {
		seed = seed*1664525 + 1013904223
		noisy.Pix[index] = uint8(seed >> 24)
		seed = seed*1664525 + 1013904223
		noisy.Pix[index+1] = uint8(seed >> 24)
		seed = seed*1664525 + 1013904223
		noisy.Pix[index+2] = uint8(seed >> 24)
		noisy.Pix[index+3] = 255
	}
	payload := encodePNGPayload(t, noisy)
	message := callImageError(t, map[string]any{
		"operation": "resize", "image": payload, "resize": map[string]any{"width": 2000, "height": 1500},
	})
	if !strings.Contains(message, "tool result can carry") {
		t.Fatalf("error = %q", message)
	}
	// The same picture as JPEG fits, which is the way out the message names.
	response := callImage(t, map[string]any{
		"operation": "resize", "image": payload, "resize": map[string]any{"width": 2000, "height": 1500},
		"format": "jpeg", "quality": 80,
	})
	if response.MediaType != "image/jpeg" || response.Bytes > MaxImageOutputBytes {
		t.Fatalf("jpeg fallback = %d bytes as %s", response.Bytes, response.MediaType)
	}
}

func TestImages_JPEGSubsamplingIsDescribed(t *testing.T) {
	// image/jpeg writes 4:2:0, and a greyscale image has no chroma at all.
	grey := image.NewGray(image.Rect(0, 0, 16, 16))
	response := callImage(t, map[string]any{"operation": "info", "image": encodeJPEGPayload(t, grey, 80)})
	if response.ColorType != "greyscale" || response.Subsampling != "" {
		t.Fatalf("greyscale jpeg info = %+v", response)
	}
}

func TestImages_UnknownHostFunction(t *testing.T) {
	if _, err := ImageHostCall("images.sharpen", nil); err == nil {
		t.Fatal("an unknown host function was accepted")
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// TestImages_MalformedInputCannotPanic pins the availability property: a tool
// call carries a model-authored string, so every decoder path has to survive
// rubbish. These cases used to panic - and a panic on the host-call goroutine
// takes the daemon down, which means every model on the machine.
func TestImages_MalformedInputCannotPanic(t *testing.T) {
	tiny := encodePNGPayload(t, imageFixture(8, 8))
	for _, entry := range []struct {
		name    string
		request map[string]any
	}{
		{
			// SOI, a SOF0 marker declaring a two-byte segment, then EOI: reading
			// the fields after that segment walked off the end of the slice.
			name:    "jpeg with a truncated frame header",
			request: map[string]any{"operation": "info", "image": base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x02, 0xff, 0xd9})},
		},
		{
			// A frame header that claims 200 components in a 10-byte segment.
			name:    "jpeg claiming more components than it has",
			request: map[string]any{"operation": "info", "image": base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x0a, 0x08, 0x00, 0x10, 0x00, 0x10, 0xc8, 0xff, 0xd9})},
		},
		{
			// Two dimensions near 2^32 multiply to 2^64, which wraps to zero in
			// int arithmetic and passes the budget check.
			name: "resize to overflowing dimensions",
			request: map[string]any{
				"operation": "resize", "image": tiny,
				"resize": map[string]any{"width": 4294967296, "height": 4294967296},
			},
		},
		{
			name: "resize to one enormous side",
			request: map[string]any{
				"operation": "resize", "image": tiny,
				"resize": map[string]any{"width": 1 << 40},
			},
		},
		{
			name:    "pixel budget that overflows",
			request: map[string]any{"operation": "analyze", "image": tiny, "maxPixels": 1 << 40},
		},
	} {
		// The contract is that the call returns, either with an error or a
		// result: a panic never reaches this line.
		encoded, err := json.Marshal(entry.request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ImageHostCall("images.apply", encoded); err != nil {
			if strings.Contains(err.Error(), "failed on malformed input") {
				t.Fatalf("%s: the panic guard caught an unhandled case: %v", entry.name, err)
			}
			continue
		}
		t.Logf("%s: accepted without error", entry.name)
	}
}

// TestImages_OversizedDimensionsAreRefused pins the boundary check itself: an
// impossible allocation is rejected by the budget arithmetic before NewRGBA is
// reached. (The recover is a separate backstop; see the test below.)
func TestImages_OversizedDimensionsAreRefused(t *testing.T) {
	encoded := []byte(`{"operation":"resize","image":"` + encodePNGPayload(t, imageFixture(4, 4)) + `","resize":{"width":9007199254740992,"height":9007199254740992}}`)
	_, err := ImageHostCall("images.apply", encoded)
	if err == nil {
		t.Fatal("an impossible allocation was accepted")
	}
	if strings.Contains(err.Error(), "failed on malformed input") {
		t.Fatalf("the refusal came from the recover, not from the budget: %v", err)
	}
}

// TestImages_PanicGuardIsInstalled proves the recover itself works. A decoder bug
// must not be able to take the daemon down, and the only way to test that is to
// panic on purpose: applyImagePanicHook is nil in production and set here.
func TestImages_PanicGuardIsInstalled(t *testing.T) {
	applyImagePanicHook = func() { panic("deliberate test panic") }
	t.Cleanup(func() { applyImagePanicHook = nil })
	_, err := ImageHostCall("images.apply", json.RawMessage(`{"operation":"info","image":"aGVsbG8="}`))
	if err == nil {
		t.Fatal("a panic in the operation was not converted to an error")
	}
	if !strings.Contains(err.Error(), "failed on malformed input") {
		t.Fatalf("error = %v, want the guard's message", err)
	}
}

// TestImages_ExifOrientationMatchesTheSpec pins all eight orientations against
// the quadrant mapping the Exif specification describes, on a 2x2 image whose
// four pixels are distinguishable. Only 1, 3, 6 and 8 were covered before, which
// is why a swap of 5 and 7 went unnoticed.
func TestImages_ExifOrientationMatchesTheSpec(t *testing.T) {
	// Four quadrants: top-left red, top-right green, bottom-left blue,
	// bottom-right yellow. Every transform permutes them differently.
	red := color.RGBA{R: 255, A: 255}
	green := color.RGBA{G: 255, A: 255}
	blue := color.RGBA{B: 255, A: 255}
	yellow := color.RGBA{R: 255, G: 255, A: 255}
	build := func() *image.RGBA {
		fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
		fixture.SetRGBA(0, 0, red)
		fixture.SetRGBA(1, 0, green)
		fixture.SetRGBA(0, 1, blue)
		fixture.SetRGBA(1, 1, yellow)
		return fixture
	}
	// The corners a viewer should see for each orientation, as the specification
	// defines them (1 = as stored, 2 = mirrored, 6 = 90 clockwise, and so on).
	for _, entry := range []struct {
		orientation int
		topLeft     color.RGBA
		topRight    color.RGBA
		bottomLeft  color.RGBA
		bottomRight color.RGBA
	}{
		{1, red, green, blue, yellow},
		{2, green, red, yellow, blue},
		{3, yellow, blue, green, red},
		{4, blue, yellow, red, green},
		{5, red, blue, green, yellow},
		{6, blue, red, yellow, green},
		{7, yellow, green, blue, red},
		{8, green, yellow, red, blue},
	} {
		applied := applyExifOrientation(build(), entry.orientation)
		bounds := applied.Bounds()
		if bounds.Dx() != 2 || bounds.Dy() != 2 {
			t.Fatalf("orientation %d produced %v", entry.orientation, bounds)
		}
		got := [4]color.RGBA{
			applied.RGBAAt(0, 0), applied.RGBAAt(1, 0),
			applied.RGBAAt(0, 1), applied.RGBAAt(1, 1),
		}
		want := [4]color.RGBA{entry.topLeft, entry.topRight, entry.bottomLeft, entry.bottomRight}
		if got != want {
			t.Fatalf("orientation %d = %v, want %v", entry.orientation, got, want)
		}
	}
}

// TestImages_CropCanResizeTheResult pins the parameters the image_crop tool
// documents. The daemon's crop branch ignored request.Resize entirely, so an
// outputWidth was accepted and then silently dropped.
func TestImages_CropCanResizeTheResult(t *testing.T) {
	fixture := imageFixture(40, 30)
	response := callImage(t, map[string]any{
		"operation": "crop", "image": encodePNGPayload(t, fixture),
		"crop":   map[string]any{"x": 0, "y": 0, "width": 20, "height": 10},
		"resize": map[string]any{"width": 10, "height": 10},
	})
	if response.Width != 10 || response.Height != 10 {
		t.Fatalf("crop with a resize produced %dx%d", response.Width, response.Height)
	}
	if !response.DidResize || response.Filter != "bilinear" {
		t.Fatalf("the resize was not reported: %+v", response)
	}
	img := decodeResponsePNG(t, response)
	if img.Bounds().Dx() != 10 || img.Bounds().Dy() != 10 {
		t.Fatalf("the encoded crop is %v", img.Bounds())
	}
	// One side may be given on its own, keeping the crop's aspect ratio.
	response = callImage(t, map[string]any{
		"operation": "crop", "image": encodePNGPayload(t, fixture),
		"crop":   map[string]any{"x": 0, "y": 0, "width": 20, "height": 10},
		"resize": map[string]any{"width": 10},
	})
	if response.Width != 10 || response.Height != 5 {
		t.Fatalf("aspect ratio was not kept: %dx%d", response.Width, response.Height)
	}
}

// TestImages_CropRejectsAnEmptyRectangle pins the error a caller gets for a
// zero-sized crop: it must name the rectangle, not surface a PNG encoder error
// about a 0x0 image.
func TestImages_CropRejectsAnEmptyRectangle(t *testing.T) {
	fixture := imageFixture(10, 10)
	message := callImageError(t, map[string]any{
		"operation": "crop", "image": encodePNGPayload(t, fixture),
		"crop": map[string]any{"x": 0, "y": 0, "width": 0, "height": 5},
	})
	if !strings.Contains(message, "must be positive") {
		t.Fatalf("empty crop error = %q", message)
	}
}

// TestImages_AnalyzeHandlesPartialTransparency pins the statistics of an image
// whose alpha is neither 0 nor 255. A half-transparent region used to be counted
// as transparent while its colour still went into the sums, so the means were
// divided by a smaller number than they were built from: brightness 65280/255
// and a dominant share of 25600% were the result.
func TestImages_AnalyzeHandlesPartialTransparency(t *testing.T) {
	fixture := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			// White at half alpha. image.RGBA is alpha-premultiplied, so the
			// stored value is 128*128/255 - writing 128 would mean a dim white.
			fixture.SetRGBA(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 128})
			premultiplied := color.RGBA{R: 255, G: 255, B: 255, A: 128}
			fixture.Set(x, y, premultiplied)
		}
	}
	stats := analyzeImage(fixture, 0, 6)
	if stats.MeanRed != 255 || stats.MeanGreen != 255 || stats.MeanBlue != 255 {
		t.Fatalf("mean of a half-transparent white image = %d,%d,%d, want 255,255,255",
			stats.MeanRed, stats.MeanGreen, stats.MeanBlue)
	}
	if stats.MeanLuma != 255 || stats.Brightness != 255 {
		t.Fatalf("brightness = %d/%d, want 255", stats.MeanLuma, stats.Brightness)
	}
	for cell, value := range stats.LumaGrid {
		if value < 0 || value > 255 {
			t.Fatalf("grid cell %d = %d, outside 0-255", cell, value)
		}
	}
	if len(stats.Dominant) != 1 || stats.Dominant[0].Share > 1 || stats.Dominant[0].Share < 0.99 {
		t.Fatalf("dominant colours = %+v, want one at about 100%%", stats.Dominant)
	}
	if stats.TransparentSamples != 256 {
		t.Fatalf("transparent samples = %d, want all 256 counted as not opaque", stats.TransparentSamples)
	}
	// A fully transparent image has no colour at all: it must not report the
	// loop's initial range as if it were a measurement.
	empty := image.NewRGBA(image.Rect(0, 0, 8, 8))
	blank := analyzeImage(empty, 0, 6)
	if blank.MeanRed != 0 || blank.MinLuma != 0 || blank.MaxLuma != 0 || len(blank.Dominant) != 0 {
		t.Fatalf("a fully transparent image reported %+v", blank)
	}
}

// TestImages_BoxDownscaleDoesNotOverflow pins the accumulator width: a cell can
// cover 25 million pixels, and 255*25e6 overflows uint32. A uniform white image
// downscaled to one pixel came out mid grey (83,83,83) instead of white.
func TestImages_BoxDownscaleDoesNotOverflow(t *testing.T) {
	const side = 5000 // 25 MP, the daemon's own pixel ceiling
	white := image.NewRGBA(image.Rect(0, 0, side, side))
	for index := 0; index < len(white.Pix); index++ {
		white.Pix[index] = 255
	}
	response := callImage(t, map[string]any{
		"operation": "resize", "image": encodePNGPayload(t, white),
		"resize": map[string]any{"width": 1, "height": 1}, "maxOutputPixels": 100,
	})
	if response.Width != 1 || response.Height != 1 {
		t.Fatalf("result = %dx%d", response.Width, response.Height)
	}
	result := decodeResponsePNG(t, response)
	if pixel := result.RGBAAt(0, 0); pixel.R != 255 || pixel.G != 255 || pixel.B != 255 {
		t.Fatalf("a white image became %+v; the accumulator overflowed", pixel)
	}
}

// TestImages_HasAlphaFollowsTheColorType pins the metadata: an RGBA file carries
// an alpha channel even when every byte is opaque, and analyze used to answer
// "no alpha" for it while info answered "yes".
func TestImages_HasAlphaFollowsTheColorType(t *testing.T) {
	opaqueRGBA := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			opaqueRGBA.SetRGBA(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	// Go's encoder writes RGB for an opaque image, so the RGBA case is built by
	// hand: a colour-type-6 file whose alpha bytes are all 255.
	payload := opaqueRGBAAsPNG(t, opaqueRGBA)
	for _, operation := range []string{"info", "analyze", "crop"} {
		request := map[string]any{"operation": operation, "image": payload}
		if operation == "crop" {
			request["crop"] = map[string]any{"x": 0, "y": 0, "width": 4, "height": 4}
		}
		response := callImage(t, request)
		if !response.HasAlpha {
			t.Fatalf("%s reports hasAlpha=false for an RGBA image", operation)
		}
	}
}

// opaqueRGBAAsPNG writes an RGBA image as a colour-type-6 PNG, which Go's own
// encoder will not do for an opaque one: that choice is exactly the case under
// test.
func opaqueRGBAAsPNG(t *testing.T, img *image.RGBA) string {
	t.Helper()
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	var raw bytes.Buffer
	for y := 0; y < height; y++ {
		raw.WriteByte(0) // filter: none
		if _, err := raw.Write(img.Pix[y*img.Stride : y*img.Stride+width*4]); err != nil {
			t.Fatal(err)
		}
	}
	compressed := zlibDeflateForTest(t, raw.Bytes())
	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	header := []byte{0, 0, 0, 0, 0, 0, 0, 0, 8, 6, 0, 0, 0}
	header[0] = byte(width >> 24)
	header[1] = byte(width >> 16)
	header[2] = byte(width >> 8)
	header[3] = byte(width)
	header[4] = byte(height >> 24)
	header[5] = byte(height >> 16)
	header[6] = byte(height >> 8)
	header[7] = byte(height)
	writeChunkForTest(&out, "IHDR", header)
	writeChunkForTest(&out, "IDAT", compressed)
	writeChunkForTest(&out, "IEND", nil)
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

func writeChunkForTest(out *bytes.Buffer, kind string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	out.Write(length[:])
	out.WriteString(kind)
	out.Write(data)
	crc := crc32ForTest(append([]byte(kind), data...))
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc)
	out.Write(sum[:])
}

func zlibDeflateForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zlib.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func crc32ForTest(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}
