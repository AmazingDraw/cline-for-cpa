# Changelog

改代码必升号。机制长文在 [docs/](docs/)，现行用法 [README](README.md)。

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
