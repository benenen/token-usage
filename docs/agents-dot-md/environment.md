# 开发环境 / 推送 / 发布

> 凭据存放约定、Git 提交身份、发布顺序；部署和运维操作前先看本文。

## 凭据
API key、数据库 DSN、服务器账号**不入库**。DSN 走 `--dsn` 或 `$TOKENUSAGE_DSN`；watcher 的 API key 用 `admin` 命令生成（只显示一次，库里只存哈希）。本地凭据放未入库文件（如 `dev-env.local.md`，`.env` 已在 `.gitignore`）。

## Git
- 用仓库本地已配好的身份提交，见 AGENTS.md《Git 约定》。
- `bin/`、`dist/` 是构建产物，已忽略，不提交。

## 发布顺序
1. **先发服务端，再发 watcher。** `/ingest` 开了 `DisallowUnknownFields`，新版 watcher 多出的字段会被旧服务端拒收。
2. 表结构随服务端启动自动执行，无需单独迁移；改表前确认 `schema` 仍然幂等。
3. 只改了看板（`web/`）也要重新构建服务端，看板是嵌进二进制的。
4. 部署细节（服务端启动参数、watcher 安装成系统服务）见 `README.md` 与 `cmd/watcher/README.md`。
