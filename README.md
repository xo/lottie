<hr/>

## DEPRECATED

NOTE: This repo has been deprecated. A Pure Go lottie decoder is now available
in the [`okage`](https://github.com/xo/okage) package.

<hr/>

# lottie

Render Lottie animations to Go images. Pure Go, no cgo, no browser.

The package runs the ThorVG vector engine — the WebAssembly build published as
[`@thorvg/webcanvas`](https://www.npmjs.com/package/@thorvg/webcanvas) — on
[wazero](https://wazero.io). Only ThorVG's software rasteriser is used, so
frames are drawn into WebAssembly linear memory and read back as
`*image.NRGBA`.

```go
ctx := context.Background()

r, err := lottie.New(ctx)
if err != nil {
    log.Fatal(err)
}
defer r.Close(ctx)

a, err := r.Load(ctx, data, lottie.Options{Width: 512, Height: 512})
if err != nil {
    log.Fatal(err)
}
defer a.Close(ctx)

frames, err := a.Frames(ctx) // []image.Image, one per frame
if err != nil {
    log.Fatal(err)
}

buf, _ := lottie.EncodePNG(frames[0])
os.WriteFile("frame0.png", buf, 0o644)
```

One-shot form, straight to PNG bytes:

```go
pngs, err := lottie.RenderPNG(ctx, data, lottie.Options{Width: 256, Height: 256})
```

## API

|                                                            |                                                         |
| ---------------------------------------------------------- | ------------------------------------------------------- |
| `New(ctx, opts...) (*Renderer, error)`                     | Compiles the module and starts the engine. Reuse it.    |
| `(*Renderer).Load(ctx, data, Options) (*Animation, error)` | Parses a Lottie JSON document onto a fixed-size canvas. |
| `(*Animation).Frame(ctx, n float32) (*image.NRGBA, error)` | One frame; fractional `n` interpolates.                 |
| `(*Animation).Frames(ctx) ([]image.Image, error)`          | Every frame, in order.                                  |
| `(*Animation).FramesAt(ctx, n) ([]image.Image, error)`     | `n` frames spread evenly across the timeline.           |
| `(*Animation).TotalFrames/Duration/FPS/Size`               | Timing and geometry from the file.                      |
| `Render` / `RenderPNG`                                     | One-shot helpers that own the engine for you.           |

`Options.Width`/`Height` default to the animation's own composition size.
`Options.Quality` (0–100) bounds the cost of Lottie blurs and shadows.
`Options.DisableSmartRender` turns off ThorVG's partial-redraw optimisation.

## Test animations

`testdata/rect.json` is a small hand-written animation the tests rely on. For a
broader corpus, `scripts/fetch-noto-emoji.sh` pulls the Noto animated emoji —
the set behind <https://googlefonts.github.io/noto-emoji-animation/> — as
Lottie JSON:

```sh
scripts/fetch-noto-emoji.sh -n 25           # a sample
scripts/fetch-noto-emoji.sh -f 'cat|rocket' # by name or codepoint
scripts/fetch-noto-emoji.sh -f cat -l       # list what that would fetch
scripts/fetch-noto-emoji.sh                 # everything, ~880 files
```

Files land in `testdata/emoji/` as `<codepoint>-<name>.json`, where the name is
the emoji's own label — `1f680-rocket.json`. Existing files are skipped unless
`-F` is passed, downloads run 8-wide (`-j`), and a response is only moved into
place once it parses as JSON, so an interrupted run never leaves a truncated
file behind. A few emoji are listed in the metadata but have no Lottie asset
published; those are reported as `not published` and do not fail the run, while
a genuine download error does. The Noto emoji are CC BY 4.0; the script writes a
`NOTICE` recording that alongside the downloads.

`testdata/emoji/` is gitignored — it is a fetched corpus, not source.

## Looking at the output

`TestWriteOutput` rasterises `testdata/rect.json` and writes every frame as a
PNG into `testdata/output/`, alongside an `index.html` that shows them as a
checkerboard-backed grid and loops them at the animation's own frame rate:

```sh
go test -run TestWriteOutput -v ./...
open testdata/output/index.html
```

The directory is wiped and rebuilt on each run, and is gitignored. Nothing is
compared against a golden file — it exists so render changes can be eyeballed.

If `testdata/emoji/` has been populated, `TestWriteEmojiOutput` renders the
middle frame of every emoji into `testdata/output/emoji/` with its own
`index.html`. That doubles as a wide compatibility check across real-world
Lottie features; a file that fails to load or render is reported individually
rather than failing the whole run. The test skips when nothing is downloaded.

## Concurrency

A `Renderer` is safe for concurrent use but serialises internally — one
WebAssembly instance has one linear memory and is not reentrant. For real
parallelism, give each goroutine its own `Renderer`. The WebAssembly compile is
the expensive part of `New`, so pool them rather than creating one per request.

## How it fits together

`@thorvg/webcanvas` ships an Emscripten build that normally needs its
JavaScript glue. Two things make it usable from Go instead:

**The C API is exported directly.** The bundle's TypeScript layer calls
`tvg_picture_load_data`, `tvg_swcanvas_set_target` and friends as plain
WebAssembly exports. Embind is linked in, but only for the accessor and
asset-resolver callbacks, so it can be bypassed entirely — nothing in the
render path goes through it.

**Nothing in the software path needs a host beyond memory.** The module
imports 156 functions: the Emscripten C runtime, the WebGL and WebGPU
bindings, and embind's registration hooks. Every import is declared with the
signature read out of the binary's own type section, so none are left
undefined, but only nine need real behaviour — heap growth, `abort`,
`__assert_fail`, the `fd_*` triple and the C++ exception hooks. The embind
hooks fire once during static construction and are answered with zeros; the
GL and WebGPU imports are never reached at all. `WithStubTracing` will show
you if a future build changes that.

Pixels come out as `ABGR8888S`, which in little-endian memory is the byte
order `R,G,B,A` — exactly `image.NRGBA`'s layout, so a frame is one `copy`
with no per-pixel conversion.

## Upgrading thorvg.wasm

The published build minifies its export names: `tvg_engine_init` is exported as
`$b`, `malloc` as `Zb`. Those names are stable within a release and change
between releases, so `symbols.go` maps ThorVG's C-API names to them, recovered
from the JavaScript glue that ships alongside the module.

`scripts/fetch-thorvg.sh` does the upgrade: it resolves a release from the npm
registry, downloads the tarball, copies out `thorvg.wasm`, and rewrites
`symbols.go` from the glue that came with it.

```sh
scripts/fetch-thorvg.sh              # latest release
scripts/fetch-thorvg.sh -v 1.1.0     # a specific one
scripts/fetch-thorvg.sh -n           # report what it would write, change nothing
go test ./...
```

It needs only `curl`, `tar` and `awk`; `gofmt` is used to align the generated
maps if it is on the PATH. A mismatch between the two files shows up as a
`thorvg.wasm does not match symbols.go` error on the first call, not as silent
corruption, and the script refuses to write at all if it cannot recover
`__wasm_call_ctors`, the runtime imports, or at least 100 exports — so a build
whose glue format has changed fails loudly rather than producing a plausible
but wrong table.

Currently pinned to **@thorvg/webcanvas 1.1.1**.

## Notes and limits

- Input is Lottie JSON. A `.lottie`/dotLottie archive needs unzipping first;
  pass the `animations/*.json` entry.
- External assets referenced by a Lottie (images, fonts) are not resolved.
  ThorVG exposes `tvg_picture_set_asset_resolver` for this and the symbol is in
  the table, but the callback needs a function-table trampoline that is not
  wired up here.
- The bundled build is single-threaded; `WithThreads` is accepted but the
  published module ignores anything but 0. The `@thorvg/webcanvas/thread`
  variant needs shared memory and is not supported.
- Frames render at roughly 1.5 ms at 512×512 for a simple animation on a
  modern x86 core. Complex files with blurs are considerably slower; lower
  `Options.Quality` if that matters more than fidelity.

## Licence

This package is yours to license as you wish. The bundled `thorvg.wasm` is
ThorVG, MIT licensed — keep `LICENSE.thorvg` alongside it when redistributing.
