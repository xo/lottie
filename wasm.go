package lottie

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// wasmImport is one entry of a module's import section.
type wasmImport struct {
	Module  string
	Name    string
	Params  []api.ValueType
	Results []api.ValueType
}

// parseImports reads the type and import sections of a WebAssembly binary and
// returns every imported function with its signature.
//
// The published ThorVG build is produced by Emscripten with minified import
// names (all of them live in module "a" and are called "a", "b", ... "Vb"), so
// the signatures cannot be hard-coded by name. Reading them from the binary
// keeps the host module correct across upstream releases: every import is
// declared with the exact arity the module expects, and only the handful that
// need real behaviour are overridden.
func parseImports(d []byte) ([]wasmImport, error) {
	if len(d) < 8 || string(d[:4]) != "\x00asm" {
		return nil, fmt.Errorf("lottie: not a WebAssembly binary")
	}
	p := 8
	var err error

	uleb := func() uint32 {
		var r, s uint32
		for {
			if p >= len(d) {
				err = fmt.Errorf("lottie: truncated wasm binary")
				return 0
			}
			b := d[p]
			p++
			r |= uint32(b&0x7f) << s
			if b&0x80 == 0 {
				return r
			}
			s += 7
		}
	}

	type sig struct{ params, results []api.ValueType }
	var types []sig
	var out []wasmImport

	for p < len(d) && err == nil {
		id := d[p]
		p++
		size := int(uleb())
		if err != nil || p+size > len(d) {
			return nil, fmt.Errorf("lottie: malformed wasm section")
		}
		end := p + size

		switch id {
		case 1: // type
			for i, n := uint32(0), uleb(); i < n && err == nil; i++ {
				p++ // 0x60 func
				var s sig
				for j, a := uint32(0), uleb(); j < a; j++ {
					s.params = append(s.params, d[p])
					p++
				}
				for j, b := uint32(0), uleb(); j < b; j++ {
					s.results = append(s.results, d[p])
					p++
				}
				types = append(types, s)
			}
		case 2: // import
			for i, n := uint32(0), uleb(); i < n && err == nil; i++ {
				l := uleb()
				mod := string(d[p : p+int(l)])
				p += int(l)
				l = uleb()
				name := string(d[p : p+int(l)])
				p += int(l)
				kind := d[p]
				p++
				switch kind {
				case 0: // func
					ti := uleb()
					if int(ti) >= len(types) {
						return nil, fmt.Errorf("lottie: import type index out of range")
					}
					out = append(out, wasmImport{mod, name, types[ti].params, types[ti].results})
				case 1: // table
					p++
					lim := d[p]
					p++
					uleb()
					if lim != 0 {
						uleb()
					}
				case 2: // memory
					lim := d[p]
					p++
					uleb()
					if lim&1 != 0 {
						uleb()
					}
				case 3: // global
					p += 2
				default:
					return nil, fmt.Errorf("lottie: unknown import kind %d", kind)
				}
			}
		}
		p = end
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hostImports registers the module ThorVG imports from.
//
// The build pulls in three families of imports: the Emscripten C runtime, the
// WebGL/WebGPU bindings, and the embind registration callbacks. Only the first
// matters here — the software rasteriser never touches a GPU, and embind is
// bypassed entirely because the ThorVG C API is exported directly. The
// registration calls still run during static construction, so they are
// answered with zero-returning stubs rather than left undefined.
func (r *Renderer) hostImports(ctx context.Context, rt wazero.Runtime, imports []wasmImport) error {
	byModule := map[string][]wasmImport{}
	for _, im := range imports {
		byModule[im.Module] = append(byModule[im.Module], im)
	}

	rev := make(map[string]string, len(importSym))
	for canonical, minified := range importSym {
		rev[minified] = canonical
	}

	for modName, ims := range byModule {
		b := rt.NewHostModuleBuilder(modName)
		for _, im := range ims {
			fn := r.hostFunc(rev[im.Name], im)
			b.NewFunctionBuilder().
				WithGoModuleFunction(fn, im.Params, im.Results).
				Export(im.Name)
		}
		if _, err := b.Instantiate(ctx); err != nil {
			return fmt.Errorf("lottie: host module %q: %w", modName, err)
		}
	}
	return nil
}

// hostFunc returns the implementation for one import. canonical is the
// Emscripten name if this import is one we implement, or "" for a stub.
func (r *Renderer) hostFunc(canonical string, im wasmImport) api.GoModuleFunc {
	switch canonical {
	case "emscripten_resize_heap":
		// sbrk failed and the allocator is asking for a bigger linear memory.
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			want := uint32(stack[0])
			mem := mod.Memory()
			cur := mem.Size()
			if want <= cur {
				stack[0] = 1
				return
			}
			pages := (want-cur+pageSize-1)/pageSize + 1
			if _, ok := mem.Grow(pages); !ok {
				stack[0] = 0
				return
			}
			stack[0] = 1
		}

	case "abort":
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			panic(abortError{"thorvg called abort()"})
		}

	case "__assert_fail":
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			msg := readCString(mod, uint32(stack[0]))
			file := readCString(mod, uint32(stack[1]))
			panic(abortError{fmt.Sprintf("thorvg assertion failed: %s (%s:%d)", msg, file, uint32(stack[2]))})
		}

	case "__cxa_throw", "__cxa_rethrow", "__resumeException":
		// ThorVG itself is built without exceptions; these only exist because
		// embind is linked in. Reaching one means something upstream threw.
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			panic(abortError{"thorvg: C++ exception escaped the module"})
		}

	case "fd_write":
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			fd, iov, cnt, pnum := uint32(stack[0]), uint32(stack[1]), uint32(stack[2]), uint32(stack[3])
			mem := mod.Memory()
			var total uint32
			for i := uint32(0); i < cnt; i++ {
				ptr, _ := mem.ReadUint32Le(iov + i*8)
				n, _ := mem.ReadUint32Le(iov + i*8 + 4)
				if buf, ok := mem.Read(ptr, n); ok && r.Stderr != nil {
					if fd == 1 || fd == 2 {
						r.Stderr.Write(buf)
					}
				}
				total += n
			}
			mem.WriteUint32Le(pnum, total)
			stack[0] = 0
		}

	case "fd_close", "fd_seek":
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = 0 // success; there is no filesystem behind this
		}

	default:
		// Unused GL/WebGPU binding or embind registration hook.
		n := len(im.Results)
		name := im.Name
		return func(ctx context.Context, mod api.Module, stack []uint64) {
			if r.TraceStubs != nil {
				fmt.Fprintf(r.TraceStubs, "lottie: stub import %s.%s called\n", im.Module, name)
			}
			for i := 0; i < n; i++ {
				stack[i] = 0
			}
		}
	}
}

// abortError is panicked from host functions and recovered at the call
// boundary so a broken animation cannot take the process down.
type abortError struct{ msg string }

func (e abortError) Error() string { return e.msg }

func readCString(mod api.Module, ptr uint32) string {
	if ptr == 0 {
		return ""
	}
	mem := mod.Memory()
	var out []byte
	for i := uint32(0); i < 4096; i++ {
		b, ok := mem.ReadByte(ptr + i)
		if !ok || b == 0 {
			break
		}
		out = append(out, b)
	}
	return string(out)
}
