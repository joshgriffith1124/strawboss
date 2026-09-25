package clipboard

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"
)

func TestFitSmallPNGPassesThrough(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 5))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	f, err := Fit(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Data, data) {
		t.Error("passthrough data is not byte-identical")
	}
	if f.MediaType != "image/png" || f.Width != 10 || f.Height != 5 {
		t.Errorf("got %+v, want image/png 10x5", f)
	}
}

func TestFitDownscalesLongEdge(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3000, 1000))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	f, err := Fit(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if f.Width != MaxEdge {
		t.Errorf("width = %d, want %d", f.Width, MaxEdge)
	}
	if f.Height < 522 || f.Height > 524 {
		t.Errorf("height = %d, want 523±1", f.Height)
	}
	if _, _, err := image.Decode(bytes.NewReader(f.Data)); err != nil {
		t.Errorf("fitted data does not decode: %v", err)
	}
}

func TestFitGifBecomesPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	f, err := Fit(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if f.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png", f.MediaType)
	}
}

func TestFitGarbageFails(t *testing.T) {
	_, err := Fit([]byte("not an image"))
	if err == nil || !strings.Contains(err.Error(), "clipboard: decode image") {
		t.Fatalf("err = %v, want error wrapping 'clipboard: decode image'", err)
	}
}

func TestDownscalePreservesSolidColor(t *testing.T) {
	want := color.RGBA{R: 204, G: 102, B: 51, A: 255}
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, want)
		}
	}
	dst := downscale(src, 10, 10)
	wr, wg, wb, wa := want.RGBA()
	cr, cg, cb, ca := dst.At(3, 4).RGBA()
	if cr != wr || cg != wg || cb != wb || ca != wa {
		t.Errorf("pixel (3,4) = %d,%d,%d,%d, want %d,%d,%d,%d", cr, cg, cb, ca, wr, wg, wb, wa)
	}
}
