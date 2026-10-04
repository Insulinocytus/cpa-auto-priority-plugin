# Repository Guidelines

## Agent skills

### Issue tracker

工单和规格保存在 GitHub Issues。操作工单前读取 `docs/agents/issue-tracker.md`。

### Triage labels

使用五个默认 triage 标签。分诊或修改标签前读取 `docs/agents/triage-labels.md`。

### Domain docs

采用 single-context 布局。探索代码前读取 `docs/agents/domain.md`，按其中规则查阅术语表和 ADR。

## Project Overview

CLIProxyAPI 原生 Go 共享库插件，按额度排序时间调整**认证文件**的 `priority`，不是模型优先度。支持 Codex、Claude、Antigravity、Devin、Meta、Kimi、xAI；Codex/Claude 同时考虑有效重置卡。宿主就绪后立即同步一轮，随后按 cron 重复；没有独立服务、数据库或手动触发入口。

## Architecture & Data Flow

- `cmd/plugin/main.go` 提供 CGO ABI；`runtime.go` 处理 register/reconfigure、配置验证、只读状态与 worker 生命周期。init 不启动同步，也不保留宿主 callback 指针。
- 根包 `priority` 的入口是 `New(config, client, now)` 与 `Synchronizer.Sync(ctx)`：检查有效启用 → 读取 credentials 快照 → 筛选并探测物理认证 → 查询额度/卡 → provider 内排名 → 写回。
- `quota.go` 统一分派 provider、代发请求、重试和时间解析；各 provider 文件解析上游契约。请求经宿主 `requests/api-call`，以 `auth_index` 选择认证，保留宿主 token/代理语义；Meta DCA 使用专用 token。元数据下载仅临时提取必要字段。
- 排序按额度周期从长到短比较，较早排序时间更优；公共前缀相同时，已知层更多者更优。同序列同档，档位从 0 连续递增，**更优者 priority 更大**；不按套餐拆组。Kimi `.com`/`.ai` 各自按宿主别名归组。不可排序文件写 `-1`，不等于认证失效。
- 只按认证 ID 窄 `PATCH credentials/fields` 的 `priority`，每项写前重查启用状态；保留 token、disabled、代理等其他字段。物理认证探测依赖确切响应，未知响应整轮 fail closed；写成功仅为 `acknowledged`，磁盘持久化仍 `unverified`。
- 单实例 `Sync` 加锁串行；runtime 单 worker，长轮次错过的 cron 不补跑。每轮重读认证和额度，不缓存排序时间。配置先验证再替换 generation；quiesce/shutdown 取消并等待 worker，状态由独立 mutex 保护。

## Key Directories

- 根目录：`priority` 库及 provider 实现；没有 `src/` 层。
- `cmd/plugin/`：原生共享库适配、调度和生命周期测试。
- `docs/research/`：上游接口证据、支持边界和未知项；修改 provider 前查对应契约。
- `docs/agents/`：按任务加载的领域、工单和 triage 规则；当前无脚本或 Make/CI 任务封装，直接用 Go 命令。

## Development Commands

在仓库根目录执行：

```sh
go fmt ./...                              # 格式化 Go 代码
go vet ./...                              # 静态检查
go test ./...                             # 全包测试
go test . -run TestSync                    # 单轮同步定向测试
go test ./cmd/plugin -run 'TestStartup|TestShutdown|TestInvalid|TestCron|TestReadiness'
go test -race ./...                        # 完整并发检查
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/cpa-auto-priority.so ./cmd/plugin
```

构建行使用 POSIX shell；macOS 改为 `.dylib`。Windows PowerShell：

```powershell
$env:CGO_ENABLED='1'; go build -buildmode=c-shared -o plugins/cpa-auto-priority.dll ./cmd/plugin
```

运行需由 CLIProxyAPI 加载共享库，宿主 YAML 配置见 `README.md`；库 basename/plugin ID 必须为 `cpa-auto-priority`。`main()` 为空，`go run ./cmd/plugin` 不会启动插件。真实宿主启用后会写认证文件 priority，不是 dry-run。

## Code Conventions & Common Patterns

- 使用 gofmt 格式、Go 导出/私有命名；根包按 provider 分文件。JSON/YAML 字段和结果错误码用 snake_case；ABI metadata 用 PascalCase、capabilities 用 snake_case。
- 复用 `quota.go` 的分派、`upstream`、`retryOnce`、时间 helper；provider 返回排序时间序列及状态码，不另建 HTTP 层或注册框架。用指针/`json.RawMessage` 区分缺失、null 和零值，按字段契约解析时间单位。
- `New` 注入 `*http.Client` 和 `func() time.Time`；runtime 额外注入 `wait`。异步通过 `context`、cancel/done channel 与 mutex 管理，不加并行 provider fan-out。
- 管理端 401/403 是整轮致命错误，停止 worker；上游认证错误只影响该认证。必要失败请求最多两次，已成功步骤不重复；写失败不重试。调用方同时检查 `error`、`Round.Status` 和逐项状态，不能将 nil error 当全轮成功。
- 卡到期只提前已有额度周期的排序时间，不创建缺失周期；不过期卡不改变排序时间。同周期重置时间歧义应拒绝，不擅自取最早值；剩余额度和套餐期限不参与排名。
- 保持敏感信息边界：不日志/缓存完整 auth 或额度响应，不输出 token/管理密钥，不覆盖宿主代理，不购买或消费重置卡。

## Important Files

- `sync.go`：公开 API、认证筛选/探测、排名、受控写回；`quota.go`：provider 分派及共享请求/时间逻辑。
- `codex.go`、`claude.go`：自然额度与重置卡；其余同名 provider 文件：各自认证前提和周期解析。
- `cmd/plugin/main.go`：ABI 入口；`cmd/plugin/runtime.go`：严格 YAML 配置、cron/timezone、generation 和状态路由。
- `go.mod` / `go.sum`：工具链最低版本及依赖；`README.md`：构建、宿主配置、运行限制；`docs/research/cliproxyapi-contracts.md`：固定上游契约和证据。

## Runtime/Tooling Preferences

- Go 1.25+、CGO、目标平台 C 编译器；依赖管理用 Go modules。现有外部依赖为 `yaml.v3` 与 `robfig/cron/v3`，不依赖整个 CPA Go module。
- 兼容基准是 CPA `8ef43e4`、ABI 1、RPC schema 6；`config_yaml` 是 JSON base64 YAML。升级宿主需重新核对 ABI、管理接口与物理认证探测契约。
- 管理 URL 必须本宿主 HTTP(S) origin，非 loopback 必须 HTTPS；所有来源都要求显式密钥，禁止 redirect。密钥不入库，管理 UI 不保证隐藏配置。
- cron 仅标准五字段；默认 `0 0 * * *` 和 `time.Local`，可单独配置 IANA timezone。按日历而非固定 24 小时触发；仅复用 cron parser/Next，不启用 job runner。

## Testing & QA

- Go 标准库 `testing`，表驱动 `t.Run` 和内联 JSON fixture。根测试为 `priority_test`，插件测试为 `main`；复用 `sync_test.go` 的有状态 management store 与 `cmd/plugin/clock_test.go` 的受控时钟。
- 优先从完整 `Sync` seam 验证最终 priority、业务字段不变及错误隔离；调度通过注入 now/wait 和 channel 推进，真实超时只作死锁 watchdog，不等待真实午夜。测试不监听端口、不访问真实网络/账号或加载宿主。
- provider 变更覆盖周期 precedence、缺层/同档、输入顺序、请求契约及独立重试；生命周期变更覆盖非法配置保留旧任务、禁用/重新启用、取消和 join。新增 fixture 明确合成数据或上游证据来源。
- 定向回归后运行完整 race 与 vet；当前没有配置覆盖率百分比门槛。UT 不证明真实共享库加载、部署 ABI、真实账号接口或宿主内存/磁盘持久化；报告这些验证限制。
