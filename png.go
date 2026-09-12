package lottie

import (
	"bytes"
	"context"
	"image"
	"image/png"
)

// Render is the one-shot form: it spins up an engine, loads data, rasterises
// every frame and tears everything down again.
//
// Reach for New + Load instead when rendering more than one animation — the
// WebAssembly compile dominates the cost of a small render.
func Render(ctx context.Context, data []byte, opts Options) ([]image.Image, error) {
	r, err := New(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close(ctx)

	a, err := r.Load(ctx, data, opts)
	if err != nil {
		return nil, err
	}
	defer a.Close(ctx)

	return a.Frames(ctx)
}

// RenderPNG rasterises every frame and encodes each one as a PNG.
func RenderPNG(ctx context.Context, data []byte, opts Options) ([][]byte, error) {
	frames, err := Render(ctx, data, opts)
	if err != nil {
		return nil, err
	}
	return EncodePNGs(frames)
}

// EncodePNG encodes a single frame.
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodePNGs encodes a sequence of frames, reusing one encoder buffer pool.
func EncodePNGs(frames []image.Image) ([][]byte, error) {
	enc := png.Encoder{
		CompressionLevel: png.DefaultCompression,
		BufferPool:       newPNGPool(),
	}
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		var buf bytes.Buffer
		if err := enc.Encode(&buf, f); err != nil {
			return nil, err
		}
		out = append(out, buf.Bytes())
	}
	return out, nil
}

type pngPool struct{ ch chan *png.EncoderBuffer }

func newPNGPool() png.EncoderBufferPool { return &pngPool{ch: make(chan *png.EncoderBuffer, 1)} }

func (p *pngPool) Get() *png.EncoderBuffer {
	select {
	case b := <-p.ch:
		return b
	default:
		return nil
	}
}

func (p *pngPool) Put(b *png.EncoderBuffer) {
	select {
	case p.ch <- b:
	default:
	}
}
