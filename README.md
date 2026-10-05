# orb-scheduler-core

把工作负载的资源请求、可用节点容量、放置约束与调度优先级记录成可查询的服务，支持按节点、队列和标签试算放置结果并追溯每一次调度决定。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `orb-scheduler-core.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /v1/placements`

提交一次放置试算。请求体：

- `namespace`、`name`、`queue`：非空且无首尾空白的字符串。
- `priority`：32 位有符号整数。
- `resources`：`cpu`（毫核）与 `memory`（MiB），均为正的 64 位有符号整数。
- `nodes`：数组，可为空；每项含非空无首尾空白的 `name`、非负 64 位整数 `cpu`、`memory`，以及可选的字符串键值对象 `labels`（省略视为 `{}`）；节点名必须唯一。
- `selector`：可选，字符串键值对象，省略视为 `{}`。

服务在**标签包含 selector 全部键值且容量足够**（节点 `cpu`/`memory` 均不小于请求）的节点中，按节点名 UTF-8 字节序取最小者；试算不扣减容量。命中返回 `status="placed"`、`node` 为节点名、`reason` 为 `null`；否则 `status="rejected"`、`node` 为 `null`、`reason` 为 `"no_eligible_node"`。

记录以 `(namespace, name)` 标识且不可修改：

- 首次提交：**201**，返回完整记录（保留全部输入字段及 `status`、`node`、`reason`）。
- 相同内容重复提交：**200**，返回原记录。比较时会补齐省略的 `selector`/`labels` 默认值（`{}`），忽略对象成员顺序，但保留数组顺序。
- 不同内容重复提交：**409** `PlacementConflictError`，原记录不变。
- 非法 JSON、尾随数据、未知/重复字段、缺必填字段、类型或范围错误、节点名重复：**400** `InvalidPlacementInputError`，不保存记录。

请求示例：

```json
{
  "namespace": "team-a",
  "name": "job-1",
  "queue": "default",
  "priority": 10,
  "resources": {"cpu": 500, "memory": 256},
  "selector": {"zone": "cn"},
  "nodes": [
    {"name": "node-a", "cpu": 1000, "memory": 512, "labels": {"zone": "cn"}}
  ]
}
```

### `GET /v1/placements`

两种形式：

- 带 `name` 时必须且只能同时带 `namespace`：命中返回 **200** 与单条记录；不存在返回 **404** `PlacementNotFoundError`。`name` 与 `queue`/`node` 组合、缺少 `namespace` 均为 **400**。
- 不带 `name` 时，可用 `namespace`、`queue`、`node` 任意组合做交集筛选（`node` 只可能命中已放置的记录）；无条件返回全部。响应为 `{"items":[...]}`，按 `namespace` 再 `name` 的 UTF-8 字节序排序，无匹配时为空数组。

未知参数、重复参数、空值参数或畸形查询串均返回 **400** `InvalidPlacementInputError`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

| HTTP | code | 场景 |
|---|---|---|
| 400 | `InvalidPlacementInputError` | 请求体或查询参数不合法 |
| 404 | `PlacementNotFoundError` | 指定的 `(namespace, name)` 不存在 |
| 409 | `PlacementConflictError` | 同一标识已存在但内容不同 |
| 503 | `storage_unavailable` | 存储不可用 |

