<div align="center">

# CPA Auto Priority

*按额度重置时间，自动排列 CLIProxyAPI 认证文件的 `priority`*

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white)](go.mod)
[![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-8ef43e4-4c1?style=flat-square)](https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473)
[![Plugin ABI](https://img.shields.io/badge/Plugin_ABI-v1-blue?style=flat-square)](cmd/plugin/main.go)

[功能](#功能) • [排序规则](#排序规则) • [支持的 Provider](#支持的-provider) • [安装](#安装) • [查看同步结果](#查看同步结果) • [常见问题](#常见问题)

</div>

`cpa-auto-priority` 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生 cgo 插件。它查询每个认证文件的额度重置时间，在同一 Provider 内排序，再把结果写回认证文件的 `priority`：**额度越早重置、或适用的重置卡越早到期，账号越优先被使用**，避免额度在重置前白白浪费。

插件随宿主启动后立即同步一轮，之后按 cron 定时执行。

> [!WARNING]
> 插件会覆盖认证文件原有的 `priority`。不支持的 Provider、查询失败或没有可用重置时间的认证文件会被写为 `-1`。停用插件不会恢复原值，启用前请先备份认证目录。

## 功能

- **按 Provider 分组排序**：同一 Provider 的认证文件互相比较，不按套餐拆分，不跨 Provider 比较。
- **多层额度周期**：月、周、五小时等周期从长到短逐层比较。
- **重置卡感知**：Codex 和 Claude 的有效重置卡到期时间会提前对应周期的排序时间。
- **只改 `priority`**：用窄 PATCH 写回，不碰 token、`disabled`、代理等其他字段。
- **无需额外进程**：以共享库形式由宿主加载，所有交互都走带鉴权的 management API，上游请求由宿主代发并套用各认证文件自己的代理。
- **安全默认**：不写日志；状态和错误中不含密钥、token 或上游响应；management 鉴权失败立即停止，避免宿主封禁 IP。

## 排序规则

1. 对每个认证文件求出各额度周期的**排序时间** = min(自然重置时间, 适用有效重置卡的最早到期时间)。重置卡只能提前已有周期，不会凭空新增周期；不过期的卡不影响排序。
2. 从最长的额度周期开始比较，排序时间越早越优先；相同则比较下一层。
3. 前面各层都相同时，多出一层已知时间的认证文件优先。
4. 排序依据完全相同的认证文件共享同一档位。档位从 `0` 起连续编号，**数字越大越优先**。
5. 无法参与比较的认证文件（不可排序认证文件）写为 `-1`。这不代表认证已失效，插件也不会禁用或删除它。

例如四个 Codex 认证文件：

| 认证文件 | 周额度重置 | 五小时额度重置 | 写入的 `priority` |
| --- | --- | --- | --- |
| `a.json` | 10-08 00:00 | 10-04 03:00 | `2` |
| `b.json` | 10-08 00:00 | — | `1` |
| `c.json` | 10-12 00:00 | 10-04 01:00 | `0` |
| `d.json` | 查询失败 | 查询失败 | `-1` |

> [!NOTE]
> 剩余额度、使用率、是否耗尽、余额、计费周期、套餐或 token 到期时间**都不参与排序**。插件只读取重置卡信息，不购买、领取或消费卡。

宿主仍负责禁用、额度冷却和同档位内的选择；提高 `priority` 不能绕过这些规则。

## 支持的 Provider

所有查询都需要宿主为认证文件提供 `auth_index`，无需为各 Provider 单独配置。

| Provider | 参与排序的额度 | 条件与限制 |
| --- | --- | --- |
| Codex | 账号通用额度（按接口返回的周期）；通用周 / 五小时重置卡 | 模型、代码审查专属额度不参与；月额度不套用周卡。 |
| Claude | 通用周、五小时额度及对应重置卡 | 需要 OAuth 认证；模型 / 用途专属窗口不参与。 |
| Antigravity | 周、五小时额度 | 需要 `project_id`。多个额度组同周期重置时间不一致或部分缺失时不可排序。 |
| Devin | 周、日额度 | 套餐期限不参与。 |
| Meta | 周额度、滚动窗口额度 | 认证文件须可从宿主下载且含有效 `dca_token`，普通 LLM key 不适用。 |
| Kimi | 月、周及接口明确返回的其他周期 | 支持 Kimi Coding `.com` / `.ai`（分别归组），不支持 Moonshot 开放平台；认证文件须可下载。 |
| xAI | 明确的周额度 | 仅 Grok CLI OAuth，认证文件须可下载；API key、月账期、余额不参与。 |

Codex 和 Claude 的重置卡查询失败或详情不完整时，该认证文件同样不可排序。各 Provider 的接口依据见 [CLIProxyAPI 接口调查](docs/research/cliproxyapi-contracts.md)。

## 安装

> [!NOTE]
> 本插件还没有上架 [CLIProxyAPI 插件商店](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)，需要手动放进宿主的插件目录。[Release](https://github.com/Insulinocytus/cpa-auto-priority-plugin/releases) 提供 linux amd64/arm64、windows amd64/arm64 预编译包，其他平台（包括 macOS）请自行构建。

### 前置条件

- 支持原生插件的 CLIProxyAPI。兼容契约基于 [`8ef43e4`](https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473)（ABI 1、RPC schema 6），其他版本请自行核对。
- 宿主的 management key。

### 1. 放入插件目录

宿主会依次扫描 `<plugins.dir>/<GOOS>/<GOARCH>/` 和 `<plugins.dir>/`，只识别当前平台的扩展名（Linux `.so`、macOS `.dylib`、Windows `.dll`）。插件 ID 取自文件名，文件名必须是 `cpa-auto-priority.<ext>` 或带版本号的 `cpa-auto-priority-v<版本>.<ext>`。下面两种方式都按插件商店的布局放到平台子目录。

#### 方式一：下载预编译包

每个 zip 的根目录只有一个 `cpa-auto-priority.<ext>`，SHA-256 见同一 Release 中的 `checksums.txt`。Linux 包要求 glibc 2.17 及以上，与上游 CLIProxyAPI 的 Linux 发布版一致。

```sh
# Linux amd64（arm64 把两处 amd64 都换成 arm64）
VERSION=0.1.1
PLUGIN_DIR=/path/to/cliproxyapi/plugins/linux/amd64
curl -LO "https://github.com/Insulinocytus/cpa-auto-priority-plugin/releases/download/v$VERSION/cpa-auto-priority_${VERSION}_linux_amd64.zip"
mkdir -p "$PLUGIN_DIR"
unzip -o "cpa-auto-priority_${VERSION}_linux_amd64.zip" -d "$PLUGIN_DIR"
```

```powershell
# Windows amd64（arm64 把两处 amd64 都换成 arm64）
$Version = '0.1.1'
$PluginDir = 'C:\path\to\cliproxyapi\plugins\windows\amd64'
Invoke-WebRequest -OutFile plugin.zip "https://github.com/Insulinocytus/cpa-auto-priority-plugin/releases/download/v$Version/cpa-auto-priority_${Version}_windows_amd64.zip"
Expand-Archive -Force plugin.zip $PluginDir
```

#### 方式二：自行构建

需要 [Go 1.25+](https://go.dev/dl/)，以及与宿主 GOOS/GOARCH 一致的 C 编译器（Windows 使用 MinGW gcc）。在仓库根目录执行：

```sh
# Linux amd64（macOS 把 linux/amd64 换成 darwin/arm64 等，扩展名改为 .dylib）
VERSION=0.1.1
PLUGIN_DIR=/path/to/cliproxyapi/plugins/linux/amd64
mkdir -p "$PLUGIN_DIR"
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X main.pluginVersion=$VERSION" -o "$PLUGIN_DIR/cpa-auto-priority-v$VERSION.so" ./cmd/plugin
rm "$PLUGIN_DIR/cpa-auto-priority-v$VERSION.h"
```

```powershell
# Windows amd64（PowerShell）
$Version = '0.1.1'
$PluginDir = 'C:\path\to\cliproxyapi\plugins\windows\amd64'
New-Item -ItemType Directory -Force $PluginDir | Out-Null
$env:CGO_ENABLED = '1'
go build -trimpath -buildmode=c-shared -ldflags "-X main.pluginVersion=$Version" -o "$PluginDir\cpa-auto-priority-v$Version.dll" ./cmd/plugin
Remove-Item "$PluginDir\cpa-auto-priority-v$Version.h"
```

`go build -buildmode=c-shared` 会额外生成一个 `.h` 头文件，宿主用不到，可以删除。`VERSION` 用于文件名和插件上报给宿主的版本，自行构建时可以填任意合法版本号；不传 `-ldflags` 时版本为 `dev`。

### 2. 配置宿主

把以下内容合并进 CLIProxyAPI 的配置文件（已有 `plugins` 段时直接合并）：

```yaml
plugins:
  enabled: true                      # 全局插件开关，默认 false
  dir: plugins                       # 默认 plugins；相对路径按宿主进程的工作目录解析，支持 ~
  configs:
    cpa-auto-priority:
      enabled: true                  # 本插件开关，默认 false
      priority: 0                    # 宿主的插件排序，与认证文件 priority 无关
      management_url: http://127.0.0.1:8317
      management_key: YOUR_MANAGEMENT_KEY
      cron: "0 0 * * *"              # 可选，默认每天零点
      timezone: Asia/Shanghai        # 可选，默认宿主系统时区
```

全局的 `plugins.enabled` 和本插件的 `enabled` 都要设为 `true`；在管理中心单独打开插件开关不会改动全局开关。

| 配置项 | 说明 |
| --- | --- |
| `management_url` | 宿主的 HTTP(S) origin，必须带 `http://` 或 `https://`，不能带路径（包括 `/v0/management`）、query、fragment 或用户名密码。插件运行在宿主进程内，填 `http://127.0.0.1:<宿主端口>` 即可；Docker 部署同样填容器内的端口，而不是映射到宿主机的端口。非 loopback 地址必须用 HTTPS。 |
| `management_key` | 宿主的 management key，必填，本机访问也需要。 |
| `cron` | 标准 5 段 cron，默认 `0 0 * * *`，例如 `*/30 * * * *` 表示每半小时。不支持秒字段、`@daily` / `@every`、`?` 及内嵌时区。 |
| `timezone` | IANA 时区名，例如 `Asia/Shanghai`、`UTC`。 |

除上表和 `enabled`、`priority` 外，配置中出现其他字段会导致 `invalid_plugin_config`。唯一的例外是 `store`：从插件商店安装时宿主会写入这段安装记录，插件会忽略它。

> [!IMPORTANT]
> 原生插件在宿主进程内运行，拥有和宿主相同的权限，请只加载自己信任的构建产物。配置文件里有明文 management key，请妥善保护，不要提交到版本库。插件不跟随重定向，`management_url` 必须能直接访问。

### 3. 加载插件

宿主在启动和重载配置时扫描插件目录，并加载已启用的插件；如果保存配置后插件没有出现，重启 CLIProxyAPI。加载成功后，管理中心的插件列表中会显示 `cpa-auto-priority`，宿主日志中会出现 `pluginhost: plugin registered plugin_id=cpa-auto-priority`。插件会等待 management API 就绪后自动执行第一轮同步，无需单独启动进程，也没有手动触发接口。

> [!TIP]
> 使用官方 Docker 镜像时，宿主的工作目录是 `/CLIProxyAPI`，把插件目录挂载到 `/CLIProxyAPI/plugins` 即可，例如 `- ./plugins:/CLIProxyAPI/plugins`。

### 更新插件

同一插件存在多个文件时，宿主优先选择带版本号且版本最高的文件，并在下次重载配置时切换过去。因此可以把新版本放成带版本号的文件名（例如把预编译包里的库文件重命名为 `cpa-auto-priority-v0.1.1.so`）；不带版本号的旧文件会被忽略。

> [!TIP]
> Windows 不允许覆盖正在被加载的 DLL。更新时请换一个新的版本号文件名，或者先停止宿主再替换。

## 查看同步结果

插件注册了一个只读状态接口：

```sh
curl -H "Authorization: Bearer YOUR_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/auto-priority/status
```

```json
{
  "phase": "completed",
  "round": {
    "status": "completed",
    "results": [
      {
        "name": "account.json",
        "provider": "codex",
        "priority": 0,
        "query_status": "ok",
        "write_status": "acknowledged",
        "persistence": "unverified"
      }
    ]
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `phase` | `starting`、`waiting`（等待宿主就绪）、`completed`、`empty`（没有可更新的认证文件）、`write_failed`、`failed`、`disabled`、`stopped` 等。 |
| `error` | 整轮失败时的错误码，例如 `management_authentication_failed`。 |
| `query_status` | 该认证文件的额度查询结果，`ok` 以外的值说明它为何被写为 `-1`。 |
| `write_status` | `acknowledged` 表示宿主接受了更新；`failed` 表示写回失败。 |
| `persistence` | 恒为 `unverified`：HTTP 200 不代表宿主已经写入磁盘。 |

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 管理中心显示「未注册」/ 状态接口返回 404 | 插件没有注册成功。先确认全局与插件自身的 `enabled` 都为 `true`，再在宿主日志中搜索 `pluginhost`（Docker 用 `docker logs <容器名> 2>&1 \| grep pluginhost`），按下面几行处理。 |
| 日志出现 `plugin loaded` 之后又有 `plugin.register failed: ...` | 文件已成功加载，是插件拒绝了配置；冒号后面是具体错误码。紧随其后的 `returned invalid metadata or no capabilities` 是同一原因导致的，无需单独处理。 |
| `invalid_management_url: expected HTTP(S) origin ...` | `management_url` 缺少 `http://`、带了路径（如 `/v0/management`）或 query，或者没有写在 `cpa-auto-priority:` 下面。改成 `http://127.0.0.1:<宿主端口>`。 |
| `invalid_management_url: remote management requires HTTPS` | `http://` 后面不是本机地址（如公网 IP、域名、Docker 服务名）。改用 `127.0.0.1`，或改用 HTTPS。 |
| `invalid_management_key` | `management_key` 为空或含换行。 |
| `invalid_plugin_config` | 配置中有拼错或多余的字段，或 YAML 缩进有误。对照[配置宿主](#2-配置宿主)中的字段检查。 |
| `failed to load plugin ... dlopen ...` | 共享库无法加载：包的架构与宿主不一致（`uname -m`），glibc 低于 2.17，或系统使用 musl。 |
| `requires cgo on this platform` | 宿主是不支持插件的构建（如 `_no-plugin` 版本），请换用官方标准版或 Docker 镜像。 |
| 一直是 `waiting` | 首轮就绪检查未通过。检查 `management_url`、宿主日志和版本兼容性。 |
| `empty` | 没有可更新的物理认证文件（只处理 `source` 为 `file` 的认证文件）。后续 cron 仍会检查新增文件。 |
| `management_authentication_failed` | management key 错误或被宿主拒绝。定时任务会就此停止，用同一个 key 重新配置**不会**重试；改成正确的 key 后才会恢复。 |
| `invalid_cron` / `invalid_timezone` | 使用标准 5 段 cron，时区单独写在 `timezone`。 |
| `physical_auth_unconfirmed` | 无法确认认证文件可以安全更新，本轮不写入。请核对宿主 management API 版本。 |
| `priority` 为 `-1` | 查看该文件的 `query_status`，常见值有 `no_reset_time`、`unsupported_provider`、`unsupported_quota_auth`、`quota_period_ambiguous`、`missing_project_id`。 |
| `write_status` 为 `failed` | 写回不重试也不回滚，其他文件可能已经更新。检查宿主日志与文件权限，修正后等待下一轮。 |

> [!CAUTION]
> 宿主可能因为连续鉴权失败封禁来源 IP，而插件无法解除封禁。请不要用错误的 key 反复尝试。

其他行为说明：

- 失败的额度查询最多重试一次；单个认证文件的失败不影响同组其他文件。
- 同步串行执行，耗时较长时错过的 cron 触发会直接跳过，不排队也不补跑。
- 停用插件会停止后续同步，已写入的 `priority` 保持不变。

> [!NOTE]
> 已在 CLIProxyAPI v8.0.15 官方 Docker 镜像（Linux）上实际加载，并对 Claude、Codex 的 OAuth 认证文件完成同步和写回。其他 Provider 尚未用真实账号验证，宿主的磁盘持久化也未经插件确认（`persistence` 恒为 `unverified`）。建议先用少量认证文件确认效果；升级宿主后请重新确认兼容性。

## 开发

```sh
go test ./...        # 全部测试，不访问网络
go test -race ./...  # 修改 cmd/plugin 的并发逻辑时运行
go vet ./...
gofmt -l .           # 应无输出
```

`cmd/plugin` 依赖 cgo，对它运行 test / vet 同样需要 C 编译器。

- [术语表](GLOSSARY.md)：认证文件、额度周期、排序时间等领域词汇的定义。
- [CLIProxyAPI 接口调查](docs/research/cliproxyapi-contracts.md)：宿主与各 Provider 的接口契约，修改同步逻辑或 Provider 前必读。
