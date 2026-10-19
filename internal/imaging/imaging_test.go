package imaging

import (
	"bytes"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"testing"
)

func createTestImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	return img
}

func TestScale(t *testing.T) {
	tests := []struct {
		name       string
		origW      int
		origH      int
		maxEdge    int
		wantW      int
		wantH      int
	}{
		{
			name:    "landscape image downscaled correctly",
			origW:   4000,
			origH:   2000,
			maxEdge: 2048,
			wantW:   2048,
			wantH:   1024,
		},
		{
			name:    "portrait image downscaled correctly",
			origW:   2000,
			origH:   4000,
			maxEdge: 2048,
			wantW:   1024,
			wantH:   2048,
		},
		{
			name:    "square image downscaled correctly",
			origW:   3000,
			origH:   3000,
			maxEdge: 2048,
			wantW:   2048,
			wantH:   2048,
		},
		{
			name:    "image within bounds not scaled up",
			origW:   800,
			origH:   600,
			maxEdge: 2048,
			wantW:   800,
			wantH:   600,
		},
		{
			name:    "exact max edge not changed",
			origW:   2048,
			origH:   1024,
			maxEdge: 2048,
			wantW:   2048,
			wantH:   1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := createTestImage(tt.origW, tt.origH)
			scaled := scale(src, tt.maxEdge)
			bounds := scaled.Bounds()

			if bounds.Dx() != tt.wantW || bounds.Dy() != tt.wantH {
				t.Errorf("scale(%dx%d, maxEdge=%d) got dimensions %dx%d, want %dx%d",
					tt.origW, tt.origH, tt.maxEdge, bounds.Dx(), bounds.Dy(), tt.wantW, tt.wantH)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	t.Run("successfully normalizes PNG to JPEG", func(t *testing.T) {
		src := createTestImage(3000, 1500)
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatalf("failed to encode test png: %v", err)
		}

		out, err := Normalize(&buf, 2048, 85)
		if err != nil {
			t.Fatalf("Normalize returned unexpected error: %v", err)
		}

		decoded, format, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("failed to decode normalized image config: %v", err)
		}

		if format != "jpeg" {
			t.Errorf("expected format jpeg, got %s", format)
		}
		if decoded.Width != 2048 || decoded.Height != 1024 {
			t.Errorf("expected dimensions 2048x1024, got %dx%d", decoded.Width, decoded.Height)
		}
	})

	t.Run("returns error on invalid image stream", func(t *testing.T) {
		invalidReader := bytes.NewReader([]byte("not an image"))
		_, err := Normalize(invalidReader, 2048, 85)
		if err == nil {
			t.Error("expected error for invalid image stream, got nil")
		}
	})
}
