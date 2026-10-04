# CLIProxyAPI 接口调查

调查基准：上游提交 [`8ef43e4df3b216a42493105d31c2873b69191473`](https://github.com/router-for-me/CLIProxyAPI/commit/8ef43e4df3b216a42493105d31c2873b69191473)，提交时间 2026-10-04T05:31:41Z。本记录是源码调查，不是运行时验证；尚未确认用户部署版本。

## Priority 与 provider 分组

- 数字越大的 priority 越优先；未设置或无法解析的值按 0 处理。相同 priority 保留在同一候选层，层内继续使用宿主的选择策略。[解析与分层](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/selector.go#L364-L381)、[层内选择](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/selector.go#L549-L647)。
- 不可用状态在优先度选择前过滤；提高 priority 不能绕过禁用或额度冷却。[可用性检查](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/selector.go#L823-L912)。
- 混合 provider 路由会跨 provider 比较 priority，并选择全体候选中的最大值。插件按 provider 独立计算，不等于宿主路由也隔离这些数值。这是尚待用户明确的边界。[跨 provider 比较](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/scheduler.go#L493-L549)。
- 现有会话亲和性与 Codex WebSocket 偏好可能保留其他选择行为，priority 不是对所有请求的强制使用顺序。[WebSocket 偏好](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/scheduler.go#L1335-L1359)。

## 窄范围 priority 更新

现有接口：`PATCH /v8/management/credentials/fields`，兼容路径为 `/v0/management/auth-files/fields`。正文是平铺字段：

```json
{"name":"credential.json","priority":7}
```

`name` 按认证 ID 或文件名定位，不能替换成 `auth_index`。该接口同步运行时 priority 属性并更新管理器，而不是替换完整 auth JSON。[更新处理](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files_fields.go#L257-L415)、[priority 同步](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files_fields.go#L688-L709)。

管理接口需要管理密钥，loopback 不自动免认证。[管理鉴权](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/handler.go#L263-L393)。`Manager.Update` 对常规持久化错误记录日志而非返回失败，因此 HTTP 200 本身不能证明磁盘写入成功，也没有已确认的组级事务保证。[更新与持久化](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/conductor_lifecycle.go#L273-L301)。

`host.auth.save` 是完整认证文件保存接口，不是 priority patch。完整读改写存在覆盖并发 token 刷新字段的风险；其文件重建路径也未直接同步 priority 路由属性，不能作为已证明的即时 priority 更新接口。[保存与重建](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/auth_callbacks.go#L277-L406)。

## 原生插件后台任务

原生共享库 init 接收实例级宿主回调表。后台调用可使用实例上下文，不需要保留某次 RPC 的 `host_callback_id`；后者是请求作用域，不能用于 cron。[原生宿主回调](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/host_callbacks_unix.go#L23-L57)、[可省略的 RPC ID](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/http_operation_bridge.go#L295-L437)。

上游的 `scheduler.pick` 指认证选择，不是定时任务调度；未发现宿主 cron 注册接口。插件需自行管理定时任务和 shutdown；宿主关闭回调实例后会卸载库，因此插件 shutdown 返回前必须停止并等待自己的后台任务退出。[ABI 方法](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/pluginabi/types.go#L37-L122)、[卸载流程](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/loader_unix.go#L217-L250)。

`host.http.do` 不自动继承某个 auth 的独立 proxy；后台查询额度时不能假定每账号代理已得到应用。[HTTP 调度](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/host_callbacks.go#L204-L219)、[代理选用](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/http_bridge.go#L217-L234)。

## 周额度与重置卡数据来源

额度来源依据官方管理界面提交 [`ee79a794526a30c03748a8864a9ac6589a31833b`](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/ee79a794526a30c03748a8864a9ac6589a31833b)。以下是该界面实现的上游接口契约，不代表每个用户账号都会返回相应字段；尚未使用真实账号调用这些接口。

官方额度 adapter 覆盖 Antigravity、Claude、Codex、Devin、Kimi、Meta、xAI，不能据此承诺所有 CPA provider 都提供周额度数据。[已注册 adapters](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/index.ts)。

### 各 provider 的周重置时间

| Provider | 官方界面查询方式 | 周重置字段与边界 |
| --- | --- | --- |
| Claude | `GET https://api.anthropic.com/api/oauth/usage` | 通用周额度是 `seven_day.resets_at`；还存在多个模型或用途专属周窗口，不应未经确认就取所有窗口的最小值。ISO 时间。[数据层](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/claude/data.ts) |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage` | `rate_limit` 的窗口包含 `limit_window_seconds`、`reset_at`、`reset_after_seconds`；604800 秒才明确是周窗口，不能把所有 `secondary_window` 都视为周额度。存在月窗口、code-review 和 additional windows。[数据层](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts) |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`，正文含 `project`；多个官方 URL 见 constants | `groups[].buckets[]` 中 `window=weekly/week` 与 `resetTime/reset_time`。可能多个组有不同周重置时刻，没有已确认的唯一通用周字段。[端点](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/constants.ts)、[bucket 解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/builders.ts#L67-L113) |
| Kimi | `GET https://api.kimi.com/coding/v1/usages` 或 `api.kimi.ai` 对应地址，仅 `Authorization: Bearer $TOKEN$`；域名来自 `credentials/download` 的 `domain` → `base_url` → `type` → provider，只映射到这两个固定 URL | `usage`（供应商旧 CLI 定义为周池）、`limits[]` 的 `detail` 或自身，周期取 `window/item/detail` 的 `duration`+`timeUnit`；`usages.limit_month_total.reset_time` 为月总额度。重置为 `reset_at/resetAt/reset_time/resetTime`，或相对秒 `reset_in/resetIn/ttl/window`。官方会员文档：新套餐无周额度、旧套餐有周额度，均有月总额度及五小时窗口。[端点](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/constants.ts#L147-L153)、[域名选择](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/kimiQuota.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/builders.ts#L252-L389)、[供应商旧 CLI](https://github.com/MoonshotAI/kimi-cli/blob/9ab1286b8fe4e6bcd116949a27ce5e0ac3389c82/src/kimi_cli/ui/shell/usage.py#L81-L202)、[会员说明](https://www.kimi.com/code/docs/en/kimi-code/membership.html) |
| xAI | Grok CLI OAuth：`GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`，官方 Grok CLI headers，`x-userid` 仅来自 auth 的 sub/user id 字段 | 只有 `config.currentPeriod`/`current_period` 的明确 weekly `type` 与同一对象的 `end` 是周额度重置。月周期、`billingPeriodEnd`、余额、金额、订阅和 token 到期不是额度重置；官方界面的 billing-period 回退与 `chat/completions` 付费健康探测不能复用。API key 没有已查证的自然重置接口。[请求与身份](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/xai/data.ts#L42-L247)、[周期隔离](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/builders.ts#L394-L594)、[消费者周池 FAQ](https://docs.x.ai/grok/faq)、[API key 管理](https://docs.x.ai/developers/rest-api-reference/management/auth) |
| Devin | `POST https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus` | `userStatus.planStatus.weeklyQuotaResetAtUnix`，Unix 秒。[请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/devin/requests.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/devinQuota.ts#L23-L51) |
| Meta | `POST https://api.meta.ai/muse-code/key`，需 auth 文件中的 DCA token，不是普通 LLM key | `subs_usage.weekly.resets_at`，官方界面按 Unix 秒显示。响应可能包含密钥和 PII，只能提取所需字段，不记录完整响应。[请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/meta/requests.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/metaQuota.ts)、[单位换算](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/meta/MetaQuotaBody.tsx#L45) |

### 手动重置卡

- Codex：`GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits`，查询原始 `credits[]` 与 `available_count`；`/wham/usage` 也可返回 `rate_limit_reset_credits` 汇总。界面兼容 `applicable_available_count`，但它不是卡的剩余次数或周期字段。详见下面的第一方契约与限制。
- Claude：`GET https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1`，读取 `cedar_ember`。`grants[].ends_at` 为到期时间，`resets_left` 为剩余次数，`starts_at`、`paused`、`clears`、`usable_now`、`use_requires_limit` 等描述适用性；顶层有 `eligible`、`at_limit`、`weekly_resets_at`、`cooldown_until`。[原始字段与状态查询](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/claudeResetGrants.ts#L143-L266)。
- **用户确认的排序条件（不是上游 API 保证）**：排序考虑仍有剩余次数、已生效、未过期且适用于所比较额度的权益，不要求账号此刻已经触及 limit；多张取最早到期的有效卡。插件不自动消费卡。该规则包括月额度；但不能将未经证明适用月额度的卡用于月额度。
- 已检查的其余额度 adapters 未建立手动卡到期数据契约，不能声称这些 provider 存在或不存在卡。未知 provider 不能凭空生成周重置或重置卡时间。Kimi Extra Usage、xAI Extra Usage Credits 是付费余额，不是重置卡。
- `GET /v8/management/credentials/download?name=<文件名>` 从宿主 `AuthDir` 读取磁盘原文，不按认证 ID，也不是 manager 内存快照，可能含 secrets；只能临时提取域名/身份字段。[下载实现](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files_crud.go#L25-L48)。pinned `api-call` 对 xAI 有专用 token 解析/刷新，对 Kimi 不主动刷新 token。[token 解析](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/api_tools.go#L255-L482)。

#### Codex 重置权益：第一方语义与接口边界

调查于 2026-10-04；OpenAI 源码基准 [`afb436df8b70bb5bc57b86d9a3e829968988cd21`](https://github.com/openai/codex/tree/afb436df8b70bb5bc57b86d9a3e829968988cd21)，管理界面仍使用本节的 `ee79a794` 基准。只读取公开源码、测试与文档，没有账号调用或消费/领取操作。

- **OpenAI 发布语义**：[How banked Codex resets work](https://help.openai.com/en/articles/20001498-how-banked-codex-resets-work) 将 banked reset 定义为保存到账户、直到使用或过期的一次性权益；原文 “Using a full banked reset refreshes your 5-hour and weekly Codex usage windows”。消费只在成功刷新至少一个适用窗口时发生；无窗口需要刷新则保留。文档允许在 Usage 页面使用，也可能在达到上限后显示选项，未要求先耗尽额度。**full banked reset 的五小时与周范围已证明，月范围未证明**；同文明确 affected usage limits 随 offer、plan、workspace、region 变化，不能推广成所有卡都覆盖所有窗口。[Work/Codex 说明](https://help.openai.com/en/articles/20001516-managing-usage-with-gpt-6-astra-in-work-and-codex) 也确认 full banked reset 刷新五小时与周窗口。
- **不要混用产品**：[付费 instant reset](https://help.openai.com/en/articles/20001507-paid-weekly-work-and-codex-rate-limit-resets) 可在到达上限前购买，立即刷新五小时与周额度，但明确不能保存为 banked reset。其购买条件不能当作 banked 卡的 API 条件。自动/global reset 不产生卡。[referral 条款](https://help.openai.com/en/articles/20001271-chatgpt-desktop-referral-promotions) 说明奖励只有完成条件后才发放、作用范围依 offer；banked referral reset 默认加入 bank 后 30 天过期，但 offer 可覆盖，不能硬编码 30 天或以它推算额度周期。
- **实际 GET 与账号选择**：OpenAI [client](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/client/rate_limit_resets.rs) 使用 `GET /wham/rate-limit-reset-credits`（base 为 `https://chatgpt.com/backend-api`），同一 client 的 [公共 header](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/client.rs) 加认证、User-Agent 与配置时的 `ChatGPT-Account-Id`，不加账号 query/body。管理界面 [constants](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/constants.ts) 用 `Authorization: Bearer $TOKEN$`、`Content-Type: application/json` 与 codex-tui User-Agent；[data.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts) 对详情 GET 再加 `Accept: application/json`、`OpenAI-Beta: codex-1`、`Originator: Codex Desktop`。后两项是该界面请求选择，不是已证明的 OpenAI 必需 header。`Chatgpt-Account-Id` 从 [resolver](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resolvers.ts) 的 file/metadata/attributes `chatgpt_account_id` 或 camelCase，随后 `id_token` 解析；没有 ID 时界面省略 header，不证明省略仍可正确选择多 workspace 账号。subscription 的 `?account_id=...` 不属于重置卡 URL。
- **原始字段与剩余权益**：OpenAI [backend types](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/types.rs) 要求详情的 `credits` 数组、`available_count` 整数；每行 `id`、`reset_type`、`status`、`granted_at` 是字符串，`expires_at`、`title`、`description` 可缺失/null。没有每卡 `remaining` 或 `resets_left` 字段；banked 卡是一次性，按 available 状态表征未消费权益。OpenAI [状态类型](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitResetCreditStatus.ts) 是 available/redeeming/redeemed/unknown，不能将非 available 状态算作剩余卡。管理界面 [normalizer](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resetCredits.ts) 只保留 `reset_type=codex_rate_limits`、`status=available`、非空 `expires_at`；`id`/`granted_at` 缺失补空字符串，兼容 camelCase。这是界面宽松解析，不是完整的有效卡验证。
- **grant 与 expiry**：OpenAI [app-server credit contract](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitResetCredit.ts) 明确 `grantedAt` 是发放时刻、`expiresAt` 是过期时刻/null 表示不失效，app-server 单位为 Unix 秒；原始 backend 的对应字段是字符串，不能把这两层单位混同。没有独立 starts_at/激活日期，未证明 granted_at 是另一个生效窗口的起点。界面用 ISO 解析 expiry 做显示，不比较 granted_at 或当前时间来过滤未来发放/已过期行；仅通过该 normalizer 不足以证明已生效且未过期。
- **汇总不等于完整详情**：OpenAI [summary contract](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitResetCreditsSummary.ts) 明确 `credits=null` 表示只知道数量，空数组表示已查询但无 available 行，backend 可截断详情使行数小于 `availableCount`。因此不能用数组长度证明总数或最早到期卡完整性，也不能把数量存在当作存在可比较的到期时间。
- **applicable_available_count 的证明边界**：管理界面 [tests/codexQuota.test.ts](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/tests/codexQuota.test.ts) 在 `allowed=true`、`limit_reached=false`、周使用率 1% 的 fixture 中同时有 `available_count=1`、`applicable_available_count=0`，仍期待 reset support=true；基准 `canResetQuota` 只看 availableCount>0。引入 applicable 的 [51b034d](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/commit/51b034dd914719c3bd6b5ab0eb64bc8b103ca0d4) 曾用它控制按钮，现在已不同。**可证明 available 与 applicable 不等价、applicable=0 不能直接等于没有未消费卡；不能证明该字段等价于 at_limit，也不能用聚合数量映射某张卡的周/月适用性。**检查的 OpenAI backend types 未建模 applicable 字段，发布文档也未给出计算公式。
- **已知类型的范围证据与限制**：OpenAI [client contract test](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/backend-client/src/client/rate_limit_resets_tests.rs) 明确把 `reset_type: "codex_rate_limits"`、`status: "available"` 与 `title: "Full reset (Weekly + 5 hr)"` 放在同一详情行；另一同类型行允许 `expires_at: null` 且不带 title。结合上述发布的 full banked 语义，**当前已知 full Codex 卡的周+五小时路径有第一方证据，可据此实现该范围；没有月额度证据**。这是一条客户端 fixture，不是 backend schema 对所有未来 offer 的无条件保证。[reset type](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/app-server-protocol/schema/typescript/v2/RateLimitResetType.ts) 只有 codexRateLimits/unknown，没有 calendar period；[TUI](https://github.com/openai/codex/blob/afb436df8b70bb5bc57b86d9a3e829968988cd21/codex-rs/tui/src/chatwidget/reset_credits.rs) 仅以 backend title/description 显示，缺失时回退文案 “Full reset”/“Reset your current usage limits”，不按 resetType 选择范围。月额度卡、code-review/additional windows 的覆盖范围、其他/未来类型均未证明；不能从 title 文案、expiry 间隔或账号存在月窗口推造 quota_period/scope 字段。所比较额度的适用性无法建立，或汇总正数但详情截断而无法确定最早有效 expiry 时，应作为所需数据未知处理，不可忽略正数权益或假定覆盖所有周期。

### 管理 API 代理查询

`POST /v8/management/requests/api-call` 支持 `auth_index`、`method`、`url`、`header`、`data`，header/body 中 `$TOKEN$` 由宿主替换。返回 `status_code`、`header` 与字符串 `body`；管理请求成功不代表上游状态成功。代理按显式 `proxy_url`、所选 auth 的代理、全局代理、直连依次选用。[宿主接口与代理规则](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/api_tools.go#L32-L85)。

将所有比较时间转成绝对时刻；不能比较显示字符串。相对重置秒数必须以本次观测时刻为基准，不能沿用昨天的倒计时。官方界面兼容 ISO 与 Unix 秒/毫秒。[时间解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resetInstants.ts)。

## 用户已确认的规则

- 按 provider 分组，不按套餐拆分；排序依据全部相同则给相同 priority。
- 启动立即执行，用户可配置 cron，默认 `0 0 * * *`；默认宿主时区，可用 `timezone` 覆盖。
- 必要查询失败后只对失败查询重试一次；仍失败则将该 auth 的 `priority` 设为 `-1`，不参与排序；其余 auth 正常排序。不再采用 provider 整组不改的旧规则。
- 按较长额度周期优先比较；有月额度时先比较月额度重置时间或适用月卡到期时间，相同再比较周额度重置时间或适用周卡到期时间。不再因只有月额度而自动跳过。
- 插件只修改 auth file 的 `priority` 数值，不修改任何 CLIProxyAPI 选择、路由、禁用或额度规则；保留宿主混合 provider 路由行为。
- 同 provider 中，一份 auth 有月额度、另一份只有周额度时，各自取最长周期的排序时间比较；相同才继续比较下一层。
- 没有任何可用额度重置时间、必要查询重试后仍失败，或明确返回凭据失效的 auth，统一设 `priority=-1`，不参与时间比较。不因此修改 `disabled` 或删除认证文件。
- 正常参与排序的档位从最低 `0` 起，越优先数字越大；三个不同档位由高到低为 `2、1、0`。相同排序依据给相同数字，不可排序 auth 固定为 `-1`。
- 剩余额度量、额度使用率或是否耗尽不作为插件的排序参数或依据，由 CLIProxyAPI 自行处理。判断重置卡权益是否耗尽所需的卡剩余次数，不是账号的剩余额度量。
- 已比较的排序时间相同、进入下一层时，有下一层可用重置时间的 auth 排在没有下一层的 auth 之前。后者仍正常参与排序，不因此设为 `-1`；双方都没有下一层则同档。这比较的是时间是否存在，不是剩余额度量。
