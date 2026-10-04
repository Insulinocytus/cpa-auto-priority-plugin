# Issue tracker: GitHub

本仓库的工单和规格保存在 GitHub Issues，使用 `gh` CLI 操作。

## 仓库定位

从 `git remote -v` 推断目标仓库；`gh` 在克隆目录中自动识别仓库。
没有 GitHub remote 时，先配置正确的 remote，再操作工单。

## 常用操作

- 创建：`gh issue create --title "..." --body "..."`。多行正文使用当前 shell 支持的多行字符串，或通过 `--body-file` 读取文件。
- 读取：`gh issue view <number> --comments`；需要结构化正文、标签及评论时使用 `--json number,title,body,labels,comments`。
- 列出：`gh issue list --state open --json number,title,body,labels,comments`，按需使用 `--label` 和 `--state`。
- 评论：`gh issue comment <number> --body "..."`。
- 添加或移除标签：`gh issue edit <number> --add-label "..."` 或 `--remove-label "..."`。
- 关闭：`gh issue close <number> --comment "..."`。

## Pull requests as a triage surface

**PRs as a request surface: no.**

## 技能操作约定

- “publish to the issue tracker”：创建 GitHub issue。
- “fetch the relevant ticket”：运行 `gh issue view <number> --comments`。
- GitHub 的 issue 和 PR 共用编号；无法判断类型时，先用 `gh pr view <number>` 识别，若不是 PR，再读取 issue。

## Wayfinding operations

供 `/wayfinder` 使用：一个 map issue 管理多个 child issues。

- Map：带 `wayfinder:map` 标签的 issue，正文包含 Notes / Decisions-so-far / Fog。
- Child ticket：通过 GitHub sub-issue 关联到 map；不可用时，在 map 正文维护任务列表，并在 child 正文顶部写 `Part of #<map>`。类型标签为 `wayfinder:<type>`，其中 type 为 `research`、`prototype`、`grilling` 或 `task`。
- Blocking：优先使用 GitHub 原生 issue dependencies。添加依赖：
  `gh api --method POST repos/<owner>/<repo>/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-db-id>`。
  数据库 ID 通过 `gh api repos/<owner>/<repo>/issues/<n> --jq .id` 获取，不使用工单编号或 node_id。
  原生依赖不可用时，在 child 正文顶部写 `Blocked by: #<n>, #<n>`。
- Frontier：按 map 顺序选择尚未关闭、没有 assignee、没有未关闭 blocker 的 child。原生依赖用 `issue_dependencies_summary.blocked_by` 判断；文本依赖逐一检查 blocker 状态。
- Claim：开始工作前执行 `gh issue edit <n> --add-assignee @me`。
- Resolve：评论答案，再关闭 child，最后向 map 的 Decisions-so-far 追加摘要和链接。
