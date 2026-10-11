# apiserve 与 gateway 并存实测记录

日期：2026-10-09
执行时 HEAD：特性分支的最后一个提交（当时的多提交历史现已压缩为单个提交，故此处不记录 hash）
Go 版本：`go1.26.7 windows/amd64`
构建：`go build -tags "goolm,stdjson" -o picoclaw.exe ./cmd/picoclaw`、`go build -tags "goolm,stdjson" -o picoclaw-api.exe ./cmd/picoclaw-api`
环境：Windows 10（PowerShell），`PICOCLAW_HOME` 指向一次性临时目录

本记录的目的：证伪 spec §3.3 的两个假设——① 接入层不写 PID 文件所以不与 gateway 冲突；② 同 home 并行时两者互不阻塞。

## 配置

```json
{
  "model_list": [
    { "model_name": "test", "model": "openai/gpt-4o", "api_key": "sk-dummy-for-acceptance" }
  ],
  "agents": {
    "defaults": { "model_name": "test" },
    "list": [ { "id": "main", "default": true } ]
  }
}
```

> **记录一条计划缺陷**：计划 Task 11 的 Step 1 原本只给 `{"agents":{"list":[{"id":"main","default":true}]}}`，实测**服务无法启动**——`apiserve.New` 会构建 provider，缺 `model_list` 时直接退出：
>
> ```
> Error: create provider: model "" not found in model_list: model is required
> ```
>
> 这比计划预期的"能启动、调用时返回 500"更强：**没有可用模型时，`/health` 与 `/ready` 都无法被验证**。计划文本已按此更正。
>
> 另：`Set-Content -Encoding UTF8` 在 Windows PowerShell 5.1 会写入 **BOM**，而配置加载器拒绝 BOM（`invalid character 'ï' looking for beginning of value`），gateway 与 apiserve **行为一致**地拒绝。这与本方案无关，但同一次实测中先踩到，故记录：写 `config.json` 必须无 BOM。

## 检查结果

| # | 检查项 | 命令 | 实际输出 | 结论 |
|---|---|---|---|---|
| 0 | 两个二进制均可构建 | `go build -tags "goolm,stdjson" -o picoclaw.exe ./cmd/picoclaw` 等两条 | `exit=0` / `exit=0` | PASS |
| 1 | api 健康 | `Invoke-WebRequest http://127.0.0.1:18900/health` | `200` | PASS |
| 2 | api 就绪 | `Invoke-WebRequest http://127.0.0.1:18900/ready` | `200` | PASS |
| 3 | gateway 健康 | `Invoke-WebRequest http://127.0.0.1:18790/health` | `200` `{"status":"ok","uptime":"14.9265737s","pid":27700}` | PASS |
| 4 | gateway 就绪 | `Invoke-WebRequest http://127.0.0.1:18790/ready` | `200` | PASS |
| 5 | **PID 不冲突** | `Get-ChildItem $PICOCLAW_HOME -Force` | home 下只有 gateway 写的 `.picoclaw.pid`，**接入层没有新增任何 PID 文件** | PASS |
| 6 | PID 内容正确 | `Get-Content $PICOCLAW_HOME/.picoclaw.pid` | `{ "pid": 27700, "token": "...", "version": "dev", "port": 18790, "host": "localhost" }`，与 gateway 实际 pid 27700 一致 | PASS |
| 7 | **DB 落在指定 home 且 WAL 生效** | `Get-ChildItem $PICOCLAW_HOME -Filter 'apiserve.db*'` | 运行期为 `apiserve.db \| apiserve.db-shm \| apiserve.db-wal`。`-wal`/`-shm` 只会由 WAL 模式创建，且本次为全新临时目录，故为 WAL 生效的**强证据**（严格意义上的直接证据是 `PRAGMA journal_mode`，见 `pkg/apiserve/store.go` 的 pragma 断言测试） | PASS |
| 8 | **没有写到默认 `~/.picoclaw`** | `Test-Path "$env:USERPROFILE\.picoclaw"` 与 `Get-ChildItem <默认 home> -Filter 'apiserve.db*' -Recurse` | 默认 home `C:\Users\admin\.picoclaw` **完全不存在**；因此其中不可能有 `apiserve.db`（隔离 home 下则为 1 个） | PASS |
| 9 | **CLI 可在两个服务运行时建 key** | `picoclaw-api keys create --home $PICOCLAW_HOME --name smoke-test` | 成功返回 `id` / `name` / `api_key` / 一次性提示，**无 `SQLITE_BUSY`** | PASS |
| 10 | 无鉴权被拒 | `POST /v1/messages`（无 Authorization） | `401` `{"error":{"code":"unauthorized","message":"missing bearer token","request_id":"f8fc5d2468c2f895"}}` | PASS |
| 11 | 带鉴权打通全链路 | `POST /v1/messages`（Bearer） | `500` `{"error":{"code":"agent_error","message":"LLM call failed after retries: API request failed:\n Status: 401\n Body: {\"error\":{\"message\":\"Incorrect API key provided: sk-dummy***********ance...` | PASS（见下） |
| 12 | agent 自发现 | `GET /v1/agents`（Bearer） | `200` `{"agents":["main"]}` | PASS |
| 13 | 用量端点 | `GET /v1/usage?group_by=agent`（Bearer） | `200` `{"group_by":"agent","rows":[]}` | PASS |
| 14 | 关停无残留 | `Stop-Process` 两个进程后复查两端口与进程表 | 两端口均 `down`；`picoclaw` / `picoclaw-api` 进程 `none` | PASS |

> 第 8 项在**首次记录时被漏掉**（brief Step 5 明确要求"同时确认没有写到默认 `~/.picoclaw`"），由 task reviewer 指出后补做。补做时两个进程已关停，因此在**残留状态**上验证——这恰好是更强的检查：若接入层曾写过默认 home，文件会留在那里。

## 未通过项与结论

**没有 FAIL 项。** 两点需要说明：

1. **第 11 项是"链路已通、模型调用未验证"**。响应里的 401 来自 OpenAI 对 dummy key 的拒绝，说明 鉴权 → 路由 → Agent 装配 → provider → **真实外部 LLM 调用** 整条链路都已执行；唯一没验证的是"模型正常返回"这一步，需要真实 key 或本地模型。这与 spec §9 的 M1 断言口径一致：M1 只要求链路与鉴权成立。第 13 项 `rows` 为空正是因为该轮在计量前就失败了——**本记录因此没有演示"一条落库的用量行"**，那需要真实模型。

2. **关停后 `apiserve.db-wal` / `apiserve.db-shm` 仍留在 home 下**，这是 SQLite 的正常行为（下次打开时检查点回收），不是泄漏。已确认没有额外的锁文件或临时文件残留。

## 使用红线（必须随本文档一起交付给接入方）

同一个 `conversation_id` 不要同时经 gateway 渠道与 API 访问：两者共享 `<workspace>/sessions/` 目录，
而 `ProcessDirect*` 路径没有会话并发守卫（`pkg/agent/turn_state.go:298` 的 `registerActiveTurn` 是无条件 `Store`；
`pkg/agent/turn_coord.go:31` 只是它的调用点），跨进程无任何保护。
选定一个入口后不要混用。

## 复现步骤

1. `$home = <一次性目录>`；写入上面的 `config.json`（**无 BOM**）
2. `go build -tags "goolm,stdjson" -o picoclaw.exe ./cmd/picoclaw`、`go build -tags "goolm,stdjson" -o picoclaw-api.exe ./cmd/picoclaw-api`
3. `PICOCLAW_HOME=$home ./picoclaw-api.exe --home $home`（后台）
4. `PICOCLAW_HOME=$home ./picoclaw.exe gateway`（后台）
5. 依次执行上表的第 1–12 项
6. `Stop-Process` 两个进程，复查端口与进程表（第 13 项）
