# 受理事务与查询可见性说明

本文档在不改变任何生产逻辑、数据库结构与存储编码的前提下，沿 **POST /v1/placements** 的解析、受理、保存、响应路径，说明一条记录**何时成为可查询的已提交内容**，以及**写入回滚后 GET /v1/placements 能看到什么**。每条结论都标注实际源码位置（文件与函数）与对应验证用例；所有可见性结论都以同一个真实 SQLite 文件上的写入与独立读取为依据，不以内存替身推定。

README、生产源码、数据库格式、错误响应结构、`GET /healthz`、调度选择、默认值、交集筛选与排序均保持现状；对象共享边界（内存别名）见 `docs/immutability-boundary.md`，本文不重复，只聚焦事务可见性。本说明仅新增文档与测试。

## 1. 事务在受理路径上的实际位置

POST /v1/placements 一次请求经过四层，事务只存在于最末一层：

| 步骤 | 位置 | 与事务的关系 |
|---|---|---|
| 解析 | `internal/api/placement.go` `createPlacement` → `internal/placement/parse.go` `ParsePlacementInput` | 纯内存，不触库；失败即 400 `InvalidPlacementInputError`，没有任何事务被开启 |
| 受理（默认值+试算） | `internal/service/service.go` `Accept`：`NormalizeInput(p)` 后 `p.Status, p.Node, p.Reason = schedule(p)` | 纯内存，不触库 |
| 保存 | `internal/store/placement.go` `Submit` | **唯一的写事务**，见下 |
| 响应 | `createPlacement` 的 `c.JSON(201/200)` / `writeAPIError(409/503)` | 在事务结束之后才序列化 |

`Submit`（`internal/store/placement.go`）内事务的四个关键位置：

1. **事务开始**：`tx, err := s.db.BeginTx(ctx, nil)`（第 25 行）。此前 `model.CanonicalInput(p)` 已在事务外把请求半边序列化为独立字节。
2. **首次写入**：`tx.ExecContext(ctx, "INSERT INTO placements ...")`（第 31 行）。这是事务内唯一的写语句；从这一刻起该行只对**本事务所在连接**可见。
3. **提交**：`tx.Commit()`（第 36 行，仅 INSERT 成功时到达）。**只有 Commit 返回 nil 之后，该行才成为其他连接可查询的已提交内容**。随后 `out := *p; return &out, true, nil`。
4. **重复标识读取**：INSERT 报主键冲突（`isDuplicateIdentity`，仅凭驱动结构化错误码判定）时，`tx.QueryRowContext(ctx, "SELECT ... WHERE namespace = ? AND name = ?")`（第 46 行）在**同一事务内**读出**此前已提交**的原行，经 `decodeRecord` 重建返回，`created=false`。该分支没有任何写，函数返回时由第 29 行 `defer tx.Rollback()` 回滚这个只读事务——原行是上一次某次 `Submit` 提交的内容，不是本次写入的。

任何非重复错误（含编码失败之外的存储故障）都走 `return nil, false, fmt.Errorf(...)`，同样由 `defer tx.Rollback()` 收尾：**未提交即回滚，半个字也不会对其他连接可见**。

## 2. 业务结果、持久化结果、HTTP 响应的先后关系

以 `Accept`（`internal/service/service.go`）为轴：

- **Created（201）**：`Submit` 内部先 `Commit` 成功，才返回 `created=true`；`Accept` 再返回；`createPlacement` 最后 `c.JSON(201, stored)`。顺序是 **持久化提交 → 业务结果 → HTTP 响应**。因此客户端收到 201 时，记录必然已是已提交内容，后续任何 GET 都能读到。
- **Identical（200）/ Conflict（409）**：本次请求**没有产生任何写入**（INSERT 冲突后只读了已提交原行并回滚只读事务）。响应内容来自**此前某次已提交**的行；Conflict 时 `SameInput` 判定内容不同，原记录不被覆盖（`Accept` 的 `!same → Conflict` 分支）。
- **503 `storage_unavailable`**：`Submit` 出错即 `storageError(err)`（`ErrStorageUnavailable`），事务已回滚，**该次写入不存在于任何已提交视图中**。
- **400**：解析或查询参数校验失败，根本未触库。

GET /v1/placements 一侧（`internal/api/placement.go` `listPlacements` → `internal/service/query.go` `Query`）：

- 单条：`Query` 调 `Store.Get`（`internal/store/placement.go` 第 64 行），`sql.ErrNoRows` 映射为 `model.ErrNotFound` → `ErrPlacementNotFound` → **404 `PlacementNotFoundError`**。
- 列表：`Query` 调 `Store.List`（同文件第 82 行），单条 `SELECT ... ORDER BY namespace, name`；无匹配时 `Query` 把 nil 归一成非 nil 空切片（`query.go` 第 70–72 行），HTTP 渲染为字面量 **`{"items":[]}`**。
- `Get`/`List` 都不开显式事务：每条 SQL 语句在连接上自成一个隐式读事务，**读到的是该语句开始时全库的已提交状态**。

## 3. 三个观察时刻的可见性（同一 SQLite 文件，写入与独立读取）

实验结构：写入方持有一个**未提交**的写事务（INSERT 已执行），读取方是**独立打开同一文件**的另一组连接（生产形态的 `store.Open`，或挂在路由上的同一个 Store）。结论：

| 观察时刻 | 单条查询 GET ?namespace=&name= | 列表查询 GET /v1/placements |
|---|---|---|
| **提交前**（INSERT 已执行，未 Commit） | **404 `PlacementNotFoundError`**，与从未写入相同 | **200**，`items` 不含该记录；既有记录原样 |
| **提交后**（Commit 返回成功） | **200**，完整记录：原输入全部字段 + 调度结论（`status`/`node`/`reason`） | **200**，`items` 含该记录，仍按 namespace、name 字节序 |
| **回滚后**（INSERT 后 Rollback） | **404 `PlacementNotFoundError`**，与提交前完全相同 | **200**，`items` 仍不含该记录；既有记录逐字节不变 |

依据：SQLite 的已提交读（read-committed）语义——一个事务未提交的修改只对持有它的连接可见；回滚则使其从未存在。本仓库用例在同一真实数据库文件上验证了全部三行：`internal/store/visibility_test.go` `TestUncommittedInsertInvisibleToIndependentReader`（存储层三时刻）与 `internal/api/transaction_visibility_test.go` `TestVisibilityAcrossCommitAndRollbackOverHTTP`（HTTP 层三时刻，含既有记录逐字节不变的断言）。

无匹配列表始终为 `{"items":[]}`，由既有用例 `internal/api/immutability_test.go` `TestImmutHTTPQueryFilterSortEmptyMatrix`（字面量断言）与 `internal/service/query_test.go` `TestQueryListEmptyResultIsNonNilEmpty` 证明，本文不重复设用例。

## 4. 同一 Store 的连接等待 ≠ 独立连接的读取

`internal/store/store.go` `Open` 对**每个** Store 设置 `db.SetMaxOpenConns(1)`（第 30 行）与 `PRAGMA busy_timeout=5000`（第 31 行），并启用 WAL（第 23 行）。这产生两种必须区分的场景：

- **共享同一 Store**：全部读写竞争**唯一一条连接**。写事务（`Submit` 的 `BeginTx`…`Commit`）持有该连接期间，同一个 Store 上的 `Get`/`List` **不会与写并发执行**——它们在连接池里排队等待，直到事务结束或等待方上下文超时（`busy_timeout` 是 SQLite 锁等待上限，连接池等待由调用方 context 约束）。用例 `internal/store/visibility_test.go` `TestSameStoreReadWaitsForOpenWriteTransaction` 证明：写事务未结束时同一 Store 的读只会阻塞到 context 超时，既读不到未提交行，也读不到已提交行——因为它根本没执行。
- **分别打开同一文件**：两个 `store.Open`（或一个 Store 加一个裸连接）各有自己的连接。WAL 让读取方在写入方持有未提交事务时**仍能读**——读到的是**最后一个已提交**状态（§3 的"提交前"行）。WAL 字样只说明这种跨连接的读写不互锁，**不能据此推定同一 Store 内的读写会同时执行**——`SetMaxOpenConns(1)` 已经把它们串行化了。

**单次列表结果所依据的已提交视图**：`List` 的一条 SELECT 在其隐式读事务内看到一个一致的已提交快照（WAL 下即语句开始时的已提交状态）。但**每一次 HTTP 请求都是一次独立读取**：两个先后到达的 GET 不共享任何快照，若两者之间恰好有别的写入提交，两次结果可以不同。本文与用例都不把多个请求解释成共享一个快照。

## 5. 不以内存替身推定事务结论

`internal/service/fakestore` 是 map 加 JSON 克隆的进程内替身，没有事务、提交或回滚概念，其"写入立即可读"的行为对 SQLite 的可见性边界**不作任何证明**。本文全部可见性结论（§3、§4）只依据真实 SQLite 文件上的用例；替身用例（`accept_fake_test.go`、`query_fake_test.go`、`memory_router_test.go`）仅用于隔离业务流与注入故障，沿用既有分工（对照 `docs/immutability-boundary.md` §9）。

## 6. 保持不变的既有边界（引用既有用例，不重复验证）

- 首次受理 **201**、相同内容重试 **200**、不同内容 **409 `PlacementConflictError`** 且不覆盖原记录：`internal/api/immutability_test.go` `TestImmutHTTPAcceptedLifecycleMatrix`、`TestImmutHTTPRejectedLifecycleMatrix`；`internal/api/content_regression_test.go` 全文件。
- 非法输入 **400 `InvalidPlacementInputError`**：`TestImmutHTTPInvalidInputMatrix`、`internal/api/placement_test.go` `TestInvalidBodies`。
- 存储不可用 **503 `storage_unavailable`**（POST 与 GET 同构，消息不含 SQL/路径）：`TestImmutHTTPStorageUnavailableMatrix`、`internal/service/accept_test.go` `TestAcceptStorageUnavailable`、`internal/service/query_test.go` `TestQueryStorageUnavailable`。
- 单条不存在 **404 `PlacementNotFoundError`**：`TestImmutHTTPMissingIdentityMatrix`、`internal/api/placement_test.go` `TestGetMissingRecordReturnsNotFound`、`internal/service/query_test.go` `TestQuerySingleMissingIdentity`。
- 调度选择、默认值补齐、交集筛选、排序、`GET /healthz`：`internal/service/schedule_test.go`、`internal/model/input_test.go`、`TestImmutHTTPQueryFilterSortEmptyMatrix`、`internal/api/router_test.go` `TestHealthzReportsOK`。
- 跨 reopen 的持久化：`internal/api/placement_test.go` `TestRecordsSurviveReopen`、`internal/store/legacy_test.go`。

## 7. 结论 ↔ 源码 ↔ 用例索引

| 结论 | 源码依据 | 验证用例 |
|---|---|---|
| 事务开始/首次写入/提交/重复标识读取的位置 | `internal/store/placement.go` `Submit`：`BeginTx`（25）、`INSERT`（31）、`Commit`（36）、冲突分支 `SELECT`（46）、`defer tx.Rollback()`（29） | 本文 §1；`internal/store/duplicate_test.go` `TestSubmitDuplicateIdentityReadsOriginalAndChangesNothing` |
| 提交前独立读取不可见：单条 404、列表不含 | SQLite 已提交读语义 + `Get`/`List` 实现 | `internal/store/visibility_test.go` `TestUncommittedInsertInvisibleToIndependentReader`；`internal/api/transaction_visibility_test.go` `TestVisibilityAcrossCommitAndRollbackOverHTTP` |
| 提交后两种查询读到完整原输入与调度结论 | `Submit` 的 `Commit` → `decodeRecord` 重建 | 同上两个用例的"提交后"阶段 |
| 回滚后单条仍 404、列表仍不含、既有记录不变 | `defer tx.Rollback()`；SQLite 回滚语义 | 同上两个用例的"回滚后"阶段 |
| 201 ⇒ 已提交；503 ⇒ 未提交 | `Submit` 内 `Commit` 先于返回；`Accept` 的 `storageError`；`createPlacement` 的状态映射 | 提交顺序由 §2 源码结构给出；503 行为见 `TestImmutHTTPStorageUnavailableMatrix` |
| 同一 Store 内读写不并发，读等待连接 | `internal/store/store.go` `Open`：`SetMaxOpenConns(1)`、`busy_timeout=5000` | `internal/store/visibility_test.go` `TestSameStoreReadWaitsForOpenWriteTransaction` |
| 独立连接在 WAL 下读到最后已提交状态 | `Open` 的 `PRAGMA journal_mode=WAL`；两个独立 `Open` | `TestUncommittedInsertInvisibleToIndependentReader`（提交前可读既有行、不可见未提交行） |
| 单次列表 = 一条 SELECT 的已提交视图；多次请求不共享快照 | `internal/store/placement.go` `List`（单条 `SELECT ... ORDER BY`）；`Get`/`List` 均不开显式事务 | 源码结构结论；三时刻用例中每次 GET 独立发起 |
| 无匹配列表为字面量 `{"items":[]}` | `internal/service/query.go`（nil 归一）；`listPlacements` 渲染 | 既有 `TestImmutHTTPQueryFilterSortEmptyMatrix`、`TestQueryListEmptyResultIsNonNilEmpty` |
| 错误结构与消息边界不变 | `internal/api/placement.go` `writeAPIError` 及各状态映射 | 既有 `TestImmutHTTP*` 系列（§6） |

## 8. 未经验证的范围

以下范围本文不下结论，留待需要时单独验证：

- **多进程**同时打开同一文件的可见性与锁行为（用例均为单进程内多连接）。
- `busy_timeout=5000` **被耗尽**后的具体错误形态（用例只证明等待存在，未等待到 5 秒上限）。
- 写入方提交**恰好落在**读取方语句开始瞬间的竞态归属（属于 SQLite 快照时序，未构造该竞态）。
- 一个事务内**多条写**的中间可见性（`Submit` 每事务只有一条 INSERT，用例同样只覆盖单写事务）。
- 连接池等待的**上限与取消细节**（用例以 300ms context 超时证明"等待而非并发"，未标定更长的等待行为）。
