package lottie

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"testing"
)

func load(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/rect.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRenderFrames(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx, WithStderr(os.Stderr))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	a, err := r.Load(ctx, load(t), Options{Width: 256, Height: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	if a.TotalFrames() != 30 {
		t.Errorf("TotalFrames = %v, want 30", a.TotalFrames())
	}
	if got := a.FPS(); got < 29 || got > 31 {
		t.Errorf("FPS = %v, want ~30", got)
	}

	frames, err := a.Frames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 30 {
		t.Fatalf("got %d frames, want 30", len(frames))
	}

	for i, f := range frames {
		if f.Bounds() != image.Rect(0, 0, 256, 256) {
			t.Fatalf("frame %d bounds %v", i, f.Bounds())
		}
	}

	// The rotating rect must actually differ between frames.
	a0 := frames[0].(*image.NRGBA)
	a15 := frames[15].(*image.NRGBA)
	if bytes.Equal(a0.Pix, a15.Pix) {
		t.Error("frame 0 and 15 are identical; animation is not advancing")
	}

	// Centre pixel sits inside the rect and must be the fill colour
	// (255, 76, 25) at full opacity.
	r8, g8, b8, aa := a0.At(128, 128).RGBA()
	if aa>>8 != 255 {
		t.Errorf("centre alpha = %d, want 255", aa>>8)
	}
	if r8>>8 < 240 || g8>>8 > 110 || b8>>8 > 60 {
		t.Errorf("centre colour = %d,%d,%d; want approx 255,76,25", r8>>8, g8>>8, b8>>8)
	}

	// Corner is outside the rect and must be fully transparent.
	if _, _, _, ca := a0.At(2, 2).RGBA(); ca != 0 {
		t.Errorf("corner alpha = %d, want 0", ca>>8)
	}
}

func TestRenderPNG(t *testing.T) {
	ctx := context.Background()
	pngs, err := RenderPNG(ctx, load(t), Options{Width: 128, Height: 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(pngs) != 30 {
		t.Fatalf("got %d pngs", len(pngs))
	}
	img, err := png.Decode(bytes.NewReader(pngs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 128, 128) {
		t.Errorf("bounds %v", img.Bounds())
	}
}

func TestNativeSize(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	a, err := r.Load(ctx, load(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if w, h := a.Size(); w != 256 || h != 256 {
		t.Errorf("native size = %dx%d, want 256x256", w, h)
	}
}

func TestBadData(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if _, err := r.Load(ctx, []byte("not json"), Options{Width: 8, Height: 8}); err == nil {
		t.Error("expected an error for garbage input")
	}
	if _, err := r.Load(ctx, nil, Options{}); err == nil {
		t.Error("expected an error for empty input")
	}
}

func TestReuseRenderer(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	for i := 0; i < 3; i++ {
		a, err := r.Load(ctx, load(t), Options{Width: 64, Height: 64})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Frame(ctx, 5); err != nil {
			t.Fatal(err)
		}
		a.Close(ctx)
	}
}

func BenchmarkFrame(b *testing.B) {
	ctx := context.Background()
	r, _ := New(ctx)
	defer r.Close(ctx)
	data, _ := os.ReadFile("testdata/rect.json")
	a, err := r.Load(ctx, data, Options{Width: 512, Height: 512})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close(ctx)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Frame(ctx, float32(i%30)); err != nil {
			b.Fatal(err)
		}
	}
}
