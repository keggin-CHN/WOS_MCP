package main

import (
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"math"
)

// toGray converts an image to grayscale values (0-255).
func toGray(img image.Image) [][]uint8 {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	gray := make([][]uint8, h)
	for y := 0; y < h; y++ {
		gray[y] = make([]uint8, w)
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()

			gray[y][x] = uint8((0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 256)
		}
	}
	return gray
}

// gaussianBlur applies a simple 5x5 Gaussian blur.
func gaussianBlur(src [][]uint8) [][]uint8 {
	h := len(src)
	if h == 0 {
		return src
	}
	w := len(src[0])
	dst := make([][]uint8, h)
	for y := range dst {
		dst[y] = make([]uint8, w)
	}

	kernel := [5][5]float64{
		{1, 4, 7, 4, 1},
		{4, 16, 26, 16, 4},
		{7, 26, 41, 26, 7},
		{4, 16, 26, 16, 4},
		{1, 4, 7, 4, 1},
	}
	kSum := 273.0

	for y := 2; y < h-2; y++ {
		for x := 2; x < w-2; x++ {
			var sum float64
			for ky := -2; ky <= 2; ky++ {
				for kx := -2; kx <= 2; kx++ {
					sum += float64(src[y+ky][x+kx]) * kernel[ky+2][kx+2]
				}
			}
			dst[y][x] = uint8(sum / kSum)
		}
	}
	return dst
}

// sobelGradients computes Sobel gradient magnitude and direction.
func sobelGradients(src [][]uint8) ([][]float64, [][]float64) {
	h := len(src)
	w := len(src[0])
	mag := make([][]float64, h)
	dir := make([][]float64, h)
	for y := range mag {
		mag[y] = make([]float64, w)
		dir[y] = make([]float64, w)
	}

	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			gx := -float64(src[y-1][x-1]) + float64(src[y-1][x+1]) +
				-2*float64(src[y][x-1]) + 2*float64(src[y][x+1]) +
				-float64(src[y+1][x-1]) + float64(src[y+1][x+1])

			gy := -float64(src[y-1][x-1]) - 2*float64(src[y-1][x]) - float64(src[y-1][x+1]) +
				float64(src[y+1][x-1]) + 2*float64(src[y+1][x]) + float64(src[y+1][x+1])

			mag[y][x] = math.Sqrt(gx*gx + gy*gy)
			dir[y][x] = math.Atan2(gy, gx)
		}
	}
	return mag, dir
}

// nonMaxSuppression applies non-maximum suppression for edge thinning.
func nonMaxSuppression(mag, dir [][]float64) [][]float64 {
	h := len(mag)
	w := len(mag[0])
	result := make([][]float64, h)
	for y := range result {
		result[y] = make([]float64, w)
	}

	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			angle := dir[y][x] * 180 / math.Pi
			if angle < 0 {
				angle += 180
			}

			var q, r float64
			if (angle >= 0 && angle < 22.5) || (angle >= 157.5 && angle <= 180) {
				q = mag[y][x+1]
				r = mag[y][x-1]
			} else if angle >= 22.5 && angle < 67.5 {
				q = mag[y+1][x+1]
				r = mag[y-1][x-1]
			} else if angle >= 67.5 && angle < 112.5 {
				q = mag[y+1][x]
				r = mag[y-1][x]
			} else {
				q = mag[y-1][x+1]
				r = mag[y+1][x-1]
			}

			if mag[y][x] >= q && mag[y][x] >= r {
				result[y][x] = mag[y][x]
			}
		}
	}
	return result
}

// cannyEdge performs Canny edge detection (equivalent to cv2.Canny(img, low, high)).
func cannyEdge(gray [][]uint8, lowThresh, highThresh float64) [][]uint8 {
	blurred := gaussianBlur(gray)
	mag, dir := sobelGradients(blurred)
	suppressed := nonMaxSuppression(mag, dir)

	h := len(gray)
	w := len(gray[0])
	edges := make([][]uint8, h)
	for y := range edges {
		edges[y] = make([]uint8, w)
	}

	// Double threshold + hysteresis
	const strong uint8 = 255
	const weak uint8 = 75

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if suppressed[y][x] >= highThresh {
				edges[y][x] = strong
			} else if suppressed[y][x] >= lowThresh {
				edges[y][x] = weak
			}
		}
	}

	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			if edges[y][x] == weak {
				connected := false
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if edges[y+dy][x+dx] == strong {
							connected = true
						}
					}
				}
				if connected {
					edges[y][x] = strong
				} else {
					edges[y][x] = 0
				}
			}
		}
	}

	return edges
}

// boundingRect finds the bounding rectangle of non-zero pixels (like cv2.boundingRect).
func boundingRect(edges [][]uint8) (int, int, int, int) {
	h := len(edges)
	if h == 0 {
		return 0, 0, 0, 0
	}
	w := len(edges[0])
	minX, minY := w, h
	maxX, maxY := 0, 0

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if edges[y][x] > 0 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	if maxX < minX || maxY < minY {
		return 0, 0, 0, 0
	}
	return minX, minY, maxX - minX + 1, maxY - minY + 1
}

// cropEdges extracts a sub-region from edge map.
func cropEdges(edges [][]uint8, x, y, w, h int) [][]uint8 {
	cropped := make([][]uint8, h)
	for dy := 0; dy < h; dy++ {
		cropped[dy] = make([]uint8, w)
		for dx := 0; dx < w; dx++ {
			if y+dy < len(edges) && x+dx < len(edges[0]) {
				cropped[dy][dx] = edges[y+dy][x+dx]
			}
		}
	}
	return cropped
}

// matchTemplate performs template matching using normalized cross-correlation
// (equivalent to cv2.matchTemplate with TM_CCOEFF_NORMED).
func matchTemplate(bg, tmpl [][]uint8) (int, int) {
	bgH := len(bg)
	bgW := len(bg[0])
	tmplH := len(tmpl)
	tmplW := len(tmpl[0])

	if tmplH > bgH || tmplW > bgW || tmplH == 0 || tmplW == 0 {
		return 0, 0
	}

	// Compute template mean
	var tmplSum float64
	tmplCount := float64(tmplH * tmplW)
	for y := 0; y < tmplH; y++ {
		for x := 0; x < tmplW; x++ {
			tmplSum += float64(tmpl[y][x])
		}
	}
	tmplMean := tmplSum / tmplCount

	// Precompute template deviation
	var tmplDev float64
	for y := 0; y < tmplH; y++ {
		for x := 0; x < tmplW; x++ {
			d := float64(tmpl[y][x]) - tmplMean
			tmplDev += d * d
		}
	}
	tmplDev = math.Sqrt(tmplDev)

	bestScore := -1.0
	bestX, bestY := 0, 0

	for sy := 0; sy <= bgH-tmplH; sy++ {
		for sx := 0; sx <= bgW-tmplW; sx++ {
			// Compute bg patch mean
			var patchSum float64
			for y := 0; y < tmplH; y++ {
				for x := 0; x < tmplW; x++ {
					patchSum += float64(bg[sy+y][sx+x])
				}
			}
			patchMean := patchSum / tmplCount

			// NCC
			var num, patchDev float64
			for y := 0; y < tmplH; y++ {
				for x := 0; x < tmplW; x++ {
					dPatch := float64(bg[sy+y][sx+x]) - patchMean
					dTmpl := float64(tmpl[y][x]) - tmplMean
					num += dPatch * dTmpl
					patchDev += dPatch * dPatch
				}
			}
			patchDev = math.Sqrt(patchDev)
			denom := tmplDev * patchDev
			if denom == 0 {
				continue
			}
			score := num / denom
			if score > bestScore {
				bestScore = score
				bestX = sx
				bestY = sy
			}
		}
	}
	return bestX, bestY
}

// solveSliderOffset takes two base64-decoded images and returns the X offset.
// This is a pure Go port of the Python OpenCV-based slider solver.
func solveSliderOffset(bgImg, sliderImg image.Image) int {
	bgGray := toGray(bgImg)
	sliderGray := toGray(sliderImg)

	bgEdges := cannyEdge(bgGray, 100, 200)
	sliderEdges := cannyEdge(sliderGray, 100, 200)

	x, y, w, h := boundingRect(sliderEdges)
	if w > 0 && h > 0 {
		sliderEdges = cropEdges(sliderEdges, x, y, w, h)
	}

	matchX, _ := matchTemplate(bgEdges, sliderEdges)
	return matchX
}

// edgesToImage converts edge map to image.Image for debugging.
func edgesToImage(edges [][]uint8) *image.Gray {
	h := len(edges)
	if h == 0 {
		return image.NewGray(image.Rect(0, 0, 0, 0))
	}
	w := len(edges[0])
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: edges[y][x]})
		}
	}
	return img
}
