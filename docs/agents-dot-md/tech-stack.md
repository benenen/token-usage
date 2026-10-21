# 技术栈与实现规范

> 技术选型、数据访问约定、构建与测试命令；写代码前先看本文，按既有方式扩展。

系统结构见 `architecture.md`，两边不重复。

## 一、技术栈

| 类别 | 选型 | 备注 |
|---|---|---|
| 语言 | Go 1.23 | 纯 Go，**不开 CGO**（`make build`） |
| 数据库 | PostgreSQL（测试镜像 `postgres:15.13-alpine`） | 驱动 `github.com/jackc/pgx/v5`，连接池 `pgxpool` |
| 读 opencode SQLite | `modernc.org/sqlite` | 纯 Go；不能换成需要 CGO 的驱动 |
| CLI | `spf13/cobra` | server 的 `admin` 子命令、watcher 命令行 |
| 系统服务安装 | `kardianos/service` | watcher 装成 systemd / launchd / Windows 服务 |
| HTTP | 标准库 `net/http` + `ServeMux` | 不引入 Web 框架 |
| 看板 | 原生 JS + `//go:embed` | 无构建步骤、无前端框架 |
| 集成测试 | testcontainers-go | 自动起一次性 Postgres，**需要 Docker** |

## 二、数据访问约定

- SQL 直接写在 Go 常量里（`insertSQL`、`editInsertSQL` 等），用 `pgx` 执行；没有 ORM。
- 批量写入把整批记录序列化成 JSON 作为一个参数传进 SQL，在库里展开，一次往返完成。
- 表结构变更只改 `schema.go` 的 `const schema`，保持幂等；不另建迁移工具。

## 三、构建与验证命令

```bash
make build       # bin/token-usage-server + bin/token-usage-watcher
make test        # go test ./...（server 包会起 Postgres 容器，约 2 分钟）
make vet
go mod tidy

# 单个测试
go test ./internal/watcher/parsers/ -run TestAstronCodePatchApplyEndEmitsEdits -v
go test ./internal/server/ -run TestWebHandlerEmbedAndRoutes -v
```

## 四、测试分布

- `internal/watcher/parsers/*_test.go`：各解析器用内联 JSONL 固件测试（`writeFixture`），不依赖外部环境。新增解析器照此加固件测试。
- `internal/watcher/uploader_test.go`：上传与离线 spool。
- `internal/server/*_test.go`、`cmd/server/admin_test.go`：经 `internal/testpostgres` 起一次性 Postgres 容器。`testpostgres` 故意不接受外部 DSN，测试不会误清开发或生产库。
- 本机已有真实会话数据时，可以写一个临时测试对真实目录跑一遍解析器做对账，跑完删掉，不要提交。

## 五、看板

- `internal/server/web/static/app.js`：`el(tag, attrs, children)` / `clear(node)` / `textContent` 构造 DOM（禁止 `innerHTML`，见 AGENTS.md）。
- 配色表：`MODEL_PALETTE`（模型）、`TOOL_COLORS`（工具）、`LANG_COLORS`（语言），未命中的走 fallback 调色板。
