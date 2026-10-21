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
# Force-align the three identity-version fallbacks with live latest before
# every build. The runtime sends max(cache, fallback) for each one, so a
# fallback that lags live means a fresh install can send a stale identity.
# Each fallback is checked against THE SAME source the runtime uses:
#
#   defaultClientVersion     Desktop app   GitHub tag desktop-v*  + cache "desktop"
#   defaultCLIClientVersion  CLI           npm `cline`            + cache "cli"
#   defaultCoreVersion       X-Core-Version npm `@cline/core`     + cache "core"
#
# Any fallback behind its live reference, or any source with no reference at
# all (cache missing AND probe failed), is a hard failure (exit 1).
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
CORE_FALLBACK="$(extract_go_string_const "$HEADERS_GO" defaultCoreVersion)"
if [[ -z "$DESKTOP_FALLBACK" || -z "$CLI_FALLBACK" || -z "$CORE_FALLBACK" ]]; then
  echo "ERROR: cannot read defaultClientVersion / defaultCLIClientVersion / defaultCoreVersion from $HEADERS_GO" >&2
  exit 1
fi

CACHE_JSON="${HOME}/.cli-proxy-api/data/cline-version-cache.json"
cache_field() {
  [[ -f "$CACHE_JSON" ]] || return 0
  python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get(sys.argv[2],{}).get('version',''))" "$CACHE_JSON" "$1" 2>/dev/null || true
}
CACHE_DESKTOP="$(cache_field desktop)"
CACHE_CLI="$(cache_field cli)"
CACHE_CORE="$(cache_field core)"

npm_latest() {
  curl -fsSL --max-time 5 "https://registry.npmjs.org/$1/latest" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('version',''))" 2>/dev/null || true
}
github_desktop_latest() {
  # First desktop-v* tag in the newest releases (same API the runtime probes).
  # GITHUB_TOKEN, if set, only raises the rate limit; it is never printed.
  local auth=()
  [[ -n "${GITHUB_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  curl -fsSL --max-time 8 "${auth[@]}" -H "Accept: application/vnd.github.v3+json" \
    "https://api.github.com/repos/cline/cline/releases?per_page=30" 2>/dev/null \
    | python3 -c "
import sys,json
for r in json.load(sys.stdin):
    t=r.get('tag_name','')
    if t.startswith('desktop-v'):
        print(t[len('desktop-v'):]); break" 2>/dev/null || true
}
PROBE_DESKTOP=""
PROBE_CLI=""
PROBE_CORE=""
if command -v curl >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
  PROBE_DESKTOP="$(github_desktop_latest)"
  PROBE_CLI="$(npm_latest cline)"
  PROBE_CORE="$(npm_latest @cline/core)"
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
REQ_CORE="$(pick_latest "$CACHE_CORE" "$PROBE_CORE")"

echo "identity fallbacks: desktop=${DESKTOP_FALLBACK}  cli=${CLI_FALLBACK}  core=${CORE_FALLBACK}"
echo "live reference:     desktop=${REQ_DESKTOP:-?} (cache=${CACHE_DESKTOP:--} github=${PROBE_DESKTOP:--})"
echo "                    cli=${REQ_CLI:-?} (cache=${CACHE_CLI:--} npm=${PROBE_CLI:--})"
echo "                    core=${REQ_CORE:-?} (cache=${CACHE_CORE:--} npm=${PROBE_CORE:--})"

FAIL=0
check_fallback() {
  # $1=label $2=const $3=fallback $4=required
  if [[ -z "$4" ]]; then
    echo "ERROR: no live ${1} version (cache missing and probe failed); cannot verify ${2}." >&2
    FAIL=1
  elif semver_lt "$3" "$4"; then
    echo "ERROR: ${2}=${3} lags live ${1} ${4}. Update plugin/cline_headers.go ${2} to ${4}." >&2
    FAIL=1
  fi
}
check_fallback desktop defaultClientVersion "$DESKTOP_FALLBACK" "$REQ_DESKTOP"
check_fallback cli defaultCLIClientVersion "$CLI_FALLBACK" "$REQ_CLI"
check_fallback core defaultCoreVersion "$CORE_FALLBACK" "$REQ_CORE"
if [[ "$FAIL" -ne 0 ]]; then
  exit 1
fi
if [[ "${CHECK_UA_ONLY:-0}" == "1" ]]; then
  echo "identity versions OK (CHECK_UA_ONLY=1, not building)"
  exit 0
fi

echo "building $OUT with $GO_BIN (GOOS=$OS GOARCH=$GOARCH)"
echo "release checklist (also enforced above for client versions):"
echo "  1. plugin/cline_headers.go  defaultClientVersion + defaultCLIClientVersion + defaultCoreVersion (= live latest)"
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
