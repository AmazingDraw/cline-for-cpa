# Changelog

> 只记「改了什么 / 为什么」。机制长文、宿主源码坐标与验证命令在 [docs/](docs/)；
> 现行用法见 [README](README.md)。版本号与产物一致，改代码必升号。

## 0.3.25 — 2026-09-27 ｜ 修 0.3.24 升级后「缺凭证」与面板表单拦 key

0.3.24 拆掉 config key 运行时兜底是对的，但把两条预先存在的缝暴露成了全面 401。

- **execute 读不到宿主交来的凭证**：宿主 `pluginapi.ExecutorRequest` 无 JSON tag，
  线上是 PascalCase `StorageJSON`。`json:"storage_json"` 对不上（下划线过不了
  大小写折叠），OAuth 文件和 `cline-key-monochord.json` 都交过来了，插件却当成空。
  以前靠 config 兜底「碰巧能打」；兜底一拆，16ms 全员 `missing_credentials`。
  现与 quota/auth 对齐，PascalCase / snake_case 都认。
- **面板填 key 报「请先修复插件配置表单错误」**：`api_keys` 注册成 `type: array` 后，
  管理中心把控件当成 JSON textarea，粘贴原始 key 或描述里的单引号 `['sk-1']`
  都过不了 `JSON.parse`。改为 `type: string`，加载时同时接受数组、单个 key、
  换行列表、JSON 数组字符串。

## 0.3.24 — 2026-09-27 ｜ Key 凭证治理：开关生效、增删同步、面板入口统一

治理方案（报告见 `~/Desktop/cline-for-cpa-Key凭证治理方案-20260926.md`）落地。
原则一句话：**config 负责播种，宿主负责路由，插件只负责执行；
插件永远不使用宿主没有交给它的凭证。**

### ① 开关生效（拆掉私有兑底）

- **`fallbackAPIKey` 不再回落 config key（`plugin/oauth.go`）**：
  key 只来自宿主交来的凭证本身。原先 OAuth 失败时会静默掏出 config 里的
  `api_key` 继续打 —— 恰是插件自己在调用点注释里立誓绝不做的事
  （"a silent switch into the billing pool"）。现在 OAuth 失败 → 干净报错。
- **`quota.go` 同步拆兑底**：无凭证时 `quota_fetch_failed`，绝不静默借用。
- **插件现在能看见 disabled 并拒绝执行**：`clineOAuthStorage` 新增 `Disabled`；
  检查位于磁盘修复**之后**（宿主交来的精简凭证由文件补全后依然能被拦住）。
- **磁盘修复补齐 `Disabled` 字段**（`plugin/oauth_refresh_lock.go` + `plugin/oauth.go`）：
  `reloadStorageFromDisk` 原先只按 email 找文件，**纯 key 凭证（无 email）的
  磁盘记录永远修不回来**；补上希腊序名称分支，`withDiskFallback` 的
  逐字段合并表也补入 `Disabled` —— 这是「加字段必须同步改合并表」的活例。

### ② 增删同步（所有权采纳）

- **采纳机制（`plugin/config.go`）**：无标记但 key 在配置里的 `cline-key-*.json`
  自动补 `managed_by` 标记（只补标记，**绝不覆盖 disabled**）。
  此前这类文件对回收逻辑不可见 —— 设置里删 key 后凭证文件原地活着、
  仍可路由仍计费（孤儿态），却「看起来删成功了」。
- **面板 key 入口统一**：注册字段删去单值 `api_key`，只保留 `api_keys` 数组；
  旧配置中的 `api_key` 在加载时自动并入数组（去重、不丢 key、打迁移日志）。
- **`resolveAPIKeys` 只认数组**；`CLINE_API_KEY` 环境变量通道彻底移除
  （三端均未设置过此变量，纯死路）。

### ③ 其它决策落地

- **models_updater 改 OAuth 优先**（`plugin/models_updater.go`）：原先是 config key 优先，
  意味着被面板关掉的 key 仍从这里打到 api.cline.bot。现改为凭证目录里的
  OAuth token 优先、config key 兑底 —— 该调用是拉取模型目录元数据（控制面，
  不产生推理计费），config key 仍是此处唯一可用的兑底，已注释固化
  「控制面唯一例外」。
- **Stream Guard 静默默认 60s → 120s**（`plugin/streamguard/guard.go`）：
  态考模型（实证 glm-5.3-flash）生成中途常有超一分钟的停帧，旧默认会把
  上游仍在生产的回合拦截掉；首帧 60s / 心跳 180s 不变。

### 测试

- 反转旧行为契约：`TestResolveCredentialsFallsBackToPluginAPIKeyWhenRefreshFails`
  → `TestResolveCredentialsNeverFallsBackToConfigAPIKey`（同场景改断言报错且
  bearer 不含 config key）。
- 新增：无凭证→ErrNoCredentials、停用凭证被拒（含磁盘标志位）、采纳保全
  disabled、托管件在 key 存活时不被篡改、删 key 同步回收、迁移去重、
  models_updater OAuth 优先、quota 永不借用 config key。
- `TestHandleQuotaFetchPlanError` 曾在旧写法下「错着过」（意外走到了无凭证
  路径），已改回 storage_json 供给，重新覆盖它本来要测的 500 路径。

## 0.3.24 补充（随版本一起构建）｜ 静态模型表跟随上游再漂移（发现上游振荡）

- 按铁律重跑 `tools/modelmeta`：上游 6 行 `MaxOutputTokens` 相对 0.3.22 补充 3
  修正后的值再次变化（deepseek-v4.1-flash 943718↔384000、glm-5.3 131072↔943717、
  glm-5.3-flash 131072↔128000，free/pass/cloud 三层同步）。
- **上游值在振荡**：01:06 会话逐条对账时为 943718/131072（当时 0 不一致），
  18:00 再抓已翻回。这些值仅影响对外声明的元数据（不钳制请求、不会拒单），
  但会让客户端对输出上限的认知来回摆动。
- **暂不写入 pinnedMeta**：两侧值都曾是「上游真值」，钉哪边都是猜。
  若继续振荡（例如一周内再翻一次），建议按振荡周期钉定或在展示层取并集，
  届时再定。`--check` 已归零。

## 0.3.22 补充 3（测试，不影响产物）｜ 补 errors / quota / version_updater 覆盖率

审查方案 B′：三块近零覆盖文件补表驱动单元测试，不升 `PluginVersion`。

- **`plugin/quota.go`**：identifier / describe / reset、`planToQuotaFetch` 窗口与状态、
  httptest 拉取 plan/usage-limits、无凭证、API Key / OAuth 取额度、404 合成 Free Tier、
  invalid_grant 重授权。
- **`plugin/errors.go`**：补齐 envelope 构造、stall 边沿、传输错误分型、OAuth 401 分型、
  本地 failure 构造器。
- **`plugin/version_updater.go`**：GitHub / npm URL 改为可在测试里指向 `httptest`
  （默认值不变），覆盖桌面 tag、npm fallback、磁盘快照垃圾过滤、`StartVersionUpdater`。

## 0.3.22 补充 2（工具链，不影响产物）｜ linux 部署端部署后自动收敛历史备份

- **`build` (linux native) 末尾新增 `prune_superseded_backups`**：
  - linux 部署流程把旧 `.so` 改名为 `.so.bak-<时间戳>` 以便回滚，但从不回收，
    目录一度累积 **18 个 cline 备份 / 127 MB**（含一份 v0.2.8 误标件、两份重复的 v0.3.3）；
  - 现在每次 linux 部署端构建后**只保留最新的 1 个 `.bak`**，其余自删；
  - 保留 1 个而非清零：刚构建出的 `.so` 是当前版本，最新的 `.bak` 是唯一的回滚点。
- **作用域严格受限**：只匹配 `cline-for-cpa-*.so.bak*`。
  该目录与 cursor / antigravity / usage-statistics 共用，
  范围放宽就会在下次构建 cline 时毁掉别的插件的回滚链 —— 已实测 cursor 产物不受影响。
- **两个实测踩到的坑**（都只在容器的 dash 下暴露，本地 macOS 测不出来）：
  1. **dash 的 `ls` + glob 二次展开不可移植**：`for f in $(ls -1t "$OUT"/pat)`
     对一个能匹配 3 个文件的模式**只吐出 1 个**，剪枝静默失效。改用 shell 原生 glob
     收集候选，再把已展开的字面路径交给 `ls -1t` 排序（无 glob 重解析、文件名无空格）。
  2. **`set -eu` 下的尾句陷阱**：函数末尾若是裸的 `[ -n "$keep" ] && echo`，
     在「无备份」时返回 1，会让**构建成功却以非零码退出**，
     在全新 linux 部署端首次部署时读起来就像失败。已改为显式 if/else + `return 0`。

## 0.3.22 补充（工具链，不影响产物）｜ linux 部署端构建脚本防腐：改跑 tarball 内的副本

- **修 linux 部署端构建的隐性故障源 (`build` (linux native))**：
  - 该脚本在容器里有**两份**——tarball 内的成员，以及 `/app/build-src/` 下单独投放的旧副本；
    而 `sh /app/build-src/`build` (linux native) 执行的是**后者**，它会腐化。
  - 2026-09-26 实测踩中：容器跑的是旧副本，其 `sed` 没有 `-E`，
    解析不了已改为 `var` 的 `PluginVersion` 行，直接报
    `cannot determine plugin version from plugin/register.go`；
    **而被解出来的、就在同一个 tarball 里的新脚本本身完全正确**。
  - 改为 `docker exec cliproxy sh -c 'cd /tmp/cline-for-cpa-src && sh `build` (linux native)'`，
    强制执行 tarball 内的副本，**脚本与它所构建的源码不可能来自不同修订**。
  - 已在头部注释写明新调用方式与事故原因。
  - **刻意不升 PluginVersion**：本次只改构建脚本，`.so` 产物与 0.3.22 完全一致
    （MD5 `176f49dd…` 双端一致），升版只会逼双端做一次无意义重编译与线上重启。

## 0.3.22 — 2026-09-26 ｜ 清偿剩余工程债：日志轮转、dist 剪枝、O(1) 希腊名查表、CI 四道闸

审查报告列出 7 项工程债，本轮清偿其中 5 项（覆盖率提升属方案 B′，未做）。

- **日志不再无限增长 (`plugin/persist.go`)**：
  - 新增 `rotateLogIfNeeded`：`logCredentialEvent` 写入前检查大小，超过 **5 MiB** 即轮转，
    保留一代历史为 `.1`；
  - 日志是纯 append 的（宿主不转发插件 stderr，这是唯一可诊断凭证事故的地方），
    但此前**没有任何裁剪**，linux 长跑会无限膨胀；
  - 此处用 `os.Rename` 是安全的：日志在 auth 目录之外，没有 fsnotify 监听它。
- **`dist/` 自我清理 (`build.sh`)**：
  - 新增 `--clean`，并使每次构建自动剪枝：**只保留当前版本 + 上一个版本**（留作回滚）；
  - 此前 `--deploy` 会清理**宿主**插件目录，却从不清理本地 `dist/`，
    审查时已积至 47 个产物 / 318 MB，本轮回落至 14 MB。
- **希腊名判定 O(1000) → O(1) (`plugin/greek_filename.go`)**：
  - `isGreekKeySequenceName` 原先每次调用循环 1000 次拼字符串，
    而它在 `auth.parse` / `auth.refresh` 热路径上对每个凭证文件都会跑一遍
    （宿主每 30s 轮询一次）；改为包级 `greekNameSet` 预生成表 + map 查找。
- **删除死代码 (`plugin/models.go`)**：`containsString` 仅被测试调用，已改用 `slices.Contains`。
- **新增 CI (`.github/workflows/ci.yml`)**：`gofmt` / `go vet` / `go test -race` /
  `go build -buildmode=c-shared`（linux/amd64）四道闸卡在 push 与 PR；
  另有一个定时 + 手动触发的 `modelmeta --check` 漂移检测（需 `CLINE_BEARER` secret，
  因依赖上游故不阻塞 PR）。
- **回归测试**：新增日志轮转与希腊名表完备性测试。

> ⚠️ 已知遗留：linux 部署端仍为 `0.3.19`，需在 cliproxy 容器内跑 `build` (linux native) 补齐
> （CGO c-shared 不能从 darwin 交叉编译）。本轮剪枝脚本曾误删本地 `dist/linux/amd64/`
> 下的 `.so`，已修复为「仅剪当前平台产物」；linux 部署端已部署的 `0.3.19` 未受影响，
> 但本地 `.so` 需随 linux 部署端 重新构建一并恢复。

## 0.3.21 — 2026-09-26 ｜ 代码审查修复：刷新契约跟随配置、命名函数去副作用、stall 竞争加固

本轮为全量代码审查（报告见桌面）后的精准闭环修复，改动面小、风险可控。

- **B1 修复：刷新契约不再写死 600 (`plugin/config.go`)**：
  - `syncConfigKeyCredential` 生成/更新 key 凭证时，`refresh_interval_seconds` 改用
    **生效配置值 `hostRefreshInterval(cfg)`**，不再是编译期常量；
  - 「已存在同 key 文件」分支新增同步：改配置后老凭证文件的该字段会真正被更新，
    修复「插件自报一个值、文件里存另一个值」的双真源分裂。
- **B2 修复：取名函数不再有写副作用 (`plugin/greek_filename.go`, `plugin/auth.go`)**：
  - 拆出纯函数 `resolveGreekKeyFileName()`（只读不写），`keyAuthFileName` /
    `keyAuthFileNameForDir`（auth.parse / auth.refresh 热路径，宿主 30s 轮询一次）
    改调纯函数，不再在「算文件名」时触发目录改名；
  - 迁移改走 `migrateKeyFile()`：**先写目标文件 → 再删源文件**，全程不经 `os.Rename`，
    与 `safeInPlaceWrite` 同一原则（避免宿主 fsnotify 收到 `NOTE_DELETE` 误判为凭证删除而注销）。
- **B3 修复：Stream Guard stall 跨 goroutine 竞争加固 (`plugin/executor.go`)**：
  - `collectStreamOnce` 与 `runGuardedProxy` 中由看门狗定时器 goroutine 写入的 `stall`
    纳入互斥锁保护，不再依赖 `cancel()` 建立的 happens-before 运气。
- **工程化加固**：
  - `PluginVersion` 由 `const` 改为 `var`，使 `build.sh` / `build` (linux native) 的
    `-ldflags -X` 真正生效（原先打在 const 上是空操作，`PLUGIN_VERSION` 覆盖会产出
    「文件名新、内嵌版本旧」的静默谎言）；同步修正两脚本的 BSD sed（需 `-E`）。
  - 全仓 `gofmt` 归位；新增 `plugin/review_fixes_test.go` 固化 B1/B2 回归。

## 0.3.20 — 2026-09-26 ｜ 固化 stealth/pixel-canary 免费模型并缩短模型发现缓存周期

- 重新生成 `modelmeta_gen.go`：固化 `stealth/pixel-canary` 等实验免费模型元数据；
- 模型发现缓存周期收紧至 12 小时（`plugin/models_updater.go`）。

## 0.3.19 — 2026-09-26 ｜ 净化 Web 管理面板配置字段 HTML 实体转义提示

- **去除配置字段描述中的双引号 (`plugin/register.go`)**：
  - 将 `api_keys` 提示说明中的 `["sk-1", "sk-2"]` 替换为单引号语法 `['sk-1', 'sk-2']`；
  - 彻底规避前端管理面板在将 `description` 插入 HTML DOM 时未经实体反解导致的 `[&#34;sk-1&#34;, ...]` 视觉乱码。
- **双端同步**：darwin（`v0.3.19.dylib`）与 linux（`v0.3.19.so`）编译部署。

## 0.3.18 — 2026-09-26 ｜ 注册 Web 管理面板 api_keys (array) 可视化配置字段

- **注册 `api_keys` Array 字段 (`plugin/register.go`)**：
  - 在 `ConfigFields` 元数据中注册 `type: "array"` 的 `api_keys` 字段；
  - Web 管理面板（`Settings ➔ Plugins ➔ Cline`）直接渲染多行大文本框，支持可视化输入 JSON 数组 `["sk-1", "sk-2"]`，无上限托管多个备用 Key。
- **双端同步**：darwin（`v0.3.18.dylib`）与 linux（`v0.3.18.so`）编译部署。

## 0.3.17 — 2026-09-26 ｜ 希腊乐律序列命名体系、多 Key 支持与防交叉污染防火墙

- **移植希腊乐律序数命名算法 (`plugin/greek_filename.go`)**：
  - 与 `cursor-for-cpa` 家族级规范完全统一，Cline 纯 Key 凭证命名正式升级为希腊乐律序列（`cline-key-monochord.json`、`cline-key-dichord.json` 等）；
  - 新增自动平滑迁移函数 `migrateLegacyKeyFiles`，启动时自动检测并原地重命名旧式 `cline-key-<末4>.json`。
- **拓展多 Key 配置与自动托管 (`plugin/config.go`)**：
  - 配置同时支持单个 `api_key: "sk_..."` 与多 key 列表 `api_keys: ["sk_1...", "sk_2..."]`；
  - 自动为每个配置的 key 生成对应的希腊序数凭证文件并维持托管生命周期。
- **凭证防交叉污染防火墙 (`plugin/auth.go`, `plugin/persist.go`, `plugin/config.go`)**：
  - 彻底根除纯 Key 凭证被误灌入 OAuth Token（`access_token`/`refresh_token`）导致优先级漂移的 Bug；
  - 强行切断 Key 文件从全局 OAuth 借用 Token 的暗渠；在 `oauth_first` 调度策略下，坚决锁定 Key 凭证优先级为 `0`（OAuth 独占 `1`），消灭两类凭证争抢 priority=1 的异常态。
- **版本指纹更新**：更新官方桌面端基线版本为 `0.0.36`。
- **双端同步**：darwin（`v0.3.17.dylib`）与 linux（`v0.3.17.so`）编译部署。

## 0.3.16 — 2026-09-25 ｜ 根治凭证刷新属性丢失与 macOS REMOVE 假删除抖动

- **Attributes 完整继承与 path 兜底保真 (`plugin/auth.go`)**：
  - 针对宿主在插件返回非空 `Attributes` 时不再继承原属性的机制，插件主动解析宿主传入的 `req.Attributes`，保留 `path`、`source`、`source_backend` 等系统级只读字段，仅增量注入 `priority`；
  - 增设 `authFilePath` 兜底回填机制，确保刷新返回的 `auth.Attributes["path"]` 恒不为空，彻底根除管理面板 `ListAuthFiles` 误判为空项导致凭证蒸发的深层 Bug。
- **安全原地截断覆写防 `REMOVE` 抖动 (`plugin/persist.go`, `plugin/config.go`)**：
  - 将所有 auth 文件的更新写盘从 `os.Rename` 原子替换改为原子的原地截断覆写（`safeInPlaceWrite`，`O_WRONLY|O_TRUNC|O_CREATE`）；
  - 保持底层 inode 不变，阻断 macOS Darwin 内核对重命名覆盖抛出 `NOTE_DELETE` / `NOTE_RENAME` 事件，使文件监听器仅收到 `fsnotify.Write`，彻底杜绝宿主误判删除及双重写盘竞争。
- **双端同步**：darwin（`v0.3.16.dylib`）与 linux（`v0.3.16.so`）编译部署。

## 0.3.15 — 2026-09-25 ｜ 凭证能力级模型动态隔离（免费账号自动屏蔽 cline-pass/*）

- **实现 `model.for_auth` 动态模型隔离**：
  - 插件针对每个凭证的底层套餐类型进行自动研判：普通免费 OAuth 凭证仅暴露 `cline-free/*` 与 `stealth/*` 系列模型；
  - 拥有 `Cline Pass` 付费订阅或配置有效 `api_key` 的凭证才暴露 `cline-pass/*` 订阅模型。
- **消灭跨套餐“脏轮询”风险**：
  - 宿主调度器（Scheduler）在接收到 `cline-pass/*` 请求时，在算法层原生感知并直接跳过所有免费账号，0 碰壁、0 延迟，精准命中付费凭证。
- **双端同步**：darwin（`v0.3.15.dylib`）与 linux（`v0.3.15.so`）编译部署。

## 0.3.14 — 2026-09-25 ｜ 官方 Gemini 3.8 Flash 接入与动态模型池自进化

- **新增模型动态发现与快照自进化 (`plugin/models_updater.go`)**：
  - 接入官方免费池最新主力模型 `cline-free/gemini-3.8-flash` 与实验池 `stealth/space-bunny-alpha`；
  - 移除已失效的过时黑名单（`solar-pro4`）；动态模型快照本地持久化，断网重启基线自进化；
  - 采用 24 小时低频静默更新策略，保障系统极致稳定与对齐。
- **错误提示优化**：精炼免费模型额度用尽提示，去除说教文本。
- **双端同步**：darwin（`v0.3.14.dylib`）与 linux（`v0.3.14.so`）编译部署。

- **指纹真源完全内聚**：
  - 官方桌面版全套指纹（`HTTP-Referer: https://cline.bot`、`X-Title: Cline`、`X-Client-Type: cline-desktop`、`X-Platform: desktop`、`X-IS-MULTIROOT: false` 以及动态 `0.0.34` 三头联动）由插件底层统一自洽输出，彻底消灭“部分写在配置文件、部分写在代码常量、部分在快照”的混乱状态。
  - 用户配置文件 `config.yaml` 极简化，仅保留业务必需的 `api_key`、`refresh_interval_seconds` 与 `credential_preference`。
- **动态版本精准对齐桌面端产品线**：
  - `version_updater.go` 针对 `desktop` 精准追踪官方 GitHub Releases 的 `desktop-v*` 发布源（当前 `0.0.34`），彻底区分 CLI 产品线（`3.0.65`），版本指纹 100% 严密自洽。
- **双端同步**：darwin（`v0.3.12.dylib`）与 linux（`v0.3.12.so`）编译部署。

## 0.3.11 — 2026-09-24 ｜ 客户端版本本地磁盘快照（断网重启基线自进化）

- **新增版本磁盘快照自动持久化 (`cline-version-cache.json`)**：
  - 动态版本引擎探测到官方新版后，自动在本地数据目录保存版本快照；
  - **断网冷启动基线自进化**：宿主重启时先自动载入磁盘快照；即便遇到完全断网或开机无网的极端情况，基线版本也是上次在线探活到的最新官方版本（如 `@cline/core` 当前 `0.0.86`、CLI 当前 `3.0.65`），彻底摆脱对代码死常量的硬依赖。
- **双端同步**：darwin（`v0.3.11.dylib`）与 linux（`v0.3.11.so`）编译部署。

## 0.3.10 — 2026-09-24 ｜ 自适应动态客户端版本引擎（三头联动 / 无感跟进 / 零延迟防抖）

- **新增动态客户端版本引擎 (`plugin/version_updater.go`)**：
  - 自动追踪上游官方发布源（NPM registry `@cline/core` 与 `cline`），后台平滑探测最新发布版本；
  - **零阻塞冷启动与多级回退**：启动时直接采用已知安全默认值（`0.0.34`）或内存缓存，后台异步静默刷新，TTL 6 小时 + 失败 10 分钟防抖，对用户请求零延迟影响；
  - **三头合一自动对齐**：解析出的最新版本号自动同步注入 `User-Agent: Cline/<ver>`、`X-Client-Version`、`X-Platform-Version` 与 `X-Core-Version`，防伪指纹永远处于官方最新状态；
  - **用户显式配置绝对优先**：`config.yaml` 若配置了 `client_version`，则绝对优先尊重用户配置。
- **双端同步**：darwin（`v0.3.10.dylib`）与 linux（`v0.3.10.so`）编译部署。

## 0.3.9 — 2026-09-24 ｜ 免费账户 Plan 404 优雅降级与额度结构对齐

- **免费账户 Plan 404 优雅降级**：未订阅 Cline Pass 的普通免费账号，调用 `GET /users/me/plan` 上游返回 404 时，插件不再上报红色的 `quota_fetch_failed` 错误，而是优雅降级合成合法的 `Free Tier` 响应包（标记 Plan 为 `Free Tier`、价格 `$0.00`、展示每日各模型滑动窗口限流说明），面板不再展示刺眼的红色报错。
- **双端同步**：darwin（`v0.3.9.dylib`）与 linux（`v0.3.9.so`）编译部署。

## 0.3.8 — 2026-09-24 ｜ 官方 Tier 免费模型放行与 Auth 优先级调度闭环

- **支持官方 Tier 机制暴露免费模型**：
  - 判定放行逻辑改为按元数据 `Tier == "free"` 放行，不仅支持 `cline-free/`，同时支持类似 `stealth/` 前缀等实验/神秘免费模型。
  - `NormalizeModel` 与 `splitNamespace` 扩展支持 `stealth/` 命名空间透传，`stealth/space-bunny-alpha` 原生暴露并直通上游。
- **彻底闭环 Auth 优先级调度失效问题**：
  - 修正 `handleAuthParse` 凭证类型判定：只要具有 OAuth Token 均认定为 OAuth 凭证，不再受意外残留的 `api_key` 影响导致优先级被错误降级为 0。
  - `handleAuthRefresh` 轮询/刷新分支补齐 `Attributes["priority"]` 与 `meta["priority"]`，防止刷新后丢失优先级。
  - `syncConfigAPIKeyCredential` 增强物理文件同步逻辑：自动遍历清理 OAuth 文件残留的 `api_key`，并同步写入物理 `"priority": 1` 与 `"priority": 0`，确保宿主原生读取与内存 Attributes 双保险一致。
- **双端同步**：darwin（`v0.3.8.dylib`）与 linux（`v0.3.8.so`）均已部署生效。

## 0.3.7 — 2026-09-24 ｜ 全面对齐官方规范（Mid-Stream / 推理细节 / Usage 缓存 / 模型漂移）

- **标准 Mid-Stream Error 拦截与解析**：严格遵循官方规范，流式中途若上游抛出 `finish_reason: "error"`
  （如 `context_length_exceeded` / `content_filter` / `rate_limit`），不再向下游交付残缺破损文本，
  而是精准解析错误代码与信息并优雅报错。
- **补齐结构化深度推理透传 (`reasoning_details`)**：官方规范指明部分模型下发结构化思考详情；
  聚合器完整收集 `delta.reasoning_details` 并在非流式输出中原样保留。
- **标准化 Usage 与 Prompt Caching 透传**：官方 API 规范标准 `cost` 计费与 `prompt_tokens_details.cached_tokens`
  缓存命中指示，聚合器自动标准化补齐，方便客户端与面板分析成本与缓存率。
- **刷新模型元数据目录 (Model Drift)**：上游 `deepseek-v4.1-flash` 最大输出 Token 上限由 384,000 上调至 **393,216**；
  同步上游目录变更并吸纳新推荐免费模型 `space-bunny-alpha`。
- **双端同步**：darwin（`v0.3.7.dylib`）与 linux 部署端 容器原生编译（`v0.3.7.so`）均已部署。

## 0.3.6 — 2026-09-24 ｜ 支持凭证调度偏好（默认 OAuth 优先）

- **新增 `credential_preference` 配置项**：官方标准枚举字段（`enum`），在管理面板 `/plugins`
  中配置 Cline 插件时提供下拉选项：
  - `oauth_first`（默认）：**OAuth 优先**。正常状态下 100% 流量独占走 OAuth 凭证，只有在 OAuth
    出现 429 额度撞限或网络/鉴权故障时，宿主秒级平滑切到 API Key 备用。
  - `key_first`：**API Key 优先**。流量优先走 API Key，耗尽或异常时切 OAuth。
  - `round_robin`：**平级轮询**。两者平级按 round-robin 均匀分流。
- **底层动态 Priority 注入**：插件在 `auth.parse` 时根据配置偏好，为高优先级的凭证赋予 `priority: 1`，
  次优先级赋予 `priority: 0`，完美利用宿主底层的分桶调度器（`selector.go`），实现零人工干预调度。
- **双端同步**：darwin（`v0.3.6.dylib`）与 linux 部署端 容器内原生编译（`v0.3.6.so`）均已部署生效。

## 0.3.5 — 2026-09-24 ｜ 配置 key 自动生成凭证 + 邮箱解析自愈

- **配置 key 自动托管生成凭证**：若在配置中填入 `api_key`（或环境变量 `CLINE_API_KEY`），
  插件自动在 `auths/` 生成 `cline-key-<末4>.json`，清空 key 时自动移出对应凭证，免手动编写复制。
- **邮箱自愈解析修复**：
  - 修复 `auth.parse` 误将显示标签当作 `email` 导致回写到磁盘 auth 文件的问题；
  - 收紧 `enrichKeyOnlyIdentity` 邮箱判定逻辑（`isPlausibleEmail`），已中毒的假邮箱或标签会自动触发真邮箱解析自愈。
- **linux 容器路径兼容**：`clineAuthDir` 优先识别容器内 `/app/auths`，提升 linux 部署端 Docker 兼容性。

## 0.3.4 — 2026-09-23 ｜ 清掉源码注释里被推翻的旧结论

- `plugin/oauth.go:hostWakeAt` 的注释断言「宿主只按 **15 分钟 loop** 轮询」—— 这正是 2026-09-23
  实测**证伪**的那条（宿主是 per-auth 堆顶定时器，按 `Auth.NextRefreshAfter` 精确定时唤醒，
  `refreshCheckInterval` 仅 5s），且在文档里早已更正，只剩源码注释还挂着，与
  [503根因与刷新契约](docs/503根因与刷新契约.md) 正文直接打架。已按实测重写，并把
  「为什么默认 600s 而不是更大」的真实理由写进去：宿主对「问了还没刷出新票」有 **30s**
  退避重试（`refreshIneffectiveBackoff`），lead 越大只多出空转调用。
- **为什么为此升号**：注释挪行会改动 `pclntab`（panic 栈回溯的行号表，`-s -w` 不剥离），
  实测产物 sha256 由 `c420c0d9…` 变为 `93fdb3af…` —— 功能等价但**字节不同**。
  让两个不同二进制共用 `v0.3.3` 就是 `.bak-mislabeled` 那类「标签说谎」，故升号并双端重发。
- 双端同步：Mac dylib 重发并加载（`/v1/models` 仍 14 个）；linux 部署端 `.so` 重发（旧 `v0.3.3.so`
  留 `.bak`），插件仍 `enabled: false`。**无行为变更**。

## 0.3.3 — 2026-09-23 ｜ 再藏一个免费模型 + 报错文案精炼

- **拉黑 `cline-free/solar-pro4`** → 暴露降为 **14 个**（11 订阅 + 3 免费，`/v1/models` 实测无泄漏）。
- **报错文案精炼 11 处**（仅动偏长的，语义不变）：stall 三类、免费与订阅额度、401 重新授权、
  503「订阅凭证暂时不可用」、缺少凭证、登录失效（同一句散在 `errors.go`/`auth.go`/`quota.go` 三处）。
  - `上游静默超时：已连续约 60 秒无入站帧，可稍后重试。` → `上游静默超时：约 60 秒无入站帧，可稍后重试。`
- **修正过期语义**：`模型无效或不在 cline-pass/ 命名空间内。` → `模型无效或不在服务命名空间内。`
  （0.3.0 起还有 `cline-free/`，旧文案会把免费模型判成无效）。短文案全部原样保留。
- **元数据刷新抓到真实漂移**：`deepseek-v4.1-flash`（free / pass / cloud 三行）的
  `max_completion_tokens` 被上游由 `943718` 改为 **`384000`** —— 正是 `--check` 门禁要拦的那类改动。
- **双端同步**：Mac dylib 已部署并 `lsof` 实证加载；linux 容器内原生构建
  （旧 `v0.2.9.so` 留 `.bak`，覆盖前确认无进程映射），linux 部署端 仍 `enabled: false`。

## 0.3.2 — 2026-09-23 ｜ 暴露改黑名单 + key-only 身份解析

- **黑名单取代白名单**：服务命名空间内一切默认放出，新模型跑一次元数据刷新自动出现；要藏就在
  `plugin/models.go:excludedModels` 加一行（带命名空间 = 只藏该 id，裸 leaf = 藏所有同名）。本次拉黑千问三项。
- **key-only 也能拿到账号身份**：`GET /users/me` 对 `sk_…` 同样返回邮箱；key-only 的 `auth.refresh`
  尽力解析 → 缓存 12h + 写回 auth 文件（失败不影响请求，未解析成功则把下次刷新缩到 5 分钟）。
- **文件名通用化**：不再从标签拼名字 —— OAuth→`cline-<邮箱>.json`、纯 key→`cline-key-<末4>.json`、
  无身份→`cline.json`（绝不写 `nas`/`mac` 这类运行位置）。

## 0.3.1 — 2026-09-23 ｜ 修 0.3.0 的文件名缺陷

- **文件名与标题解耦**：0.3.0 拿标题拼文件名，新 key 会生成 `cline-API Key ····fa21.json`
  —— 带空格与 `···` 的**路径**。三处调用点统一改走 `credentialFileName()`。
- 顺带实测：纯 key 打 `/users/me/plan` 与 `/usage-limits` 均 200，与 OAuth 同 `planHistoryId`、同数值。

## 0.3.0 — 2026-09-23 ｜ 接入免费池 + 元数据补齐

- **命名空间感知路由**：`cline-free/*` 保免费池、`cline-pass/*` 保订阅池、无前缀默认 pass。
  0.2.x 会把一切改写成 `cline-pass/<leaf>`，让免费请求悄悄烧订阅额度。
- **订阅模型全量放出** + **模型元数据补齐**（窗口 / 模态 / 支持参数落进宿主 `ModelInfo`）。
- **一键元数据工具** `tools/modelmeta`：两份上游数据离线合并成 `plugin/modelmeta_gen.go`，
  `--check` 幂等且可作门禁；合并按 override → 精确 → 唯一前缀，多候选不猜。
- **额度错误分型**（对齐官方 CLI 的文案子串规则）+ **节流路由日志**（免费池生效的唯一直接证据）。
- 详见 [docs/模型与元数据.md](docs/模型与元数据.md)。

## 0.2.9 — 2026-09-23 ｜ 修 forced refresh 静默空转

- **P1**：`refreshOAuthIfNeeded(force=true)` 原来以「四字段不相等」判定"他人已轮换 → 采用磁盘副本"，
  于是任何无害差异都会让**强制刷新变成静默空转**（不发起刷新、bearer 不变、零日志）。
  改为 `authReplacedOnDisk()` 严格判定：仅「磁盘 bearer ≠ 本次被拒的 bearer **且** 仍可用」才跳过交换。
- **可观测**：跳过 / 未产出 / bearer 未变三种决策全部留痕，另加「无 refresh_token 且 bearer 已过期」节流告警。
- 新增 `plugin/force_refresh_test.go`（5 例，httptest 计数上游调用）。

## 0.2.8 — 2026-09-23 ｜ 修 503 根因（宿主刷新契约）

- **根因**：宿主只为内置 provider（codex/claude/antigravity/kimi/xai）注册 refresh lead，`cline`
  不在其中 → 宿主**从不提前刷新** → token 到期撞 401 → 一次 401 判死（`shouldRefresh()` 永久 false）
  → 之后每次请求在**选择器层 2–48ms 秒拒 503**，且没有 `Retry-After`；只有重新授权或重启宿主能解。
  实测凭证本身完好（拿 refresh token 直连上游 200）。
- **A. 刷新契约**：新增 `refresh_interval_seconds`（默认 600），写进 `Auth.Metadata` 并用于
  `Auth.NextRefreshAfter`（`auth.parse` 与 `auth.refresh` 两条路径都返回，宿主重启后仍生效）。
- **B. 401 语义拆分**：换到新 token 仍被拒 → 401 `cline_reauth_required`；换不到新 token →
  **503 `oauth_refresh_unavailable` + retryable**，宿主 backoff 重试而不判死。
- **C/D**：凭证从磁盘补齐（多候选拒绝猜）+ 失败包络加 `retryable` + 「host polled auth.refresh」
  节流日志（token 未临期就出现 = 契约生效铁证）。
- 完整机制与源码坐标：[docs/503根因与刷新契约.md](docs/503根因与刷新契约.md)。

## 0.2.7 — 2026-09-23 ｜ 过滤非法 reasoning_effort

- 上游对该字段大小写敏感且**未知值静默返回空内容**（11 次 A/B 实测）。现在去空格 + 小写后校验白名单，
  合法写回、认不出的**丢弃**并记日志（不做自动调档注入，不擅自改思考预算）。

## 0.2.6 — 2026-09-23 ｜ 控制面超时对齐官方

- `http_timeout_seconds` 默认 `60 → 30`（官方 `DEFAULT_HTTP_TIMEOUT_MS`）；流式不受此限制。
  至此刷新机制 4 处偏差（D1–D4）全部对齐。

## 0.2.5 — 2026-09-23 ｜ 刷新机制与官方逐条对齐

- **D1** 瞬时失败沿用窗口对齐官方 **30s grace**；**D2** "他人已旋转"改用官方**四字段比对**；
  **D3** 新增**抗复活**：刷新期间凭证被登出/替换则跳过回写，绝不复活他人已替换的凭证。
- **D4**（上游超时 60s）有意保留并在文档标注理由。

## 0.2.4 — 2026-09-23 ｜ 补全客户端身份头

- 官方固定发 **10 个身份头**，我们此前只发 4 个；现已补齐，新增 `profile`（`cli`/`desktop` 一键切换）
  与 `platform` / `platform_version` / `core_version` / `is_multi_root` / `task_id`。
- `X-Task-ID` 默认**不发**（无会话概念，不伪造）。

## 0.2.3 — 2026-09-23 ｜ 修会话卡死（含 0.2.2）

- **连接层真因**：改用带阶段超时的 HTTP 客户端（dial 10s / 响应头 30s / idle 20s 回收）。
  此前复用被 NAT 静默丢弃的 keep-alive 连接 → 永远等不到响应，表现为 60s 静默卡死；curl 每次新建连接故不复现。
- **无输出即换连接重放一次**（未吐字节时不影响计费、不重复输出）。
- **0.2.2**：刷新后**必须原子写盘**（否则宿主下次拿已作废的旧 token → `invalid_grant` → 误判需重授权）；
  新增插件自有日志 `logs/cline-for-cpa.log`（宿主不转发插件 stderr，此前凭证事件完全不可观测）。

## 0.2.1 — 2026-09-23 ｜ 非流式可用

- 上游对 `stream:false` 固定返回 `{"error":"empty response content"}`；插件改为**内部一律流式**，
  再把 SSE 增量聚合回 `chat.completion`（content / reasoning / tool_calls 跨分片合并 / usage）。
  新增 `force_stream_upstream`（默认 `true`）。

## 0.2.0 — 2026-09-23 ｜ 对齐官方 OAuth 机制

- 移植 `cline/cline`（Apache-2.0）：**跨进程 `flock` 刷新锁**（文件名与官方一致、60s 超时、崩溃自释）、
  **进程内单飞**、**锁内重读**、失败语义（`invalid_grant|invalid_token|unauthorized` → 唯一重授权路径；
  瞬时故障保留 token，**绝不清凭证**）、**401 自愈**（强制刷新 + 未吐字前重放一次）。
- **凭证优先级修正**：OAuth 有效期间不再被同文件 `api_key` 抢占；兜底用 key 时打显式告警。
- **请求头配置化**（消除 6 处硬编码）+ 401 文案分流（OAuth 失效才提重新授权，仅 key 场景才提 API Key）。
- 细节与差距分析：[docs/官方OAuth机制对齐.md](docs/官方OAuth机制对齐.md)。

## 0.1.3 — 2026-09-23

- OAuth 刷新失败时先回落到 auth 文件 / 插件 `api_key` 再报错；linux 部署端 `.so` 同号同步并冒烟通过。

## 0.1.2 — 2026-09-23

- 修 ABI 模型载荷：必须发 PascalCase `OwnedBy`/`ID`，否则宿主无 tag 结构体解析为空 → `auth_unavailable`。

## 0.1.1 — 2026-09-23

- 结构化 OpenAI 错误体（`message`/`type`/`code`/`retryable`）+ Stream Guard 三档错误码
  （流式经 `host.stream.close` 传 JSON）。方案见 [docs/错误码方案.md](docs/错误码方案.md)。

## 0.1.0 — 2026-09-22

- 首个可用版本：Stream Guard 三档超时（60/60/180，keepalive 不算进展）、OAuth 刷新 + `providers.json`
  bootstrap、WorkOS device 登录（ABI `auth.login.start` / `auth.login.poll`）、配额面板、双端部署冒烟。
- 明确不做插件内自动重试；配额状态按上游原样展示。
