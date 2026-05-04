package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func rgbaImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 10), G: uint8(y * 10), B: 100, A: 255})
		}
	}
	return img
}

func rgbaWithAlpha(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	return img
}

func TestToNRGBAAndCloneNRGBA(t *testing.T) {
	t.Parallel()
	src := rgbaImage(3, 3)
	n := toNRGBA(src)
	if n.Bounds().Dx() != 3 {
		t.Fatal()
	}
	c := cloneNRGBA(n)
	c.Set(0, 0, color.NRGBA{A: 0})
	if n.At(0, 0) == c.At(0, 0) {
		t.Fatal("clone should be independent")
	}
}

func TestHasAlphaAndFlattenIfNeeded(t *testing.T) {
	t.Parallel()
	opaque := rgbaImage(2, 2)
	if hasAlpha(opaque) {
		t.Fatal()
	}
	flat := flattenIfNeeded(opaque)
	if flat.Bounds().Dx() != 2 {
		t.Fatal()
	}
	trans := rgbaWithAlpha(2, 2)
	if !hasAlpha(trans) {
		t.Fatal()
	}
	flat2 := flattenIfNeeded(trans)
	if flat2.Bounds().Dx() != 2 {
		t.Fatal()
	}
}

func TestResizeImage(t *testing.T) {
	t.Parallel()
	img := rgbaImage(10, 20)
	out := resizeImage(img, 5)
	if out.Bounds().Dx() != 5 || out.Bounds().Dy() != 10 {
		t.Fatalf("%v", out.Bounds())
	}
	// zero width / bad target falls back
	z := image.NewNRGBA(image.Rect(0, 0, 0, 0))
	out2 := resizeImage(z, 100)
	if out2.Bounds().Dx() != 0 {
		t.Fatal()
	}
	out3 := resizeImage(img, 0)
	if out3.Bounds().Dx() != 10 {
		t.Fatal()
	}
}

func TestRotateTransforms(t *testing.T) {
	t.Parallel()
	img := rgbaImage(2, 3)
	r180 := rotate180(img)
	if r180.At(0, 0) != img.At(1, 2) {
		t.Fatal("rotate180")
	}
	r90 := rotate90CW(img)
	if r90.Bounds().Dx() != 3 || r90.Bounds().Dy() != 2 {
		t.Fatal(r90.Bounds())
	}
	r270 := rotate90CCW(img)
	if r270.Bounds().Dx() != 3 || r270.Bounds().Dy() != 2 {
		t.Fatal(r270.Bounds())
	}
}

func TestApplyEXIFOrientationAndExifHelpers(t *testing.T) {
	t.Parallel()
	img := rgbaImage(4, 4)
	// random bytes -> orientation 1 -> unchanged
	out := applyEXIFOrientation(img, []byte{1, 2, 3})
	if out.Bounds() != img.Bounds() {
		t.Fatal()
	}
	if exifOrientation([]byte{0}) != 1 {
		t.Fatal()
	}
	if len(extractAllEXIF([]byte{0})) != 0 {
		t.Fatal()
	}
}

func TestValidateSourceImageSizeRejectsTooManyPixels(t *testing.T) {
	t.Parallel()
	if err := validateSourceImageSize(10, 10, 99); err == nil {
		t.Fatal("expected pixel limit error")
	}
	if err := validateSourceImageSize(10, 10, 100); err != nil {
		t.Fatalf("expected image at limit to pass: %v", err)
	}
	if err := validateSourceImageSize(10, 10, 0); err != nil {
		t.Fatalf("expected disabled limit to pass: %v", err)
	}
}

func TestEncodeByExtFormats(t *testing.T) {
	t.Parallel()
	img := rgbaImage(8, 8)
	for _, tc := range []struct {
		ext string
	}{
		{".jpg"}, {".jpeg"}, {".png"}, {".gif"}, {".tif"}, {".tiff"}, {".webp"}, {".unknown"},
	} {
		b, err := encodeByExt(img, tc.ext)
		if err != nil || len(b) == 0 {
			t.Fatalf("%s: %v len=%d", tc.ext, err, len(b))
		}
	}
}

func TestEncodeWebP(t *testing.T) {
	t.Parallel()
	b, err := encodeWebP(rgbaImage(4, 4))
	if err != nil || len(b) < 10 {
		t.Fatal(err, len(b))
	}
}

func TestAdjustOpacityAndApplyWatermark(t *testing.T) {
	t.Parallel()
	base := rgbaImage(40, 40)
	wm := rgbaImage(10, 10)
	out := applyWatermark(base, wm, 0.2, 0.01, 0.5)
	if out == nil || out.Bounds().Dx() != 40 {
		t.Fatal()
	}
	// nil branches
	if applyWatermark(nil, wm, 0.2, 0, 1) != nil {
		t.Fatal()
	}
	if applyWatermark(base, nil, 0.2, 0, 1) != base {
		t.Fatal()
	}
	// defaults for scale/margin/opacity
	out2 := applyWatermark(base, wm, -1, -1, -1)
	if out2.Bounds().Dx() != 40 {
		t.Fatal()
	}
	out3 := applyWatermark(base, wm, 0.2, 0, 2)
	if out3 == nil {
		t.Fatal()
	}
	op := adjustOpacity(wm, 0.5)
	if op == nil || len(op.Pix) != len(wm.Pix) {
		t.Fatal()
	}
}

func TestNewProcessor_NoWatermark(t *testing.T) {
	t.Parallel()
	p, err := NewProcessor(Config{EnableWatermark: false}, nil)
	if err != nil || p == nil || p.watermark != nil {
		t.Fatalf("%v %#v", err, p)
	}
}

func TestNewProcessor_WithWatermark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wm.png")
	var buf bytes.Buffer
	_ = png.Encode(&buf, rgbaImage(8, 8))
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := NewProcessor(Config{
		EnableWatermark: true,
		WatermarkPath:   path,
	}, nil)
	if err != nil || p == nil || p.watermark == nil {
		t.Fatalf("%v", err)
	}
}

func TestNewProcessor_WatermarkMissingFile(t *testing.T) {
	t.Parallel()
	_, err := NewProcessor(Config{EnableWatermark: true, WatermarkPath: "/no/such/file.png"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
