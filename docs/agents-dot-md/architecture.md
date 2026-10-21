# 系统架构

> 组件边界、关键链路、数据流与设计决策；动结构、加模块、接新的 CLI 来源前先看本文。

本文回答「这个系统由哪些部分组成、数据怎么流过它们、为什么这么切」。技术选型和验证命令在 `tech-stack.md`；改动时必须守住的约束在顶层 `AGENTS.md`《硬性约束》，这里不重复。

## 一、整体结构

```
各 AI CLI 的会话记录                    watcher（每台机器一个）                server
~/.claude/projects/**.jsonl ─┐
~/.codex/sessions/**.jsonl  ─┤   Scanner ─> parsers.For(tool)          POST /ingest (Bearer tuk_…)
~/.acode/sessions/**.jsonl  ─┼─> checkpoint ─> Uploader ──批量 HTTP──>  ResolveAPIKey ─> Store.Insert
~/.pi/agent/sessions/**     ─┤              └─> --buffer 离线目录          │  (insertSQL / editInsertSQL)
opencode.db (SQLite)        ─┘                                            ▼
                                                    Postgres: usage_detail/daily, edit_detail/daily,
                                                              users, api_keys, model_prices
                                                                          │
                                         看板 (embed web/) <── /summary 等读接口（读时计价）
```

- 两个二进制，都用 cobra：`cmd/server`（根命令起 HTTP 服务，`admin` 子命令在 `admin.go`）和 `cmd/watcher`（命令实现在 `cmd/watcher/cli/`）。
- server 进程内还跑 `worker.go`：定时重建当天 / 近 N 天的 daily 汇总做对账，并定时从 LiteLLM 同步 `model_prices`。

## 二、包与职责

| 包 | 职责 |
|---|---|
| `internal/types` | 两个二进制共用的线上传输类型。`IngestRequest` **没有** `user_id`；`UsageRecord` 每条带 `tool`。 |
| `internal/server/schema.go` | `const schema`，启动时幂等执行。 |
| `internal/server/store.go` | `*Store` 封装 `pgxpool`：`Insert`、`Aggregate` / `ClockAggregate`、`RebuildDaily`、`model_prices` 增删改查。`store_repair.go` 是 `admin codex-repair` 的重放逻辑。 |
| `internal/server/auth.go` | API key 生成（`tuk_` + 192 位 base64url）与 `ResolveAPIKey`，库里只存 sha256 hex。 |
| `internal/server/api.go` | HTTP handler 与 `Register(mux)`；只有 `/ingest` 需要鉴权。 |
| `internal/server/pricing.go` / `pricesync.go` | 内置价格表（按模型名前缀，最长前缀胜出）与 LiteLLM 同步。 |
| `internal/server/web.go` + `web/` | `//go:embed` 嵌入的看板。 |
| `internal/watcher/scanner.go` | 增量遍历来源；`Source.Root` 可以是目录或单个文件，先 stat 再分派。 |
| `internal/watcher/parsers/` | 每种工具一个解析器，按 `Source.Tool` 选择，`init()` 里 `register`。 |
| `internal/watcher/uploader.go` | 批量 POST、Bearer 鉴权、离线 spool 目录。 |
| `internal/watcher/checkpoint.go` | 原子写（tmp + rename）的 JSON 进度文件。 |
| `internal/watcher/defaults.go` | `KnownToolDefaults`：不传 `--source` 时自动探测的各工具路径。 |

## 三、解析器类型

| 工具 | 文件 | 读取方式 |
|---|---|---|
| claude-code | `claudecode.go` | JSONL 只追加，按 offset 续读 |
| pi | `pi.go` | 同上 |
| codex / astron-code | `codex.go`（共用） | 文件大小或 inode 变化就整份重读，服务端主键去重；用量取 `total_token_usage` 的累计差值 |
| opencode | `opencode.go` | 单个 SQLite 库，不遍历目录 |

`Scan` 返回 `ScanResult{Usage, Edits}`；`Edits`（文件编辑事件，带语言和增删行数，见 `lang.go` 的 `langFromPath` / `diffLineCounts`）可选，只统计 token 的解析器留空即可。

## 四、关键链路：一条用量记录从产生到入库

1. CLI 往会话文件追加一行 → watcher 下一次 tick（默认 5s）`Scanner` 发现文件变化，调用对应解析器。
2. 解析出的记录按批交给 `Uploader`，POST `/ingest`；失败则写入 `--buffer`。
3. 服务端用 Bearer key 经 `ResolveAPIKey` 得到 `user_id`，`MachineID` 来自请求体（watcher 取主机名）。
4. `Store.Insert` 一条 SQL 完成明细去重插入 + daily 累加。
5. 本次扫描的所有批次都落地后，watcher 才推进 checkpoint；中途失败下次重发，服务端去重。

## 五、新增一个工具来源

解析器以下的流水线（`Scanner.Sources`、checkpoint、批量上传、鉴权、表里的 `tool` 列、看板配色）与格式无关，不用动。要加的是：

1. `internal/watcher/parsers/<tool>.go`：实现 `Parser`，`func init() { register("<tool>", parser{}) }`。格式与已有工具相同就直接复用已有解析器再注册一个名字（astron-code 复用 codex 就是这样）。
2. `internal/watcher/defaults.go` 的 `KnownToolDefaults` 加一条，路径可以是目录或单个文件。
3. `web/static/app.js`：`TOOL_COLORS` 加工具配色；模型名陌生时在 `MODEL_PALETTE` 加配色。
4. `README.md` 的支持工具列表同步更新。

CLI 已支持重复的 `--source tool=path`，用户自定义路径不需要改代码。

## 六、计价

- 费用在**读取时**计算（`/summary`），从不落库，所以改价格会追溯重算历史，不需要回填。
- 价格存在 `model_prices` 表，带 `valid_from` / `valid_to` 历史；`pricing.go` 的 `defaultRates` 只用作初始种子和库里还没有的新模型的兜底，按模型名前缀匹配、最长前缀胜出。
- worker 定期从 LiteLLM 的价格表同步（`pricesync.go`）；启动时可用 `--pricing rates.json` 覆盖。

## 七、设计取舍

- **daily 汇总放在写路径，而不是定时任务**：看板读 daily 表即可，不用扫明细；代价是表结构变化时 SQL 要同步改。worker 的重建只做兜底对账。
- **身份只在服务端解析**：watcher 只持有 API key，不能自报用户，避免冒充和配置漂移。
- **codex 类解析器整份重读**：会话文件小（约 100 KB），整份重读最简单，靠服务端主键去重保证幂等。
