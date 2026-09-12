#!/usr/bin/env bash
#
# Fetch a published @thorvg/webcanvas release and regenerate the bindings for
# it: thorvg.wasm plus a matching symbols.go.
#
# The ThorVG WebAssembly module is Emscripten output built with minified export
# names -- tvg_engine_init is exported as "$b", malloc as "Zb". Those names are
# stable within a release but change between releases, so they are recovered
# from the JavaScript glue that ships alongside the module, where each minified
# name is assigned to its real one:
#
#   cr=a._free=e.Yb,dr=a._malloc=e.Zb,...,a._tvg_engine_init=e.$b,...
#
# Usage:
#   scripts/fetch-thorvg.sh [-v VERSION] [-o DIR] [-n]
#
#   -v VERSION  release to fetch, or "latest" (default: latest)
#   -o DIR      package directory to write into (default: the repo root)
#   -n          report what would be written without touching any file
#
# Run the tests afterwards. A mismatch between thorvg.wasm and symbols.go shows
# up as a "thorvg.wasm does not match symbols.go" error on the first call,
# not as silent corruption:
#
#   scripts/fetch-thorvg.sh
#   go test ./...
#
# The source can be overridden for testing:
#   THORVG_REGISTRY  base registry URL (default: https://registry.npmjs.org)
#   THORVG_TARBALL   path to a local .tgz to use instead of downloading

set -euo pipefail

REGISTRY="${THORVG_REGISTRY:-https://registry.npmjs.org}"
PKG="@thorvg/webcanvas"
VERSION="latest"
OUT_DIR=""
DRY_RUN=0

while getopts ":v:o:nh" opt; do
  case "$opt" in
    v) VERSION="$OPTARG" ;;
    o) OUT_DIR="$OPTARG" ;;
    n) DRY_RUN=1 ;;
    h) awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
    \?) echo "unknown option -$OPTARG" >&2; exit 2 ;;
    :)  echo "option -$OPTARG needs an argument" >&2; exit 2 ;;
  esac
done

for cmd in curl tar awk; do
  command -v "$cmd" >/dev/null || { echo "need $cmd on PATH" >&2; exit 1; }
done

# Repo root, so the script works from anywhere.
cd "$(dirname "${BASH_SOURCE[0]}")/.."
[ -n "$OUT_DIR" ] || OUT_DIR="$PWD"
[ -d "$OUT_DIR" ] || { echo "no such directory: $OUT_DIR" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------- download ---

if [ -n "${THORVG_TARBALL:-}" ]; then
  [ -f "$THORVG_TARBALL" ] || { echo "no such tarball: $THORVG_TARBALL" >&2; exit 1; }
  cp "$THORVG_TARBALL" "$tmp/pkg.tgz"
  tarball="$THORVG_TARBALL"
  echo "using local tarball $THORVG_TARBALL" >&2
else
  echo "resolving $PKG@$VERSION" >&2
  curl -fsSL --retry 3 --retry-delay 1 -o "$tmp/manifest.json" "$REGISTRY/$PKG/$VERSION" \
    || { echo "no such release: $PKG@$VERSION" >&2; exit 1; }

  # The manifest is one long line, so pull the field out rather than relying on
  # key order or on jq being installed.
  tarball="$(grep -o '"tarball":"[^"]*"' "$tmp/manifest.json" | head -1 | sed 's/^"tarball":"//; s/"$//')"
  [ -n "$tarball" ] || { echo "no tarball URL in the registry manifest" >&2; exit 1; }

  echo "downloading $tarball" >&2
  curl -fsSL --retry 3 --retry-delay 1 -o "$tmp/pkg.tgz" "$tarball"
fi

mkdir -p "$tmp/x"
tar xzf "$tmp/pkg.tgz" -C "$tmp/x"

dist="$tmp/x/package/dist"
glue="$dist/webcanvas.js"
wasm="$dist/thorvg.wasm"
manifest="$tmp/x/package/package.json"
for f in "$glue" "$wasm" "$manifest"; do
  [ -f "$f" ] || { echo "the package has no ${f#$tmp/x/package/}; its layout has changed" >&2; exit 1; }
done

# The version comes from the package manifest rather than the tarball name, so
# a locally built or renamed tarball still reports what it actually is. The
# manifest's "scripts" block has a "version" key of its own, so take the first
# value that looks like a version rather than the first key that matches.
version="$(grep -o '"version"[[:space:]]*:[[:space:]]*"[0-9][^"]*"' "$manifest" \
  | head -1 | sed 's/.*"\([0-9][^"]*\)"$/\1/')"
[ -n "$version" ] || version="$(basename "$tarball" | sed 's/^webcanvas-//; s/\.tgz$//')"
case "$version" in
  [0-9]*) ;;
  *) echo "could not determine the version of $(basename "$tarball")" >&2; exit 1 ;;
esac

# ----------------------------------------------------------------- extract ---

cat > "$tmp/extract.awk" <<'AWK'
# Recover the minified names from the Emscripten glue, as TSV:
#
#   export<TAB>real<TAB>minified
#   import<TAB>real<TAB>minified

# classify matches one entry of the WebAssembly import object against the
# behaviour of the runtime functions this package implements. The bodies are
# minified, so each probe keys off a string literal or constant that survives
# minification. Order matters: the first probe that matches wins.
function classify(e,   key, body, n) {
  if (match(e, /^[ \t\n]*[A-Za-z_$][A-Za-z0-9_$]*[ \t\n]*:/) != 1) return
  key  = substr(e, RSTART, RLENGTH)
  body = substr(e, RSTART + RLENGTH)
  gsub(/[ \t\n:]/, "", key)
  n = length(body)
       if (index(body, "Assertion failed"))                          im["__assert_fail"]          = key
  else if (index(body, "j(\"\")") && n < 30)                         im["abort"]                  = key
  else if (index(body, "2147483648"))                                im["emscripten_resize_heap"] = key
  else if (index(body, "llseek"))                                    im["fd_seek"]                = key
  else if (index(body, "getStreamFromFD") && index(body, ".close(")) im["fd_close"]               = key
  else if (index(body, "t+=8"))                                      im["fd_write"]               = key
  else if (index(body, "throw new J(") || index(body, ").init("))    im["__cxa_throw"]            = key
  else if (index(body, "throw H||"))                                 im["__cxa_rethrow"]          = key
  else if (index(body, "throw new v"))                               im["__resumeException"]      = key
}

{ doc = doc $0 "\n" }

END {
  # Exports: `a._tvg_engine_init=e.$b`, `dr=a._malloc=e.Zb`.
  rest = doc
  while (match(rest, /[A-Za-z_$]+\._?(tvg_[A-Za-z0-9_]*|malloc|free)[ \t]*=[ \t]*e\.[A-Za-z_$][A-Za-z0-9_$]*/)) {
    m = substr(rest, RSTART, RLENGTH)
    rest = substr(rest, RSTART + RLENGTH)
    eq = index(m, "=")
    lhs = substr(m, 1, eq - 1)
    rhs = substr(m, eq + 1)
    sub(/^[ \t]*e\./, "", rhs)
    dot = 0
    for (j = length(lhs); j > 0; j--) if (substr(lhs, j, 1) == ".") { dot = j; break }
    name = substr(lhs, dot + 1)
    sub(/^_/, "", name)
    sub(/[ \t]*$/, "", name)
    ex[name] = rhs
  }

  # __wasm_call_ctors has no name of its own to match on, but Emscripten's
  # run() calls it immediately before clearing FS.ignorePermissions:
  #   ...,qt.init(),Qr.Xb(),qt.ignorePermissions=!1,...
  # so it is the last no-argument call before that landmark.
  i = index(doc, "ignorePermissions=!1")
  if (i > 0) {
    lo = i - 200
    if (lo < 1) lo = 1
    win = substr(doc, lo, i - lo)
    while (match(win, /\.[A-Za-z_$][A-Za-z0-9_$]?\(\)/)) {
      ctors = substr(win, RSTART + 1, RLENGTH - 3)
      win = substr(win, RSTART + RLENGTH)
    }
    if (ctors != "") ex["__wasm_call_ctors"] = ctors
  }

  # Imports: walk the `={a:` object literal, tracking nesting, string literals
  # and escapes, and hand each top-level entry to classify().
  start = index(doc, "={a:")
  if (start > 0) {
    n = length(doc)
    depth = 0; q = ""; esc = 0; entry = ""
    for (i = start + 1; i <= n; i++) {
      c = substr(doc, i, 1)
      if (q != "") {
        entry = entry c
        if (esc) esc = 0
        else if (c == "\\") esc = 1
        else if (c == q) q = ""
        continue
      }
      if (c == "\"" || c == "'" || c == "`") { q = c; entry = entry c; continue }
      if (c == "{" || c == "[" || c == "(") {
        depth++
        if (depth == 1) entry = ""; else entry = entry c
        continue
      }
      if (c == "}" || c == "]" || c == ")") {
        depth--
        if (depth == 0) { classify(entry); break }
        entry = entry c
        continue
      }
      if (c == "," && depth == 1) { classify(entry); entry = ""; continue }
      entry = entry c
    }
  }

  for (k in ex) printf "export\t%s\t%s\n", k, ex[k]
  for (k in im) printf "import\t%s\t%s\n", k, im[k]
}
AWK

awk -f "$tmp/extract.awk" "$glue" | LC_ALL=C sort > "$tmp/syms.tsv"

# The names go into Go source unquoted-escaped, so refuse anything that is not
# a plain identifier rather than emitting source that will not compile.
if bad="$(awk -F'\t' '$2 !~ /^[A-Za-z_][A-Za-z0-9_]*$/ || $3 !~ /^[A-Za-z_$][A-Za-z0-9_$]*$/' "$tmp/syms.tsv")" \
   && [ -n "$bad" ]; then
  echo "refusing to write unexpected symbol names:" >&2
  echo "$bad" >&2
  exit 1
fi

n_export=$(awk -F'\t' '$1=="export"' "$tmp/syms.tsv" | wc -l | tr -d ' ')
n_import=$(awk -F'\t' '$1=="import"' "$tmp/syms.tsv" | wc -l | tr -d ' ')

if [ "$n_export" -lt 100 ]; then
  echo "only recovered $n_export exports; the glue format has changed" >&2
  exit 1
fi
if ! grep -q '^export	__wasm_call_ctors	' "$tmp/syms.tsv"; then
  echo "could not find __wasm_call_ctors; inspect the startup path in webcanvas.js" >&2
  exit 1
fi
for want in __assert_fail abort emscripten_resize_heap fd_write; do
  grep -q "^import	$want	" "$tmp/syms.tsv" \
    || { echo "could not identify import \"$want\"" >&2; exit 1; }
done

# ---------------------------------------------------------------- generate ---

gen() {
  printf '// Code generated by scripts/fetch-thorvg.sh. DO NOT EDIT.\n\npackage lottie\n\n'
  printf '// wasmPackageVersion is the @thorvg/webcanvas release these symbols were\n'
  printf '// extracted from. The shipped thorvg.wasm must come from the same release.\n'
  printf 'const wasmPackageVersion = "%s"\n\n' "$version"

  printf '// exportSym maps ThorVG C-API names to the minified WebAssembly export names\n'
  printf '// used by the published build.\n'
  printf 'var exportSym = map[string]string{\n'
  awk -F'\t' '$1=="export" { printf "\t\"%s\": \"%s\",\n", $2, $3 }' "$tmp/syms.tsv"
  printf '}\n\n'

  printf '// importSym maps the Emscripten runtime imports this package implements to\n'
  printf '// their minified names. Every other import is auto-stubbed.\n'
  printf 'var importSym = map[string]string{\n'
  awk -F'\t' '$1=="import" { printf "\t\"%s\": \"%s\",\n", $2, $3 }' "$tmp/syms.tsv"
  printf '}\n'
}

gen > "$tmp/symbols.go"

if command -v gofmt >/dev/null; then
  gofmt -w "$tmp/symbols.go"
else
  echo "warning: gofmt not on PATH; symbols.go will be valid but unaligned" >&2
fi

if [ "$DRY_RUN" -eq 1 ]; then
  echo >&2
  echo "dry run: would write to $OUT_DIR" >&2
  echo "  thorvg.wasm  $(wc -c < "$wasm" | tr -d ' ') bytes" >&2
  echo "  symbols.go   $n_export exports, $n_import imports, @thorvg/webcanvas $version" >&2
  exit 0
fi

cp "$wasm" "$OUT_DIR/thorvg.wasm"
cp "$tmp/symbols.go" "$OUT_DIR/symbols.go"
chmod 644 "$OUT_DIR/thorvg.wasm" "$OUT_DIR/symbols.go"

echo >&2
echo "wrote thorvg.wasm and symbols.go ($n_export exports, $n_import imports)" >&2
echo "for @thorvg/webcanvas $version into $OUT_DIR" >&2
echo >&2
echo "next: go test ./..." >&2
