# 代码 checklist（强制自检）

> 写完 / 改完代码必须逐条对照的硬性自检清单；新增条目直接追加本文件。

本文件由 `AGENTS.md`（即 `CLAUDE.md`）拆出，收敛「写代码时必须逐条自检」的硬性条目。顶层只留一条强制指针，条目细节都在这里；新增条目直接追加到本文件，不要写回 `AGENTS.md`。

**通用条目**是跨语言的硬红线；**语言专项**由 agents-dot-md skill 的 `scaffold.py` 按本仓库实际语言注入，是该语言公认的固定规范。落地时请按本项目实际情况增删改——不适用的直接删掉，别留着占位；踩过的坑按下面的格式追加。

## 怎么写一条 checklist

每条包含三部分，缺一不可——只写规则不给例子，下一个 agent 照样会踩：

1. **规则**：一句话说清「必须怎样 / 禁止怎样」。
2. **反例**：真实踩过的写法，标明后果（报什么错、线上出过什么事故）。
3. **正例**：照抄就对的写法。

条目来源优先级：**线上事故 > 评审反复提的意见 > 团队口头约定**。没踩过的坑不必预防性地写进来，这里只放硬性红线；软性建议归 `coding-guidelines.md`。

---

# 通用条目（跨语言）

## 1. 写库 SQL 必须幂等、可重放

watcher 会重发（整份重读的解析器每次都重发、失败重试也会重发），服务端靠主键 / `event_id` 去重。新增写入一律 `INSERT … ON CONFLICT`，汇总表只累加真正插入明细的行（见 AGENTS.md《硬性约束》第 1 条）。

- 反例：先 `SELECT` 判断是否存在再 `INSERT`，或直接把整批记录累加进 daily——重发一次就重复计数。
- 正例：照 `insertSQL` 的 CTE 写法，`INSERT … ON CONFLICT DO NOTHING RETURNING` 的结果再喂给 daily 的 upsert。

## 2. 禁止 1+N：循环里不查库、不调远程接口

循环体内逐条查询 / RPC / HTTP 调用，量一上来就是必然的超时事故。

- 反例：逐个 id 查一次库，`N` 条数据打 `N` 次请求。
- 正例：一次批量查询后在内存里按 key 分组；远程调用同理，优先批量接口。

## 3. 对外接口的返回结构 / 错误码 / 鉴权方式不得随意改动

已发布接口的返回体字段、错误码语义、鉴权方式是契约，前端和其他服务依赖它。确需变更时先确认调用方，或新增版本化接口并存，不要就地改语义。

- 线上传输类型：`internal/types`，watcher 与 server 共用；`/ingest` 开了 `DisallowUnknownFields`，加字段要先发服务端（见 `environment.md`）。
- 鉴权：只有 `/ingest` 需要 `Authorization: Bearer tuk_…`，身份由 `ResolveAPIKey` 解析，请求体不带 `user_id`。

## 4. 敏感信息不入库

密钥、密码、token、内网凭据一律不写进受版本控制的文件（含配置文件、测试用例、注释、提交信息）。走环境变量或配置中心；本地开发凭据放未入库文件（见 `environment.md`）。

提交前自检：`git diff` 里有没有出现形如密码 / `AK` / `SK` / 私钥的字面量。

## 5. 沿用既有技术选型，不引入第二套同类方案

新增三方依赖前先检查：根构建文件是否已有同类能力、能否复用项目内的公共封装。无充分理由不得并存两套 HTTP 客户端 / ORM / JSON 库 / 日志门面。

分层、命名、依赖注入方式跟随所在模块既有风格（详见 `tech-stack.md`），不做跨模块风格漂移。

## 6. 看板 DOM 只用 `el()` / `textContent`，禁止 `innerHTML`

- 反例：`node.innerHTML = `<td>${model}</td>``——模型名、项目路径都来自用户机器，可注入脚本；安全 hook 也会直接拦下这类改动。
- 正例：`node.appendChild(el("td", {}, [model]))`，清空用 `clear(node)`。

## 7. watcher 解析器不得因单行坏数据中断整个文件

会话文件可能写到一半、格式也会随 CLI 升级变化。单行 JSON 解析失败就跳过这一行，不要返回错误让整个文件卡住；字段形状可能变化时用 `json.RawMessage` 兼容新旧两种（参照 `codexToolCall.Output`）。

---

# Go 专项

## G1. 每个 error 都要处理，并保留错误链

返回的 `error` 要么就地处理，要么向上返回；包装错误使用 `%w`，让调用方可以用 `errors.Is` / `errors.As` 判断根因。不要用字符串匹配错误，也不要用 `_` 丢掉有意义的错误。

- 反例：`data, _ := io.ReadAll(r)`、`return fmt.Errorf("读取失败: %v", err)`
- 正例：`return fmt.Errorf("读取配置 %s: %w", path, err)`

## G2. Context 沿调用链传递，创建后及时 cancel

会阻塞、访问网络或数据库的函数把 `context.Context` 作为第一个参数，并把上游 context 继续传下去。`context.WithCancel` / `WithTimeout` 返回的 `cancel` 必须及时 `defer cancel()`，避免定时器和子任务泄漏。

- 反例：下层函数重新用 `context.Background()`，或创建 timeout 后丢掉 `cancel`。
- 正例：`func Load(ctx context.Context, id string) error`；派生超时时紧接着写 `defer cancel()`。

## G3. 每个 goroutine 都必须有明确的退出路径

启动 goroutine 前写清谁关闭输入、谁取消 context、谁等待退出。禁止只启动不回收；共享状态使用 channel、mutex 或原子操作保护，并用 `go test -race ./...` 检查关键并发路径。

- 反例：后台 `go func() { for { work() } }()`，没有取消条件。
- 正例：循环同时监听工作 channel 与 `ctx.Done()`，调用方用 `WaitGroup` 等待退出。

## G4. I/O 资源必须关闭，HTTP 响应先检查状态

文件、HTTP response body、数据库 rows 等资源在成功创建后立即安排关闭，并检查 `Close` / `rows.Err()` 等延迟错误。HTTP 客户端设置超时，读取 body 前先处理非 2xx 状态。

- 反例：`client.Get(...)` 后直接读取 body，不设超时、不关闭响应体。
- 正例：检查请求错误后立即 `defer resp.Body.Close()`，再校验状态码并读取受限大小的响应体。

## G5. 公共 API 避免暴露可变内部状态

slice、map、指针传递的是共享数据；返回或保存后可能被外部修改时要复制。并发读写 map 必须同步，结构体中包含 mutex 时禁止按值复制。

- 反例：getter 直接返回结构体内部的 map，调用方可以绕过锁修改它。
- 正例：锁内复制 map / slice 后返回副本；含 mutex 的结构体只传指针。

## G6. 接口由使用方定义，保持小而具体

接口放在消费它的包中，只声明调用方真正需要的方法；函数能接收具体类型时不预先抽象。返回具体类型，让调用方按需要定义接口。

- 反例：生产者包预先定义包含十几个方法的 `Service` 接口，所有调用方被迫依赖它。
- 正例：消费方按实际调用定义 `type UserFinder interface { Find(context.Context, string) (User, error) }`。

## G7. 提交前统一格式化并跑静态检查

改动过的 Go 文件必须经过 `gofmt`；按项目约定运行 `go test ./...`、`go vet ./...`，并对并发相关改动补跑 `go test -race ./...`。生成代码按项目命令重新生成，不手改生成产物。

- 反例：只运行单个包的测试就提交跨包改动，或手改 `*.pb.go`。
- 正例：先跑项目生成命令，再运行 `gofmt`、全量测试与项目约定的静态检查。

---

# JavaScript 专项

## S1. 一律 `===`，声明用 `const` / `let`

禁止 `==`（隐式类型转换）和 `var`（函数作用域 + 提升）。默认 `const`，需要重新赋值才用 `let`。

## S2. 不允许游离的 Promise

异步调用必须 `await`、`return`，或显式 `.catch(...)`。漏掉会变成 unhandled rejection——Node 18+ 默认直接让进程退出。

- 反例：`doAsync()` 单独一行
- 正例：`await doAsync()` / `void doAsync().catch(err => log.error(...))`

## S3. 循环里不串行 await（1+N 的 JS 版）

`for (const id of ids) { await fetchOne(id) }` 会把 N 次请求串起来。改用 `Promise.all(ids.map(...))`；量大时用并发上限池，别一次打爆下游。

## S4. 错误对象整体交给日志，不要只打 message

`console.error(err.message)` 丢掉堆栈和 cause。传整个 error 对象；跨层抛出时用 `new Error("上下文", { cause: err })` 保留链路。

## S5. 捕获要精确，不吞错

禁止空 `catch {}`。`try` 块只包住可能抛错的那几行，不要整个函数体裹一层。

## S6. 模块顶层不做副作用 I/O

顶层只做定义与导出；连接、读配置、发请求放进显式的初始化函数，避免 import 顺序决定运行结果。

## S7. 对外数据必须校验后再用

接口返回、`JSON.parse` 结果、用户输入在使用前先校验形状与必填字段，不要直接深层解构后透传到下游。
