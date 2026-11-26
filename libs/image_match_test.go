package libs

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"sync"
	"testing"
)

func encodeTestPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

func encodeTestJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	return buf.Bytes()
}

func makePattern(width, height int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8((x*37 + y*53 + (x*y)%97) % 256)
			img.SetNRGBA(x, y, color.NRGBA{
				R: v,
				G: uint8((int(v)*3 + 41) % 256),
				B: uint8((int(v)*7 + 13) % 256),
				A: 255,
			})
		}
	}
	return img
}

func putPattern(dst *image.NRGBA, src image.Image, left, top int) {
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			dst.Set(left+x, top+y, src.At(x+src.Bounds().Min.X, y+src.Bounds().Min.Y))
		}
	}
}

func TestImageFrameMatch(t *testing.T) {
	templateImage := makePattern(16, 16)
	screen := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < screen.Bounds().Dy(); y++ {
		for x := 0; x < screen.Bounds().Dx(); x++ {
			screen.SetNRGBA(x, y, color.NRGBA{R: 25, G: 35, B: 45, A: 255})
		}
	}
	putPattern(screen, templateImage, 32, 20)

	frame, err := NewImageFrame(encodeTestPNG(t, screen))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestPNG(t, templateImage))
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.Match(tpl, ImageMatchOptions{Threshold: 0.9})
	if err != nil {
		t.Fatalf("Match returned error: %v, score=%f", err, got.Score)
	}
	if got.X != 40 || got.Y != 28 {
		t.Fatalf("unexpected match center: got (%d,%d), want (40,28), score=%f", got.X, got.Y, got.Score)
	}
	if got.Score < 0.9 || got.Score > 1.000001 {
		t.Fatalf("unexpected score: %f", got.Score)
	}
}

func TestImageFrameMatchAll(t *testing.T) {
	templateImage := makePattern(16, 16)
	screen := image.NewNRGBA(image.Rect(0, 0, 128, 64))
	for y := 0; y < screen.Bounds().Dy(); y++ {
		for x := 0; x < screen.Bounds().Dx(); x++ {
			screen.SetNRGBA(x, y, color.NRGBA{R: 21, G: 31, B: 41, A: 255})
		}
	}
	putPattern(screen, templateImage, 16, 20)
	putPattern(screen, templateImage, 80, 20)

	frame, err := NewImageFrame(encodeTestPNG(t, screen))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestPNG(t, templateImage))
	if err != nil {
		t.Fatal(err)
	}

	got, err := frame.MatchAll(tpl, ImageMatchOptions{Threshold: 0.9, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("MatchAll returned %d results, want 2: %+v", len(got), got)
	}
	centers := map[[2]int]bool{}
	for _, p := range got {
		centers[[2]int{p.X, p.Y}] = true
	}
	if !centers[[2]int{24, 28}] || !centers[[2]int{88, 28}] {
		t.Fatalf("unexpected centers: %+v", got)
	}
}

func TestJPEGDecodeAndMatch(t *testing.T) {
	templateImage := makePattern(24, 20)
	screen := image.NewNRGBA(image.Rect(0, 0, 100, 70))
	for y := 0; y < screen.Bounds().Dy(); y++ {
		for x := 0; x < screen.Bounds().Dx(); x++ {
			screen.SetNRGBA(x, y, color.NRGBA{R: 20, G: 30, B: 40, A: 255})
		}
	}
	putPattern(screen, templateImage, 40, 20)

	frame, err := NewImageFrame(encodeTestJPEG(t, screen))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestJPEG(t, templateImage))
	if err != nil {
		t.Fatal(err)
	}
	got, err := frame.Match(tpl, ImageMatchOptions{Threshold: 0.75})
	if err != nil {
		t.Fatalf("JPEG Match failed: %v (score=%f)", err, got.Score)
	}
	if got.X != 52 || got.Y != 30 {
		t.Fatalf("unexpected JPEG match center: %+v", got)
	}
}

func TestTransparentTemplate(t *testing.T) {
	templateImage := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 4; y < 12; y++ {
		for x := 4; x < 12; x++ {
			v := uint8((x*31 + y*47) % 256)
			templateImage.SetNRGBA(x, y, color.NRGBA{R: v, G: 255 - v, B: v / 2, A: 255})
		}
	}
	screen := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < screen.Bounds().Dy(); y++ {
		for x := 0; x < screen.Bounds().Dx(); x++ {
			screen.SetNRGBA(x, y, color.NRGBA{R: 90, G: 90, B: 90, A: 255})
		}
	}
	putPattern(screen, templateImage, 20, 12)

	frame, err := NewImageFrame(encodeTestPNG(t, screen))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestPNG(t, templateImage))
	if err != nil {
		t.Fatal(err)
	}
	got, err := frame.Match(tpl, ImageMatchOptions{Threshold: 0.9})
	if err != nil {
		t.Fatalf("transparent template failed: %v (score=%f)", err, got.Score)
	}
	if got.X != 28 || got.Y != 20 {
		t.Fatalf("unexpected transparent-template center: %+v", got)
	}
}

func TestTransparentTemplateRejected(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 0})
		}
	}
	_, err := NewImageTemplate(encodeTestPNG(t, img))
	if err != ErrTransparentTemplate {
		t.Fatalf("got %v, want ErrTransparentTemplate", err)
	}
}

func TestImageMatchValidation(t *testing.T) {
	img := makePattern(16, 16)
	frame, err := NewImageFrame(encodeTestPNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestPNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frame.Match(tpl, ImageMatchOptions{Threshold: math.NaN()}); err != ErrInvalidMatchThreshold {
		t.Fatalf("got %v, want ErrInvalidMatchThreshold", err)
	}

	large := makePattern(32, 32)
	largeTpl, err := NewImageTemplate(encodeTestPNG(t, large))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frame.Match(largeTpl, ImageMatchOptions{Threshold: 0.8}); err != ErrTemplateLarger {
		t.Fatalf("got %v, want ErrTemplateLarger", err)
	}
}

func TestConcurrentMatchReadOnly(t *testing.T) {
	pattern := makePattern(16, 16)
	screen := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	putPattern(screen, pattern, 24, 16)
	frame, err := NewImageFrame(encodeTestPNG(t, screen))
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := NewImageTemplate(encodeTestPNG(t, pattern))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := frame.Match(tpl, ImageMatchOptions{Threshold: 0.9})
			if err != nil || got.X != 32 || got.Y != 24 {
				t.Errorf("concurrent Match: result=%+v err=%v", got, err)
			}
		}()
	}
	wg.Wait()
}
