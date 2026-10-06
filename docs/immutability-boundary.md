# 放置记录不可修改边界说明

本文档在不改变任何生产逻辑的前提下，说明一条合法放置请求从**解析 → 默认值补齐 → 试算 → 保存 → 再次查询**全过程中的对象与数据边界：谁与谁共享内存、修改一个内存对象会影响谁、什么内容在什么时刻被固定。每条结论都标注实际源码位置（文件与函数）与对应验证用例。

README、生产源码、数据库格式、兼容别名、错误响应结构与 `GET /healthz` 均保持现状；本说明仅新增文档与测试。

## 1. 三个必须分开的观察面

讨论"改了一个东西，另一个东西变不变"时，存在三个不同的观察面，结论各不相同：

| 观察面 | 是什么 | 产生位置 |
|---|---|---|
| **A. HTTP 返回数据** | gin 序列化后写入响应的 JSON 字节，与任何 Go 对象再无关联 | `internal/api/placement.go` `createPlacement` 的 `c.JSON(...)`（201/200）、`listPlacements`（记录 / `{"items":...}`） |
| **B. 直接调用服务所得对象** | `service.Accept` / `service.Query` 返回的 `*model.Placement`，其与调用者输入对象是否别名，取决于存储后端 | `internal/service/service.go` `Accept`、`internal/service/query.go` `Query` |
| **C. SQLite 已提交内容** | `placements` 表的一行：`input_json`（canonical 字节）+ `status`/`node`/`reason` 列，只在首次 `INSERT` 时写入，之后路径上没有任何 UPDATE | `internal/store/store.go` `schema`、`internal/store/placement.go` `Submit` |

**核心结论先行：无论 B 面的内存别名如何，C 面在受理成功时即被固定，A 面是每次请求时重新序列化的快照。受理之后对任何内存对象的就地修改，都不会改变 C 面，也不会出现在后续任何一次 A 面响应中。**

## 2. 全链路追踪

以一个含 selector、两个候选节点（各带 labels）的合法请求为例。

1. **解析**（`internal/placement/parse.go` `ParsePlacementInput`）
   - 严格对象遍历 `readStrictObject`/`readObjectMembers` 把 JSON 成员读进 `map[string]json.RawMessage`，因此 JSON 文本里的**对象成员顺序在此即丢失**；数组则按 `dec.More()` 顺序 append（`parseNodes`），**数组顺序被保留**。
   - 函数在 `parse.go` 内 `p := &model.Placement{}` 自建对象，最后调用 `model.NormalizeInput(p)` 补齐省略的 selector/labels/nodes。HTTP 调用者提供的是 `[]byte`，没有 Go 对象可被修改。
2. **默认值补齐**（`internal/model/input.go` `NormalizeInput`）
   - 原地把 nil selector、nil 节点 labels 填成空 map，nil nodes 填空切片；只动 nil 字段，显式空值保持原样，不重排数组。
3. **试算**（`internal/service/service.go` `Accept` → `internal/service/schedule.go` `schedule`）
   - `Accept` 再次对入参 `p` 调 `NormalizeInput(p)`，随后 `p.Status, p.Node, p.Reason = schedule(p)`：**直接把调度结果写在调用者传入的对象上**。
   - `schedule` 只读 `p.Nodes`/`p.Selector`/`p.Resources`，不修改输入字段；选中时返回的是新分配指针 `&n`，拒绝时返回 `nil, &r`（`schedule.go` 中 `n := chosen` / `r := ReasonNoNode`）。因此结果指针与任何节点字符串都不别名。
4. **保存**（`internal/store/placement.go` `Submit`）
   - 先 `payload, _ := model.CanonicalInput(p)`：在**副本**上补齐并 `json.Marshal` 成独立字节串（map 键由 `encoding/json` 排序，数组顺序保留），随后 INSERT 绑定的是 `string(payload)`。从这一刻起 C 面固定，之后修改 `p` 无法触及已绑定/已写入的值。
   - INSERT 成功（首次）：`out := *p; return &out, true` —— 返回值是 `p` 的**浅拷贝**（详见 §4）。
   - 主键 `PRIMARY KEY (namespace, name)` 冲突（重试/冲突）：不再写入，转而 `SELECT ... ` 再经 `decodeRecord` → `model.DecodeInput` **重建一个全新对象**返回；`created=false`。业务层据此用 `SameInput(p, stored)` 区分 Identical 与 Conflict。
5. **再次查询**（`internal/store/placement.go` `Get`/`List` → `decodeRecord`）
   - 每次都从行数据重建：`model.DecodeInput` 产出新的 request 半边，`node`/`reason` 列经 `sql.NullString` 后取局部变量地址成为**新指针**。Query 返回的对象与历史上任何输入/返回对象都不共享内存。

## 3. 三种时序下，输入对象与返回对象的关系

设输入对象为调用者解析/构造的 `p`，返回对象为 `Accept` 的 `rec`。

| 时序 | SQLite（生产） | fakestore（内存替身） |
|---|---|---|
| **首次受理** Created | `rec != p`，但 `rec` 是 `out := *p` 浅拷贝：标量独立；**selector map、nodes 底层数组、labels map、Node/Reason 指针与 `p` 别名共享** | `rec` 是 `clone(p)` 的 JSON 深拷贝，与 `p` **完全独立** |
| **相同内容重试** Identical | 返回值来自冲突分支的 `decodeRecord`，即**从行重建的新对象**，与本次重试输入 `p2` 无别名，内容为原记录 | 返回 `clone(kept)`，与 `p2` 无别名，内容为原记录 |
| **内容冲突** Conflict | 同上：从行重建并原样返回；冲突输入不参与任何写入 | 返回 `clone(kept)`；冲突输入只被 `clone` 留存在调用记录里，不进入 `records` |

两后端在 B 面的**唯一差异**是首次受理返回值的浅/深拷贝；重试与冲突路径两后端都返回独立重建对象。C 面（再查内容）两后端完全一致。该差异不能互相推定：SQLite 行为依据是 `internal/store/placement.go` 的 `out := *p` 与 `decodeRecord`；替身行为依据是 `internal/service/fakestore/fakestore.go` 的 `clone`（JSON 往返深拷贝）。

## 4. 首次受理后，逐类字段就地修改输入对象的影响矩阵

"顺序调用完成后单独修改其中一项"——在 `Accept` 返回后修改调用者仍持有的输入对象 `p`：

| 修改项 | SQLite：返回对象 `rec` | fakestore：返回对象 `rec` | 两后端：再次查询（C 面） |
|---|---|---|---|
| 普通标量（`Priority`/`Resources`/`Namespace` 等） | **不变**（浅拷贝复制了值） | 不变 | 不变 |
| selector map 就地改键值（`p.Selector["zone"]=...`） | **跟着变**（同一 map） | 不变（深拷贝） | 不变 |
| 候选节点**元素就地改**（`p.Nodes[0].Name=...`） | **跟着变**（共享底层数组） | 不变 | 不变 |
| 候选节点**换头**（`p.Nodes = []Node{...}`） | 不变（`rec` 保留自己的 slice header） | 不变 | 不变 |
| 节点 labels map 就地改 | **跟着变**（同一 map） | 不变 | 不变 |
| 调度结果指针换头（`p.Node = &other`） | 不变（`rec` 保留自己的指针副本） | 不变 | 不变 |
| 调度结果指针目标就地改（`*p.Node = "x"`，placed 才有） | **跟着变**（浅拷贝复制了同一指针） | 不变（深拷贝产生新指针） | 不变 |
| 拒绝记录的 `*p.Reason = ...` | **跟着变**（同上，rejected 的 Reason 指针共享，Node 为 nil 无指针可共享） | 不变 | 不变（仍 `no_eligible_node`） |

解读：SQLite 首次返回值的别名只影响**调用者手里这两个 Go 对象彼此的观感（B 面）**。它不构成"记录可被修改"——C 面在第 4 步 INSERT 前已由 `CanonicalInput` 序列化为独立字节并绑定写入；因此任何一次 Get/List/HTTP GET（以及 reopen 数据库后）读到的都是受理时的原内容。若调用者需要首次返回值也完全独立，应在调用前/后自行深拷贝；业务并不依赖该别名。

## 5. 重试与冲突之后修改输入对象

- **相同内容重试**：`rec2` 在两后端都是从已存记录重建/克隆的独立对象。修改重试输入 `p2`（标量、map、数组、指针任意一种）：`rec2` 不变，再次查询不变，记录数始终为 1。
- **内容冲突**：返回的是**未被触碰的原记录**；修改被拒绝的冲突输入对象不影响该返回值，不影响再查，也不新增行。即使冲突内容"本可被调度到不同节点"，调度结果也绝不参与内容比较（`SameInput` 的 `inputPayload` 只含 request 半边，`Status/Node/Reason` 被排除）。
- **拒绝放置记录同样适用**：相同内容（含省略 ↔ 显式 `{}` 默认值差异）重试得原拒绝记录；更小、本可放置的同标识请求得 409，原 rejected/node=null/reason 不变。

## 6. 查询返回对象修改后再查

`Query` 单条与列表两条路径分别走 `Get`/`List`，在 SQLite 上每次 `decodeRecord` 重建、在 fakestore 上每次 `clone`：返回对象（含其 selector/labels map、nodes 元素、Node/Reason 指针）都是**一次性副本**。就地修改它——包括改 map、改数组元素、改调度指针目标——不会回写存储，也不会影响下一次查询或任何 HTTP 响应。列表无匹配时，`Query` 还会把 nil 归一成非 nil 空切片（`internal/service/query.go`），HTTP 序列化为字面量 `{"items":[]}`。

## 7. 默认值补齐与内容比较的副作用

- **`NormalizeInput` 是原地的**（`internal/model/input.go`）：
  - 经 HTTP：它作用于 parser 自建对象，HTTP 调用者无对象可被改。
  - 经直接服务入口：`Accept` 对调用者传入的 `p` 调用它（`internal/service/service.go`），**会修改调用者对象**——调用后 `p.Selector` 从 nil 变为非 nil 空 map，省略的 labels/nodes 同理。此外 `schedule` 的结果也直接写在 `p` 上。
- **`SameInput`/`CanonicalInput` 不产生该副作用**：`CanonicalInput` 先 `cp := *p`（并单独深拷 Nodes 切片）再在 `cp` 上 normalize、marshal；`Submit` 的序列化与受理后的内容比较都走它，因此**比较与存储序列化不修改调用者对象**。对仍带 nil 字段的记录调用 `SameInput` 比较后，nil 依旧是 nil。

## 8. 节点数组顺序 与 对象成员顺序 的不同影响

- **对象成员顺序与转义拼写无关内容**：成员经 map 解析，canonical 编码由 `encoding/json` 对 map 键排序、结构体字段顺序固定。顶层/resources/selector/labels 成员重排、`c` 这类转义、省略 ↔ 显式 `{}`，都产生**相同 canonical 字节 → 重试 200，返回原记录**。
- **节点数组顺序就是内容**：JSON 数组顺序保真地进入 `[]Node` 并在 canonical 字节中保留；交换两个节点（哪怕两次试算都选中同名节点、调度结果完全相同）产生**不同 canonical 字节 → 409**，原数组顺序不变。

## 9. SQLite 与内存替身的源码依据（如实对照）

| 事项 | SQLite（`internal/store`） | fakestore（`internal/service/fakestore`） |
|---|---|---|
| 首次写入固定内容 | INSERT 绑定 `model.CanonicalInput(p)` 的独立字节（`Submit`） | `records[id] = clone(p)`（JSON 深拷贝，`Submit`） |
| 首次返回值 | `out := *p` 浅拷贝，引用字段与输入别名 | `clone(p)` 深拷贝，全独立 |
| 重复标识返回 | SELECT 后 `decodeRecord` 重建 | `clone(existing)` |
| 单条/列表读取 | `Get`/`List` → `decodeRecord` 每次重建 | `Get`/`List` 每次 `clone` |
| 标识唯一 | 表主键 `PRIMARY KEY (namespace, name)` 保证 | map key `identity{namespace,name}` 保证 |
| 排序 | SQL `ORDER BY namespace, name`（BINARY≈UTF-8 字节序） | `sort.Slice` 同序比较 |

两者在"已提交内容不可变、重试返原记录、冲突不覆盖、读取均为副本"上结果相同；仅"首次受理返回值与输入的内存别名"不同（§4），且该差异不波及任何持久化或跨调用行为。**替身用于隔离业务流（无 SQLite 驱动、可注入故障、记录调用），数据库行为一律以 SQLite 用例为准，从不以替身结果推定。**

## 10. HTTP 状态与错误码边界（本套用例确定预期）

- 首次提交合法请求（含被试算拒绝的）：**201**，响应含完整记录；rejected 时 `node=null`、`reason="no_eligible_node"`，省略的 selector/labels 渲染为 `{}`。
- 相同内容重试（默认值差异/成员重排/转义不计）：**200**，响应体与首次**逐字节一致**。
- 内容冲突（含仅数组换序、本可改判的 rejected 重提）：**409** `PlacementConflictError`，原记录与列表不变。
- 非法输入（坏 JSON、尾随数据、未知/重复字段、缺必填、类型/范围错、节点重名等）：**400** `InvalidPlacementInputError`，不落任何记录。
- 单条查询标识不存在：**404** `PlacementNotFoundError`。
- 存储不可用（关库后 POST/GET）：**503** `storage_unavailable`，message 不含 SQL/路径。
- GET 列表：namespace/queue/node 交集筛选；node 只匹配已放置记录；按 namespace 再 name 的 UTF-8 字节序；无匹配返回字面量空数组 `{"items":[]}`。

## 11. 结论 ↔ 源码 ↔ 用例索引

| 结论 | 源码依据 | 验证用例 |
|---|---|---|
| 首次返回值为浅拷贝、引用字段别名（SQLite） | `internal/store/placement.go` `Submit`（`out := *p`） | `internal/service/immutability_test.go` `TestImmutSQLiteCreatedReturnShallowCopiesCaller`、`TestImmutSQLiteCreatedRejectedReasonPointerAliases` |
| 首次返回值为深拷贝（fakestore） | `internal/service/fakestore/fakestore.go` `Submit`/`clone` | `TestImmutFakeCreatedReturnIsDeepCopy` |
| 五类字段就地修改不改变再查内容（双后端） | `CanonicalInput` 序列化固定 + `decodeRecord`/`clone` 重建 | `TestImmutSQLiteCreatedReturnShallowCopiesCaller`、`TestImmutQueriedObjectsAreDisposableCopiesOnBothBackends` |
| 相同重试 200/Identical 返原记录，改重试输入无效 | `Accept` 冲突分支 + `SameInput`；`Submit` 冲突分支 | `TestImmutIdenticalRetryReturnsStoredOriginalOnBothBackends`；API：`TestImmutHTTPAcceptedLifecycleMatrix`、`TestImmutHTTPRejectedLifecycleMatrix` |
| 冲突 409/Conflict 不覆盖，含 rejected 与换序 | `SameInput`（仅 request 半边）、主键约束 | `TestImmutConflictLeavesOriginalAndReturnIndependentOnBothBackends`；API：同上两个 lifecycle 用例 |
| 查询对象是一次性副本 | `decodeRecord` / `clone` | `TestImmutQueriedObjectsAreDisposableCopiesOnBothBackends`；API：`TestImmutDirectObjectMutationsInvisibleOverHTTP` |
| Accept 的默认值补齐与试算会改调用者对象；比较不会 | `internal/service/service.go` `Accept`；`internal/model/input.go` `CanonicalInput`（`cp := *p`） | `TestImmutNormalizeFillsCallerButComparisonLeavesNil`（既有 `internal/model/input_test.go` `TestSameInputRules` 亦证比较不改参数） |
| 成员顺序无关、数组顺序是内容 | `internal/placement/parse.go`（map 解析/数组保序）、`CanonicalInput`（map 键排序） | `TestImmutMemberOrderVersusArrayOrderOnBothBackends`；API：`TestImmutHTTPAcceptedLifecycleMatrix` |
| HTTP 201/200/409/400/404/503 与错误结构 | `internal/api/placement.go`、`internal/api/router.go` | `internal/api/immutability_test.go` 各 `TestImmutHTTP*Matrix` |
| 列表筛选/排序/空数组/拒绝记录不被 node 命中 | `internal/service/query.go`、`internal/store/placement.go` `List` | `TestImmutHTTPQueryFilterSortEmptyMatrix`、`TestImmutHTTPRejectedLifecycleMatrix` |
| 跨 reopen 的持久化是数据库行为（非替身推定） | `internal/store/placement.go` + schema | 既有 `internal/api/placement_test.go` `TestRecordsSurviveReopen`、`internal/store/legacy_test.go` |
