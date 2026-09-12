#!/usr/bin/env bash
#
# Download the Noto animated emoji as Lottie JSON into testdata/emoji.
#
# The set is the one behind https://googlefonts.github.io/noto-emoji-animation/.
# That page reads a metadata document listing every emoji; this script reads the
# same document and pulls the "lottie" asset for each entry.
#
# Usage:
#   scripts/fetch-noto-emoji.sh [-o DIR] [-n COUNT] [-j JOBS] [-f REGEX] [-F] [-l]
#
#   -o DIR     output directory (default: testdata/emoji)
#   -n COUNT   stop after COUNT emoji; 0 means all (default: 0)
#   -j JOBS    parallel downloads (default: 8)
#   -f REGEX   only emoji whose name or codepoint matches this ERE
#   -F         re-download files that already exist
#   -l         list the selected "codepoint<TAB>name<TAB>url" rows and exit
#
# Examples:
#   scripts/fetch-noto-emoji.sh -n 25             # a small sample
#   scripts/fetch-noto-emoji.sh -f 'cat|dog'      # just the animals you want
#   scripts/fetch-noto-emoji.sh -f cat -l         # see what that would fetch
#   scripts/fetch-noto-emoji.sh                   # everything (~880 files)
#
# The endpoints can be overridden for testing:
#   NOTO_API_URL     metadata document
#   NOTO_ASSET_HOST  base URL for assets (overrides the host in the metadata)
#   NOTO_JSON_TOOL   force "jq" or "python3" instead of preferring whichever is
#                    installed, to exercise both readers
#
# Licence: the Noto animated emoji are published by Google under CC BY 4.0.
# A NOTICE file recording that is written alongside the downloads.

set -euo pipefail

API_URL="${NOTO_API_URL:-https://googlefonts.github.io/noto-emoji-animation/data/api.json}"

# The metadata document ships "host" and "asset_url_pattern" as empty strings
# rather than omitting them, so they cannot be used as-is. These are the values
# the site itself falls back to; anything non-empty in the document wins.
DEFAULT_HOST="https://fonts.gstatic.com/s/e/notoemoji/latest"
DEFAULT_PATTERN='/$codepoint/$key.$ext'

OUT_DIR="testdata/emoji"
LIMIT=0
JOBS=8
FILTER=""
FORCE=0
LIST_ONLY=0

while getopts ":o:n:j:f:Flh" opt; do
  case "$opt" in
    o) OUT_DIR="$OPTARG" ;;
    n) LIMIT="$OPTARG" ;;
    j) JOBS="$OPTARG" ;;
    f) FILTER="$OPTARG" ;;
    F) FORCE=1 ;;
    l) LIST_ONLY=1 ;;
    h) awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; exit 0 ;;
    \?) echo "unknown option -$OPTARG" >&2; exit 2 ;;
    :)  echo "option -$OPTARG needs an argument" >&2; exit 2 ;;
  esac
done

case "$LIMIT" in ''|*[!0-9]*) echo "-n needs a non-negative integer, got '$LIMIT'" >&2; exit 2 ;; esac
case "$JOBS"  in ''|*[!0-9]*) echo "-j needs a positive integer, got '$JOBS'" >&2; exit 2 ;; esac
[ "$JOBS" -gt 0 ] || { echo "-j needs a positive integer, got '$JOBS'" >&2; exit 2; }

command -v curl >/dev/null || { echo "need curl on PATH" >&2; exit 1; }
JSON_TOOL="${NOTO_JSON_TOOL:-}"
if [ -n "$JSON_TOOL" ]; then
  command -v "$JSON_TOOL" >/dev/null \
    || { echo "NOTO_JSON_TOOL=$JSON_TOOL is not on PATH" >&2; exit 1; }
elif command -v jq >/dev/null; then
  JSON_TOOL=jq
elif command -v python3 >/dev/null; then
  JSON_TOOL=python3
else
  echo "need either jq or python3 to read the metadata" >&2
  exit 1
fi

# Repo root, so the script works from anywhere.
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mkdir -p "$OUT_DIR"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"; rm -f "$OUT_DIR"/.dl.*' EXIT

STATE="$tmp/state"
mkdir -p "$STATE"

echo "fetching metadata from $API_URL" >&2
curl -fsSL --retry 3 --retry-delay 1 -o "$tmp/api.json" "$API_URL"

# Emit "codepoint<TAB>slug<TAB>url" per emoji.
#
# Every icon's "name" is just "emoji_u<codepoint>", so the readable label comes
# from the first tag (":smile:" -> "smile"); that is what -f matches against.
extract() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -r \
      --arg hostOverride "${NOTO_ASSET_HOST:-}" \
      --arg defaultHost "$DEFAULT_HOST" \
      --arg defaultPattern "$DEFAULT_PATTERN" '
      def nonempty: select(type == "string" and . != "");

      (($hostOverride | nonempty) // (.host? | nonempty) // $defaultHost
        | sub("/+$"; "")) as $host
      | ((.asset_url_pattern? | nonempty) // $defaultPattern) as $pat
      | .icons[]
      | select(.codepoint? | nonempty)
      | . as $i
      | ($pat
          | gsub("\\$codepoint"; $i.codepoint)
          | gsub("\\$key"; "lottie")
          | gsub("\\$ext"; "json")) as $path
      | (((($i.tags? // []) | map(nonempty) | first) // ($i.name? | nonempty) // $i.codepoint)
          | ascii_downcase
          | gsub("[^a-z0-9]+"; "-")
          | sub("^-+"; "")
          | sub("-+$"; "")) as $slug
      | [ $i.codepoint, (if $slug == "" then $i.codepoint else $slug end), ($host + $path) ]
      | @tsv
    ' "$tmp/api.json"
  else
    DEFAULT_HOST="$DEFAULT_HOST" DEFAULT_PATTERN="$DEFAULT_PATTERN" \
    python3 - "$tmp/api.json" <<'PY'
import json, os, re, sys

def nonempty(v):
    return v if isinstance(v, str) and v else None

doc = json.load(open(sys.argv[1]))

host = (nonempty(os.environ.get("NOTO_ASSET_HOST"))
        or nonempty(doc.get("host"))
        or os.environ["DEFAULT_HOST"]).rstrip("/")
pattern = nonempty(doc.get("asset_url_pattern")) or os.environ["DEFAULT_PATTERN"]

for icon in doc.get("icons", []):
    cp = nonempty(icon.get("codepoint"))
    if not cp:
        continue
    path = (pattern.replace("$codepoint", cp)
                   .replace("$key", "lottie")
                   .replace("$ext", "json"))
    label = next((t for t in icon.get("tags") or [] if nonempty(t)), None) \
        or nonempty(icon.get("name")) or cp
    slug = re.sub(r"[^a-z0-9]+", "-", label.lower()).strip("-") or cp
    print("\t".join([cp, slug, host + path]))
PY
  fi
}

extract > "$tmp/all.tsv"

if [ ! -s "$tmp/all.tsv" ]; then
  echo "metadata parsed but produced no entries; the document shape has changed" >&2
  exit 1
fi

if [ -n "$FILTER" ]; then
  # Match against codepoint and slug, not the URL. The pattern is lowercased to
  # match the lowercased fields.
  re_lc="$(printf '%s' "$FILTER" | tr '[:upper:]' '[:lower:]')"
  awk -F'\t' -v re="$re_lc" 'tolower($1) ~ re || tolower($2) ~ re' "$tmp/all.tsv" > "$tmp/sel.tsv"
else
  cp "$tmp/all.tsv" "$tmp/sel.tsv"
fi

if [ "$LIMIT" -gt 0 ]; then
  head -n "$LIMIT" "$tmp/sel.tsv" > "$tmp/sel.head.tsv"
  mv "$tmp/sel.head.tsv" "$tmp/sel.tsv"
fi

if [ "$LIST_ONLY" -eq 1 ]; then
  cat "$tmp/sel.tsv"
  exit 0
fi

total=$(wc -l < "$tmp/sel.tsv" | tr -d ' ')
if [ "$total" -eq 0 ]; then
  echo "no emoji matched" >&2
  exit 1
fi
echo "downloading $total emoji into $OUT_DIR with $JOBS parallel jobs" >&2

# Each job records its outcome as a marker file rather than printing a line the
# parent has to parse; the counts are then race-free, since xargs has reaped
# every job before they are read.
json_ok() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -e . >/dev/null 2>&1 < "$1"
  else
    python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" >/dev/null 2>&1
  fi
}
export -f json_ok

# One download. Writes to a temp file and only moves it into place once the body
# parses as JSON, so an interrupted or error-page response never leaves a
# half-written .json behind for the tests to trip over.
fetch_one() {
  local cp slug url dest tmpfile code
  IFS=$'\t' read -r cp slug url <<< "$1"

  if [ -z "$cp" ] || [ -z "$url" ]; then
    : > "$STATE/fail.$$"
    echo "FAIL (malformed metadata row)" >&2
    return 0
  fi

  dest="$OUT_DIR/${cp}-${slug}.json"

  if [ "$FORCE" -eq 0 ] && [ -s "$dest" ]; then
    : > "$STATE/skip.$cp"
    echo "skip $dest" >&2
    return 0
  fi

  tmpfile="$(mktemp "$OUT_DIR/.dl.XXXXXX")"
  # curl's own error line is suppressed; one line per emoji is clearer. -w still
  # reports the status when -f makes curl fail, which separates "this emoji has
  # no Lottie asset" -- a handful are listed in the metadata but never published
  # -- from "the download broke". Only the latter is a failure.
  if ! code="$(curl -fsSL --retry 3 --retry-delay 1 --max-time 60 \
                    -o "$tmpfile" -w '%{http_code}' "$url" 2>/dev/null)"; then
    rm -f "$tmpfile"
    if [ "$code" = 404 ]; then
      : > "$STATE/absent.$cp"
      echo "none $cp (no lottie asset published)" >&2
    else
      : > "$STATE/fail.$cp"
      echo "FAIL $cp ($url)" >&2
    fi
    return 0
  fi

  if ! json_ok "$tmpfile"; then
    rm -f "$tmpfile"
    : > "$STATE/fail.$cp"
    echo "FAIL $cp (not JSON)" >&2
    return 0
  fi

  chmod 644 "$tmpfile"
  mv "$tmpfile" "$dest"
  : > "$STATE/ok.$cp"
  echo "ok   $dest" >&2
}
export -f fetch_one
export OUT_DIR FORCE STATE JSON_TOOL

tr '\n' '\0' < "$tmp/sel.tsv" \
  | xargs -0 -P "$JOBS" -I{} bash -c 'fetch_one "$@"' _ {}

count() { find "$STATE" -maxdepth 1 -name "$1.*" | wc -l | tr -d ' '; }
ok=$(count ok)
skipped=$(count skip)
absent=$(count absent)
failed=$(count fail)
have=$(find "$OUT_DIR" -maxdepth 1 -name '*.json' | wc -l | tr -d ' ')

cat > "$OUT_DIR/NOTICE" <<EOF
Noto animated emoji, downloaded from:
  $API_URL

Copyright Google Inc.
Licensed under the Creative Commons Attribution 4.0 International License
(CC BY 4.0): https://creativecommons.org/licenses/by/4.0/

Retrieved $(date -u +%Y-%m-%dT%H:%M:%SZ) by scripts/fetch-noto-emoji.sh
EOF

echo >&2
echo "done: $ok downloaded, $skipped already present, $absent not published, $failed failed" >&2
echo "      $have lottie files in $OUT_DIR" >&2
[ "$failed" -eq 0 ]
