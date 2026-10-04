# CPA Auto Priority

为 CLIProxyAPI 自动调整认证文件的 `priority`，优先使用额度更早重置、或适用重置卡更早到期的账号。启用后立即同步一次，之后按配置的 cron 定时更新。

## 安装与启用

需要支持原生插件的 CLIProxyAPI，以及已导入宿主的认证文件。当前兼容契约基于 [CLIProxyAPI `8ef43e4`](https://github.com/router-for-me/CLIProxyAPI/tree/8ef43e4df3b216a42493105d31c2873b69191473)（ABI 1、RPC schema 6）；其他版本需核对兼容性。

> [!WARNING]
> 插件会覆盖原有认证文件 priority；不支持的 provider、查询失败或没有可用重置时间的物理认证文件也会被设为 `-1`。停用插件不会恢复原值，启用前建议备份认证目录。

### 1. 构建插件

安装 Go 1.25+ 和目标平台 C 编译器，在本仓库根目录执行：

Linux：

```sh
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/cpa-auto-priority.so ./cmd/plugin
```

macOS 使用同一命令，将输出扩展名改为 `.dylib`。Windows PowerShell：

```powershell
$env:CGO_ENABLED='1'; go build -buildmode=c-shared -o plugins/cpa-auto-priority.dll ./cmd/plugin
```

将生成的共享库放入宿主的插件目录。文件名必须为 `cpa-auto-priority`，扩展名按平台选择；编译平台和架构须与宿主一致。

### 2. 配置宿主

将以下配置合并进 CLIProxyAPI 的 YAML；已有 `plugins` 段时直接合并，不要重复添加：

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

| 配置项 | 说明 |
| --- | --- |
| `management_url` | 当前宿主的 HTTP(S) origin，如 `http://127.0.0.1:8317`；不附加 `/v0/management`、其他业务路径、query 或 fragment，也不含用户名密码。非 loopback 地址必须 HTTPS。 |
| `management_key` | 宿主管理密钥，必须填写；本机访问也需要鉴权。 |
| `cron` | 标准五字段 cron，默认 `0 0 * * *`。例如 `*/30 * * * *` 每半小时同步；不支持秒字段、`@daily`、`@every`、`?` 或内嵌时区。 |
| `timezone` | IANA 时区，如 `Asia/Shanghai`、`UTC`；省略时使用宿主进程的系统时区。 |

> [!IMPORTANT]
> 保护包含管理密钥的配置文件，不要提交到版本库；管理 UI 不保证隐藏该字段。管理地址不会跟随重定向，应填写可直接访问的宿主地址。

### 3. 启动宿主

重启 CLIProxyAPI，使其加载共享库和配置。插件会等待管理接口就绪，随后自动同步；无需单独启动插件进程，也没有手动触发接口。

## 查看同步结果

用宿主管理密钥访问只读状态接口，将地址和密钥替换成自己的配置：

```sh
curl -H "Authorization: Bearer YOUR_MANAGEMENT_KEY" http://127.0.0.1:8317/v0/management/auto-priority/status
```

响应示例：

```json
{"phase":"completed","round":{"status":"completed","results":[{"name":"account.json","provider":"codex","priority":0,"query_status":"ok","write_status":"acknowledged","persistence":"unverified"}]}}
```

`phase=completed` 表示本轮已完成，仍需查看各文件的 `query_status` 和 `write_status`。`acknowledged` 表示宿主管理接口接受了更新；`persistence=unverified` 表示插件未验证磁盘持久化结果。

## 优先度如何决定

- 同一 provider 内比较，不按套餐拆分。各自先取最长额度周期的排序时间，相同再比较下一层；例如有月额度时先月、再周、再短周期。
- 排序时间越早越优先；此前时间相同时，有下一层时间的账号优先于没有下一层的账号。
- 有适用的有效重置卡时，取卡最早有限到期时间与该周期自然重置时间中较早者。卡只影响已有周期；没有到期时间的卡不提前排序。插件只读取卡信息，不购买、领取或消费卡。
- 排序依据相同则 priority 相同；不同档位从 `0` 连续向上编号，数字越大越优先。例如三个不同档位按优先顺序为 `2、1、0`。
- 剩余额度、使用率、是否耗尽、套餐或 token 到期时间不参与排序。不可排序的文件为 `-1`，不代表已经证实认证失效，也不会因此禁用或删除文件。

插件只修改 priority，不修改认证 token、disabled、代理或宿主选择规则。宿主仍处理禁用、额度冷却和同档账号选择；会话亲和性等行为也可能影响实际使用顺序。不同 provider 独立编号，但宿主的混合 provider 路由仍可能跨 provider 比较这些数字。

## 支持范围

所有额度查询都需要宿主提供对应认证的 `auth_index`；无需为每个 provider 单独配置插件。

| Provider | 可参与排序的额度 | 使用条件与限制 |
| --- | --- | --- |
| Codex | 通用额度，按接口返回的周期比较；支持通用周/五小时重置卡 | 模型、代码审查专属额度不参与；月额度不套用周卡。 |
| Claude | 通用周、五小时额度及对应重置卡 | 需要 OAuth 认证；模型/用途专属窗口不参与，未支持月窗口/月卡。 |
| Antigravity | 周、五小时额度 | 需要 `project_id`。多个额度组同周期的重置时间不同或部分缺失时不可排序；只查询 daily、daily sandbox 地址，不覆盖自定义企业/GCP 地址或回退 production。 |
| Devin | 周、日额度 | 需要自然重置时间；套餐期限不参与。 |
| Meta | 周、滚动窗口额度 | 认证文件须可从宿主认证目录下载并含有效 `dca_token`，普通 LLM key 不适用。 |
| Kimi | 月、周及接口明确返回的其他周期 | 支持 Kimi Coding `.com`/`.ai`，不支持 Moonshot 开放平台余额；认证文件须可下载。只有月额度也可排序。 |
| xAI | 明确的周额度 | 仅 Grok CLI OAuth，认证文件须可下载；API key、月账期、余额不参与。 |

Codex/Claude 必须成功取得所需的额度与卡详情；卡查询失败或详情不完整也会使该认证不可排序。其余 provider 当前不使用重置卡。Kimi `.com` 与 `.ai` 按宿主别名分别归组。

## 常见问题

| 状态或现象 | 处理方式 |
| --- | --- |
| 状态接口返回 404 | 检查共享库文件名、插件目录、全局与本插件启用开关，以及宿主是否成功加载插件。 |
| `waiting` | 尚未完成首轮就绪检查。若持续出现，检查管理地址、宿主日志及版本兼容性。 |
| `empty` | 本轮没有可更新的物理认证文件；检查认证是否已导入且为文件来源。插件仍会在后续 cron 检查新增认证。 |
| `management_authentication_failed` | 检查管理密钥和宿主访问限制，修正配置后让宿主重新配置插件或重启。此错误会停止后续 cron；宿主已有的来源 IP 封禁不会被插件清除。 |
| `invalid_cron` / `invalid_timezone` | 使用五字段 cron，并将 IANA 时区单独填入 `timezone`。 |
| priority 为 `-1` | 查看该文件的 `query_status`：可能是缺重置时间、查询失败、认证元数据缺失、额度周期歧义或不支持的 provider/认证类型。 |
| `physical_auth_unconfirmed` | 无法确认认证文件可安全更新，本轮不写入；核对宿主管理接口版本。 |
| `write_failed` / `write_status=failed` | 查看宿主日志与文件权限。写入不重试、不回滚，其他文件可能已更新；修正后等待下一轮。 |

必要查询失败最多重试一次；普通单文件错误不阻止其他文件更新。管理鉴权失败则立即终止本轮并停止定时任务。宿主可能因连续鉴权失败封禁来源 IP，不要用错误密钥反复尝试。

同步串行执行，长轮次错过的 cron 不会排队补跑；停用插件会停止后续同步，但已写入的 priority 保留。

> [!NOTE]
> 尚未验证真实 CLIProxyAPI 加载、真实账号接口及宿主磁盘持久化兼容性，也不保证 Home 模式。建议先在少量认证文件上确认效果。更新宿主或上游接口后需重新确认兼容性。

接口依据和具体支持边界见 [CLIProxyAPI 接口调查](docs/research/cliproxyapi-contracts.md)；领域术语见 [术语表](GLOSSARY.md)。
