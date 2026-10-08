package terminalpet

import (
	"fmt"
	"image"
	"sort"
	"strings"
)

const sixelBandHeight = 6

func encodeSixel(source image.Image) ([]byte, error) {
	if source == nil || source.Bounds().Empty() {
		return nil, fmt.Errorf("sixel image dimensions must be non-zero")
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	pixels := make([]int16, width*height)
	for index := range pixels {
		pixels[index] = -1
	}
	used := make(map[uint8]struct{})
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			red, green, blue, alpha := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			if alpha>>8 < 128 {
				continue
			}
			index := uint8((red>>13)<<5 | (green>>13)<<2 | blue>>14)
			pixels[y*width+x] = int16(index)
			used[index] = struct{}{}
		}
	}
	colors := make([]int, 0, len(used))
	for index := range used {
		colors = append(colors, int(index))
	}
	sort.Ints(colors)

	var output strings.Builder
	output.WriteString("\x1bP9;1;0q")
	fmt.Fprintf(&output, "\"1;1;%d;%d", width, height)
	for _, index := range colors {
		red := ((index >> 5) & 7) * 100 / 7
		green := ((index >> 2) & 7) * 100 / 7
		blue := (index & 3) * 100 / 3
		fmt.Fprintf(&output, "#%d;2;%d;%d;%d", index, red, green, blue)
	}
	for bandTop := 0; bandTop < height; bandTop += sixelBandHeight {
		active := activeSixelColors(pixels, width, height, bandTop, colors)
		for colorPosition, colorIndex := range active {
			fmt.Fprintf(&output, "#%d", colorIndex)
			for x := 0; x < width; x++ {
				mask := byte(0)
				for bit := 0; bit < sixelBandHeight && bandTop+bit < height; bit++ {
					if pixels[(bandTop+bit)*width+x] == int16(colorIndex) {
						mask |= 1 << bit
					}
				}
				output.WriteByte('?' + mask)
			}
			if colorPosition+1 < len(active) {
				output.WriteByte('$')
			}
		}
		if bandTop+sixelBandHeight < height {
			if len(active) > 0 {
				output.WriteByte('$')
			}
			output.WriteByte('-')
		}
	}
	output.WriteString("\x1b\\")
	return []byte(output.String()), nil
}

func activeSixelColors(pixels []int16, width, height, bandTop int, colors []int) []int {
	activeSet := make(map[int16]struct{})
	for y := bandTop; y < min(height, bandTop+sixelBandHeight); y++ {
		for x := 0; x < width; x++ {
			if color := pixels[y*width+x]; color >= 0 {
				activeSet[color] = struct{}{}
			}
		}
	}
	active := make([]int, 0, len(activeSet))
	for _, color := range colors {
		if _, ok := activeSet[int16(color)]; ok {
			active = append(active, color)
		}
	}
	return active
}
