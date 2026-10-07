#!/usr/bin/env bash
# Build cline-for-cpa as a CLIProxyAPI C ABI plugin.
# On macOS → dist/cline-for-cpa-v${VERSION}.dylib
# On Linux → dist/cline-for-cpa-v${VERSION}.so
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

VERSION="${PLUGIN_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  # Single source of truth: the plugin's own version constant. Hardcoding it here
  # once produced a build that was correctly compiled but named with the previous
  # version — the host loads by filename, so a mislabel is a silent lie.
  #
  # The pattern accepts an optional `const`/`var` prefix, because PluginVersion
  # is a var so the -X ldflags below can actually stamp it; the sed must keep
  # finding it there. -E is required: BSD sed (macOS) does not accept \+ in a
  # basic regex, and would silently yield an empty VERSION under `set -u`.
  VERSION="$(sed -E -n 's/^[[:space:]]*(const[[:space:]]+)?(var[[:space:]]+)?PluginVersion[[:space:]]*=[[:space:]]*"([^"]*)".*/\3/p' "$ROOT/plugin/register.go" | head -1)"
fi
if [[ -z "$VERSION" ]]; then
  echo "cannot determine plugin version from plugin/register.go" >&2
  exit 1
fi
GO_BIN="${GO_BIN:-}"
if [[ -z "$GO_BIN" ]]; then
  if [[ -x /opt/homebrew/bin/go ]]; then
    GO_BIN=/opt/homebrew/bin/go
  else
    GO_BIN="$(command -v go)"
  fi
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64) GOARCH=amd64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

mkdir -p dist
EXT=so
[[ "$OS" == "darwin" ]] && EXT=dylib
OUT="dist/cline-for-cpa-v${VERSION}.${EXT}"

# dist/ accumulated every historical artifact (47 files / 318 MB at the time of
# the 2026-09-26 review) because nothing pruned it: --deploy cleans the *host*
# plugin dir but never the local build output. Keep the current version and the
# previous one — the previous is what you fall back to when the new build turns
# out to be broken — and drop everything older.
prune_dist() {
  local keep_new="$1" keep_old="${2:-}"
  local f base
  # Only the artifact family for the platform being built. Pruning the linux
  # .so files during a darwin build silently destroyed the NAS artifacts — they
  # cannot be regenerated from macOS, since a CGO c-shared library must be built
  # natively on linux/amd64 inside the container.
  for f in dist/cline-for-cpa-v*."$EXT"; do
    [[ -e "$f" ]] || continue
    base="$(basename "$f")"
    [[ "$base" == "$keep_new" || "$base" == "$keep_old" ]] && continue
    rm -f "$f"
  done
}

if [[ "${1:-}" == "--clean" ]]; then
  prune_dist "" ""
  find dist -name '*.h' -delete 2>/dev/null || true
  echo "cleaned dist/ ($(du -sh dist | cut -f1) remains)"
  exit 0
fi

# ---------------------------------------------------------------------------
# Force-align Desktop/CLI UA fallbacks with live latest before every build.
# Prefer ~/.cli-proxy-api/data/cline-version-cache.json; also probe npm when
# reachable. If hardcoded defaults lag the known latest, FAIL (not warn).
# ---------------------------------------------------------------------------
semver_lt() {
  # true (0) iff $1 < $2 under version sort
  local a="$1" b="$2"
  [[ -z "$a" || -z "$b" ]] && return 1
  [[ "$a" == "$b" ]] && return 1
  [[ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | head -1)" == "$a" ]]
}

extract_go_string_const() {
  # $1=file $2=const name → bare string value
  sed -E -n "s/^[[:space:]]*${2}[[:space:]]*=[[:space:]]*\"([^\"]*)\".*/\1/p" "$1" | head -1
}

HEADERS_GO="$ROOT/plugin/cline_headers.go"
DESKTOP_FALLBACK="$(extract_go_string_const "$HEADERS_GO" defaultClientVersion)"
CLI_FALLBACK="$(extract_go_string_const "$HEADERS_GO" defaultCLIClientVersion)"
if [[ -z "$DESKTOP_FALLBACK" || -z "$CLI_FALLBACK" ]]; then
  echo "ERROR: cannot read defaultClientVersion / defaultCLIClientVersion from $HEADERS_GO" >&2
  exit 1
fi

CACHE_JSON="${HOME}/.cli-proxy-api/data/cline-version-cache.json"
CACHE_DESKTOP=""
CACHE_CLI=""
if [[ -f "$CACHE_JSON" ]]; then
  # Prefer python for robust JSON; fall back to grep if unavailable.
  if command -v python3 >/dev/null 2>&1; then
    CACHE_DESKTOP="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('desktop',{}).get('version',''))" "$CACHE_JSON" 2>/dev/null || true)"
    CACHE_CLI="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('cli',{}).get('version',''))" "$CACHE_JSON" 2>/dev/null || true)"
  fi
fi

PROBE_DESKTOP=""
PROBE_CLI=""
if command -v curl >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
  PROBE_CLI="$(curl -fsSL --max-time 5 "https://registry.npmjs.org/cline/latest" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('version',''))" 2>/dev/null || true)"
  PROBE_DESKTOP="$(curl -fsSL --max-time 5 "https://registry.npmjs.org/@cline/core/latest" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('version',''))" 2>/dev/null || true)"
fi

pick_latest() {
  local best=""
  local v
  for v in "$@"; do
    [[ -z "$v" ]] && continue
    if [[ -z "$best" ]] || semver_lt "$best" "$v"; then
      best="$v"
    fi
  done
  printf '%s' "$best"
}

REQ_DESKTOP="$(pick_latest "$CACHE_DESKTOP" "$PROBE_DESKTOP")"
REQ_CLI="$(pick_latest "$CACHE_CLI" "$PROBE_CLI")"

echo "client UA fallbacks: desktop=${DESKTOP_FALLBACK}  cli=${CLI_FALLBACK}"
echo "live reference:      desktop=${REQ_DESKTOP:-?} (cache=${CACHE_DESKTOP:--} probe=${PROBE_DESKTOP:--})  cli=${REQ_CLI:-?} (cache=${CACHE_CLI:--} probe=${PROBE_CLI:--})"

FAIL=0
if [[ -n "$REQ_DESKTOP" ]] && semver_lt "$DESKTOP_FALLBACK" "$REQ_DESKTOP"; then
  echo "ERROR: defaultClientVersion=${DESKTOP_FALLBACK} lags live desktop ${REQ_DESKTOP}." >&2
  echo "       Update plugin/cline_headers.go defaultClientVersion to ${REQ_DESKTOP} before building." >&2
  FAIL=1
fi
if [[ -n "$REQ_CLI" ]] && semver_lt "$CLI_FALLBACK" "$REQ_CLI"; then
  echo "ERROR: defaultCLIClientVersion=${CLI_FALLBACK} lags live CLI ${REQ_CLI}." >&2
  echo "       Update plugin/cline_headers.go defaultCLIClientVersion to ${REQ_CLI} before building." >&2
  FAIL=1
fi
if [[ -z "$REQ_DESKTOP" && -z "$REQ_CLI" ]]; then
  echo "ERROR: no live desktop/CLI version from cache or npm probe; cannot verify fallbacks." >&2
  echo "       Ensure ${CACHE_JSON} exists or network reachability to registry.npmjs.org." >&2
  FAIL=1
fi
if [[ "$FAIL" -ne 0 ]]; then
  exit 1
fi

echo "building $OUT with $GO_BIN (GOOS=$OS GOARCH=$GOARCH)"
echo "release checklist (also enforced above for client versions):"
echo "  1. plugin/cline_headers.go  defaultClientVersion + defaultCLIClientVersion (= live latest)"
echo "  2. go run ./tools/modelmeta && go run ./tools/modelmeta --check"
CGO_ENABLED=1 GOOS="$OS" GOARCH="$GOARCH" "$GO_BIN" build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X cline-for-cpa/plugin.PluginVersion=${VERSION}" \
  -o "$OUT" .
rm -f "dist/cline-for-cpa-v${VERSION}.h" "${OUT%.${EXT}}.h" cline-for-cpa-v${VERSION}.h 2>/dev/null || true
# go c-shared may drop .h next to the output
find dist -name '*.h' -delete 2>/dev/null || true

# Prune superseded artifacts, sparing the one we just built and the one before it.
PREV="$(ls -1 dist/cline-for-cpa-v*."$EXT" 2>/dev/null |
  grep -v "cline-for-cpa-v${VERSION}\.${EXT}$" | sort -V | tail -1 || true)"
prune_dist "cline-for-cpa-v${VERSION}.${EXT}" "$(basename "${PREV:-/nonexistent}")"

echo "built $OUT"

if [[ "${1:-}" == "--deploy" ]]; then
  DEST="${HOME}/.cli-proxy-api/plugins/${OS}/${GOARCH}"
  mkdir -p "$DEST"
  # Remove older versions of this plugin only
  rm -f "$DEST"/cline-for-cpa-v*.dylib "$DEST"/cline-for-cpa-v*.so 2>/dev/null || true
  cp "$OUT" "$DEST/"
  echo "deployed → $DEST/$(basename "$OUT")"
  echo "NOTE: did not restart CLIProxyAPI / 8317 — enable in config and restart yourself."
fi
