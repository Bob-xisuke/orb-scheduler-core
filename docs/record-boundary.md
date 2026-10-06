# 放置记录的不可修改边界：源码说明

本文说明 `POST /v1/placements`（受理）与 `GET /v1/placements`（查询）在**首次受理、相同内容重试、内容冲突**三种情形下，输入对象、返回对象与已提交记录之间的关系。所有结论都引用当前源码文件与函数，并由 `internal/service/accept_boundary_test.go`、`internal/service/accept_boundary_fake_test.go`、`internal/api/placement_boundary_test.go` 中的用例验证。本文不新增接口、不改变任何生产逻辑。

## 1. 三个层面的"记录"必须分开讨论

同一次请求经过三种形态，修改其中一个对其余两个的影响各不相同：

1. **HTTP 返回数据**：`internal/api/placement.go` 中 `createPlacement`/`listPlacements` 用 `c.JSON`（placement.go:55、57、89、92）把记录序列化成响应字节。字节离服务进程后即与服务端内存无关；客户端改写响应体不可能影响服务端。
2. **直接调用服务所得对象**：`service.Accept` / `service.Query` 返回的 `*model.Placement`。它与调用者输入对象之间是否共享内存，取决于存储实现的返回路径（见第 3、4 节），SQLite 与内存替身在此**有一处真实差异**。
3. **SQLite 已提交内容**：`internal/store/placement.go` 的 `Submit` 在事务里执行 `INSERT` 并 `Commit`（placement.go:28-37）。整张源码中**不存在任何 UPDATE 或 DELETE 语句**；身份重复时只 `SELECT` 已有行（placement.go:43-56）。这是"记录不可修改"在存储层的全部依据：一旦提交，任何后续调用——无论重试、冲突还是查询——都改不动已提交的字节。

## 2. 合法请求的完整路径

一次合法 POST 从解析到再查询经过（括号内为源码位置）：

1. **解析**：`placement.ParsePlacementInput`（internal/placement/parse.go:71）新建 `&model.Placement{}`（parse.go:88），逐字段校验后调用 `model.NormalizeInput(p)`（parse.go:116）补齐省略的 `selector`/`labels`。每次调用都返回全新对象，HTTP 层两次请求之间不共享任何输入内存。
2. **默认值补齐（第二次，幂等）**：`service.Accept` 再次调用 `model.NormalizeInput(p)`（internal/service/service.go:63）。直接调用服务（不经 HTTP/解析）的调用者对象也在此被补齐。
3. **试算**：`schedule(p)`（internal/service/schedule.go:13）把结果**写回调用者对象**：`p.Status, p.Node, p.Reason = schedule(p)`（service.go:64）。容量只探测不扣减；空候选数组与"无合格节点"一样得到 `rejected` + `no_eligible_node`。
4. **保存**：`s.st.Submit(ctx, p)`（service.go:66）。SQLite 实现先把请求半边编码为规范字节 `model.CanonicalInput(p)`（store/placement.go:17），再尝试 INSERT；身份已存在时读取原行并经 `decodeRecord`（store/placement.go:125）解码返回，`created=false`。
5. **内容比较（仅未插入时）**：`model.SameInput(p, stored)`（service.go:74）判定 `Identical` 还是 `Conflict`；比较只读，不产生副作用（见第 5 节）。
6. **再查询**：`service.Query`（internal/service/query.go:45）经 `Get`/`List` 取出记录；SQLite 每条记录都是 `decodeRecord` 现场解码的**新对象**（store/placement.go:61-75、79-123）。

## 3. 首次受理：输入对象与返回对象的关系

### 3.1 调用者对象被就地修改（两种存储一致）

`Accept` 在保存**之前**无条件执行 service.go:63-64，因此首次受理后，**调用者自己的输入对象**已被改写：nil 的 `Selector`/`Nodes`/`Labels` 被填成空容器（NormalizeInput，model/input.go:48-60），`Status`/`Node`/`Reason` 被写入试算结果。拒绝放置的记录同样如此：`Node` 保持 nil，`Reason` 指向 `"no_eligible_node"`。用例：`TestAcceptBoundaryCallerObjectIsNormalizedAndScheduledInPlace`（SQLite 与 fake 各一份）。

### 3.2 SQLite：Created 返回对象与输入对象共享可变状态

SQLite `Submit` 插入成功时返回 `out := *p; &out`（store/placement.go:36-37）——**结构体浅拷贝**。逐项观察（顺序调用完成后单独修改输入对象的一项，看返回对象）：

| 输入对象的成员 | 修改输入后，Created 返回对象是否跟着变 | 原因 |
|---|---|---|
| 普通字段（`Queue` 等 string/int） | **不变** | 浅拷贝按值复制了标量字段 |
| `Selector`（map） | **变** | 浅拷贝共享同一个 map |
| `Nodes`（数组元素，如 `Nodes[0].CPU`） | **变** | 浅拷贝共享同一个底层数组 |
| 节点 `Labels`（map） | **变** | 数组元素是结构体，但其中的 map 仍共享 |
| 调度结果指针（`*Node`、`*Reason`） | **变** | 浅拷贝复制的是指针本身，指向同一字符串 |

无论返回对象是否跟着变，**再次查询的记录都不变**：查询结果来自已提交字节的重新解码（第 1 节第 3 点）。用例：`TestAcceptBoundaryCreatedReturnSharesMutableStateWithInput`。

### 3.3 内存替身 fakestore：任何路径都不共享

`fakestore.Submit` 存入和返回都经过 `clone`（JSON 往返深拷贝，internal/service/fakestore/fakestore.go:87-104、159-169）。因此首次受理后修改输入对象的任何成员，Created 返回对象都**不变**。这是替身与 SQLite 的**真实差异**：3.2 表格中五行在替身下答案全为"不变"。用例：`TestAcceptBoundaryFakeCreatedReturnIsDetached`。

**不能由替身结果推定数据库行为**：若只跑 fake 用例，会得出"返回对象与输入永远隔离"的错误结论。两种存储只在以下层面结论相同——再次查询（`Get`/`List`/`Query`）得到的记录不受任何内存修改影响，因为替身同样只在首次保存克隆体、之后只读（fakestore.go:95-97）。

## 4. 相同内容重试与内容冲突：返回对象一律隔离

身份已存在时，SQLite `Submit` 走 SELECT 分支，`existing` 由 `decodeRecord` 从已提交字节**现场解码**（store/placement.go:43-56、125-140）：新的 map、新的数组、新的 `Node`/`Reason` 指针。因此：

- **相同重试（Identical）**：返回的是原记录的新解码副本。修改本次重试的输入对象（selector、nodes、labels、普通字段）不影响返回对象；修改返回对象也不影响再次查询。用例：`TestAcceptBoundaryIdenticalRetryReturnsDetachedOriginal`。
- **内容冲突（Conflict）**：返回的同样是这份新解码的原记录（service.go:78-80），冲突输入的内容不会进入返回值，更不会进入存储。注意冲突调用的**输入对象本身仍被 service.go:63-64 就地补齐并写入试算结果**——这是 Accept 的无条件前置步骤，与存储是否采纳无关；但已提交记录与再次查询结果保持原样。用例：`TestAcceptBoundaryConflictLeavesStoreAndReturnsUntouched`、`TestAcceptBoundaryRejectedRecordFollowsSameRules`（拒绝放置记录同样适用全部规则）。
- 首次受理的返回对象（3.2 的浅拷贝）与重试/冲突的返回对象（新解码）是**不同来源**的对象；同一身份在不同调用里拿到的返回对象互不共享内存。

fakestore 在重试/冲突路径返回 `clone(existing)`（fakestore.go:95-97），结论与 SQLite 相同：隔离。两条存储路径在"重试/冲突返回对象与输入隔离"上一致。

## 5. 默认值补齐有副作用，内容比较没有

- **`model.NormalizeInput`（model/input.go:48-60）就地修改**传入对象：只把 nil 的 `Selector`/`Nodes`/`Labels` 填成空容器，显式值（包括显式空对象）原样保留，且**不重排候选数组**。解析入口（parse.go:116）与 Accept（service.go:63）都调用它，所以无论从哪条路径进入，调用者对象都会被补齐。
- **`model.SameInput`（model/input.go:66-76）不修改任何一个操作数**。它委托 `CanonicalInput`（input.go:83-104），后者先 `cp := *p` 并复制 `Nodes` 数组，再在**副本**上调用 NormalizeInput；比较发生在副本的规范编码上。用例：`TestAcceptBoundarySameInputHasNoSideEffects`（另有 model 层既有用例 `TestSameInputRules` 的 "omitted defaults equal explicit empty" 子用例直接断言比较后 nil 字段仍为 nil）。

即：补齐的副作用只来自 NormalizeInput 这一条定义，内容比较本身不产生相同副作用。

## 6. 节点顺序与对象成员顺序对重试的不同影响

内容比较基于 `CanonicalInput` 的规范 JSON 编码（model/input.go:26-39 的 `inputPayload`，即存储格式本身）：

- **对象成员顺序无关**：`encoding/json` 对 map 键排序后编码，selector/labels/请求体成员的排列、转义拼写差异都不影响相等判定——重试仍得 200/Identical。源码依据：input.go:15-24 的规则注释与 input.go:99 的 `json.Marshal`。
- **节点数组顺序有关**：数组顺序在编码中原样保留（input.go:21-22），重排候选节点即构成不同内容——即使试算结果完全相同，重试也得 409/Conflict，且原记录不变。调度结果（`Status`/`Node`/`Reason`）不属于 `inputPayload`，从不参与比较（input.go:23-24）。

用例：`internal/api/placement_boundary_test.go` 的 `TestBoundaryTraceMemberOrderRetryVsNodeOrderConflict`（HTTP 层），以及既有回归 `TestComparisonIgnoresMemberOrderAndFillsDefaults`、`TestComparisonKeepsArrayOrder`。

## 7. 查询返回对象与已提交内容

- SQLite `Get`/`List` 每行都经 `decodeRecord` 新建对象（store/placement.go:61-123），修改查询返回对象的任何成员（包括 `*Node`、selector、nodes）不影响再次查询；fakestore 的 `Get`/`List` 返回 `clone`（fakestore.go:108-154），结论相同。用例：`TestAcceptBoundaryMutatingQueryResultDoesNotTouchStore`（SQLite 与 fake 各一份）。
- 查询语义保持现状：单条形式只接受 `namespace`+`name`（query.go:86-103 的 `validateQuery`）；列表形式按 `namespace`、`queue`、`node` 交集筛选，`node` 永不命中拒绝记录（其 `Node` 为 nil），结果按 `(namespace, name)` UTF-8 字节序排序，无匹配时 `Items` 为非 nil 空数组（query.go:70-73）。HTTP 层对应 `{"items":[...]}`（api/placement.go:92）。

## 8. 结论与验证用例索引

| 结论 | 源码依据 | 验证用例 |
|---|---|---|
| Accept 就地补齐默认值并写回试算结果（含冲突调用、含拒绝记录） | service.go:63-64；input.go:48-60；schedule.go:13-35 | `TestAcceptBoundaryCallerObjectIsNormalizedAndScheduledInPlace`（SQLite/fake） |
| SQLite 首次受理返回浅拷贝，与输入共享 map/数组/指针，普通字段不共享 | store/placement.go:36-37 | `TestAcceptBoundaryCreatedReturnSharesMutableStateWithInput` |
| fakestore 首次受理返回深拷贝，完全隔离（与 SQLite 的真实差异） | fakestore.go:87-104、159-169 | `TestAcceptBoundaryFakeCreatedReturnIsDetached` |
| 重试/冲突返回新解码的原记录，与本次输入隔离 | store/placement.go:43-56、125-140；service.go:74-81 | `TestAcceptBoundaryIdenticalRetryReturnsDetachedOriginal`、`TestAcceptBoundaryConflictLeavesStoreAndReturnsUntouched` |
| 任何内存修改都不影响再次查询；存储层无 UPDATE | store/placement.go 全文件（仅 INSERT/SELECT）；fakestore.go:95-97 | `TestAcceptBoundaryMutatingQueryResultDoesNotTouchStore`（SQLite/fake） |
| 补齐修改调用者对象，比较无副作用 | input.go:48-60 vs input.go:83-104 | `TestAcceptBoundarySameInputHasNoSideEffects`；model 层 `TestSameInputRules` |
| 成员顺序无关、节点顺序有关 | input.go:15-24、99 | `TestBoundaryTraceMemberOrderRetryVsNodeOrderConflict`（HTTP） |
| 201/200/409/400/404/503 与查询语义保持 | api/placement.go:36-93；api/router.go；query.go | `internal/api/placement_boundary_test.go` 全文件 |
