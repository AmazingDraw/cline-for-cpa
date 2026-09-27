# cline-for-cpa

CLIProxyAPI C ABI plugin: OpenAI-compatible `chat/completions` → **Cline**.
Stream Guard + host refresh contract. **OAuth only** (management-panel login).

| Item | Notes |
| :-- | :-- |
| **Version** | `plugin/register.go` (`PluginVersion`) |
| **Artifacts** | darwin/arm64 `.dylib` · linux/amd64 `.so` — native CGO `c-shared`, no cross-compile |
| **Models** | `cline-pass/*` (subscription) · `cline-free/*` + `stealth/*` (free) |

This public tree is a sanitized export. Private deploy runbooks are not included.

---

## Config

```yaml
plugins:
  configs:
    cline-for-cpa:
      enabled: true
      refresh_interval_seconds: 600   # missing → periodic 503
```

Disable a same-named `openai-compatibility` `cline-pass` proxy or Stream Guard is bypassed.

Defaults: first frame 60s / silence 120s / heartbeat-only 180s / `force_stream_upstream: true`.
`profile: cli` switches identity headers. Source of truth: `plugin/config.go`.

---

## When bumping the plugin

Before `bash build.sh` / a version bump, refresh **static fallbacks** (not the live probes):

1. **Desktop UA fallback** — `defaultClientVersion` in `plugin/cline_headers.go`
2. **Model catalog** — `go run ./tools/modelmeta` then `--check` (`plugin/modelmeta_gen.go`)

`build.sh` prints this checklist on every build.

---

## Build

```bash
bash build.sh              # → dist/cline-for-cpa-v<version>.{dylib|so}
bash build.sh --deploy     # host plugin dir; prunes older same-plugin files
```

Reload the host after deploy. On Linux, build inside an image with Go + a C toolchain matching the runtime.

---

## Credentials

One hot host only: Cline rotates `refreshToken` on every refresh
([cline/cline#13821](https://github.com/cline/cline/issues/13821)); `flock` is per-machine.

Re-login only for `invalid_grant` / `invalid_token` / `unauthorized`.

---

## Troubleshooting

| Symptom | What to do |
| :-- | :-- |
| All 503 / `auth_unavailable`, 2–48ms | Host marked auth dead — restart/reauth; keep `refresh_interval_seconds` |
| 429 `cline_*_limit` | Wait for reset or switch pool |
| Stream hangs ~300s | Stream Guard bypassed |
| HTTP 200 empty body | Illegal `reasoning_effort` |
| Free-pool proof | `grep -a "route "` in the plugin log |

---

## Dual-repo sync

`scripts/sync-to-open-source.sh` archives **private HEAD only**, scrubs, fail-closed scans, pushes the public mirror. Dry run: `DRY_RUN=1 bash scripts/sync-to-open-source.sh`

Dropped from the public tree: `build-nas.sh`, `docs/`, `dist/`, the private sync script.

MIT — [LICENSE](LICENSE). Changelog: [CHANGELOG.md](CHANGELOG.md).
