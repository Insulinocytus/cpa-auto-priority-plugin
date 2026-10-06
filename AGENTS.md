# Repository Guidelines

## Project Overview

CLIProxyAPI 的原生 cgo 插件（`cpa-auto-priority`）。它根据额度的**重置时间**，在同一 Provider 内给认证文件排序，然后只写回 `priority` 字段：数值越大越优先，按 0 起的稠密档位分配（例如 2/1/0），不可排序的认证文件写 `-1`。插件启动时立即跑一轮，之后按 cron 定时执行。

- 领域词汇以 `GLOSSARY.md` 为准，包括认证文件、Provider、额度周期、额度组、重置时间、有效重置卡、排序时间、不可排序认证文件。写 issue 标题、测试名、假设和重构提案时都用这些词，不要用它列出的禁用同义词。

## Agent skills

### Issue tracker

工单和规格保存在 GitHub Issues。操作工单前读取 `docs/agents/issue-tracker.md`。

### Triage labels

使用五个默认 triage 标签。分诊或修改标签前读取 `docs/agents/triage-labels.md`。

### Domain docs

采用 single-context 布局。探索代码前读取 `docs/agents/domain.md`，按其中规则查阅术语表和 ADR。

## Architecture & Data Flow

```
host ──dlopen──▶ cmd/plugin/main.go (C ABI, ABI v1)
                   └─ JSON RPC ─▶ cmd/plugin/runtime.go (lifecycle + 单 worker cron)
                                    └─ priority.Synchronizer.Sync (sync.go)
                                         ├─ GET  /v8/management/plugins        (是否 effective_enabled)
                                         ├─ GET  /v8/management/credentials    (需要 observed_at)
                                         ├─ PATCH credentials/fields {name}    (探测是否为物理认证文件)
                                         ├─ quota() 中按 provider switch ─▶ POST requests/api-call ($TOKEN$ 由 host 替换)
                                         └─ PATCH credentials/fields {name: id, priority}
```

- 插件与 host 的所有交互都走**带鉴权的 management HTTP API**，不使用 host callback。init 之后 host callback 指针一律不保存。
- `runtime.go` 负责以下几件事：
  - RPC：`plugin.register/reconfigure/quiesce/shutdown`、`management.register/handle`。
  - 状态路由：`GET /v0/management/auto-priority/status`。注意这里是 v0，而调用 host 用的是 v8，两者不要统一。
  - cron 解析。
  - 生命周期相位。
- `stop()` 必须取消并等待 worker 退出后才能返回，因为 host 在 shutdown 后会卸载动态库。
- 每代配置只有一个 worker；`Sync` 全程持有 `Synchronizer.mu`，查询串行执行。错过的 cron 触发直接跳过，不排队也不补跑。
- 排序规则（`compare`）：
  - 先比最长的额度周期，较早的排序时间排在前面。
  - 前缀相同时，多出一层已知时间的那一方胜出。
  - 排序时间 = min(自然重置时间, 适用的有效重置卡的最早到期时间)，由 `applyCardExpiry` 计算。重置卡只能把已有层的时间提前，不能凭空新建一层。
- 分组以 provider 为单位（已小写并 trim）。例外：`kimi.com`→`kimi`，`kimi.ai`→`kimi-ai`，别名映射在 `Sync` 里。
- 跳过以下文件：`runtime_only`、`source != "file"`、`path` 为空。探测返回 409 的 plugin_virtual 也跳过。探测结果无法确认时，整轮按 fail closed 处理（`physical_auth_unconfirmed`）。

## Key Directories

- 仓库根目录是 `package priority`，目录名和包名不同。这里放同步引擎和各 Provider 适配器。
- `cmd/plugin/` 是 `package main`，包含 cgo C ABI 外壳（`main.go`）和生命周期/调度（`runtime.go`）。
- `docs/research/cliproxyapi-contracts.md` 记录 host 与各 Provider 的线上契约，钉在上游 CLIProxyAPI `8ef43e4`、Management Center `ee79a79`。**改 sync、写回或任何 Provider 之前先读它。**

## Development Commands

```sh
go test ./...          # 全部测试，无网络、无环境变量
go test -race ./...    # cmd/plugin 并发多，改 runtime 时跑
go vet ./...
gofmt -l .             # 应无输出
# 产物：c-shared 动态库（需要 CGO_ENABLED=1 和 C 编译器；Windows 用 MinGW gcc）
go build -buildmode=c-shared -o cpa-auto-priority.dll ./cmd/plugin   # Linux 用 .so，macOS 用 .dylib
```

- 发版：推 `v<SemVer>` 标签，`.github/workflows/build.yml` 用 zig 交叉编译 linux（glibc 2.17）/windows 的 amd64/arm64、打包并发布 Release；带 `-` 的标签发 pre-release。版本只来自标签（`-X main.pluginVersion`），本地构建为 `dev`。
- host 从文件名推导插件 ID，可带可选的 `-v<version>` 后缀，例如 `cpa-auto-priority-v0.1.0.so`。host 扫描 `plugins/<GOOS>/<GOARCH>/` 和 `plugins/`。
- `go build -buildmode=c-shared` 还会生成一个 `.h`。`.gitignore` 不会忽略它，所以产物要输出到仓库外。
- `cmd/plugin` 依赖 cgo，所以对该包执行 vet/test 也需要 C 工具链。

## Code Conventions & Common Patterns

- **Provider 不走接口或注册表**：`quota.go` 里的 `quota()` 是唯一的 switch 决策点，未知 provider 一律不查询。新增 Provider 的步骤：
  1. 新建 `<name>.go`，写 `func (s *Synchronizer) <name>(ctx, file authFile, …) ([]time.Time, string)`。用 `quotaPeriods{}` 按周期秒数建 key（`7*86400`、`18000`、`monthLayer`，原始时长先过 `quotaLayer`）。用 `periods.add` 检测歧义，失败时返回 `quota_period_ambiguous`。最后返回 `ordered(periods)`。
  2. 在 `quota()` 里加一个 case。要么直接返回（适合自己处理重试的 Provider），要么设置 `query`，复用 `retryOnce`。
  3. 需要原始认证文件时用 `quotaMetadata`，它要求文件名唯一。用完只取所需字段，不要整份保留。
  4. 在 `sync_test.go` 的 `managementStore` 的 api-call switch 里登记上游 URL、headers 和 body，否则 fake 会返回 400 `unsupported query`。
  5. 把契约和源码链接补进 `docs/research/cliproxyapi-contracts.md`。
- 上游请求只走 `s.upstream(...)`，即 `requests/api-call`：
  - 凭据用 `$TOKEN$` 占位，由 host 替换。Meta 例外，它用下载得到的 `dca_token`。
  - **绝不发送 `proxy_url`**，由 host 套用该认证文件自己的代理。
  - 不跟随重定向（`CheckRedirect` 返回 `ErrUseLastResponse`）。
- **排序输入只有两类：自然重置时间和有效重置卡的到期时间。** 剩余额度、`used_percent`、`remainingFraction`、余额、计费周期、套餐日期、token 过期时间、`Date` header 都不参与排序。不要推断或编造卡片和重置字段。
- 时间处理：
  - 一律转成绝对时刻。`instant`/`unix` 把 ≥1e11 的值当作毫秒，小数走 `big.Rat` 以保留纳秒精度。
  - 相对倒计时每轮都以 `s.now()` 为锚重新计算，不缓存。
  - `optionalUnixSeconds`：缺失、null 或 ≤0 视为未知层；非数字文本视为 malformed。
- 重试：`queryAttempts = 2`，只重试失败的那一次查询，且只重试一次。
  - 以下情况不重试：`ok`、`no_reset_time`、`credentials_invalid`、`missing_dca_token`、management 鉴权错误。
  - 写回不重试，失败记为 `write_failed`，本轮继续处理其余文件。
- 错误处理：
  - 统一用固定的 snake_case 码，例如 `errors.New("quota_response_malformed")`，不用 `fmt.Errorf` 包装。哨兵错误是 `ErrManagementAuthentication` 和 `ErrPluginNotEnabled`，用 `errors.Is` 判断。
  - 单个文件的问题记成 status 字符串，不作为 error 返回。整轮失败时返回 `(Round, error)`，Round 里带部分结果。
  - management 返回 401/403 时立即中止整轮，worker 永久停止，因为反复 401 可能导致 IP 被封。用同一个 key 重新配置**不会**重试，只有换成正确的 key 才会恢复。
- **安全**：
  - 不写日志（代码里没有任何 logging），可观测性只靠状态路由。
  - 错误、状态和 Round JSON 中不能出现 key、token、上游 body、PII 或 project id。
  - 配置的 URL 只接受 http(s)；纯 http 只允许 loopback。不允许 userinfo、path、query。key 里不能含 CR/LF。
- 写回只用窄 PATCH `{name: <auth id>, priority}`，不传 `auth_index`，也不碰 `disabled` 等其他字段。不要用 `host.auth.save`，它是整文件保存。`Persistence` 永远记为 `unverified`，因为 HTTP 200 不代表落盘。
- 配置只来自 host 下发的 `config_yaml`，没有环境变量或 flag：
  - 支持的 key：`enabled`、`priority`、`cron`（默认 `0 0 * * *`）、`timezone`（IANA，默认 host 本地时区，已嵌入 `time/tzdata`）、`management_url`、`management_key`。另有 `store`：host 从插件商店安装时写入的安装记录，插件只接受、不校验也不读取（`hostOwned`）。
  - 解码使用 `KnownFields(true)`，其余未知 key 会报 `invalid_plugin_config`。
  - `pluginConfig` 必须保持可比较（用 `==` 判断配置是否相同）。
  - 非法的 reconfigure 不影响正在运行的那一代配置。
- cron 只接受标准 5 段格式，不支持 `@descriptor`、`CRON_TZ=`/`TZ=`、秒字段、`?`，也不允许空列表项。只用 robfig 的 `Parse`/`Next`，不用它的 runner。
- RPC 外层字段用 snake_case；`Metadata`/`ConfigFields` 用上游 Go wire 类型的 PascalCase。
- 命名：Provider 方法是以 provider 命名的小写驼峰（`codex`、`codexUsage`、`codexCards`）。状态码和 JSON tag 都用 snake_case。
- 注释只写简短的 "why" 和不变量，必要时引用 "pinned host" 或 "official UI"。

## Important Files

- `cmd/plugin/main.go`：
  - `cliproxy_plugin_init` 要求 ABI 为 1。
  - `cliproxyPluginCall` 限制请求不超过 2 MiB。
  - 响应用 `C.CBytes` 分配，由 host 通过 `cliproxyPluginFree` 释放。
- `cmd/plugin/runtime.go`：
  - `pluginRuntime.configure/run/stop/handle`。
  - `registration()`：schema_version 6，Version 取 `pluginVersion`。
  - `parseSchedule`。
- `sync.go`：
  - `New(config, client, now)`：HTTP 和观测时间是仅有的两个注入点。
  - `Sync`、`compare`、`applyCardExpiry`、`PluginID`。
- `quota.go`：
  - Provider switch 和 `retryOnce`。
  - `upstream`、`quotaMetadata`。
  - 时间解析和 `quotaLayer`（28–31 天算作月度）。
- `claude.go`、`codex.go`、`antigravity.go`、`kimi.go`、`xai.go`、`devin.go`、`meta.go`：每个 Provider 一个文件。只有 Claude 和 Codex 有重置卡契约。
- `GLOSSARY.md`、`docs/research/cliproxyapi-contracts.md`：领域定义和契约的唯一来源。

## Runtime/Tooling Preferences

- 工具链由 `mise.toml` 固定（Go、zig），本地与 CI（`jdx/mise-action`）一致；`go.mod` 的 `go 1.25.0` 是最低语言版本。代码用到了 `for range N`、`min` 和泛型。依赖只有 `gopkg.in/yaml.v3` 和 `github.com/robfig/cron/v3`，新增依赖前先确认标准库做不到。
- 需要 cgo 和 C 编译器。产物要与 host 的 GOOS/GOARCH 一致；交叉编译用 `CC="zig cc -target <triple>"`，triple 以 workflow 为准。
- 文档用简体中文，标识符用英文。

## Testing & QA

- 只用标准库 `testing`，断言手写成 `if … { t.Fatalf(...) }`。不用 testify、httptest、`t.Parallel`，也没有 build tag。
- 根包测试是外部黑盒测试（`package priority_test`），只通过 `priority.New(...).Sync(ctx)` 这个完整轮次的接口来测，这是 issue #1/#4 的约定。**不要给私有解析器写单测。**
  - 搭建：`s := store(credential("a.json", "codex"), …)`，设置 `s.usage[authIndex]`，然后调用 `synchronizer(t, s).Sync(context.Background())`。时钟固定在 2026-10-04T00:00:00Z。
  - 断言：`s.files[i]["priority"]`、`round.Results[i].QueryStatus/WriteStatus`，以及精确的 `s.queryCount`/`cardQueryCount`（用来验证重试次数）。
  - 惯例：
    - 加一个健康的同组认证文件，证明失败被隔离。
    - 把输入顺序反过来再跑一次。
    - 对 `json.Marshal(round)` 做 `strings.Contains` 检查，确认没有泄露密钥。
    - 未改动的字段用 `reflect.DeepEqual` 确认保持原样。
  - 命名形如 `TestSync<Provider><Behavior>`，表驱动测试用匿名 struct 配合 `t.Run`。
- `cmd/plugin` 的测试是白盒测试（`package main`）：
  - 用 `newTestRuntime(client)` 得到 `controlledClock`，用 `p.configure(lifecycle(validConfig + extra))`，并 `defer p.stop()`。
  - 时间只通过 `clock.Await(t)` 和 `clock.Fire(w)` 推进。
  - fake 的共享状态要用 mutex 保护。
- HTTP 一律用内存里的 `http.RoundTripper`（`managementStore`、`transportFunc`、`scheduledStore`），不起 socket，也不用真实账号。fixture 必须是合成或脱敏数据，并在注释里写明来源（例如提取自上游 `codexQuota.test.ts@ee79a79`）。
- 没有覆盖率门槛。行为变更需要有测试锁住对使用方可见的规则：排序、重试次数、隔离、写回形态、密钥不泄露、生命周期。
