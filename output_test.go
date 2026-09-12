package lottie

import (
	"context"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outputDir is where TestWriteOutput leaves its PNGs. It is rewritten from
// scratch on every run and is not compared against anything — the point is to
// have real files to open and look at when changing the render path.
const outputDir = "testdata/output"

func TestWriteOutput(t *testing.T) {
	ctx := context.Background()

	if err := os.RemoveAll(outputDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

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

	frames, err := a.Frames(ctx)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(frames))
	for i, img := range frames {
		buf, err := EncodePNG(img)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		name := fmt.Sprintf("frame%03d.png", i)
		if err := os.WriteFile(filepath.Join(outputDir, name), buf, 0o644); err != nil {
			t.Fatal(err)
		}
		if len(buf) < 8 || string(buf[1:4]) != "PNG" {
			t.Fatalf("frame %d is not a PNG", i)
		}
		names = append(names, name)
	}

	if err := writeContactSheet(outputDir, names, a); err != nil {
		t.Fatal(err)
	}

	abs, _ := filepath.Abs(outputDir)
	t.Logf("wrote %d frames (%dx%d, %.0f fps) to %s — open index.html to review",
		len(names), a.w, a.h, a.FPS(), abs)
}

// writeContactSheet drops an index.html next to the frames so they can be
// reviewed in one go, both as a grid and as a running animation.
func writeContactSheet(dir string, names []string, a *Animation) error {
	f, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()

	return contactSheetTmpl.Execute(f, struct {
		Frames        []string
		Width, Height int
		FPS           float32
		DelayMS       int
	}{
		Frames:  names,
		Width:   a.w,
		Height:  a.h,
		FPS:     a.FPS(),
		DelayMS: int(1000 / max32(a.FPS(), 1)),
	})
}

func max32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

var contactSheetTmpl = template.Must(template.New("sheet").Parse(`<!doctype html>
<meta charset="utf-8">
<title>lottie render output</title>
<style>
  body { font: 14px/1.5 system-ui, sans-serif; margin: 2rem; background: #fff; color: #222; }
  h1 { font-size: 1.1rem; }
  p { color: #666; }
  /* Checkerboard so transparent regions are obvious. */
  .checker {
    background-image:
      linear-gradient(45deg, #ddd 25%, transparent 25%, transparent 75%, #ddd 75%),
      linear-gradient(45deg, #ddd 25%, transparent 25%, transparent 75%, #ddd 75%);
    background-size: 16px 16px;
    background-position: 0 0, 8px 8px;
  }
  #player { display: block; margin-bottom: 2rem; image-rendering: auto; }
  .grid { display: flex; flex-wrap: wrap; gap: 8px; }
  figure { margin: 0; }
  figure img { display: block; width: 120px; height: auto; }
  figcaption { font-size: 11px; color: #888; text-align: center; }
</style>
<h1>lottie render output</h1>
<p>{{len .Frames}} frames &middot; {{.Width}}&times;{{.Height}} &middot; {{printf "%.0f" .FPS}} fps</p>
<img id="player" class="checker" width="{{.Width}}" height="{{.Height}}" src="{{index .Frames 0}}" alt="playback">
<div class="grid">
{{range $i, $n := .Frames}}  <figure><img class="checker" src="{{$n}}" alt="frame {{$i}}"><figcaption>{{$i}}</figcaption></figure>
{{end}}</div>
<script>
  const frames = {{.Frames}};
  const player = document.getElementById('player');
  let i = 0;
  setInterval(() => { i = (i + 1) % frames.length; player.src = frames[i]; }, {{.DelayMS}});
</script>
`))

// TestWriteEmojiOutput renders whatever scripts/fetch-noto-emoji.sh has put in
// testdata/emoji, one frame each, into testdata/output/emoji. It skips when
// nothing has been downloaded, so the suite still passes on a clean checkout.
func TestWriteEmojiOutput(t *testing.T) {
	files, err := filepath.Glob("testdata/emoji/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no emoji in testdata/emoji; run scripts/fetch-noto-emoji.sh first")
	}

	ctx := context.Background()
	dir := filepath.Join(outputDir, "emoji")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	var names []string
	var failed int
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		stem := strings.TrimSuffix(filepath.Base(path), ".json")

		if err := renderOne(ctx, r, data, dir, stem); err != nil {
			// A malformed or unsupported file should be reported, not fatal —
			// one bad emoji out of a thousand shouldn't hide the rest.
			t.Errorf("%s: %v", stem, err)
			failed++
			continue
		}
		names = append(names, stem+".png")
	}

	if err := writeGrid(dir, names); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	t.Logf("rendered %d/%d emoji (%d failed) to %s", len(names), len(files), failed, abs)
}

// renderOne rasterises the middle frame of one animation to dir/stem.png.
func renderOne(ctx context.Context, r *Renderer, data []byte, dir, stem string) error {
	a, err := r.Load(ctx, data, Options{Width: 128, Height: 128})
	if err != nil {
		return err
	}
	defer a.Close(ctx)

	img, err := a.Frame(ctx, a.TotalFrames()/2)
	if err != nil {
		return err
	}
	buf, err := EncodePNG(img)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stem+".png"), buf, 0o644)
}

// writeGrid writes a plain index.html listing every rendered still.
func writeGrid(dir string, names []string) error {
	f, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return emojiGridTmpl.Execute(f, names)
}

var emojiGridTmpl = template.Must(template.New("grid").Parse(`<!doctype html>
<meta charset="utf-8">
<title>noto emoji render check</title>
<style>
  body { font: 13px/1.4 system-ui, sans-serif; margin: 2rem; }
  .grid { display: flex; flex-wrap: wrap; gap: 10px; }
  figure { margin: 0; width: 96px; }
  figure img { display: block; width: 96px; height: 96px;
    background-image:
      linear-gradient(45deg, #e6e6e6 25%, transparent 25%, transparent 75%, #e6e6e6 75%),
      linear-gradient(45deg, #e6e6e6 25%, transparent 25%, transparent 75%, #e6e6e6 75%);
    background-size: 12px 12px; background-position: 0 0, 6px 6px; }
  figcaption { font-size: 10px; color: #777; word-break: break-all; text-align: center; }
</style>
<h1>{{len .}} emoji, middle frame</h1>
<div class="grid">
{{range .}}  <figure><img src="{{.}}" loading="lazy" alt="{{.}}"><figcaption>{{.}}</figcaption></figure>
{{end}}</div>
`))
