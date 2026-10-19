package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

func Normalize(r io.Reader, maxEdge int, quality int) ([]byte, error) {

	//strip exif metadata
	src, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("imaging: failed to decode image: %w", err)
	}

	//scale down image
	scaledImg := scale(src, maxEdge)

	// encode to jpeg
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaledImg, &jpeg.Options{
		Quality: quality,
	}); err != nil {
		return nil, fmt.Errorf("imaging: failed to encode image into jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

func scale(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()

	newW := w
	newH := h

	if w > maxEdge || h > maxEdge {
		if w >= h {
			// Landscape or square: width is the longest edge
			newW = maxEdge
			ratio := float64(maxEdge) / float64(w)
			newH = int(float64(h) * ratio)
		} else {
			// Portrait: height is the longest edge
			newH = maxEdge
			ratio := float64(maxEdge) / float64(h)
			newW = int(float64(w) * ratio)
		}
	}

	newW = max(newW, 1)
	newH = max(newH, 1)

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	//white paint
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	// scaling - catmullrom for better results
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	return dst
}
