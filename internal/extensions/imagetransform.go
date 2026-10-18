package extensions

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"sort"
	"strings"
)

// The operations behind images.apply: crop, resize, rotate, flip, analyse and
// encode. Everything runs on premultiplied RGBA (Go's own layout), which is what
// makes resizing correct around transparent edges - averaging straight alpha
// would pull colour out of invisible pixels and leave a fringe.

// toRGBA normalises any decoded image to RGBA with the origin at (0, 0).
func toRGBA(source image.Image) *image.RGBA {
	if rgba, ok := source.(*image.RGBA); ok && rgba.Bounds().Min.X == 0 && rgba.Bounds().Min.Y == 0 {
		return rgba
	}
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	return target
}

// applyExifOrientation turns the stored pixels into the picture a viewer shows.
func applyExifOrientation(source *image.RGBA, orientation int) *image.RGBA {
	switch orientation {
	case 2:
		return flipImage(source, true)
	case 3:
		return rotateImage(source, 180)
	case 4:
		return flipImage(source, false)
	case 5:
		// "Transpose": mirror across the top-left to bottom-right diagonal.
		return rotateImage(flipImage(source, true), 270)
	case 6:
		return rotateImage(source, 90)
	case 7:
		// "Transverse": mirror across the other diagonal.
		return rotateImage(flipImage(source, true), 90)
	case 8:
		return rotateImage(source, 270)
	default:
		return source
	}
}

// cropImage clamps the requested rectangle to the picture and explains every
// adjustment, so a caller that asked for something partly outside learns what it
// actually got instead of silently receiving a different region.
func cropImage(source *image.RGBA, requested imageRectangle) (imageRectangle, []string) {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if requested.Width <= 0 || requested.Height <= 0 {
		// An empty rectangle is still reported as a rectangle, so the caller
		// returns "the crop is empty" rather than an encoder error about a 0x0
		// image.
		return imageRectangle{X: clampInt2(requested.X, 0, width), Y: clampInt2(requested.Y, 0, height)},
			[]string{"the requested width and height are not positive"}
	}
	left := clampInt2(requested.X, 0, width-1)
	top := clampInt2(requested.Y, 0, height-1)
	right := clampInt2(requested.X+requested.Width, left+1, width)
	bottom := clampInt2(requested.Y+requested.Height, top+1, height)
	clipped := imageRectangle{X: left, Y: top, Width: right - left, Height: bottom - top}
	var adjusted []string
	if left != requested.X || top != requested.Y {
		adjusted = append(adjusted, fmt.Sprintf("the origin was clamped from %d,%d to %d,%d", requested.X, requested.Y, left, top))
	}
	if clipped.Width != requested.Width || clipped.Height != requested.Height {
		adjusted = append(adjusted, fmt.Sprintf("the size was clamped from %dx%d to %dx%d", requested.Width, requested.Height, clipped.Width, clipped.Height))
	}
	return clipped, adjusted
}

func cropTo(source *image.RGBA, rectangle imageRectangle) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, rectangle.Width, rectangle.Height))
	for y := 0; y < rectangle.Height; y++ {
		sourceRow := source.Pix[(rectangle.Y+y)*source.Stride+rectangle.X*4:]
		targetRow := target.Pix[y*target.Stride:]
		copy(targetRow[:rectangle.Width*4], sourceRow[:rectangle.Width*4])
	}
	return target
}

// resolveTargetSize fills in the side a caller left out, keeping the aspect
// ratio and never producing a zero side.
func resolveTargetSize(sourceWidth, sourceHeight, width, height int) (int, int, error) {
	if width <= 0 && height <= 0 {
		return 0, 0, fmt.Errorf("give a width, a height, or both")
	}
	// Refuse an impossible side before deriving the other from it: the
	// derivation multiplies by the aspect ratio, and a value past the integer
	// range makes that conversion meaningless rather than merely large.
	if width > MaxImageOutputPixels || height > MaxImageOutputPixels {
		return 0, 0, fmt.Errorf("the requested size %dx%d has a side over the largest image this tool will produce (%s pixels a side)",
			width, height, thousands(MaxImageOutputPixels))
	}
	if width <= 0 {
		width = int(math.Round(float64(sourceWidth) * float64(height) / float64(sourceHeight)))
	}
	if height <= 0 {
		height = int(math.Round(float64(sourceHeight) * float64(width) / float64(sourceWidth)))
	}
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height, nil
}

func resizeImage(source *image.RGBA, width, height int, filter string) *image.RGBA {
	switch filter {
	case "nearest":
		return resizeNearest(source, width, height)
	case "bilinear":
		return resizeBilinear(source, width, height)
	default:
		return resizeBox(source, width, height)
	}
}

func resizeNearest(source *image.RGBA, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	sourceWidth, sourceHeight := source.Bounds().Dx(), source.Bounds().Dy()
	scaleX := float64(sourceWidth) / float64(width)
	scaleY := float64(sourceHeight) / float64(height)
	for y := 0; y < height; y++ {
		sourceY := clampInt2(int((float64(y)+0.5)*scaleY), 0, sourceHeight-1)
		for x := 0; x < width; x++ {
			sourceX := clampInt2(int((float64(x)+0.5)*scaleX), 0, sourceWidth-1)
			copy(target.Pix[y*target.Stride+x*4:y*target.Stride+x*4+4], source.Pix[sourceY*source.Stride+sourceX*4:sourceY*source.Stride+sourceX*4+4])
		}
	}
	return target
}

// resizeBox averages every source pixel inside each output cell, which is the
// honest way to shrink: nothing is skipped, so nothing aliases.
func resizeBox(source *image.RGBA, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	sourceWidth, sourceHeight := source.Bounds().Dx(), source.Bounds().Dy()
	scaleX := float64(sourceWidth) / float64(width)
	scaleY := float64(sourceHeight) / float64(height)
	for y := 0; y < height; y++ {
		startY := int(float64(y) * scaleY)
		endY := clampInt2(int(float64(y+1)*scaleY), startY+1, sourceHeight)
		for x := 0; x < width; x++ {
			startX := int(float64(x) * scaleX)
			endX := clampInt2(int(float64(x+1)*scaleX), startX+1, sourceWidth)
			// uint64: a cell can cover 25 million pixels, and 255*25e6 overflows
			// uint32 - a white image downscaled to 1x1 came out mid grey.
			var sumR, sumG, sumB, sumA, count uint64
			for sourceY := startY; sourceY < endY; sourceY++ {
				row := source.Pix[sourceY*source.Stride+startX*4:]
				for sourceX := startX; sourceX < endX; sourceX++ {
					sumR += uint64(row[0])
					sumG += uint64(row[1])
					sumB += uint64(row[2])
					sumA += uint64(row[3])
					count++
					row = row[4:]
				}
			}
			out := target.Pix[y*target.Stride+x*4:]
			out[0] = uint8(sumR / count)
			out[1] = uint8(sumG / count)
			out[2] = uint8(sumB / count)
			out[3] = uint8(sumA / count)
		}
	}
	return target
}

// resizeBilinear interpolates the four neighbours around each output pixel's
// centre, which is the right filter when the target is larger than the source.
func resizeBilinear(source *image.RGBA, width, height int) *image.RGBA {
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	sourceWidth, sourceHeight := source.Bounds().Dx(), source.Bounds().Dy()
	scaleX := float64(sourceWidth) / float64(width)
	scaleY := float64(sourceHeight) / float64(height)
	for y := 0; y < height; y++ {
		sourceY := (float64(y)+0.5)*scaleY - 0.5
		y0 := clampInt2(int(math.Floor(sourceY)), 0, sourceHeight-1)
		y1 := clampInt2(y0+1, 0, sourceHeight-1)
		weightY := clampFloat(sourceY-float64(y0), 0, 1)
		topRow := source.Pix[y0*source.Stride:]
		bottomRow := source.Pix[y1*source.Stride:]
		for x := 0; x < width; x++ {
			sourceX := (float64(x)+0.5)*scaleX - 0.5
			x0 := clampInt2(int(math.Floor(sourceX)), 0, sourceWidth-1)
			x1 := clampInt2(x0+1, 0, sourceWidth-1)
			weightX := clampFloat(sourceX-float64(x0), 0, 1)
			out := target.Pix[y*target.Stride+x*4:]
			for channel := 0; channel < 4; channel++ {
				top := float64(topRow[x0*4+channel])*(1-weightX) + float64(topRow[x1*4+channel])*weightX
				bottom := float64(bottomRow[x0*4+channel])*(1-weightX) + float64(bottomRow[x1*4+channel])*weightX
				out[channel] = uint8(math.Round(top*(1-weightY) + bottom*weightY))
			}
		}
	}
	return target
}

// encodeImage renders the result. PNG is lossless and the right default for
// screenshots, charts and anything with text; JPEG exists because a photograph
// as PNG is several times the size of the tool result that may carry it, and it
// is the format a vision model was trained on anyway.
func encodeImage(source *image.RGBA, format string, quality int) ([]byte, string, error) {
	var buffer bytes.Buffer
	switch normalizeFormat(format) {
	case "jpeg":
		// JPEG has no alpha channel: a transparent PNG has to be flattened onto
		// something, and white is what a document viewer would show.
		flat := source
		if hasTransparency(source) {
			flat = image.NewRGBA(source.Bounds())
			draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
			draw.Draw(flat, flat.Bounds(), source, image.Point{}, draw.Over)
		}
		if quality <= 0 {
			quality = 85
		}
		if quality > 100 {
			quality = 100
		}
		if err := jpeg.Encode(&buffer, flat, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", fmt.Errorf("the result could not be encoded as JPEG: %w", err)
		}
		return buffer.Bytes(), "image/jpeg", nil
	default:
		encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := encoder.Encode(&buffer, source); err != nil {
			return nil, "", fmt.Errorf("the result could not be encoded as PNG: %w", err)
		}
		return buffer.Bytes(), "image/png", nil
	}
}

func normalizeFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpg", "jpeg":
		return "jpeg"
	case "png":
		return "png"
	default:
		return "png"
	}
}

func hasTransparency(source *image.RGBA) bool {
	for index := 3; index < len(source.Pix); index += 4 {
		if source.Pix[index] != 255 {
			return true
		}
	}
	return false
}

// imageStats is what image_analyze reports. The numbers are deliberately plain
// (0-255 luma, shares as fractions) so a model can quote them without knowing
// this format.
type imageStats struct {
	Sampled            int          `json:"sampled"`
	Total              int          `json:"total"`
	Stride             int          `json:"stride"`
	TransparentSamples int          `json:"transparentSamples"`
	MeanRed            int          `json:"meanRed"`
	MeanGreen          int          `json:"meanGreen"`
	MeanBlue           int          `json:"meanBlue"`
	MeanLuma           int          `json:"meanLuma"`
	Brightness         int          `json:"brightness"`
	Contrast           int          `json:"contrast"`
	MinLuma            int          `json:"minLuma"`
	MaxLuma            int          `json:"maxLuma"`
	Histogram          [8]int       `json:"histogram"`
	Detail             int          `json:"detail"`
	LumaGrid           [9]int       `json:"lumaGrid"`
	Dominant           []imageColor `json:"dominant"`
}

type imageColor struct {
	Hex   string  `json:"hex"`
	Red   int     `json:"red"`
	Green int     `json:"green"`
	Blue  int     `json:"blue"`
	Share float64 `json:"share"`
}

// analyzeImage measures the picture from a sample of its pixels: a 12 MP photo
// would otherwise spend its whole budget counting pixels, and the statistics a
// caller acts on do not change at the margin.
func analyzeImage(source *image.RGBA, samples, colors int) *imageStats {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	total := width * height
	if samples <= 0 {
		// A zero budget would divide by zero and overflow the stride to the
		// maximum int, sampling a single pixel and calling it an analysis.
		samples = 262144
	}
	stride := 1
	if total > samples {
		stride = int(math.Ceil(math.Sqrt(float64(total) / float64(samples))))
	}
	if stride < 1 {
		stride = 1
	}
	const buckets = 4096
	counts := make([]int, buckets)
	sumRed := make([]int, buckets)
	sumGreen := make([]int, buckets)
	sumBlue := make([]int, buckets)
	stats := &imageStats{Total: total, Stride: stride}
	var cellCounts [9]int
	histogram := &stats.Histogram
	var lumaSum, lumaSquares float64
	var redSum, greenSum, blueSum float64
	var edges int
	minimum := math.Inf(1)
	maximum := math.Inf(-1)
	visible := 0
	for y := 0; y < height; y += stride {
		for x := 0; x < width; x += stride {
			index := y*source.Stride + x*4
			alpha := source.Pix[index+3]
			stats.Sampled++
			if alpha < 255 {
				stats.TransparentSamples++
				if alpha == 0 {
					// Fully transparent: it carries no colour, so it is counted
					// as transparent but contributes to nothing.
					continue
				}
			}
			// visible counts exactly the pixels the sums below receive, which
			// is what every mean divides by. Counting it here rather than
			// deriving it is what keeps a half-transparent region from being
			// divided away while still in the numerator.
			visible++
			// Un-premultiply: the mean colour of a picture is the colour of its
			// visible pixels, not what the alpha multiplied it by.
			scale := 255.0 / float64(alpha)
			red := float64(source.Pix[index]) * scale
			green := float64(source.Pix[index+1]) * scale
			blue := float64(source.Pix[index+2]) * scale
			if red > 255 {
				red = 255
			}
			if green > 255 {
				green = 255
			}
			if blue > 255 {
				blue = 255
			}
			luma := 0.2126*red + 0.7152*green + 0.0722*blue
			redSum += red
			greenSum += green
			blueSum += blue
			lumaSum += luma
			lumaSquares += luma * luma
			if luma < minimum {
				minimum = luma
			}
			if luma > maximum {
				maximum = luma
			}
			histogram[clampInt2(int(luma/32), 0, 7)]++
			cell := clampInt2(int(float64(y)*3/float64(height)), 0, 2)*3 + clampInt2(int(float64(x)*3/float64(width)), 0, 2)
			stats.LumaGrid[cell] += int(luma)
			cellCounts[cell]++
			if x+stride < width && y+stride < height {
				right := y*source.Stride + (x+stride)*4
				below := (y+stride)*source.Stride + x*4
				edges += absInt(int(source.Pix[index])-int(source.Pix[right])) +
					absInt(int(source.Pix[index+1])-int(source.Pix[right+1])) +
					absInt(int(source.Pix[index+2])-int(source.Pix[right+2]))
				edges += absInt(int(source.Pix[index])-int(source.Pix[below])) +
					absInt(int(source.Pix[index+1])-int(source.Pix[below+1])) +
					absInt(int(source.Pix[index+2])-int(source.Pix[below+2]))
			}
			bucket := int(red)>>4<<8 | int(green)>>4<<4 | int(blue)>>4
			counts[bucket]++
			sumRed[bucket] += int(red)
			sumGreen[bucket] += int(green)
			sumBlue[bucket] += int(blue)
		}
	}
	if visible < 1 {
		// Every sampled pixel was fully transparent: there is no colour, no
		// range and no dominant shade, and saying so is better than reporting
		// the loop's initial values as if they were measurements.
		stats.MinLuma = 0
		stats.MaxLuma = 0
		stats.Detail = 0
		return stats
	}
	stats.MeanRed = int(math.Round(redSum / float64(visible)))
	stats.MeanGreen = int(math.Round(greenSum / float64(visible)))
	stats.MeanBlue = int(math.Round(blueSum / float64(visible)))
	mean := lumaSum / float64(visible)
	variance := lumaSquares/float64(visible) - mean*mean
	if variance < 0 {
		variance = 0
	}
	stats.MeanLuma = int(math.Round(mean))
	stats.Brightness = stats.MeanLuma
	stats.Contrast = int(math.Round(math.Sqrt(variance)))
	stats.MinLuma = int(math.Round(minimum))
	stats.MaxLuma = int(math.Round(maximum))
	stats.Detail = int(math.Round(float64(edges) / float64(visible*6)))
	// A 3x3 grid of average brightness holds the shape of the picture ("dark
	// along the bottom") in nine numbers. It is accumulated in the same pass as
	// everything else, so its denominator is the same set of pixels.
	for cell := 0; cell < 9; cell++ {
		if cellCounts[cell] > 0 {
			stats.LumaGrid[cell] = int(math.Round(float64(stats.LumaGrid[cell]) / float64(cellCounts[cell])))
		}
	}
	type entry struct {
		bucket int
		count  int
	}
	ordered := make([]entry, 0, 64)
	for bucket, count := range counts {
		if count > 0 {
			ordered = append(ordered, entry{bucket: bucket, count: count})
		}
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].count > ordered[right].count })
	limit := colors
	if limit < 1 {
		limit = 6
	}
	if limit > len(ordered) {
		limit = len(ordered)
	}
	for _, item := range ordered[:limit] {
		stats.Dominant = append(stats.Dominant, imageColor{
			Hex:   hexColor(sumRed[item.bucket]/item.count, sumGreen[item.bucket]/item.count, sumBlue[item.bucket]/item.count),
			Red:   sumRed[item.bucket] / item.count,
			Green: sumGreen[item.bucket] / item.count,
			Blue:  sumBlue[item.bucket] / item.count,
			Share: math.Min(1, float64(item.count)/float64(visible)),
		})
	}
	return stats
}

func hexColor(red, green, blue int) string {
	return fmt.Sprintf("#%02x%02x%02x", clampInt2(red, 0, 255), clampInt2(green, 0, 255), clampInt2(blue, 0, 255))
}

func clampInt2(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func clampFloat(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
