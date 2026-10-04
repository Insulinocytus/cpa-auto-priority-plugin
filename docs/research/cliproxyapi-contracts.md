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
| Kimi | `GET https://api.kimi.com/coding/v1/usages` 或 `api.kimi.ai` 对应地址 | `usage` 与 `limits[].detail` 的 `reset_at/reset_time`，或相对秒数 `reset_in/ttl`；周期由 `window.duration/timeUnit` 等确定。存在只有月额度的套餐，不应将月刷新伪称周重置。[端点](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/constants.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/builders.ts#L252-L389) |
| xAI | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` | `config.currentPeriod.type` 标识周期，`currentPeriod.end` 为期末；也支持 snake_case 与 billing-period 字段。只有明确周周期才可作为周重置，月账单和付费健康探测不是替代数据。[解析与周期隔离](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/builders.ts#L394-L594) |
| Devin | `POST https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus` | `userStatus.planStatus.weeklyQuotaResetAtUnix`，Unix 秒。[请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/devin/requests.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/devinQuota.ts#L23-L51) |
| Meta | `POST https://api.meta.ai/muse-code/key`，需 auth 文件中的 DCA token，不是普通 LLM key | `subs_usage.weekly.resets_at`，官方界面按 Unix 秒显示。响应可能包含密钥和 PII，只能提取所需字段，不记录完整响应。[请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/meta/requests.ts)、[解析](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/metaQuota.ts)、[单位换算](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/meta/MetaQuotaBody.tsx#L45) |

### 手动重置卡

- Codex：`GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits`。原始 `credits[]` 的 `reset_type=codex_rate_limits`、`status=available` 标识相关未消费权益，`expires_at` 是到期时间；另有 `available_count` 与 `applicable_available_count`，两者不能混同。请求还使用账号 header、`OpenAI-Beta: codex-1` 和 `Originator: Codex Desktop`。[过滤字段](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resetCredits.ts)、[查询请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts#L314-L410)。
- Claude：`GET https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1`，读取 `cedar_ember`。`grants[].ends_at` 为到期时间，`resets_left` 为剩余次数，`starts_at`、`paused`、`clears`、`usable_now`、`use_requires_limit` 等描述适用性；顶层有 `eligible`、`at_limit`、`weekly_resets_at`、`cooldown_until`。[原始字段与状态查询](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/services/api/claudeResetGrants.ts#L143-L266)。
- **已确认的卡条件**：排序考虑仍有剩余次数、已生效、未过期且适用于所比较额度的权益，不要求账号此刻已经触及 limit；多张取最早到期的有效卡。插件不自动消费卡。该规则已扩展到用户提出的月额度；但已检查源码没有建立月额度重置卡的独立字段契约，不能将周额度卡未经确认地用于月额度。
- 已检查的其余额度 adapters 未建立手动卡到期数据契约，不能声称这些 provider 存在或不存在卡。未知 provider 不能凭空生成周重置或重置卡时间。

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
