# CPA Auto Priority

CLIProxyAPI 原生 Go 插件：宿主管理接口就绪后立即执行一轮 Codex **自然额度重置** priority 同步，此后按标准五字段 cron 重复执行同一完整同步入口。对应 [issue #2](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/2) 和 [issue #8](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/8)。重置卡与其他 provider 查询由各自接入工单实现，本次调度不假造这些数据契约。

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
      cron: "0 0 * * *"          # 可省略；每天日历零点，不是每隔 24 小时
      timezone: Asia/Shanghai   # 可省略；默认宿主进程的系统时区
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

没有手动触发、外部替代代理或数据库。可复用的 Go 入口是 `priority.New(config, client, now)` 和 `Synchronizer.Sync(ctx)`；`client=nil` 使用带 30 秒超时的 HTTP client，`now=nil` 使用当前时刻。同实例单轮串行，取消后不继续写入。

## 单轮规则

1. 检查管理 `plugins` 中本实例的 `effective_enabled`；读取当前 `credentials` 的完整、非分页列表，必须有合法 `observed_at` 和 `files`。
2. 排除 runtime-only、非 file source、无 backing path 的条目。宿主列表**没有** `plugin_virtual` 标记，不能仅用这些字段认定物理认证：对每个候选的认证 **ID** 发无字段 `PATCH credentials/fields {"name":"<id>"}`。pinned 宿主在查 virtual 后检查字段：确切 `400/no fields to update` 确认可更新物理候选，确切 virtual `409` 排除。此探测不改认证业务字段、不持久化；其他响应 fail closed，整轮不进行 priority 写入。探测与实际更新都用 ID，避免共享文件名选错认证。
3. Codex 使用 `POST /v8/management/requests/api-call` 代发 `GET https://chatgpt.com/backend-api/wham/usage`。以 `auth_index` 选认证，`Bearer $TOKEN$` 由宿主替换；从列表的 `id_token.chatgpt_account_id` 添加账号 header（存在时）。不传 `proxy_url` 覆盖，保留宿主所选认证代理、全局代理、直连的原有语义。
4. 只比较通用 `rate_limit.primary_window/secondary_window`。用途专属的 `code_review_rate_limit` 和命名的 `additional_rate_limits` 不参与，不引入模型偏好。primary/secondary 是槽位，不代表固定周期。
5. 按明确的正整数 `limit_window_seconds` 从长到短组织时间序列；604800 秒是周，18000 秒是五小时。沿用官方管理界面分类，28–31 日统一为月层；其他明确周期按其秒数排序，不猜成周。ISO/RFC3339、Unix 秒/毫秒（官方 helper 的 `1e11` 分界）、numeric string 和相对秒数归一化为绝对时刻。相对值锚定该响应接收时刻，每轮重新观测。
   Unix 数值保留原单位的整数/十进制精度，等价的 ISO 与非整秒 Unix 编码不会因浮点转换被拆成不同档位。
6. 相同周期、相同重置时刻合为一层；同周期不同重置时刻在上游没有查证的代表窗口契约，返回 `quota_period_ambiguous`，重试一次仍不明确则该认证为 -1。**不擅自取 min/max 或按槽位选代表窗口**。
7. 同 provider 内比较各自最长周期的时间；越早越优先，相同才比较下一层，有下一层优先于没有下一层。时间序列完全相同共用档位。正常档位从最低 0 连续向上编号，不按套餐、文件名、输入顺序或额度量拆组。
8. 必要额度请求失败或 malformed 仅重试失败请求一次；仍失败、401 明确失效、无任何可用自然重置、缺查询索引、未知 provider 均只将该物理认证设为 -1，其余继续。成功确认缺少/null 的短窗口不是失败；已有窗口的损坏周期/时间是失败。有效窗口的时间明确为 null/缺失时不虚构时间。不会拿订阅或 token 过期代替重置时间。
9. 写前再次确认启用状态，仅 `PATCH /v8/management/credentials/fields {"name":"<id>","priority":N}`。不提交完整 auth JSON，不改变 disabled、token、代理或其他业务字段。写入失败返回 `write_status=failed`，不重试、不回滚，继续尝试其他认证；HTTP 2xx 且 `status=ok` 仅为 `acknowledged`，**从不声称磁盘持久化已验证**。

管理 HTTP 层的 401/403 与 Codex 上游 401 不同：任一管理请求返回 401/403 时，本轮立即终止，返回 `priority.ErrManagementAuthentication`（`management_authentication_failed`），状态为 `failed`，**同时停止后续 cron**；不重试、不继续其他查询或 priority 写入。即使错误正文为空或不是 JSON，也按 HTTP 状态处理，不输出正文。普通查询/写入失败仍按上述规则隔离，下次日历触发才开始新轮次。宿主会在同一来源 IP 鉴权失败 5 次后封禁 30 分钟（包括 loopback），因此不能将错误管理密钥当作未就绪无限轮询。修正管理配置后 reconfigure 启动新一轮；已有宿主 IP 封禁不会被插件清除。

## 调度与生命周期

- `cron` 的五字段依次为分钟、小时、月中日期、月份、星期，支持通配符、列表、范围、步长和英文月份/星期名；星期为 `0–6`（星期日为 0）。默认 `0 0 * * *`。不接受秒字段、`@daily`/`@every`、`?` 或表达式内的 `TZ=`/`CRON_TZ=`；时区只能通过 `timezone` 设置。月中日期和星期同时限制时采用标准 cron 的 OR 语义。非法表达式、不可能的日历日期和非法时区显式返回 `invalid_cron`/`invalid_timezone`，不会换成默认时间表。
- `timezone` 使用 IANA 名称，如 `Asia/Shanghai`、`America/New_York` 或 `UTC`；省略或空字符串采用宿主进程的 `time.Local`，容器中通常由容器时区配置决定。内嵌 Go 时区数据库，宿主无 zoneinfo 文件时仍可使用明确的 IANA 覆盖。按日历计算下一次触发，DST 跨日可以是 23 或 25 小时；不存在的当地时刻跳过，重复的当地时刻按 cron 日历匹配。
- 原生 init 不启动同步、不保存宿主回调指针或请求作用域 `host_callback_id`。register/reconfigure 启动实例后台任务，沿用明确鉴权的管理 HTTP 入口，不使用宿主 `scheduler.pick`，不改变认证选择策略。
- 首次同步前每秒只轮询宿主就绪；监听或快照结构未就绪时状态为 `waiting`，管理 401/403 则终止为 `failed`。初次 auth 加载与监听开放的顺序依据下述 pinned 普通模式源码；不把 init 的空集合报为成功。就绪后真的空集合为 `empty`，不写入；仍保持 cron，以便下一轮发现新增认证文件。
- 首次同步与后续触发全部调用同一 `Synchronizer.Sync`，每轮重新枚举物理认证文件并取得当前额度数据，不缓存前一轮排序时间。一个后台 worker 串行执行；长轮次覆盖的触发直接跳过，结束后计算未来的下一个日历时刻，不建立队列或追赶执行。仅使用 `robfig/cron/v3` 的解析器和 `Next`，不启用其 job runner，不添加查询/写入重试。
- 相同配置的 reconfigure 幂等；配置改变先完整验证，再取消并等待旧 generation，启动一次新的首次同步。非法配置保留旧 generation。`enabled: false` 的 reconfigure、`plugin.quiesce`、`plugin.shutdown` 和原生 shutdown 都取消并等待 worker 退出，返回后没有本实例任务继续访问宿主。
- 宿主仅切换 disable 而不发送生命周期通知时，仍存在下述 TOCTOU 限制：定时等待无法立即获知禁用；下一轮的启用检查返回 `priority.ErrPluginNotEnabled`（`plugin_not_enabled`）后停止 worker，不再读取认证、查询额度或写入 priority。重新启用时宿主即使发送相同配置的 reconfigure，也会启动新的首次同步；管理 401/403 后仍需修改配置。此行为不是实际共享库卸载验证。

## 固定兼容契约与证据

宿主基准：[CLIProxyAPI `8ef43e4`](https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473)。原生 ABI **1**、RPC schema **6**；配置是 JSON `config_yaml` 的 base64 编码 YAML；metadata 用 PascalCase，capabilities 用 snake_case。

- [ABI/schema](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/pluginabi/types.go#L5-L35)、[官方 Go wrapper](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/examples/plugin/host-callback-auth-files/go/main.go#L1-L221)。
- [Build 先加载插件](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/builder.go#L235-L275)；普通非 Home 模式的 [Run 初次 auth 加载 → NewServer → reconfigure → listener](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/service_lifecycle.go#L70-L225)。[INFERENCE] 鉴权成功的管理响应证明监听已开放，初次 auth 加载已被尝试；不证明加载成功、watcher/model 注册完成或 Home 订阅就绪。本插件不保证 Home 模式。
- [v8 管理路由](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/server_management_v8.go#L30-L51)、[列表与 Codex claims](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files.go#L651-L746)。
- [非变更探测、virtual guard 与窄更新](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/auth_files_fields.go#L257-L415)、[manager 列表/GetByID 返回 clone](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/conductor_selection.go#L1520-L1543)。探测依赖此版本校验顺序和有限错误文本；升级宿主需重新核对，不能把未知 400/409 当成功。
- [代发请求与代理选择](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/api_tools.go#L32-L238)、[持久化失败可能只记录日志](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/conductor_lifecycle.go#L273-L307)。
- [管理鉴权与来源 IP 封禁规则](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/api/handlers/management/handler.go#L301-L392)：401/403 是管理访问失败，不是可无限重试的启动就绪信号。
- [原生卸载顺序](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/pluginhost/loader_unix.go#L237-L268)：宿主先关闭 HTTP callback instance，再调用插件 shutdown，随后释放回调表并卸载库。本插件不使用该 callback instance；shutdown 同步 join 自己的 HTTP worker。源码核对与受控 UT 不证明真实卸载安全性。

Codex 基准：[官方管理界面 `ee79a79`](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/ee79a794526a30c03748a8864a9ac6589a31833b)。这是第一手实现证据，不是 OpenAI 正式发布的 wham schema：

- [周期、scope、请求 headers](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/features/quota/providers/codex/data.ts#L68-L445)、[ISO/Unix/相对时间 helper](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/src/utils/quota/resetInstants.ts#L23-L77)。
- [上游脱敏格式 fixture](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/ee79a794526a30c03748a8864a9ac6589a31833b/tests/codexQuota.test.ts#L28-L63) 含通用周窗口及独立 Spark 周窗口。UT 保留其自然额度形状，其他月/ISO/边界数据注明为合成输入；不声称采集过真实账号响应。

**限制：** 没有跨文件事务，也没有跨探测/查询/写入的快照事务。宿主 disable 不通知仍加载的库，本插件依赖 `effective_enabled` 检查；检查后立刻 disable 或 auth 集合变化仍有 TOCTOU，窄 PATCH 的 virtual guard 继续保护虚拟认证。不同 provider 的编号独立计算，宿主混合 provider 路由仍可能跨 provider 比较最终数字；插件不改变路由规则。停止插件不会恢复此前已写入的 priority。

## 测试

```sh
go test . -run TestSync                     # 完整单轮 seam 的定向 UT
go test ./cmd/plugin -run 'TestStartup|TestShutdown|TestInvalid|TestCron|TestReadiness'
go vet ./...                                # 类型/静态检查
go test -race ./...                         # 最终完整 UT，含并发检查
```

UT 使用固定时钟和有状态 HTTP transport adapter：不监听端口、不访问真实网络/账号、不加载真实宿主。覆盖多周期 precedence、跨套餐/缺层/同档、输入顺序、provider 隔离、ISO/秒/毫秒/相对时间及跨年、重试隔离、virtual/配置项排除、仅 priority 变化、并发 token 刷新保留、写入失败、显式鉴权/代理语义、敏感数据不泄露、配置注册/reconfigure/取消退出；管理读取、物理认证探测、额度代发及 priority 写入的 401/403 立即终止，修正密钥后可重新配置恢复。

调度 UT 使用受控时钟推进触发，不等待真实午夜、不监听端口、不访问网络/账号。验证首次一次执行、默认宿主时区日历零点、显式 cron/timezone、DST 23/25 小时跨日、非法配置保留旧任务、auth 新增/移除、自然重置后的新档位、两次查询上限、-1 隔离、仅 priority 更新、写失败不重试、长轮次跳过触发、关闭取消并等待宿主请求结束，以及关闭后没有后台访问。时间超时仅用作测试死锁 watchdog，不作为调度推进。

**重置卡：** 当前完整入口没有重置卡接入，“卡被消费后的新排序”由 [Codex #3](https://github.com/Insulinocytus/cpa-auto-priority-plugin/issues/3) 通过同一 `Sync` 入口的连续两轮 UT 验收，不用合成伪字段替代。接入后自动由相同入口调度。本次不加载真实插件、不做真实宿主/账号 smoke。

先前实现阶段完成过 Windows `c-shared` 编译（未加载）和无网络、受控 transport 的单轮入口运行检查。本工单的调度验证只使用上述 UT，不做 smoke、真实插件加载或真实账号验证。

**未验证：** 真实共享库加载、部署平台运行时 ABI、实际管理鉴权、真实账号接口、宿主内存与磁盘持久化兼容性。UT 和源码核对不能证明这些运行时性质；不安排真实宿主/账号 smoke。
