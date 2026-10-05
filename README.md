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

提交一次放置试算。请求体为 JSON 对象：

| 字段 | 类型 | 说明 |
|---|---|---|
| `namespace` | string | 必填，非空且无首尾空白 |
| `name` | string | 必填，非空且无首尾空白 |
| `queue` | string | 必填，非空且无首尾空白 |
| `priority` | integer | 必填，32 位有符号整数 |
| `resources` | object | 必填，`cpu`（毫核）与 `memory`（MiB）均为正 64 位整数 |
| `selector` | object | 可选，字符串键值对，省略视为 `{}` |
| `nodes` | array | 必填，可为空数组；每项含 `name`（规则同上，且节点名唯一）、非负 64 位整数 `cpu`、`memory`，以及可选的字符串键值对象 `labels`（省略视为 `{}`） |

服务从 labels 覆盖 `selector` 全部键值且容量足够的节点中，按节点名 UTF-8 字节序取最小者；试算不扣减容量。记录保留全部输入及 `status`、`node`、`reason`：选中时为 `placed`、节点名、`null`，否则为 `rejected`、`null`、`no_eligible_node`。

记录以 `(namespace, name)` 标识且不可修改：首次提交返回 201 与记录；相同内容重交（补齐默认值、忽略对象成员顺序、保留数组顺序）返回 200 与原记录；不同内容返回 409 与 `PlacementConflictError`。并发重交只会存入一条记录。

### `GET /v1/placements`

- 带 `name` 时只能同时提供 `namespace`：命中返回 200 与单条记录，不存在返回 404 与 `PlacementNotFoundError`。
- 不带 `name` 时按 `namespace`、`queue`、`node` 的交集筛选，返回 200 与 `items` 数组，按 `namespace` 再 `name` 的 UTF-8 字节序排序；无筛选条件返回全部，无匹配返回空数组。

非法 JSON、尾随数据、未知字段、缺必填字段、类型或范围错误、重复节点名，以及未知、重复、空值或组合不符的查询参数，均返回 400 与 `InvalidPlacementInputError`，且不保存任何记录。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
