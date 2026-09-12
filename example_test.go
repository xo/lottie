package lottie_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/xo/lottie"
)

// Rendering every frame of a Lottie file and encoding each one as a PNG.
func Example() {
	ctx := context.Background()

	data, err := os.ReadFile("testdata/rect.json")
	if err != nil {
		log.Fatal(err)
	}

	r, err := lottie.New(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close(ctx)

	a, err := r.Load(ctx, data, lottie.Options{Width: 256, Height: 256})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close(ctx)

	frames, err := a.Frames(ctx)
	if err != nil {
		log.Fatal(err)
	}

	dir, err := os.MkdirTemp("", "frames")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	for i, img := range frames {
		b, err := lottie.EncodePNG(img)
		if err != nil {
			log.Fatal(err)
		}
		name := filepath.Join(dir, fmt.Sprintf("frame%03d.png", i))
		if err := os.WriteFile(name, b, 0o644); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println(len(frames), "frames")
	// Output: 30 frames
}
