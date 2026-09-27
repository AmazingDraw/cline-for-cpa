# cline-for-cpa

CLIProxyAPI C ABI plugin: OpenAI-compatible `chat/completions` → **Cline** (`https://api.cline.bot/api/v1`),
with **Stream Guard** (fail fast when upstream stalls instead of sitting behind a ~300s keepalive),
and a host **refresh contract** the built-in providers already have but `cline` does not.

| Item | Notes |
| :-- | :-- |
| **Version** | see `plugin/register.go` (`PluginVersion`) |
| **Platforms** | darwin/arm64 (`.dylib`) and linux/amd64 (`.so`); build **natively** on each (CGO `c-shared`, no cross-compile) |
| **Models** | `cline-pass/*` (subscription) + `cline-free/*` + `stealth/*` (free pools) |
| **Blueprint** | `cursor-for-cpa`; watchdog semantics aligned with OpenCodex T04 + Cline desktop first/chunk timeouts |

> This public mirror is a **sanitized** export of a private working tree. Internal deployment runbooks, private docs, and host-specific paths are not included. See [Dual-repo sync](#dual-repo-sync) if you maintain a private fork.

---

## Quick start

```yaml
plugins:
  enabled: true
  configs:
    cline-for-cpa:
      enabled: true
      base_url: https://api.cline.bot/api/v1
      # OAuth only: log in via the management panel. API keys are ignored.
      refresh_interval_seconds: 600   # host refresh contract; missing → periodic 503
```

Smoke checks after install:

```bash
# ① Model count (host config may hide some via oauth-excluded-models / oauth-model-alias)
curl -s http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <client-key>" \
  | python3 -c "import json,sys;print(len([m for m in json.load(sys.stdin)['data'] if 'cline' in m['id']]))"

# ② Pool routing evidence (throttled: ≤1 line per route per hour)
grep -a "route " ~/.cli-proxy-api/logs/cline-for-cpa.log | tail

# ③ Model metadata drift
go run ./tools/modelmeta --check
```

> Turn off a same-named `openai-compatibility` `cline-pass` reverse proxy, or requests bypass Stream Guard.

---

## Capabilities

| Capability | One-liner |
| :-- | :-- |
| **Dual namespaces** | `cline-pass/*` counts toward subscription; `cline-free/*` uses the free pool; bare ids → pass (legacy clients) |
| **Expose = blacklist** | Everything in those namespaces is exposed by default; hide via `plugin/models.go:excludedModels` |
| **Per-model metadata** | Context / max output / modalities / params from `tools/modelmeta` static table |
| **Stream Guard** | First frame 60s / silence 60s / heartbeat-only 180s; heartbeat & comment frames count as alive but not progress. Timeouts & transport failures use OpenAI-shaped `{error:{message,type,code,retryable}}` (streaming via `host.stream.close`) |
| **Non-streaming** | `stream:false` is rewritten to SSE upstream then aggregated to `chat.completion` (Cline rejects non-stream) |
| **Refresh contract + 401 split** | Plugin advertises refresh lead so the host wakes it; new token still rejected → 401 reauth; cannot obtain new token → **503 retryable** (not permanently dead) |
| **Official OAuth alignment** | Cross-process `flock` / single-flight / re-read under lock / anti-zombie / 30s grace / 5‑minute lead |
| **Client identity headers** | Same 10 identity headers as official; `profile: cli` switches to CLI identity |
| **Auth card** | Title = identity (OAuth email / key `API Key ····last4`, overridable via `metadata.label`); subtitle = real filename |
| **Quota ABI** | plan + `/users/me/plan/usage-limits` percentages; works for key-only as well as OAuth |
| **Quota error typing** | Upstream puts limits in prose → typed 429 `cline_free_limit` / `cline_pass_limit` (retryable) |

> Global `streaming.keepalive-seconds` only warms the **client** socket; it does not prove upstream progress. Anti-stall is Stream Guard.

---

## Models: namespaces, exposure, metadata

| Namespace | Pool | Billing |
| :-- | :-- | :-- |
| `cline-pass/…` | ClinePass subscription | Counts toward subscription |
| `cline-free/…` | Free pool | No subscription; **per-model timed quota** |
| `stealth/…` | Experimental free | No subscription; not verified in official docs |

**Blacklist, not whitelist**: models in the served namespaces are released by default. After Cline adds models, run `go run ./tools/modelmeta` once — no code change required. To hide one, add a line in `plugin/models.go:excludedModels` (namespaced entry hides that id only; bare leaf hides the same leaf across namespaces).

Refresh metadata (offline merge of two upstream feeds into a static table — no runtime fetch):

```bash
go run ./tools/modelmeta           # pull & rewrite plugin/modelmeta_gen.go
go run ./tools/modelmeta --check   # report drift only (ignore header timestamps; exit 1 on drift)
go run ./tools/modelmeta -v        # per-row hit mode (override/exact/vendor/prefix)
bash build.sh --deploy             # rebuild after refresh
```

**Only direct proof the free pool is live**: same leaf names across pools, identical upstream response shape — so the plugin logs a throttled route line:

```
[15:55:34] route cline-free/mimo-v2.6-flash → cline-free/mimo-v2.6-flash
[15:55:36] route cline-pass/mimo-v2.6-flash → cline-pass/mimo-v2.6-flash
```

---

## Configuration

Fields under `plugins.configs.cline-for-cpa` (source of truth: `plugin/config.go`).

**Credentials & upstream**

| Field | Default | Notes |
| :-- | :-- | :-- |
| `credential_preference` | `oauth_first` | Enum kept for old YAML; API keys are ignored |
| ~~`api_key`~~ | — | Removed in 0.4.0 |
| `base_url` | `https://api.cline.bot/api/v1` | Upstream base |
| `auth_dir` | `~/.cli-proxy-api/auths` | Auth dir for refresh lock / re-read under lock |
| `share_desktop_store` | `false` | Also contend for official `providers.json.oauth-*.lock` |
| `http_timeout_seconds` | `30` | Control-plane timeout; streaming unaffected |

**Client identity (10 upstream headers)**

| Field | Default | Notes |
| :-- | :-- | :-- |
| `profile` | — | `cli` → `cline-cli` + `platform=cli`; `desktop` → `platform=desktop` |
| `client_type` | `cline-desktop` | `X-CLIENT-TYPE` |
| `client_version` | `0.0.33` | Bare version → `User-Agent: Cline/<ver>` |
| `platform` / `platform_version` / `core_version` / `is_multi_root` | derived | Per-header overrides |
| `task_id` | empty | `X-Task-ID`; omitted when empty |
| `http_referer` / `x_title` | `https://cline.bot` / `Cline` | Static headers |

**Stream & guard**

| Field | Default | Notes |
| :-- | :-- | :-- |
| `first_frame_timeout_seconds` | `60` | Stream Guard first frame |
| `stream_silence_timeout_seconds` | `60` | Silence between inbound frames |
| `stream_heartbeat_only_timeout_seconds` | `180` | Heartbeat-only ceiling (**not** retryable) |
| `force_stream_upstream` | `true` | Non-stream client → stream upstream, then aggregate |
| `reasoning_effort_normalize` | `true` | Fix/drop illegal `reasoning_effort` (unknown values → empty content upstream) |

**Host contract**

| Field | Default | Notes |
| :-- | :-- | :-- |
| `refresh_interval_seconds` | `600` | Tell the host when to poll refresh. Without it the host never pre-refreshes `cline` → eventual 503; `0` disables (not recommended) |
| `enabled` | — | Host gate: `false` → plugin not loaded |

CLI identity example:

```yaml
plugins:
  configs:
    cline-for-cpa:
      profile: cli
      client_version: "3.0.64"
```

---

## Build

`build.sh` builds for the **current** OS/arch (darwin → `.dylib`, linux → `.so`). CGO `c-shared` must be built **natively** on the target platform — do not cross-compile Darwin→Linux.

```bash
bash build.sh              # → dist/cline-for-cpa-v<version>.{dylib|so}
bash build.sh --deploy     # → ~/.cli-proxy-api/plugins/<os>/<arch>/ (prunes older same-plugin artifacts)
```

On macOS, reload the host after deploy (launchd label may differ on your machine):

```bash
# example — adjust to your host service name
launchctl kickstart -k "gui/$(id -u)/com.cli-proxy-api" && sleep 3
lsof -p $(pgrep -f cli-proxy | head -1) | grep -o "cline-for-cpa-v[0-9.]*\.dylib"
```

On linux, build inside an environment that has Go + a C toolchain matching your deployment image, then install the `.so` into the host’s plugin directory for `linux/<arch>`.

---

## Credentials & single-writer refresh

Cline rotates `refreshToken` on every refresh
([cline/cline#13821](https://github.com/cline/cline/issues/13821)).
`flock` only serializes **within one machine**, so two hosts refreshing the same OAuth file will tread on each other.

Practical rule: **one hot endpoint** holds the live OAuth auth file and refreshes; other machines use key-only (or stay disabled) if they must not compete.

OAuth only. Transient refresh failure keeps the current token if it is still valid. API keys are not used.

`invalid_grant` / `invalid_token` / `unauthorized` are the **only** cases that require re-authorization (`cline_reauth_required`). Re-login via the panel.

---

## Troubleshooting

| Symptom | Likely cause | What to do |
| :-- | :-- | :-- |
| All `cline-pass/*` **503** / `auth_unavailable`, 2–48ms, empty plugin logs | Host marked the auth permanently dead after one 401 | Restart host / reauth for immediate relief; fix with `refresh_interval_seconds` + 401 semantic split |
| 429 `cline_free_limit` / `cline_pass_limit` | Upstream encodes limits in message text | Wait for reset in the message, or switch pool |
| Stream hangs ~300s | Stream Guard bypassed | Check the three timeout fields; disable same-named openai-compatibility proxy |
| Empty content + HTTP 200 | Illegal `reasoning_effort` | Keep `reasoning_effort_normalize` on |
| Non-stream `empty response content` | Upstream rejects non-stream | Keep `force_stream_upstream` on |
| Confirm free pool | Only evidence is route log | `grep -a "route " ~/.cli-proxy-api/logs/cline-for-cpa.log` |
| Host pre-refresh contract | Log line `host polled auth.refresh (refresh contract active)` ≤1/hour | Appearing long before expiry = contract is live |

---

## Dual-repo sync

Maintainers with a **private** working tree can publish a sanitized mirror:

1. Private repo keeps deployment scripts, internal docs, and measured host tables.
2. `scripts/sync-to-open-source.sh` archives **private `HEAD` only** (never dirty worktree), drops private paths, replaces `README.md` with this public text, scrubs `CHANGELOG.md`, fail-closed scans, then commits/pushes to `AmazingDraw/cline-for-cpa`.
3. Dry run: `DRY_RUN=1 bash scripts/sync-to-open-source.sh`

Blacklisted from the public tree: `build-nas.sh`, `docs/`, `dist/`, `.DS_Store`, the private sync script itself, and `README.open-source.md` (content is applied as `README.md`).

---

## License

MIT — see [LICENSE](LICENSE).

## Changelog

See [CHANGELOG.md](CHANGELOG.md) (sanitized for the public mirror).
