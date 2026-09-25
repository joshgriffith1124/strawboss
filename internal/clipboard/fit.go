package clipboard

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	_ "image/gif"
)

const MaxEdge = 1568

// MaxBytes caps encoded size in raw bytes; base64 of this stays under the API's 5 MB limit.
const MaxBytes = 3_750_000

// Fitted is an image ready to send.
type Fitted struct {
	MediaType     string // "image/png" or "image/jpeg"
	Data          []byte
	Width, Height int
}

// Fit decodes data (png, jpeg or gif), returning it unchanged (with its media
// type and size) when it fits both limits and is png or jpeg; otherwise it
// downscales the long edge to at most MaxEdge (aspect kept, never upscaled),
// encodes as PNG, falls back to JPEG q85 then q60, and errors if still over.
func Fit(data []byte) (Fitted, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Fitted{}, fmt.Errorf("clipboard: decode image: %w", err)
	}
	fits := len(data) <= MaxBytes && cfg.Width <= MaxEdge && cfg.Height <= MaxEdge
	if fits && (format == "png" || format == "jpeg") {
		return Fitted{"image/" + format, data, cfg.Width, cfg.Height}, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Fitted{}, fmt.Errorf("clipboard: decode image: %w", err)
	}
	w, h := targetSize(cfg.Width, cfg.Height)
	if w < cfg.Width || h < cfg.Height {
		img = downscale(img, w, h)
	}
	pngData, pngErr := encodePNG(img)
	if pngErr == nil && len(pngData) <= MaxBytes {
		return Fitted{"image/png", pngData, w, h}, nil
	}
	for _, q := range []int{85, 60} {
		jpg, err := encodeJPEG(img, q)
		if err == nil && len(jpg) <= MaxBytes {
			return Fitted{"image/jpeg", jpg, w, h}, nil
		}
	}
	if pngErr != nil {
		return Fitted{}, pngErr
	}
	return Fitted{}, fmt.Errorf("clipboard: image still exceeds %d bytes after downscaling", MaxBytes)
}

// targetSize: long edge at most MaxEdge, aspect kept, never upscaling.
func targetSize(w, h int) (int, int) {
	if w <= MaxEdge && h <= MaxEdge {
		return w, h
	}
	if w >= h {
		return MaxEdge, max(1, (h*MaxEdge+w/2)/w)
	}
	return max(1, (w*MaxEdge+h/2)/h), MaxEdge
}

// downscale box-filters (area-averages) src into a w×h image.
func downscale(src image.Image, w, h int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := y * sh / h
		y1 := (y + 1) * sh / h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := x * sw / w
			x1 := (x + 1) * sw / w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			i := 4 * (y*w + x)
			dst.Pix[i] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(b / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("clipboard: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("clipboard: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}
