# Changelog

改代码必升号。机制长文在 [docs/](docs/)，现行用法 [README](README.md)。

## 0.4.9 — 2026-10-07

模型表跟随上游：新增 `cline-free/solar-mini4`；去掉 `cline-cloud/glm-5.3`、`cline-cloud/kimi-k3`（25→24 行）；`cline-pass/minimax-m3` 仅排序变化。Desktop UA 兜底仍为 **0.0.43**（对齐 live cache desktop）；CLI 仍走动态探测（cache cli **3.0.68**）。

测试：`TestExposedModelsTiers` 免费池下限 2→3（free+stealth 现为 3）。

## 0.4.8 — 2026-10-06

模型表跟随上游：免费池去掉 `stealth/space-bunny-alpha`、`cline-free/deepseek-v4.1-flash`（free+stealth 现为 2）；`openai/gpt-6-astra` / `openai/gpt-6.1-sol` Parameters 增 `verbosity`；`cline-free/mimo-v2.6-flash` / `cline-pass/mimo-v2.6-flash` Parameters 增 `logprobs`/`top_logprobs`。Desktop UA 兜底 **0.0.39→0.0.43**（对齐 live cache desktop）；CLI 仍走动态探测（cache cli **3.0.68**）。

测试：`TestExposedModelsTiers` 免费池下限 4→2，去掉 stealth 钉死与已下架 free 模型。

## 0.4.7 — 2026-10-01

模型表跟随上游：免费池去掉 `stealth/pixel-canary`（free+stealth 现为 4）；`cline-pass/glm-5.3` / `cline-cloud/glm-5.3` MaxOutputTokens 943717→943718。Desktop UA 兜底仍为 **0.0.39**（live cache 未变）。

测试：`TestExposedModelsTiers` 免费池下限 5→4，跟 live feed。

## 0.4.6 — 2026-09-30

`defaultClientVersion` 0.0.36 → **0.0.39**（对齐今晚 live cache 的 desktop UA 兜底）。

测试：ABI 窗口跟 `lookupModel`，不再钉死 1048576（开源 CI 在 0.4.5 刷新后红了）。不升号。

开源 CI：`modelmeta-drift` 补上每日 `schedule`；缺 `CLINE_BEARER` 改为失败而不是跳过。

## 0.4.5 — 2026-09-30

流式路径识别 `finish_reason:"error"`（复用 `midStreamErrorDetail`），不再当成功收尾。零输出：`server_error` fresh retry 一次；`context_length_exceeded` / `content_filter` 结构化失败且 `retryable:false`；`rate_limit` 按 429、`retryable:true`，不立即重试。已吐字则不重放，`host.stream.close` 带错误。`chat/completions` 失败与流内错误记录 `x-request-id`。402 文案指向 app.cline.bot。模型表刷新：免费池去掉 `cline-free/gemini-3.8-flash`（free+stealth 现为 5）。

## 0.4.4 — 2026-09-30

审查修补：Stop 以 `parent.Err()` 优先于首轮 stall（避免重试中途取消被报成 504）；`shouldRetryFreshConnection` 排除 4xx（含 body 带 EOF 字样的 400）；collect 重试日志补 `fresh_retry=1`。

## 0.4.3 — 2026-09-30

断流续上 P0/P1：`runGuardedProxy` 每轮独立 attempt ctx（Guard 只取消本轮），stall 分类优先于 poisoned `canceled`；`shouldRetryFreshConnection` 覆盖 `INTERNAL_ERROR` / stream error / `http2:` / unexpected EOF（仍限零输出、最多一次）。collect 路径与 async 结果优先级对齐。

## 0.4.2 — 2026-09-28

移除 OAuth-only 后无用的 API Key 路径：`resolveAPIKeys` / `apiKeyFromAuth`、希腊名 key 文件、key-only `auth.refresh` 回声与 `/users/me` 身份补全。残留 `api_key` yaml 字段仍可加载但不生效；auth 文件卫生仍会剥掉 OAuth 文件里误写的 `api_key`。

## 0.4.1 — 2026-09-27

`stream_silence_timeout_seconds` 默认 60 → **120**。

## 0.4.0 — 2026-09-27

从 **0.3.23**（改坏前最后一版）切到只走 OAuth：不播种、不执行、不解析 API Key；面板去掉 `credential_preference`。execute 同时认宿主 PascalCase `StorageJSON`。

稳定回滚点：分支 `v0.3.23-stable`。0.3.24 / 0.3.25 Key 治理线已废弃。

## 更早（一句话）

| 版本 | 要点 |
| :-- | :-- |
| 0.3.23 | 改坏前主版本；免费池 + 黑名单 + 刷新契约齐 |
| 0.3.14–0.3.22 | 动态模型/版本探测、配额、希腊名 key 文件、CI、linux 部署端构建防腐（Key 时代） |
| 0.3.0–0.3.13 | 接入 `cline-free/*`、元数据工具、`model.for_auth` 分池 |
| 0.2.x | Stream Guard、官方 OAuth flock、503 刷新契约、身份头 |
| 0.1.x | 首版可用 |

完整条目：`git log -- CHANGELOG.md`
