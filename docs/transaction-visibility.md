# 受理事务与查询可见性说明

本文档说明一次 `POST /v1/placements` 从**解析 → 受理 → 事务保存 → HTTP 响应**的路径上，记录在什么时刻成为"可查询的已提交内容"，以及写入回滚后 `GET /v1/placements` 的单条与列表查询各能看到什么。每条结论都标注实际源码位置（文件与函数）与最小验证用例。

README、生产逻辑、数据库结构、存储编码、`docs/immutability-boundary.md` 与全部既有 HTTP 约定（201/200/409/400/404/503、错误结构、`GET /healthz`、调度选择、默认值、交集筛选与排序）保持不变；本次仅新增本文档与 `internal/store/visibility_test.go`。

## 1. 先给结论

1. **可查询点只有一个：`tx.Commit()` 成功返回。** 在此之前，独立读取者无论走单条还是列表都看不到新标识；在此之后，两种查询都能读到完整的原输入（canonical 字节）与调度结论（`status`/`node`/`reason`）。
2. **回滚等于没写过。** 新标识继续 404 / 不在列表中；此前已提交的其他记录不受影响。
3. **共享同一个 `*store.Store` 的读取会等写事务结束，绝不读到中间态；分别 `Open` 同一文件的独立读取者不等待，但也只读到已提交状态。** WAL 只让"独立连接"并发；同一 Store 因 `SetMaxOpenConns(1)` 反而是串行的（§5）。
4. **单次列表查询依据的是该条 SQL 语句开始时的已提交视图**（语句级快照），多个请求不共享一个快照（§6）。
5. 以上是对**真实 SQLite 文件**的断言；内存替身 fakestore 没有事务、没有"未提交窗口"，不能用来推定这些结论（§7）。

## 2. 三个观察面（沿用不可修改边界文档的划分）

| 观察面 | 是什么 | 产生位置 |
|---|---|---|
| **A. HTTP 返回数据** | gin 序列化进响应体的 JSON 快照，与任何 Go 对象脱钩 | `internal/api/placement.go` `createPlacement` 的 `c.JSON(...)`（201/200）、`listPlacements`（记录 / `{"items":...}`） |
| **B. 业务结果（内存）** | `schedule` 在受理时算出的 `Status/Node/Reason`，写进调用者的入参对象 | `internal/service/schedule.go` `schedule`；调用点 `internal/service/service.go` `Accept` |
| **C. SQLite 已提交内容** | `placements` 表的一行：`input_json` + `status`/`node`/`reason` 列 | `internal/store/placement.go` `Submit`（`BeginTx`/`INSERT`/`Commit`），schema 见 `internal/store/store.go` |

本文档只讨论 **B 何时进入 C、C 何时对独立读取可见、A 相对 C 的先后**。

## 3. POST 全链路上的实际时序

以首次合法提交为例，严格按调用顺序：

| # | 位置 | 动作 | 可见性含义 |
|---|---|---|---|
| 1 | `internal/placement/parse.go` `ParsePlacementInput` | 严格解析请求体为 `model.Placement`；非法即返回 `ErrInvalidPlacementInput` | 失败不触库；handler 映射 **400**（`api/placement.go:42-46`） |
| 2 | `internal/service/service.go` `Accept` → `internal/model/input.go` `NormalizeInput` | 原地补省略默认值 | 只动内存 B |
| 3 | `Accept` → `internal/service/schedule.go` `schedule` | 试算，`p.Status, p.Node, p.Reason = schedule(p)` | **业务结果在内存中成立，早于任何 SQL**；只读输入字段，结果指针为新建局部量地址 |
| 4 | `internal/store/placement.go` `Submit` → `model.CanonicalInput(p)`（`:20`） | 在副本上把 request 半边序列化为 canonical 字节 | 独立字节串成形，但**尚未触库** |
| 5 | `Submit` `s.db.BeginTx(ctx, nil)`（`:25`） | **事务开始**；`defer tx.Rollback()`（`:29`）兜底 | 仅对持有 `tx` 的本 goroutine 存在 |
| 6 | `tx.ExecContext(... INSERT ...)`（`:31-34`） | **首次写入点**：绑定 canonical 字节与 status/node/reason | 只存在于该事务内；独立读取**不可见** |
| 7 | INSERT 成功分支 `tx.Commit()`（`:36-38`） | **提交：唯一的可查询点** | 返回成功后，行进入已提交数据库状态，Get/List 立即可见 |
| 8 | `Submit` 返回 `out := *p; &out, true`（`:39-40`） | `created=true`；返回值是入参浅拷贝（别名细节见不可修改边界文档 §3-4） | C 面与该浅拷贝无关，已在第 6 步前固定 |
| 9 | `Accept` 收到 `created` → 返回 `Created`（`service.go:70-72`） | 业务判定 | — |
| 10 | `createPlacement` switch → `c.JSON(201, stored)`（`api/placement.go:54-55`） | **HTTP 响应最后发出** | 客户端收到 201 时，Commit（第 7 步）必然已经成功——响应在同步调用链的更下游 |

**先后关系一句话：业务结果（B，第 3 步）→ 首次写入（事务内，第 6 步）→ 提交（可查询，第 7 步）→ 业务 Created 判定（第 9 步）→ HTTP 201 响应（第 10 步）。**

### 3.1 重试与冲突路径上的读取位置

INSERT 报主键/唯一冲突后，`Submit` **不提交、不显式回滚**（由 `defer tx.Rollback()` 结束事务），转而在**同一写事务内**执行重复标识读取：

```go
row := tx.QueryRowContext(ctx,
    "SELECT input_json, status, node, reason FROM placements WHERE namespace = ? AND name = ?", ...) // placement.go:46-49
```

- 该 SELECT 读到的是**此前已提交**的原行（本次 INSERT 已失败，未写入任何东西），经 `decodeRecord` → `model.DecodeInput` 重建全新对象，`created=false` 返回（`:50-59`）。
- `Accept` 随后用 `model.SameInput(p, stored)`（`service.go:74`）比较 request 半边：相同 → **Identical → 200**（本次无新提交，响应体来自此前已提交行）；不同 → **Conflict → 409**，`stored` 是未被触碰的原记录。
- 冲突分类只认驱动结构化结果码（`isDuplicateIdentity`，`placement.go:155-165`），不读错误文本；NOT NULL/CHECK 等非唯一冲突落到存储失败 → `ErrStorageUnavailable` → **503**。
- 因此 **200 与 409 的 HTTP 响应同样晚于（或不涉及）一次提交**：200 对应的行本来就是已提交内容；409 全程没有新提交。

## 4. GET 路径：单条与列表各读什么

`internal/api/placement.go` `listPlacements` 只做 `url.ParseQuery`（畸形串 → 400），其余在 `internal/service/query.go` `Query`：

- 参数先经 `validateQuery`（`:86-103`）：未知键、重复/空值、有 `name` 却缺 `namespace` 或同时带 `queue`/`node` → **400**，发生在触库之前。
- **单条**（有 `name`）：`store.Get` 的 `s.db.QueryRowContext(... WHERE namespace=? AND name=?)`（`placement.go:64-68`）。无行 → `sql.ErrNoRows` 转 `model.ErrNotFound`（`:71-74`）→ `Query` 包成 `ErrPlacementNotFound`（`query.go:52-55`）→ handler **404 `PlacementNotFoundError``**（`api/placement.go:80-83`）。
- **列表**（无 `name`）：`store.List` 拼 `namespace`/`queue`/`node` 交集子句并 `ORDER BY namespace, name`，一条 `s.db.QueryContext`（`placement.go:97-103`）。`node` 条件只可能命中 placed 行（rejected 行该列为 NULL）。无匹配返回非 nil 空切片（`query.go:70-72`）→ HTTP **200 字面量 `{"items":[]}`**。
- 两条路径读到行后都走 `decodeRecord`（`placement.go:128-143`）：`DecodeInput` 重建 request 半边，再覆上 `status`/`node`/`reason` 列。**读取路径不调用 `schedule`，绝不重新试算、不修改任何记录。**

## 5. 三个观察时刻 × 两种查询 × 两种连接方式

设写路径已 `BeginTx` 并完成 INSERT、尚未决策；读取者分别用**同一个 `*store.Store`** 与**第二个 `Open(同一文件)`**。

| 观察时刻 | 单条 GET（独立 Store） | 列表 GET（独立 Store） | 共享同一个 Store 的读取 |
|---|---|---|---|
| **提交前** | 404 `PlacementNotFoundError`（`Get`→`ErrNotFound`） | 200 且 `items` 不含该标识 | 不取中间态：读要从同一个一连接池借连接，而该连接被写事务占用，读**阻塞等待**，直到 Commit/Rollback |
| **提交后** | 200，完整原输入 + 调度结论 | 200，`items` 含完整记录（按 namespace/name 序） | 阻塞结束后执行，读到的就是**已提交**状态（含刚提交行） |
| **回滚后** | 仍 404 | 仍不含；此前已提交记录原样保留，列表条数不变 | 阻塞结束后读到回滚前的已提交状态；新标识仍 `ErrNotFound` |

源码依据：

- 一连接池：`store.Open` 中 `db.SetMaxOpenConns(1)`（`store.go:30`），注释明示"Serialize access through one connection … keeps concurrent submissions free of busy errors"。`Submit` 的 `tx` 借走池中唯一连接直至 `Commit`/`Rollback`；同 Store 的 `Get`/`List` 在此期间向 `database/sql` 借不到第二个连接，只能排队。
- WAL：`db.Exec("PRAGMA journal_mode=WAL")`（`store.go:23`）允许**不同连接**的读者与写者并发；`PRAGMA busy_timeout=5000`（`store.go:31`）让偶发锁竞争等待 5s 而非立即报 busy。
- 因此**不能因为开启了 WAL 就推定同一 Store 内的读写会同时执行**：同池一连接把它们串行化了；真正并发的是"分别 `Open` 同一文件"的第二个池。反过来，独立读取者在 WAL 下也**不会**看到第二个池里尚未 Commit 的写入。

验证用例（均为真实 SQLite 文件，见 §8）：

- 提交前 404/排除 → 提交后 200/含完整内容：`TestIndependentReaderHidesUncommittedThenSeesCommit`
- 回滚后仍 404/排除，且既有记录不变：`TestIndependentReaderHidesRolledBackAndKeepsExisting`
- 共享 Store 的单条读在写事务打开期间阻塞，Commit 后读到已提交行：`TestSharedStoreGetWaitsForWriterConnectionThenReadsCommitted`
- 共享 Store 的单条读在 Rollback 后解除阻塞、仍看不到该行：`TestSharedStoreGetUnblocksOnRollbackWithoutSeeingRow`

## 6. 列表看到的是"语句开始时"的已提交视图

`List` 每次调用都是**一条** `QueryContext` 语句（`placement.go:103`）；SQLite 在该语句开始时确定其已提交数据视图，语句期间不受其他连接提交影响——但这不是跨请求的共享快照：

- 写事务打开期间连续两次 `List`，两次都看到不含新行的旧视图（结果同为空）；
- Commit 之后再发**下一条** `List` 语句，取到新视图，立即包含该行。

即"多个请求不共享一个快照"，证据是 `TestIndependentListStatementsTakeFreshCommittedView`：同一次打开的写事务两侧各跑一次空列表，提交后再跑一次得到 1 条。`Get` 的 `QueryRowContext` 同为语句级读，语义一致。文档只断言这一观察到的语句级行为，**不把它声称成某个可串行化隔离级别**（未验证范围见 §9）。

## 7. 为什么这些结论不能用内存替身证明

`internal/service/fakestore/fakestore.go` 的 `Submit` 在同一把互斥锁下直接 `s.records[id] = clone(p)`（map 赋值即"生效"），没有事务、没有未提交窗口、没有连接借用、也没有等待：

- 替身里"写进行中"的外部读要么因锁串行发生在写入前、要么发生在写入后，不存在提交前不可见窗口，更不存在读阻塞；
- 因此替身只能验证业务流（受理、幂等、冲突、筛选、排序），数据库可见性一律以真实文件用例为准。这与不可修改边界文档 §9 的分工一致：**替身隔离业务流，数据库行为不以替身推定。**

## 8. HTTP 响应与持久化的先后（对外保证）

`createPlacement` 与 `Accept`/`Submit` 在同一 goroutine 上同步调用：

- 客户端收到 **201** ⇒ 第 7 步 `Commit` 已成功 ⇒ 此后任意连接（同 Store 排队后、或独立打开文件）的 Get/List 必然读得到；
- **200**（Identical）⇒ 本次未写入，响应体来自此前已提交的原行，本来就可查询；
- **409**（Conflict）⇒ 本次 INSERT 失败、事务随 `defer` 回滚，已提交内容与列表均不变；
- **503**（`storage_unavailable`）⇒ `Commit` 未成功（或 INSERT/Scan/解码出错），`defer tx.Rollback()` 结束事务，新标识不可查询；message 不含 SQL、路径与堆栈（`api/placement.go:49-51`）；
- **400** ⇒ 解析或查询参数校验失败，发生在任何 SQL 之前。

读取侧的 200（单条对象或 `{"items":...}`）只表示语句执行成功，其内容是语句开始时的已提交视图，绝不包含任何调用方未提交的写入。

## 9. 已验证结论与未验证范围

**已由本次用例验证**（`internal/store/visibility_test.go`，真实文件、`go test -race` 通过）：

1. 提交前独立单条读 = `ErrNotFound`、独立列表不含；提交后两者都读到 canonical 原输入与调度结论；
2. 显式 `Rollback` 后同提交前，且既有已提交记录内容不变；
3. 同一 Store（单连接池）内读在写事务期间阻塞，并在 Commit/Rollback 后解除、只见到已提交状态；
4. 列表为语句级视图：连续语句各自取最新已提交视图，提交前后的多次调用结果按 §5 表格变化。

**明确未验证 / 不作断言**：

- 进程崩溃、断电时 WAL 帧的持久性与 checkpoint 行为；
- 跨**进程**并发（用例只覆盖同一进程内两个 `Open` 句柄）；高并发 HTTP 请求交错下的具体调度（既有 `TestConcurrentIdenticalSubmissionsStoreOneRecord` 只证明 1×201 + N-1×200 的计数）；
- SQLite/WAL 的正式隔离级别命名、锁升级与 autocheckpoint 边界——本文只陈述观察到的语句级可见性；
- `Submit` 存储失败路径（非冲突 INSERT 错、Scan/解码错）的回滚未做故障注入联测；其 503 外显由既有 `internal/api/placement_test.go` `TestStorageFailureReturns503` 覆盖，"失败不可见"由第 2 项的回滚语义与 `defer tx.Rollback()` 源码保证；
- 驱动版本相关的细枝行为（`modernc.org/sqlite v1.34.1`）。

## 10. 结论 ↔ 源码 ↔ 用例索引

| 结论 | 源码依据 | 验证用例 |
|---|---|---|
| 事务开始于受理保存时 | `internal/store/placement.go` `Submit` `BeginTx`（`:25`） | `beginPendingInsert`（visibility_test.go 辅助） |
| 首次写入绑定 canonical 字节与调度结论 | `Submit` 的 `tx.ExecContext(INSERT…)`（`:31-34`）；字节来自 `model.CanonicalInput`（`:20`） | `TestIndependentReaderHidesUncommittedThenSeesCommit`（提交后 `SameInput` 比对） |
| 可查询点 = Commit 成功 | `Submit` `tx.Commit()`（`:36-38`） | 同上；`TestSharedStoreGetWaitsForWriterConnectionThenReadsCommitted` |
| 提交前独立单条读 = 不存在 | `Get` `QueryRowContext` → `ErrNotFound`（`:64-77`） | `TestIndependentReaderHidesUncommittedThenSeesCommit`、`TestIndependentReaderHidesRolledBackAndKeepsExisting` |
| 提交前列表不含、空匹配为 `{"items":[]}` | `List` `QueryContext`（`:97-126`）；空切片归一在 `internal/service/query.go:70-72` | 同上两例 + `TestIndependentListStatementsTakeFreshCommittedView` |
| 提交后读到完整原输入与调度结论 | 行定义 `internal/store/store.go` `schema`；`decodeRecord`（`placement.go:128-143`） | 提交后 `Get`/`List` 两路断言 |
| 回滚后不可见、既有记录不变 | `defer tx.Rollback()`（`placement.go:29`）；无 UPDATE 的 schema | `TestIndependentReaderHidesRolledBackAndKeepsExisting`、`TestSharedStoreGetUnblocksOnRollbackWithoutSeeingRow` |
| 重复标识读取发生在写事务内、读已提交原行 | `tx.QueryRowContext(SELECT…)`（`placement.go:46-49`） | 既有 `internal/store/duplicate_test.go` `TestSubmitDuplicateIdentityReadsOriginalAndChangesNothing`、`…AfterReopen` |
| 共享 Store 读写串行（读等待写事务） | `store.go` `SetMaxOpenConns(1)`（`:30`） | `TestSharedStoreGetWaits…`、`TestSharedStoreGetUnblocks…`（-race 通过） |
| 独立连接并发来自 WAL，读者只见已提交 | `store.go` `PRAGMA journal_mode=WAL`（`:23`）、`busy_timeout`（`:31`） | `TestIndependentReader…` 两例（读者在写事务期间不阻塞地读到旧视图） |
| 列表是语句级视图、非跨请求快照 | `List` 每条 `QueryContext`（`placement.go:103`） | `TestIndependentListStatementsTakeFreshCommittedView` |
| 业务结果早于持久化、HTTP 晚于提交 | `service.go:64`（`schedule(p)`）；`api/placement.go:54-61`（switch 响应） | 既有 `internal/api/placement_test.go` `TestCreatePlacementSelectsSmallestEligibleNode`、`TestRepeatSubmissionIsIdempotent`、`TestDifferentContentConflictsAndIsNotStored` |
| 201/200/409/400/404/503 与错误结构不变 | `internal/api/placement.go`、`router.go` | 既有 `internal/api/*_test.go` 全套 + 本次新增用例 |
| 替身无事务窗口，不能推定 SQLite 结论 | `internal/service/fakestore/fakestore.go` `Submit`（map 即时赋值） | 本套用例全部使用 `store.Open(临时文件)`，不使用替身 |
