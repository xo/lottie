// Package lottie renders Lottie animations to Go images without cgo.
//
// It runs the ThorVG vector engine — the WebAssembly build published as the
// npm package @thorvg/webcanvas — on wazero, a pure-Go WebAssembly runtime.
// Only ThorVG's software rasteriser is used, so there is no GPU, no browser
// and no JavaScript involved: frames are drawn into WebAssembly linear memory
// and read back as *image.NRGBA.
//
// The published build is Emscripten output with minified symbol names. The
// mapping from ThorVG C-API names to those minified names lives in the
// generated file symbols.go; see scripts/fetch-thorvg.sh for how to refresh it
// when upgrading the bundled thorvg.wasm.
package lottie

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"io"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed thorvg.wasm
var thorvgWASM []byte

const (
	pageSize = 1 << 16

	// ThorVG colour spaces. ABGR8888S is straight (non-premultiplied) alpha
	// with the word laid out 0xAABBGGRR, which on little-endian memory is the
	// byte order R,G,B,A — exactly image.NRGBA's pixel layout, so frames are
	// copied out without any per-pixel conversion.
	csABGR8888S = 2

	// Engine options for the software canvas.
	engineNone        = 0
	engineSmartRender = 2
)

// Result codes returned by the ThorVG C API.
const (
	tvgSuccess               = 0
	tvgInvalidArgument       = 1
	tvgInsufficientCondition = 2
	tvgFailedAllocation      = 3
	tvgMemoryCorruption      = 4
	tvgNotSupported          = 5
)

func resultErr(op string, code int32) error {
	switch code {
	case tvgSuccess:
		return nil
	case tvgInvalidArgument:
		return fmt.Errorf("lottie: %s: invalid argument", op)
	case tvgInsufficientCondition:
		return fmt.Errorf("lottie: %s: insufficient condition", op)
	case tvgFailedAllocation:
		return fmt.Errorf("lottie: %s: allocation failed", op)
	case tvgMemoryCorruption:
		return fmt.Errorf("lottie: %s: memory corruption", op)
	case tvgNotSupported:
		return fmt.Errorf("lottie: %s: not supported", op)
	default:
		return fmt.Errorf("lottie: %s: error %d", op, code)
	}
}

// Renderer owns a ThorVG engine instance running inside wazero.
//
// Creating one costs a WebAssembly compile (tens of milliseconds), so reuse a
// Renderer across animations. All methods are safe for concurrent use, but
// calls are serialised internally: the WebAssembly instance has a single
// linear memory and is not reentrant. For real parallelism, create one
// Renderer per goroutine.
type Renderer struct {
	// Stderr receives anything ThorVG writes to stdout/stderr. Nil discards it.
	Stderr io.Writer
	// TraceStubs, if non-nil, logs every call into an unimplemented import.
	// Useful when upgrading thorvg.wasm to see whether the new build reaches
	// code paths this package does not support.
	TraceStubs io.Writer

	mu     sync.Mutex
	rt     wazero.Runtime
	mod    api.Module
	fns    map[string]api.Function
	closed bool
}

// Option configures a Renderer.
type Option func(*config)

type config struct {
	wasm       []byte
	threads    int
	stderr     io.Writer
	traceStubs io.Writer
}

// WithWASM supplies an alternative thorvg.wasm build. It must come from the
// same @thorvg/webcanvas release as the generated symbol table, otherwise the
// minified export names will not line up.
func WithWASM(b []byte) Option { return func(c *config) { c.wasm = b } }

// WithThreads sets the ThorVG task-scheduler thread count. The published
// single-threaded build ignores anything but 0.
func WithThreads(n int) Option { return func(c *config) { c.threads = n } }

// WithStderr routes ThorVG's diagnostic output to w.
func WithStderr(w io.Writer) Option { return func(c *config) { c.stderr = w } }

// WithStubTracing logs calls into unimplemented WebAssembly imports to w.
func WithStubTracing(w io.Writer) Option { return func(c *config) { c.traceStubs = w } }

// New compiles the ThorVG module and initialises the engine.
func New(ctx context.Context, opts ...Option) (*Renderer, error) {
	cfg := config{wasm: thorvgWASM}
	for _, o := range opts {
		o(&cfg)
	}

	imports, err := parseImports(cfg.wasm)
	if err != nil {
		return nil, err
	}

	r := &Renderer{Stderr: cfg.stderr, TraceStubs: cfg.traceStubs, fns: map[string]api.Function{}}
	r.rt = wazero.NewRuntime(ctx)

	if err := r.hostImports(ctx, r.rt, imports); err != nil {
		r.rt.Close(ctx)
		return nil, err
	}

	// The module is an Emscripten "command" build without a _start export;
	// static constructors run via __wasm_call_ctors below.
	mod, err := r.rt.InstantiateWithConfig(ctx, cfg.wasm,
		wazero.NewModuleConfig().WithStartFunctions().WithName("thorvg"))
	if err != nil {
		r.rt.Close(ctx)
		return nil, fmt.Errorf("lottie: instantiate: %w", err)
	}
	r.mod = mod

	if _, err := r.call(ctx, "__wasm_call_ctors"); err != nil {
		r.rt.Close(ctx)
		return nil, err
	}
	code, err := r.call(ctx, "tvg_engine_init", uint64(uint32(cfg.threads)))
	if err != nil {
		r.rt.Close(ctx)
		return nil, err
	}
	if err := resultErr("engine_init", int32(code)); err != nil {
		r.rt.Close(ctx)
		return nil, err
	}
	return r, nil
}

// Close terminates the engine and releases the WebAssembly runtime.
func (r *Renderer) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.callLocked(ctx, "tvg_engine_term")
	return r.rt.Close(ctx)
}

// Version reports the @thorvg/webcanvas release the bundled module came from.
func (r *Renderer) Version() string { return wasmPackageVersion }

// call invokes an exported ThorVG C-API function by its canonical name.
func (r *Renderer) call(ctx context.Context, name string, args ...uint64) (res uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callLocked(ctx, name, args...)
}

func (r *Renderer) callLocked(ctx context.Context, name string, args ...uint64) (res uint64, err error) {
	fn, ok := r.fns[name]
	if !ok {
		minified, ok := exportSym[name]
		if !ok {
			return 0, fmt.Errorf("lottie: %s is not in the generated symbol table", name)
		}
		fn = r.mod.ExportedFunction(minified)
		if fn == nil {
			return 0, fmt.Errorf("lottie: export %q (%s) missing; thorvg.wasm does not match symbols.go", minified, name)
		}
		r.fns[name] = fn
	}

	defer func() {
		if p := recover(); p != nil {
			if ae, ok := p.(abortError); ok {
				err = fmt.Errorf("lottie: %s: %s", name, ae.msg)
				return
			}
			panic(p)
		}
	}()

	out, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("lottie: %s: %w", name, err)
	}
	if len(out) == 0 {
		return 0, nil
	}
	return out[0], nil
}

func (r *Renderer) malloc(ctx context.Context, n int) (uint32, error) {
	p, err := r.call(ctx, "malloc", uint64(uint32(n)))
	if err != nil {
		return 0, err
	}
	if p == 0 && n > 0 {
		return 0, errors.New("lottie: out of WebAssembly memory")
	}
	return uint32(p), nil
}

func (r *Renderer) free(ctx context.Context, ptr uint32) {
	if ptr != 0 {
		r.call(ctx, "free", uint64(ptr))
	}
}

func f32(v float32) uint64 { return uint64(api.EncodeF32(v)) }

// Options controls how an animation is rasterised.
type Options struct {
	// Width and Height are the output size in pixels. If either is zero the
	// animation's own composition size is used for both.
	Width, Height int

	// SmartRender enables ThorVG's partial-redraw optimisation. It is a good
	// default for sequential frame rendering and is on unless Disable is set.
	DisableSmartRender bool

	// Quality bounds the cost of Lottie effects such as blurs and shadows,
	// from 0 (fastest) to 100 (best). Zero means ThorVG's default of 50.
	Quality int
}

func (o Options) engineOption() uint64 {
	if o.DisableSmartRender {
		return engineNone
	}
	return engineSmartRender
}

// Animation is a loaded Lottie file bound to a software canvas of a fixed size.
//
// It holds WebAssembly memory; call Close when finished.
type Animation struct {
	r      *Renderer
	anim   uint32 // Tvg_Animation*
	pic    uint32 // Tvg_Paint* owned by anim
	canvas uint32 // Tvg_Canvas*
	buf    uint32 // pixel buffer in linear memory

	w, h   int
	total  float32
	durSec float32
	closed bool
}

// Load parses a Lottie JSON document (the raw .json bytes, or the JSON payload
// of a .lottie/dotLottie entry) and prepares a canvas for it.
func (r *Renderer) Load(ctx context.Context, data []byte, opts Options) (*Animation, error) {
	if len(data) == 0 {
		return nil, errors.New("lottie: empty animation data")
	}

	animPtr, err := r.call(ctx, "tvg_lottie_animation_new")
	if err != nil {
		return nil, err
	}
	if animPtr == 0 {
		return nil, errors.New("lottie: could not create animation")
	}
	a := &Animation{r: r, anim: uint32(animPtr)}

	picPtr, err := r.call(ctx, "tvg_animation_get_picture", animPtr)
	if err != nil || picPtr == 0 {
		a.Close(ctx)
		return nil, firstErr(err, errors.New("lottie: animation has no picture"))
	}
	a.pic = uint32(picPtr)

	// Copy the document into linear memory. ThorVG is told to take its own
	// copy (copy=1) so the scratch buffer can be released immediately.
	dataPtr, err := r.malloc(ctx, len(data))
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	if !r.mod.Memory().Write(dataPtr, data) {
		r.free(ctx, dataPtr)
		a.Close(ctx)
		return nil, errors.New("lottie: could not write animation data")
	}
	mimePtr, err := r.malloc(ctx, len("lottie")+1)
	if err != nil {
		r.free(ctx, dataPtr)
		a.Close(ctx)
		return nil, err
	}
	r.mod.Memory().Write(mimePtr, []byte("lottie\x00"))

	code, err := r.call(ctx, "tvg_picture_load_data",
		picPtr, uint64(dataPtr), uint64(uint32(len(data))), uint64(mimePtr), 0, 1)
	r.free(ctx, dataPtr)
	r.free(ctx, mimePtr)
	if err == nil {
		err = resultErr("load", int32(code))
	}
	if err != nil {
		a.Close(ctx)
		return nil, err
	}

	// Native composition size, used when the caller did not pick one.
	nw, nh, err := a.nativeSize(ctx)
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	a.w, a.h = opts.Width, opts.Height
	if a.w <= 0 || a.h <= 0 {
		a.w, a.h = int(nw+0.5), int(nh+0.5)
	}
	if a.w <= 0 || a.h <= 0 {
		a.Close(ctx)
		return nil, errors.New("lottie: animation has no usable size")
	}

	if code, err := r.call(ctx, "tvg_picture_set_size", picPtr, f32(float32(a.w)), f32(float32(a.h))); err != nil {
		a.Close(ctx)
		return nil, err
	} else if err := resultErr("set_size", int32(code)); err != nil {
		a.Close(ctx)
		return nil, err
	}

	if opts.Quality > 0 {
		r.call(ctx, "tvg_lottie_animation_set_quality", animPtr, uint64(uint32(opts.Quality)))
	}

	// Timing.
	if err := a.readTiming(ctx); err != nil {
		a.Close(ctx)
		return nil, err
	}

	// Software canvas targeting a buffer we own.
	cv, err := r.call(ctx, "tvg_swcanvas_create", opts.engineOption())
	if err != nil || cv == 0 {
		a.Close(ctx)
		return nil, firstErr(err, errors.New("lottie: could not create software canvas"))
	}
	a.canvas = uint32(cv)

	a.buf, err = r.malloc(ctx, a.w*a.h*4)
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	code, err = r.call(ctx, "tvg_swcanvas_set_target",
		cv, uint64(a.buf), uint64(uint32(a.w)), uint64(uint32(a.w)), uint64(uint32(a.h)), csABGR8888S)
	if err == nil {
		err = resultErr("set_target", int32(code))
	}
	if err != nil {
		a.Close(ctx)
		return nil, err
	}

	code, err = r.call(ctx, "tvg_canvas_add", cv, picPtr)
	if err == nil {
		err = resultErr("canvas_add", int32(code))
	}
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	return a, nil
}

func (a *Animation) nativeSize(ctx context.Context) (float32, float32, error) {
	p, err := a.r.malloc(ctx, 8)
	if err != nil {
		return 0, 0, err
	}
	defer a.r.free(ctx, p)
	if _, err := a.r.call(ctx, "tvg_picture_get_size", uint64(a.pic), uint64(p), uint64(p+4)); err != nil {
		return 0, 0, err
	}
	w, _ := a.r.mod.Memory().ReadUint32Le(p)
	h, _ := a.r.mod.Memory().ReadUint32Le(p + 4)
	return api.DecodeF32(uint64(w)), api.DecodeF32(uint64(h)), nil
}

func (a *Animation) readTiming(ctx context.Context) error {
	p, err := a.r.malloc(ctx, 8)
	if err != nil {
		return err
	}
	defer a.r.free(ctx, p)
	if _, err := a.r.call(ctx, "tvg_animation_get_total_frame", uint64(a.anim), uint64(p)); err != nil {
		return err
	}
	if _, err := a.r.call(ctx, "tvg_animation_get_duration", uint64(a.anim), uint64(p+4)); err != nil {
		return err
	}
	tf, _ := a.r.mod.Memory().ReadUint32Le(p)
	du, _ := a.r.mod.Memory().ReadUint32Le(p + 4)
	a.total = api.DecodeF32(uint64(tf))
	a.durSec = api.DecodeF32(uint64(du))
	return nil
}

// TotalFrames is the number of frames in the animation. Frame indices run from
// 0 to TotalFrames-1; fractional values interpolate.
func (a *Animation) TotalFrames() float32 { return a.total }

// Duration is the animation length in seconds.
func (a *Animation) Duration() float32 { return a.durSec }

// FPS is the animation's nominal frame rate.
func (a *Animation) FPS() float32 {
	if a.durSec == 0 {
		return 0
	}
	return a.total / a.durSec
}

// Size is the output size in pixels.
func (a *Animation) Size() (w, h int) { return a.w, a.h }

// Frame rasterises a single frame and returns it as a freshly allocated
// *image.NRGBA. Fractional frame numbers are interpolated by ThorVG.
func (a *Animation) Frame(ctx context.Context, frame float32) (*image.NRGBA, error) {
	if a.closed {
		return nil, errors.New("lottie: animation is closed")
	}
	r := a.r

	// InsufficientCondition here means "already on that frame", which is not
	// an error — it just means nothing needs re-seeking.
	code, err := r.call(ctx, "tvg_animation_set_frame", uint64(a.anim), f32(frame))
	if err != nil {
		return nil, err
	}
	if int32(code) != tvgSuccess && int32(code) != tvgInsufficientCondition {
		return nil, resultErr("set_frame", int32(code))
	}

	if code, err := r.call(ctx, "tvg_canvas_update", uint64(a.canvas)); err != nil {
		return nil, err
	} else if err := resultErr("update", int32(code)); err != nil {
		return nil, err
	}
	// draw(canvas, clear=1): start each frame from a transparent buffer.
	if code, err := r.call(ctx, "tvg_canvas_draw", uint64(a.canvas), 1); err != nil {
		return nil, err
	} else if err := resultErr("draw", int32(code)); err != nil {
		return nil, err
	}
	if code, err := r.call(ctx, "tvg_canvas_sync", uint64(a.canvas)); err != nil {
		return nil, err
	} else if err := resultErr("sync", int32(code)); err != nil {
		return nil, err
	}

	img := image.NewNRGBA(image.Rect(0, 0, a.w, a.h))

	r.mu.Lock()
	src, ok := r.mod.Memory().Read(a.buf, uint32(a.w*a.h*4))
	if ok {
		// ABGR8888S in little-endian memory is already R,G,B,A per pixel.
		copy(img.Pix, src)
	}
	r.mu.Unlock()
	if !ok {
		return nil, errors.New("lottie: could not read frame buffer")
	}
	return img, nil
}

// Frames rasterises every frame of the animation in order.
//
// The result is a []image.Image so it can be handed straight to image/gif,
// PNG encoding, or any other consumer; each element is an *image.NRGBA.
func (a *Animation) Frames(ctx context.Context) ([]image.Image, error) {
	n := int(a.total)
	if n <= 0 {
		n = 1
	}
	out := make([]image.Image, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		img, err := a.Frame(ctx, float32(i))
		if err != nil {
			return nil, fmt.Errorf("lottie: frame %d: %w", i, err)
		}
		out = append(out, img)
	}
	return out, nil
}

// FramesAt rasterises n frames spread evenly across the whole animation. It is
// the convenient way to downsample a 60fps Lottie to, say, 24 stills.
func (a *Animation) FramesAt(ctx context.Context, n int) ([]image.Image, error) {
	if n <= 0 {
		return nil, errors.New("lottie: frame count must be positive")
	}
	out := make([]image.Image, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var f float32
		if n > 1 {
			f = a.total * float32(i) / float32(n)
		}
		img, err := a.Frame(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("lottie: frame %d: %w", i, err)
		}
		out = append(out, img)
	}
	return out, nil
}

// Close releases the animation's canvas, pixel buffer and scene tree.
func (a *Animation) Close(ctx context.Context) error {
	if a == nil || a.closed {
		return nil
	}
	a.closed = true
	if a.canvas != 0 {
		a.r.call(ctx, "tvg_canvas_destroy", uint64(a.canvas))
		a.canvas = 0
	}
	if a.buf != 0 {
		a.r.free(ctx, a.buf)
		a.buf = 0
	}
	if a.anim != 0 {
		a.r.call(ctx, "tvg_animation_del", uint64(a.anim))
		a.anim = 0
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
