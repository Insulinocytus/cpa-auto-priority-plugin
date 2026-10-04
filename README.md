# CPA Auto Priority

CLIProxyAPI 原生 Go 插件：宿主管理接口就绪后立即执行一轮 Codex、Antigravity、Devin、Meta、Kimi、xAI **自然额度重置** priority 同步，并让 Codex **有效重置卡**参与排序。对应 [issue #2](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/2)、[issue #3](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/3)、[issue #5](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/5)、[issue #6](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/6) 与 [issue #7](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/7)。不实现后续工单的 cron 或其他 provider 查询。

## 构建与配置

要求 Go 1.25+、CGO 和目标平台 C 编译器。沿用上游共享库函数表，不依赖整个 CLIProxyAPI Go module。

```sh
# Linux；macOS 改为 .dylib，Windows 改为 .dll
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/cpa-auto-priority.so ./cmd/plugin
```

库的文件名（插件 ID）必须为 `cpa-auto-priority`，扩展名按平台选择。配置合并进宿主 YAML：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-auto-priority:
      enabled: true
      priority: 0                 # 宿主插件加载顺序，不是认证文件的 priority
      management_url: http://127.0.0.1:8317
      management_key: YOUR_MANAGEMENT_KEY
```

`management_url` 是**该宿主**的 HTTP(S) origin，不能含用户名、路径、query 或 fragment。非 loopback 必须 HTTPS；管理密钥必须明确提供，loopback 也需要鉴权。不会跟随 HTTP redirect。保护配置文件权限；宿主 ConfigField 没有 secret 类型，插件不能保证管理 UI 隐藏此配置。不得将密钥提交到版本库。

插件注册声明 `management_api`，提供鉴权的只读状态接口：

```text
GET /v0/management/auto-priority/status
Authorization: Bearer <management key>
```

状态包含阶段、有限错误码和逐认证文件结果，不含额度响应、token 或管理密钥。示例：

```json
{"phase":"completed","round":{"status":"completed","results":[{"name":"account.json","provider":"codex","priority":0,"query_status":"ok","write_status":"acknowledged","persistence":"unverified"}]}}
```

没有手动触发、外部替代代理、定时调度或数据库。可复用的 Go 入口是 `priority.New(config, client, now)` 和 `Synchronizer.Sync(ctx)`；`client=nil` 使用带 30 秒超时的 HTTP client，`now=nil` 使用当前时刻。同实例单轮串行，取消后不继续写入。

## 单轮规则

1. 检查管理 `plugins` 中本实例的 `effective_enabled`；读取当前 `credentials` 的完整、非分页列表，必须有合法 `observed_at` 和 `files`。
2. 排除 runtime-only、非 file source、无 backing path 的条目。宿主列表**没有** `plugin_virtual` 标记，不能仅用这些字段认定物理认证：对每个候选的认证 **ID** 发无字段 `PATCH credentials/fields {"name":"<id>"}`。pinned 宿主在查 virtual 后检查字段：确切 `400/no fields to update` 确认可更新物理候选，确切 virtual `409` 排除。此探测不改认证业务字段、不持久化；其他响应 fail closed，整轮不进行 priority 写入。探测与实际更新都用 ID，避免共享文件名选错认证。
3. 额度查询均用 `POST /v8/management/requests/api-call` 代发，以 `auth_index` 选认证，不传 `proxy_url` 覆盖，保留宿主所选认证代理、全局代理、直连的原有语义。上游 401 为凭据明确失效。
   - Codex：`GET https://chatgpt.com/backend-api/wham/usage`，`Bearer $TOKEN$` 由宿主替换；从列表的 `id_token.chatgpt_account_id` 添加账号 header（存在时）。只比较通用 `rate_limit.primary_window/secondary_window`；用途专属的 `code_review_rate_limit` 和命名的 `additional_rate_limits` 不参与，不引入模型偏好。primary/secondary 是槽位，不代表固定周期。
   - Devin：Connect-RPC `POST https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus`，header `Content-Type: application/json`、`Connect-Protocol-Version: 1`，正文 `metadata.apiKey` 为 `$TOKEN$`（宿主在正文中同样替换为所选认证的 token），其余 metadata 沿用官方界面固定值。只读 `userStatus.planStatus.weeklyQuotaResetAtUnix`（周，604800 秒）与 `dailyQuotaResetAtUnix`（日，86400 秒），按 **Unix 秒**解析（数字或数字字符串，不套用毫秒判别）；与官方解析一致，缺失、null 与非正值（含 proto3 未设值 0）为该层未知，不影响另一层；非数字文本为 malformed。`planStart/planEnd` 是套餐期限，不代替额度时间；响应中的 apiKey、email 不解码。
   - Meta：额度接口 `POST https://api.meta.ai/muse-code/key` 要求 **DCA token**。宿主对 Meta 的 `$TOKEN$` 解析为 LLM 凭据，不能冒充 DCA，因此不使用 `$TOKEN$`：先以文件名 `GET /v8/management/credentials/download?name=<name>` 读取物理认证文件，只解码 `dca_token`（须为 `dca:` 前缀、无空白），以 `Authorization: Bearer <dca>`、`x-api-version: 1.0.0`、正文 `{}` 代发。缺少或不合格的 DCA 为 `missing_dca_token`，不发额度请求，不回退 LLM key。DCA 只存在于本次请求的局部变量，不进入结果、错误或任何持久化。响应只解码 `subs_usage`（可能同时返回 api_key 与 PII）：`weekly.resets_at` 为周层；`window.resets_at` 的周期由 `window_duration_mins` 确定，有重置时间却缺少有效周期时为 malformed；均按 Unix 秒解析，缺失、null 与非正值为该层未知，非数字文本为 malformed。有效 JSON 对象但没有 `subs_usage` 是成功观测、额度未知：`no_reset_time`，不重试，不视为剩余零，也不虚构重置。
4. Meta 的 DCA 下载与额度查询是两项独立必要请求，各自失败时至多重试一次；下载已成功而额度查询失败时，只重试额度查询。
5. Codex 按明确的正整数 `limit_window_seconds` 从长到短组织时间序列；604800 秒是周，18000 秒是五小时。沿用官方管理界面分类，28–31 日统一为月层；其他明确周期按其秒数排序，不猜成周。ISO/RFC3339、Unix 秒/毫秒（官方 helper 的 `1e11` 分界）、numeric string 和相对秒数归一化为绝对时刻。相对值锚定该响应接收时刻，每轮重新观测。
   Unix 数值保留原单位的整数/十进制精度，等价的 ISO 与非整秒 Unix 编码不会因浮点转换被拆成不同档位。
6. 相同周期、相同重置时刻合为一层；同周期不同重置时刻在上游没有查证的代表窗口契约，返回 `quota_period_ambiguous`，重试一次仍不明确则该认证为 -1。**不擅自取 min/max 或按槽位选代表窗口**。
7. Codex 成功取得自然额度后，用同一 `auth_index`、账号 header 和代理语义代发 `GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits`，额外沿用管理界面的 `Accept: application/json`、`OpenAI-Beta: codex-1`、`Originator: Codex Desktop`。只读取卡详情，不使用 `/consume`、购买、领取或主动重置 mutation；账户 `credits.balance` 与重置卡无关。
8. 当前已查证的重置卡类型是 `reset_type=codex_rate_limits`、`status=available` 的一次性权益：要求合法 `granted_at` 且不晚于本次卡响应观测时间；有到期时间时必须晚于观测时间和授予时间。`redeeming`、`redeemed`、其他非 available 状态、未来授予及已到期卡均不参与；`expires_at` 缺失/null 按第一方可选字段契约表示不过期，没有可提前排序的到期时刻。多张有效卡取最早到期，与该卡适用的自然时间取 min；相等不加档位，卡不创建缺失窗口。
   当前已查证的重置卡类型（`codex_rate_limits`）仅作用于通用周（604800 秒）和五小时（18000 秒）层；**未查证月卡、其他周期或模型专属卡契约，不声称这些接口已支持或不存在**。月层保留自然时间，周卡不能覆盖月时间。`applicable_available_count` 不是持有卡数量，其计算公式未查证；不以它为零或账号未触及 limit 排除有效卡，也不按使用率、剩余额度量或耗尽状态排序。
9. 同 provider 内比较各自最长周期的排序时间；越早越优先，相同才比较下一层，有下一层优先于没有下一层。时间序列完全相同共用档位。正常档位从最低 0 连续向上编号，不按套餐、文件名、输入顺序或额度量拆组。
10. 必要请求（额度、卡、Meta DCA 下载）失败/malformed 仅重试失败请求一次；成功额度不因卡失败重复请求，成功 DCA 下载不因额度失败重做。仍失败、401 明确失效、缺少 DCA、无任何可用自然重置、缺查询索引、未知 provider 均只将该物理认证设为 -1，其余继续。卡详情必须有非负整数 `available_count` 和实际 `credits` 数组；成功零卡与失败严格区分。后端可截断详情：available 行数不足汇总时为 `reset_card_details_incomplete`；数量矛盾或必要字段损坏为 `reset_card_response_malformed`；available 卡类型未知为 `reset_card_applicability_unknown`，都不假造“无卡”。成功确认缺少/null 的短窗口不是失败；已有窗口的损坏周期/时间是失败。有效窗口的时间明确为 null/缺失时不虚构时间。不会拿订阅或 token 过期代替重置时间。宿主的 `disabled` 状态不影响查询与排序，插件也从不改变它。
11. 写前再次确认启用状态，仅 `PATCH /v8/management/credentials/fields {"name":"<id>","priority":N}`。不提交完整 auth JSON，不改变 disabled、token、DCA token、代理或其他业务字段。写入失败返回 `write_status=failed`，不重试、不回滚，继续尝试其他认证；HTTP 2xx 且 `status=ok` 仅为 `acknowledged`，**从不声称磁盘持久化已验证**。

### Antigravity

项目取自宿主列表条目的 `project_id`（宿主 `authProjectID` 读取 auth metadata 的 `project_id`，其次 attributes；Antigravity executor 本身也只用 metadata `project_id`）。缺项目时为 `missing_project_id`，不查询、不重试，该认证为 -1；不下载、不记录、不回写完整 auth JSON，也不调用 `loadCodeAssist` 补项目。经同一 `requests/api-call` 代发 `POST .../v1internal:retrieveUserQuotaSummary`，`data` 为 `{"project":"<project_id>"}`，headers 与官方界面相同（`Bearer $TOKEN$`、JSON、`antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)`），不覆盖代理。宿主对 Antigravity 的 `$TOKEN$` 先按所选认证代理刷新 OAuth token。

官方界面一次读取依次尝试 daily、daily sandbox、production 三个地址；本插件第 1 次请求 daily，失败后唯一一次重试请求 daily sandbox，总计最多两次，**不尝试 production、不叠加 fallback**。宿主生成请求同样默认 daily、不跨档 fallback。

只认 `groups[].buckets[]` 的 `window` 为 `5h/five-hour/five_hour`（五小时）或 `weekly/week`（周，大小写不敏感），时间按官方 `resetTime ?? reset_time` 取值（仅 `resetTime` 缺失/null 时取 `reset_time`；取到空串视为无时间），按第 5 点的 ISO/Unix 规则归一化。额度组是模型族（如 Gemini、Claude and GPT），不是额外周期：同周期各组重置时刻相同则合为一层；不同，或某组该周期有时间而另一组同周期无时间，均为 `quota_period_ambiguous`，**不按模型挑选、不取 min/max、不忽略缺时间的模型族**。有重置时间但 `window` 缺失或未经查证（如 `daily`）返回 `quota_period_unverified`，不猜周期。两者重试一次仍如此则该认证为 -1。`groups` 缺失/null/非数组为 malformed；所有组都没有 bucket 为 `no_quota_groups`，与官方界面一样换下一个地址重试；有 bucket 但都无重置时间为 `no_reset_time`，不重试。不读取 `remainingFraction`、套餐级别或订阅信息，响应 `Date` 推算的服务器时钟偏移只用于官方界面倒计时，不当作重置时间。未查证到 Antigravity 手动重置卡契约，不生成卡时间。

### Kimi 与 xAI

两者的域名/身份字段不在 `credentials` 列表中。插件先以列表 `name` 调 `GET /v8/management/credentials/download?name=<文件名>`，只临时解析下表字段；不缓存、不输出原文，也不用其中 token 发请求。下载是宿主 `AuthDir` 磁盘按文件名读取，不是按 ID 的内存快照；列表中文件名重复返回 `auth_metadata_ambiguous`，下载失败重试一次后为 `auth_metadata_unavailable`，均只使该认证为 -1。额度请求仍以列表 `auth_index` 代发，`$TOKEN$` 与代理由宿主按所选认证解析，不传 `proxy_url`、不带其他账号 header。

| Provider | 支持条件 | 查询 | 归一化 |
| --- | --- | --- | --- |
| `kimi`、`kimi-ai`、`kimi.ai`、`kimi.com` | 下载 `type` 缺失或为上述 Kimi 类型；`base_url` 不是 Moonshot 开放平台 | 按官方管理界面顺序：`domain` → `base_url`/`base-url` host → `type` → provider，映射到固定 `GET https://api.kimi.{com,ai}/coding/v1/usages`，仅 `Authorization: Bearer $TOKEN$` | `limits[]`（`detail` 或自身）用 `window/item/detail` 的 `duration`+`timeUnit`（秒/分/时/天/周；缺单位按分钟，未知单位 malformed），无 duration 时仅接受明确 monthly/weekly/daily 名称；顶层 `usage` 是旧官方 CLI 定义的周池；`usages.limit_month_total.reset_time` 是月层。重置取 `reset_at/resetAt/reset_time/resetTime`，否则 `reset_in/resetIn/ttl/window` 相对秒锚定本次观测。28–31 日归为月层。 |
| `xai` | 下载 `type=xai`、`auth_kind=oauth` 且无 `api_key`（Grok CLI OAuth） | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`，带官方 Grok CLI headers；`x-userid` 仅取自 `sub/subject/user_id/userId`、`oauth.sub/subject`、`user.sub/id` | 只接受 `config.currentPeriod`（或 `current_period`）的 `type` 含 `weekly` 且自身 `end`，作为单一周层。 |

排序组沿用宿主 `executorKeyFromAuth`：`kimi.com` 与 `kimi` 同组，`kimi.ai` 与 `kimi-ai` 同组；结果中的 `provider` 仍是宿主列表原值。没有重置时间的 Kimi limit 行不增加层，也不要求周期元数据。

Kimi 只有月额度时按月层正常排序，不设 -1；成功确认缺周/短周期是缺层。xAI 的月周期、`billingPeriodEnd`、余额、月金额、订阅/token 到期、使用率和产品分项都**不是**额度重置，返回 `no_reset_time`。xAI API key 没有已查证的自然重置接口，返回 `unsupported_quota_auth`；不调用 `/v1/me`、`chat/completions` 等付费健康探测，也不调用 management billing。Moonshot 开放平台只有余额接口，不用于排序。

**手动重置卡：** Kimi 与 xAI 均未找到含适用额度、剩余次数、生效/到期时间的查询契约，本插件不生成卡字段。Kimi Extra Usage 与 xAI Extra Usage Credits 是付费余额，不是重置卡。宿主 pinned `api-call` 不主动刷新 Kimi OAuth token，上游 401 按 `credentials_invalid` 处理。

管理 HTTP 层的 401/403 与 provider 上游 401 不同：任一管理请求返回 401/403 时，本轮立即终止，返回 `priority.ErrManagementAuthentication`（`management_authentication_failed`），启动状态为 `failed`；不重试、不继续其他查询或 priority 写入。即使错误正文为空或不是 JSON，也按 HTTP 状态处理，不输出正文。普通查询/写入失败仍按上述规则隔离。宿主会在同一来源 IP 鉴权失败 5 次后封禁 30 分钟（包括 loopback），因此不能将错误管理密钥当作未就绪无限轮询。修正管理配置后 reconfigure 启动新一轮；已有宿主 IP 封禁不会被插件清除。

后台只轮询宿主就绪（每秒），不是无限重试 provider 或 priority 写入。监听或响应结构未就绪时状态为 `waiting`；管理 401/403 则终止为 `failed`，不把 init 的空集合报为成功。就绪后真的空集合为 `empty`，不写入且不叫成功同步。相同配置的 reconfigure 幂等；配置改变先验证，再取消并等待旧任务，启动新一轮。quiesce/原生 shutdown 取消并等待后台退出。

## 固定兼容契约与证据

宿主基准：[CLIProxyAPI `8ef43e4`](https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473)。原生 ABI **1**、RPC schema **6**；配置是 JSON `config_yaml` 的 base64 编码 YAML；metadata 用 PascalCase，capabilities 用 snake_case。

- [ABI/schema](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/pluginabi/types.go#L5-L35)、[官方 Go wrapper](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/examples/plugin/host-callback-auth-files/go/main.go#L1-L221)。
- [Build 先加载插件](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/builder.go#L235-L275)；普通非 Home 模式的 [Run 初次 auth 加载 → NewServer → reconfigure → listener](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/service_lifecycle.go#L70-L225)。[INFERENCE] 鉴权成功的管理响应证明监听已开放，初次 auth 加载已被尝试；不证明加载成功、watcher/model 注册完成或 Home 订阅就绪。本插件不保证 Home 模式。
- [v8 管理路由](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/server_management_v8.go#L30-L51)、[列表与 Codex claims](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files.go#L651-L746)。
- [非变更探测、virtual guard 与窄更新](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files_fields.go#L257-L415)、[manager 列表/GetByID 返回 clone](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/conductor_selection.go#L1520-L1543)。探测依赖此版本校验顺序和有限错误文本；升级宿主需重新核对，不能把未知 400/409 当成功。
- [代发请求与代理选择](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/api_tools.go#L32-L238)、[持久化失败可能只记录日志](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/conductor_lifecycle.go#L273-L307)。
- [管理鉴权与来源 IP 封禁规则](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/handler.go#L301-L392)：401/403 是管理访问失败，不是可无限重试的启动就绪信号。

Codex 基准：[官方管理界面 `ee79a79`](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/ee79a794526a30c03748a8864a9ac6589a31833b)。这是第一手实现证据，不是 OpenAI 正式发布的 wham schema：

- [周期、scope、请求 headers](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts#L68-L445)、[ISO/Unix/相对时间 helper](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resetInstants.ts#L23-L77)。
- [上游脱敏格式 fixture](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/tests/codexQuota.test.ts#L28-L63) 含通用周窗口及独立 Spark 周窗口。UT 保留其自然额度形状，其他月/ISO/边界数据注明为合成输入；不声称采集过真实账号响应。
- 重置卡第一方基准：[OpenAI Codex `afb436d`](https://github.com/openai/codex/tree/afb436df8b70bb5bc57b86d9a3e829968988cd21)。[GET 与路径](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/client/rate_limit_resets.rs)、[原始详情字段](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/types.rs)、[`codex_rate_limits` 周+五小时 fixture](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/client/rate_limit_resets_tests.rs)、[详情可能截断](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitResetCreditsSummary.ts)、[官方 banked reset 语义](https://help.openai.com/en/articles/20001498-how-banked-codex-resets-work)。这证明当前已查证的重置卡类型路径，不是对所有未来 offer 的适用性承诺；新类型需要重新查证。
- `applicable_available_count=0` 但仍持有卡的[管理界面 fixture](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/tests/codexQuota.test.ts#L28-L116) 与 [GET headers](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts#L366-L410)。更完整的证据和未知项见 [接口调查](docs/research/cliproxyapi-contracts.md#codex-重置权益第一方语义与接口边界)。

Devin/Meta 基准同为官方管理界面 `ee79a79` 与宿主 `8ef43e4`，详见 [接口调查](docs/research/cliproxyapi-contracts.md#devin-与-meta-额度契约)。UT 的 Devin/Meta 响应是按其解析代码构造的合成形状，不是真实账号响应。

**Devin/Meta 支持边界：** 只比较上述自然重置字段，不比较剩余百分比、套餐等级或 provider 名称；两者均未建立重置卡契约。Meta DCA 读取依赖宿主下载接口按文件名读取 auth 目录中的同名文件，认证文件不在该目录时下载失败、该认证为 -1。下载响应含该认证文件全部密钥，插件只在内存中解码 `dca_token`，不记录、不保存。

Antigravity 基准同为官方管理界面 `ee79a79` 与宿主 `8ef43e4`，不是 Google 公开发布的 schema；窗口与项目证据见 [接口调查](docs/research/cliproxyapi-contracts.md#antigravity-额度组与项目)。上游没有 Antigravity fixture，UT 响应全部是按其类型定义构造的合成输入。

**限制：** 没有跨文件事务，也没有跨探测/查询/写入的快照事务。宿主 disable 不通知仍加载的库，本插件依赖 `effective_enabled` 检查；检查后立刻 disable 或 auth 集合变化仍有 TOCTOU，窄 PATCH 的 virtual guard 继续保护虚拟认证。不同 provider 的编号独立计算，宿主混合 provider 路由仍可能跨 provider 比较最终数字；插件不改变路由规则。停止插件不会恢复此前已写入的 priority。Antigravity 认证若在 auth 中设置自定义 `base_url`（企业/GCP），宿主生成请求会使用该地址，但官方界面与本插件的额度查询仍用固定官方地址；若账号只能在 production 地址返回额度，两次尝试后为 -1。不同额度组的周重置不同时整份认证不可排序，这是未查证归并契约时的保守结果。

## 测试

```sh
go test -run Sync .                          # 完整单轮 seam 的定向 UT
go test ./cmd/plugin -run 'TestStartup|TestShutdown|TestInvalid'
go vet ./...                                # 类型/静态检查
go test -race ./...                         # 最终完整 UT，含并发检查
```

UT 使用固定时钟和有状态 HTTP transport adapter：不监听端口、不访问真实网络/账号、不加载真实宿主。覆盖多周期 precedence、跨套餐/缺层/同档、输入顺序、provider 隔离、ISO/秒/毫秒/相对时间及跨年、重试隔离、virtual/配置项排除、仅 priority 变化、并发 token 刷新保留、写入失败、显式鉴权/代理语义、敏感数据不泄露、配置注册/reconfigure/取消退出；管理读取、物理认证探测、额度/卡代发及 priority 写入的 401/403 立即终止，修正密钥后可重新配置恢复。重置卡 UT 覆盖早/等/晚、多卡、未生效/过期/已消费/消费中/不失效、未知适用性、缺字段和截断详情、月层与五小时 precedence、尚未触及 limit、下一轮卡消费后重新排序；从完整 `Sync` 入口观察最终 priority 与存储业务状态。Kimi 覆盖月-only、月+短周期、跨套餐、相对时间重新观测、域名与 Moonshot/外部凭据拒绝；xAI 覆盖真实周额度与月账单/余额/API key 区分；并与 Codex 混合执行受控更新及失败隔离。Kimi/xAI 响应均为按上游源码字段构造的合成数据，不是账号采样。

Devin 覆盖周先于日、Unix 秒字符串/数字、套餐期限不参与、proto3 未设值与 malformed 重试；Meta 覆盖 DCA 与 LLM key 区分、含 api_key/PII 响应的安全提取、成功未知额度与 malformed/失败/歧义周期、下载与查询各自重试隔离，以及已禁用认证仍只改 priority。混合同步 UT 同轮覆盖 Codex、Antigravity、Devin、Meta、Kimi、xAI。

Antigravity UT 另覆盖项目定位与请求契约、多组同周期合并与冲突、未查证窗口、snake_case/时区/Unix 时间、缺项目、两次尝试的地址顺序与失败隔离、与 Codex 混合时独立档位。

issue #2 实现阶段另外完成 Windows `c-shared` 编译（未加载）和无网络、受控 transport 的单轮入口运行检查；临时程序与构建产物已移除。这些历史检查不证明真实宿主或磁盘兼容性。issue #3 重置卡改动只做确定性 UT，不安排 smoke、真实加载或真实账号请求。

PR #12 合并验证另用临时程序经过 loopback HTTP 服务运行混合 Codex/Kimi/xAI `Sync`：确认 Codex 卡到期参与排序、卡查询失败仅重试一次且不重查成功额度、失败仅影响该认证、其他 provider 正常写入并保留 token；临时程序已移除。服务与响应均为合成数据，不证明真实宿主或账号兼容性。

PR #14 冲突解决验证用临时程序经过 loopback HTTP 服务运行五个 provider 的 `Sync`：确认 Codex 卡到期排序及失败隔离、Meta DCA 查询失败不重做成功下载、其他 provider 正常更新、仅 priority 改变且结果不泄露敏感字段。临时程序已移除；服务与响应均为合成数据，不证明真实宿主或账号兼容性。

PR #13 冲突合并验证另用无网络临时程序运行混合 Antigravity/Codex/Kimi/xAI `Sync`：确认 Antigravity 仅按 daily → sandbox 尝试两次、失败认证为 -1，Codex 卡查询失败仅重试卡且不重查成功额度，各 provider 独立档位，只改 priority 并保留并发刷新的 token。临时程序已移除；合成 transport 不证明真实插件或账号兼容性。

PR #14 再次合并 Antigravity 后用无网络临时程序运行六个 provider 的 `Sync`：确认 daily → sandbox 恢复、Codex 卡失败不重查成功额度、Meta 查询重试不重做 DCA 下载、仅 priority 改变且结果不泄露敏感字段。临时程序已移除；合成 transport 不证明真实插件或账号兼容性。

**未验证：** 真实共享库加载、部署平台运行时 ABI、实际管理鉴权、真实账号接口、宿主内存与磁盘持久化兼容性。UT 和源码核对不能证明这些运行时性质；不安排真实宿主/账号 smoke。
