package extensions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageToolsHost serves the images.apply host call the way the daemon does, so
// these tests exercise the production path: the preset's parameter handling runs
// in the goja worker and the pixels are processed by the real Go implementation.
type imageToolsHost struct{}

func (imageToolsHost) Exec(_ context.Context, fn string, args json.RawMessage) (any, error) {
	return ImageHostCall(fn, args)
}

// imagePresetInvoke runs one image-tools call through the real worker and returns
// the raw result.
func imagePresetInvoke(t *testing.T, compiled *Compiled, endpoint, name string, arguments map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHostCaller(context.Background(), imageToolsHost{})
	out, _, err := compiled.Invoke(ctx, "onToolCall", Context{Endpoint: endpoint}, payload)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("output %s: %v", string(out), err)
	}
	return result
}

func imagePresetTool(t *testing.T, compiled *Compiled, name string, arguments map[string]any) (string, map[string]any) {
	t.Helper()
	result := imagePresetInvoke(t, compiled, "chat.completions", name, arguments)
	return imagePresetText(t, result), result
}

// imagePresetText flattens a tool result into the text the model would read.
func imagePresetText(t *testing.T, result map[string]any) string {
	t.Helper()
	switch content := result["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			entry, _ := raw.(map[string]any)
			if entry["type"] == "text" {
				if text, ok := entry["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	t.Fatalf("unrecognised tool result: %+v", result["content"])
	return ""
}

func imagePresetError(t *testing.T, compiled *Compiled, name string, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithHostCaller(context.Background(), imageToolsHost{})
	_, _, err = compiled.Invoke(ctx, "onToolCall", Context{Endpoint: "chat.completions"}, payload)
	if err == nil {
		t.Fatal("the tool accepted input it should have rejected")
	}
	return err.Error()
}

// testImage builds a deterministic image: a smooth gradient with a bright block
// in one corner, so crop, resize and analysis all have something to find.
func testImage(width, height int) *image.RGBA {
	result := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			red := uint8((x * 255) / max(1, width-1))
			green := uint8((y * 255) / max(1, height-1))
			blue := uint8(128)
			if x < width/4 && y < height/4 {
				red, green, blue = 255, 255, 240
			}
			result.SetRGBA(x, y, color.RGBA{R: red, G: green, B: blue, A: 255})
		}
	}
	return result
}

// encodeTestPNG renders a fixture as PNG bytes at the given base64 payload size.
func encodeTestPNG(t *testing.T, img image.Image) string {
	t.Helper()
	var buffer strings.Builder
	encoder := base64.NewEncoder(base64.StdEncoding, &buffer)
	if err := png.Encode(encoder, img); err != nil {
		t.Fatal(err)
	}
	_ = encoder.Close()
	return buffer.String()
}

func encodeTestJPEG(t *testing.T, img image.Image, quality int) string {
	t.Helper()
	var buffer strings.Builder
	encoder := base64.NewEncoder(base64.StdEncoding, &buffer)
	if err := jpeg.Encode(encoder, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	_ = encoder.Close()
	return buffer.String()
}

// decodeResultPNG reads a data URL produced by the preset back into an image.
func decodeResultPNG(t *testing.T, result map[string]any) image.Image {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("expected content parts, got %T", result["content"])
	}
	for _, raw := range content {
		part, _ := raw.(map[string]any)
		if part["type"] != "image" {
			continue
		}
		source, _ := part["source"].(map[string]any)
		data, _ := source["data"].(string)
		if data == "" {
			t.Fatalf("image part without data: %+v", part)
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			t.Fatalf("image part is not base64: %v", err)
		}
		img, err := png.Decode(strings.NewReader(string(decoded)))
		if err != nil {
			t.Fatalf("image part is not a PNG: %v", err)
		}
		return img
	}
	t.Fatalf("no image part in %+v", content)
	return nil
}

func TestPreset_ImageAnalyzeReportsGeometryAndTone(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestPNG(t, testImage(120, 60))
	text, _ := imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": payload})
	for _, want := range []string{"size: 120x60", "2:1", "PNG", "no transparency", "dominant colours: #"} {
		if !strings.Contains(text, want) {
			t.Fatalf("analysis is missing %q:\n%s", want, text)
		}
	}
	// A data URL must be accepted as well as a bare payload.
	text, _ = imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": "data:image/png;base64," + payload})
	if !strings.Contains(text, "120x60") {
		t.Fatalf("data URL input failed:\n%s", text)
	}
}

func TestPreset_ImageAnalyzeSeesTransparency(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	transparent := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			alpha := uint8(255)
			if x < 20 {
				alpha = 0
			}
			transparent.SetRGBA(x, y, color.RGBA{R: 10, G: 200, B: 30, A: alpha})
		}
	}
	text, _ := imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": encodeTestPNG(t, transparent)})
	if !strings.Contains(text, "transparency covers") {
		t.Fatalf("transparency was not reported:\n%s", text)
	}
	// The mean colour must come from the visible half only.
	if !strings.Contains(text, "rgb(10, 200, 30)") {
		t.Fatalf("mean colour ignored transparency:\n%s", text)
	}
}

func TestPreset_ImageCropClampsAndReports(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestPNG(t, testImage(100, 80))
	// The request runs past the right and bottom edges on purpose.
	text, result := imagePresetTool(t, compiled, "image_crop", map[string]any{
		"image": payload, "x": 60, "y": 40, "width": 80, "height": 80,
		"output": "dataUrl",
	})
	if !strings.Contains(text, "crop: 60,40 40x40") {
		t.Fatalf("crop geometry is wrong:\n%s", text)
	}
	if !strings.Contains(text, "clamped") {
		t.Fatalf("clamping was not reported:\n%s", text)
	}
	img := decodeResultPNG(t, result)
	if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 40 {
		t.Fatalf("crop result is %v", img.Bounds())
	}
	// The bright corner of the fixture ends at x=25,y=20, so a crop from 60,40
	// must not contain it: the result is the gradient, not the block.
	red, _, _, _ := img.At(0, 0).RGBA()
	if red>>8 < 100 {
		t.Fatalf("crop took the wrong region: red = %d", red>>8)
	}
}

func TestPreset_ImageResizeChoosesFiltersAndReportsSize(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestPNG(t, testImage(100, 80))
	text, result := imagePresetTool(t, compiled, "image_resize", map[string]any{
		"image": payload, "width": 50, "output": "dataUrl",
	})
	if !strings.Contains(text, "source: PNG 100x80") || !strings.Contains(text, "resize: -> 50x40 (box)") {
		t.Fatalf("resize report is wrong:\n%s", text)
	}
	img := decodeResultPNG(t, result)
	if img.Bounds().Dx() != 50 || img.Bounds().Dy() != 40 {
		t.Fatalf("resize result is %v", img.Bounds())
	}
	// A box average of a linear gradient keeps the endpoints. Row 25 is below
	// the fixture's bright corner, so it carries the gradient itself.
	leftRed, _, _, _ := img.At(0, 25).RGBA()
	rightRed, _, _, _ := img.At(49, 25).RGBA()
	if leftRed>>8 >= 8 || rightRed>>8 <= 245 {
		t.Fatalf("box average changed the gradient: %d..%d", leftRed>>8, rightRed>>8)
	}
	// Upscaling with the default filter interpolates instead of leaving gaps.
	text, result = imagePresetTool(t, compiled, "image_resize", map[string]any{
		"image": payload, "width": 200, "output": "dataUrl",
	})
	if !strings.Contains(text, "(bilinear)") {
		t.Fatalf("upscale did not report bilinear:\n%s", text)
	}
	img = decodeResultPNG(t, result)
	if img.Bounds().Dx() != 200 {
		t.Fatalf("upscale result is %v", img.Bounds())
	}
}

func TestPreset_ImageResizeRotatesAndFlips(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	// A 3x2 image with a distinct top-left pixel proves the orientation.
	small := image.NewRGBA(image.Rect(0, 0, 3, 2))
	small.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	payload := encodeTestPNG(t, small)
	_, result := imagePresetTool(t, compiled, "image_resize", map[string]any{
		"image": payload, "rotate": 90, "output": "dataUrl",
	})
	img := decodeResultPNG(t, result)
	if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 3 {
		t.Fatalf("rotate did not swap the axes: %v", img.Bounds())
	}
	// Clockwise: the top-left pixel lands at the top-right.
	red, _, _, _ := img.At(1, 0).RGBA()
	if red>>8 != 255 {
		t.Fatalf("rotated pixel is at the wrong corner (red %d)", red>>8)
	}
	_, result = imagePresetTool(t, compiled, "image_resize", map[string]any{
		"image": payload, "flip": "horizontal", "output": "dataUrl",
	})
	img = decodeResultPNG(t, result)
	red, _, _, _ = img.At(2, 0).RGBA()
	if red>>8 != 255 {
		t.Fatalf("flip did not mirror the image (red %d)", red>>8)
	}
}

func TestPreset_ImageToolsRejectUnusableInput(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestPNG(t, testImage(20, 20))
	for _, entry := range []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"a URL", map[string]any{"image": "https://example.com/cat.png", "x": 0, "y": 0, "width": 5, "height": 5}, "cannot read a URL"},
		{"a file path", map[string]any{"image": "/tmp/cat.png", "x": 0, "y": 0, "width": 5, "height": 5}, "cannot read a URL"},
		{"no image", map[string]any{"image": "", "x": 0, "y": 0, "width": 5, "height": 5}, "image data is required"},
		{"not an image", map[string]any{"image": "aGVsbG8gd29ybGQ=", "x": 0, "y": 0, "width": 5, "height": 5}, "not a PNG or a JPEG"},
		{"bad base64", map[string]any{"image": "!!!!", "x": 0, "y": 0, "width": 5, "height": 5}, "not valid base64"},
		{"zero size crop", map[string]any{"image": payload, "x": 0, "y": 0, "width": 0, "height": 10}, "must be positive"},
	} {
		got := imagePresetError(t, compiled, "image_crop", entry.arguments)
		if !strings.Contains(got, entry.want) {
			t.Fatalf("%s: error %q does not mention %q", entry.name, got, entry.want)
		}
	}
	for _, entry := range []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"bad rotate", map[string]any{"image": payload, "rotate": 45}, "rotate must be 0, 90, 180 or 270"},
		{"bad flip", map[string]any{"image": payload, "flip": "sideways"}, "flip must be"},
		{"nothing to do", map[string]any{"image": payload}, "give a width, a height, a rotation or a flip"},
		{"no image", map[string]any{"width": 10}, "image data is required"},
	} {
		got := imagePresetError(t, compiled, "image_resize", entry.arguments)
		if !strings.Contains(got, entry.want) {
			t.Fatalf("%s: error %q does not mention %q", entry.name, got, entry.want)
		}
	}
}

// TestPreset_ImageToolsReadProgressiveJPEG records why the pixels live in Go:
// image/jpeg decodes progressive files, so the preset does not have to refuse
// what a phone or a camera exported.
func TestPreset_ImageToolsReadProgressiveJPEG(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "progressive.jpg"))
	if err != nil {
		t.Skipf("no progressive fixture: %v", err)
	}
	text, _ := imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": base64.StdEncoding.EncodeToString(data)})
	if !strings.Contains(text, "JPEG 80x60") {
		t.Fatalf("a progressive JPEG was not read:\n%s", text)
	}
}

// TestPreset_ImageToolsAcceptJPEGPayload is a regression for the path guard: a
// JPEG's base64 always starts "/9j/", which an earlier guard read as a file path.
func TestPreset_ImageToolsAcceptJPEGPayload(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestJPEG(t, testImage(32, 24), 90)
	if !strings.HasPrefix(payload, "/9j/") {
		t.Skipf("this encoder produces %q, which does not exercise the guard", payload[:4])
	}
	text, _ := imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": payload})
	if !strings.Contains(text, "JPEG 32x24") {
		t.Fatalf("a JPEG payload was not decoded:\n%s", text)
	}
}

func TestPreset_ImageToolsReadBaselineJPEG(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	source := testImage(64, 48)
	payload := encodeTestJPEG(t, source, 90)
	text, _ := imagePresetTool(t, compiled, "image_analyze", map[string]any{"image": payload})
	for _, want := range []string{"JPEG 64x48", "4:2:0", "no transparency"} {
		if !strings.Contains(text, want) {
			t.Fatalf("JPEG analysis is missing %q:\n%s", want, text)
		}
	}
	// The mean colour of a smooth gradient must survive the round trip.
	if strings.Contains(text, "rgb(0, 0, 0)") {
		t.Fatalf("mean colour looks unset:\n%s", text)
	}
}

// TestPreset_ImageToolsReportTheAnthropicShape pins the endpoint behaviour: an
// image is attached where the protocol carries one, and the other endpoints get
// the summary plus the data URL only when it was asked for.
func TestPreset_ImageToolsReportTheAnthropicShape(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	payload := encodeTestPNG(t, testImage(40, 30))
	invoke := func(endpoint string, arguments map[string]any) map[string]any {
		t.Helper()
		return imagePresetInvoke(t, compiled, endpoint, "image_crop", arguments)
	}
	arguments := map[string]any{"image": payload, "x": 0, "y": 0, "width": 10, "height": 10}

	// Anthropic: auto attaches the image.
	result := invoke("anthropic.messages", arguments)
	parts, ok := result["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("anthropic auto result = %+v", result["content"])
	}
	imagePart, _ := parts[1].(map[string]any)
	if imagePart["type"] != "image" {
		t.Fatalf("anthropic auto did not attach the image: %+v", parts[1])
	}

	// Chat completions: auto is text only.
	result = invoke("chat.completions", arguments)
	if _, ok := result["content"].(string); !ok {
		t.Fatalf("chat auto should be a summary string, got %+v", result["content"])
	}
	if strings.Contains(result["content"].(string), "base64") {
		t.Fatalf("chat auto leaked image bytes into the summary")
	}
	for _, want := range []string{"crop: 0,0 10x10", "output: PNG 10x10"} {
		if !strings.Contains(result["content"].(string), want) {
			t.Fatalf("summary is missing %q:\n%s", want, result["content"])
		}
	}

	// An explicit dataUrl request carries the image on both.
	for _, endpoint := range []string{"chat.completions", "anthropic.messages"} {
		result = invoke(endpoint, map[string]any{
			"image": payload, "x": 0, "y": 0, "width": 10, "height": 10, "output": "dataUrl",
		})
		parts, ok := result["content"].([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("%s dataUrl result = %+v", endpoint, result["content"])
		}
	}
}

// TestPreset_ImageToolsRefuseAnOversizedPayloadWithItsOwnLimit pins the failure a
// user meets when an image is too large to cross the worker's protocol line: the
// error has to name the limit and the way out, because the alternative - letting
// the worker die mid-line - reports nothing actionable, and the model cannot
// follow advice it never receives.
func TestPreset_ImageToolsRefuseAnOversizedPayloadWithItsOwnLimit(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	// A payload past the extension's own room (the bundle shares the line), built
	// as a valid-looking PNG prefix plus filler so it gets past the cheap checks.
	room := compiled.HookInputRoom()
	if room <= 0 {
		t.Fatal("image-tools reports no room at all")
	}
	payload := "data:image/png;base64," + strings.Repeat("A", room)
	got := imagePresetError(t, compiled, "image_analyze", map[string]any{"image": payload})
	for _, want := range []string{"MiB", "downscale"} {
		if !strings.Contains(got, want) {
			t.Fatalf("oversized payload error %q does not mention %q", got, want)
		}
	}
	// A tool call just under the limit must still reach the tool itself, so the
	// guard is not refusing everything.
	if _, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{Endpoint: "chat.completions"},
		json.RawMessage(`{"name":"image_analyze","arguments":{"image":"aGVsbG8="}}`)); err != nil {
		if strings.Contains(err.Error(), "too large") {
			t.Fatalf("a small call was refused for size: %v", err)
		}
	}
}

// TestPreset_ImageToolsRoomAccountsForTheBundle pins why the limit is per
// extension: the bundle travels on the same line, so a large preset has less room
// and the number the caller is told has to reflect that.
func TestPreset_ImageToolsRoomAccountsForTheBundle(t *testing.T) {
	small := presetBundle(t, "image-tools")
	big := presetBundle(t, "math-tools")
	if small == nil || big == nil {
		t.Skip("the presets are not in the catalog")
	}
	if len(big.Bundle) <= len(small.Bundle) {
		t.Skip("the bundles are too similar for this comparison to mean anything")
	}
	if big.HookInputRoom() >= small.HookInputRoom() {
		t.Fatalf("a larger bundle reports no less room: math-tools %d vs image-tools %d",
			big.HookInputRoom(), small.HookInputRoom())
	}
	if big.HookInputRoom() <= 0 {
		t.Fatalf("math-tools has no room left at all: %d", big.HookInputRoom())
	}
}

// TestHookRoom_MeasuresTheRealEnvelope pins the arithmetic H6 was wrong about:
// the room has to account for the manifest config and for the JSON escaping the
// bundle grows on the wire, or an input the guard accepts still overflows the
// line and kills the worker (a 502 on a request hook).
func TestHookRoom_MeasuresTheRealEnvelope(t *testing.T) {
	root := t.TempDir()
	writeExtensionTree(t, root, map[string]string{
		"plain/manifest.yaml": "id: plain\nenabled: true\n",
		"plain/index.js":      `export default { async onRequest(ctx, r) { return r; } };`,
	})
	// A config that is large in the envelope and a bundle that is expensive to
	// escape are the two cases the old estimate missed.
	writeExtensionTree(t, root, map[string]string{
		"heavy/manifest.yaml": "id: heavy\nenabled: true\nconfig:\n  notes: \"" + strings.Repeat("x", 200<<10) + "\"\n",
		"heavy/index.js":      `export default { async onRequest(ctx, r) { return r; } };`,
	})
	writeExtensionTree(t, root, map[string]string{
		"escaped/manifest.yaml": "id: escaped\nenabled: true\n",
		// <, > and & are what json.Marshal expands, so a source dense in them
		// makes the bundle much longer on the wire than in memory.
		"escaped/index.js": "const HTML = '" + strings.Repeat("<div class=x>&</div>", 4000) +
			"';\nexport default { async onRequest(ctx, r) { return r; } };",
	})
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	plain, ok := manager.Compiled("plain")
	if !ok {
		t.Fatal("plain did not compile")
	}
	// For each extension: at exactly the reported room the call must succeed,
	// and one byte past it must be refused by the guard (not by the worker).
	for _, id := range []string{"plain", "heavy", "escaped"} {
		item, ok := manager.Compiled(id)
		if !ok {
			t.Fatalf("%s did not compile", id)
		}
		room := item.HookInputRoom()
		if room <= 0 {
			t.Fatalf("%s reports no room", id)
		}
		// A raw JSON object of exactly `room` bytes: {"pad":"...."}
		pad := room - len(`{"pad":""}`)
		if pad < 0 {
			t.Fatalf("%s: room %d is too small for the test payload", id, room)
		}
		atRoom := json.RawMessage(`{"pad":"` + strings.Repeat("y", pad) + `"}`)
		if len(atRoom) != room {
			t.Fatalf("%s: built %d bytes, want %d", id, len(atRoom), room)
		}
		// The guard is checked directly rather than by sending the payload: an
		// 8 MB request does fit the protocol line, but the worker's own memory
		// limit is smaller than the line, so a worker run would fail for a
		// reason that has nothing to do with the arithmetic under test.
		if err := checkHookInput(id, "onRequest", room, atRoom); err != nil {
			t.Fatalf("%s: an input of exactly the reported room was refused: %v", id, err)
		}
		over := json.RawMessage(`{"pad":"` + strings.Repeat("y", pad+1) + `"}`)
		err := checkHookInput(id, "onRequest", room, over)
		if err == nil {
			t.Fatalf("%s: an oversized input was accepted", id)
		}
		if !errors.Is(err, ErrHookInputTooLarge) {
			t.Fatalf("%s: the refusal did not come from the guard: %v", id, err)
		}
	}
	// The estimate has to shrink when the envelope grows.
	if heavyRoom, plainRoom := mustCompiled(t, manager, "heavy").HookInputRoom(), plain.HookInputRoom(); heavyRoom >= plainRoom {
		t.Fatalf("a large config does not reduce the room: heavy %d, plain %d", heavyRoom, plainRoom)
	}
	if escapedRoom, plainRoom := mustCompiled(t, manager, "escaped").HookInputRoom(), plain.HookInputRoom(); escapedRoom >= plainRoom {
		t.Fatalf("an escape-heavy bundle does not reduce the room: escaped %d, plain %d", escapedRoom, plainRoom)
	}
}

func mustCompiled(t *testing.T, manager *Manager, id string) *Compiled {
	t.Helper()
	item, ok := manager.Compiled(id)
	if !ok {
		t.Fatalf("%s did not compile", id)
	}
	return item
}

// TestPreset_ImageToolsInputLimitIsReachable pins the relation between the
// documented default and what a tool call can carry. A default larger than the
// protocol allows is a default that can never be honoured: the caller would meet
// the transport limit instead of the configured one.
func TestPreset_ImageToolsInputLimitIsReachable(t *testing.T) {
	compiled := presetBundle(t, "image-tools")
	if compiled == nil {
		t.Fatal("image-tools is not in the catalog")
	}
	room := compiled.HookInputRoom()
	if room <= 0 {
		t.Fatal("image-tools has no room")
	}
	// The preset's default, in bytes of decoded image, must fit in the room a
	// tool call has once its base64 expansion (4/3) is accounted for.
	const defaultBytes = 5120 * 1024
	encodedSize := (defaultBytes*4)/3 + len("data:image/png;base64,")
	if encodedSize > room {
		t.Fatalf("the preset's %d KiB default needs %d bytes on the wire, over the %d it has",
			defaultBytes/1024, encodedSize, room)
	}
	// And the daemon's own 32 MiB ceiling is explicitly unreachable, which the
	// settings hint says rather than pretending otherwise.
	preset, ok := PresetByID("image-tools")
	if !ok {
		t.Fatal("no preset")
	}
	var hint string
	for _, field := range preset.Settings {
		if field.Key == "maxInputKiB" {
			hint = field.Hint
		}
	}
	if !strings.Contains(hint, "5.9 MiB") || !strings.Contains(hint, "32 MiB") {
		t.Fatalf("the settings hint does not name both limits: %q", hint)
	}
}

// TestHookRoom_ZeroMeansNothingFits pins the inversion the review found: a room
// of zero used to be read as "no limit", so an extension whose bundle fills the
// reserve window accepted every input and then died on the wire. The bundle is
// too large for any hook input to fit, which is what zero has to mean.
func TestHookRoom_ZeroMeansNothingFits(t *testing.T) {
	// The guard is the unit under test; an input of any size must be refused.
	if err := checkHookInput("huge", "onRequest", 0, json.RawMessage(`{}`)); !errors.Is(err, ErrHookInputTooLarge) {
		t.Fatalf("room 0 did not refuse an empty input: %v", err)
	}
	if err := checkHookInput("huge", "onRequest", -5, json.RawMessage(`{}`)); !errors.Is(err, ErrHookInputTooLarge) {
		t.Fatalf("a negative room did not refuse: %v", err)
	}
	// A positive room is still enforced: two bytes of input do not fit one.
	if err := checkHookInput("tiny", "onRequest", 1, json.RawMessage(`{}`)); !errors.Is(err, ErrHookInputTooLarge) {
		t.Fatalf("a 2-byte input fit a 1-byte room: %v", err)
	}
	if err := checkHookInput("tiny", "onRequest", 2, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a 2-byte input was refused a 2-byte room: %v", err)
	}
}

// TestHookRoom_AccountsForTheContext pins that the per-call context counts
// against the line. The model snapshot travels with every hook call, and a large
// one used up the room the cached measurement had already promised.
func TestHookRoom_AccountsForTheContext(t *testing.T) {
	root := t.TempDir()
	writeExtensionTree(t, root, map[string]string{
		"ctx/manifest.yaml": "id: ctx\nenabled: true\n",
		"ctx/index.js":      `export default { async onRequest(ctx, r) { return r; } };`,
	})
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item := mustCompiled(t, manager, "ctx")
	room := item.HookInputRoom()
	if room <= 0 {
		t.Fatal("no room")
	}
	// A context large enough to matter must shrink the allowance.
	big := ContextForTest(200)
	small := Context{}
	if err := CheckHookInputFor("ctx", "onRequest", room, small, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a small context was refused: %v", err)
	}
	encoded, _ := json.Marshal(big)
	if len(encoded) < 1000 {
		t.Fatalf("the test context is only %d bytes; it cannot demonstrate anything", len(encoded))
	}
	// The context counts against the room, so the same input that fits with a
	// small one must overflow with a large one. The empty context still has a
	// size, so the test measures the difference rather than assuming zero.
	smallCost := len(mustMarshal(t, small))
	bigCost := len(mustMarshal(t, big))
	if bigCost <= smallCost+500 {
		t.Fatalf("the test context is not meaningfully larger: %d vs %d bytes", bigCost, smallCost)
	}
	pad := room - smallCost - len(`{"pad":""}`)
	if pad < 0 {
		t.Fatalf("no room to fill: %d", pad)
	}
	atRoom := json.RawMessage(`{"pad":"` + strings.Repeat("y", pad) + `"}`)
	if err := CheckHookInputFor("ctx", "onRequest", room, small, atRoom); err != nil {
		t.Fatalf("at room with a small context: %v", err)
	}
	if err := CheckHookInputFor("ctx", "onRequest", room, big, atRoom); !errors.Is(err, ErrHookInputTooLarge) {
		t.Fatalf("a large context did not count against the room: %v", err)
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
