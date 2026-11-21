---
type: index
status: Accepted
updated: 2026-11-21
---

# StuHelper Favorites

收藏模块在 PostgreSQL 的 `favorites` schema 中管理课程、教师与资料收藏、收藏夹、幂等回执、审计及收藏活动数据。本人收藏返回标题快照与目标标识。目标详情、评价、正文与文件由各自入口按当前权限提供。源码使用 AGPL-3.0-only。

## 依赖与公开接口

[模块清单](module.yaml) 声明 platform 和 catalog 为必需依赖，`favorites.manage` 另需要 identity 提供用户会话。可选依赖包括 resources、identity 和 verification。

新增收藏时，目标适配器调用 catalog 的内容准入，并按目标类型读取 catalog 或 resources。verification 提供准入所需的资格事实。

resources 缺失时，新增资料收藏和刷新资料收藏标题返回 503。本人列表、导出、移动、移除及收藏夹管理使用本模块数据，登录后按账号归属执行。课程引用接口供 reviews 从本人收藏中读取候选目标。

HTTP 接口见 [OpenAPI](contracts/favorites.openapi.json)，Go 与 TypeScript SDK 位于 [sdk](sdk)。业务规则见 [收藏规格](https://github.com/StuHelper/stuhelper/blob/develop/docs/spec/favorites.md)。活动的计数、投递和快照见 [热门活动来源](docs/spec/activity.md)。

## 收藏与收藏夹

同一账号、种类和规范目标只保存一条收藏，每条收藏属于一个收藏夹。`POST /api/v1/favorites/items` 重复添加时返回已有记录。若指定的收藏夹与已有记录不同，则返回 409。

`POST /api/v1/favorites/targets/set` 可按目标添加或移动收藏，省略收藏夹表示使用默认夹。`POST /api/v1/favorites/targets/remove` 按种类和目标 ID 幂等取消，目标未收藏时返回 `removed=false`。

个人中心可通过 `/items/{id}/move` 移动单条收藏，或用 `/items/batch-move` 和 `/items/batch-remove` 原子处理最多 100 条。普通收藏夹删除时，收藏原子移入默认夹并增加修订。默认上限为每账号 200 个收藏夹和 10000 条收藏，超过上限返回 409。

收藏夹名称去除首尾 Unicode 空白并做 NFC 归一化，允许 1 至 20 个 Unicode 码点，拒绝控制字符与不可见格式字符。名称区分大小写，同账号内唯一。

默认收藏夹固定置顶，可改名并受到删除保护。传入空白或空名称可重置默认夹的名称，客户端为未命名默认夹显示本地化标签。收藏夹的 `item_count` 和 `kind_counts` 统计夹内全部收藏。

`POST /api/v1/favorites/items/{id}/refresh` 按当前权限读取标题，变化时更新快照和修订。目标已下架或权限不足时返回原快照，提供方故障时返回 503。

## 查询与导出

列表每页最多 100 条，单值查询参数拒绝重复。收藏支持按收藏夹、种类、目标 ID 和标题筛选，按新增时间或标题排序。游标绑定账号、筛选和排序。收藏夹列表按默认标记、位置、创建时间及 ID 排序，普通读路径使用只读查询。

`GET /api/v1/favorites/items?kind=course&target_ids=course-a,course-b` 返回本人对应收藏。`target_ids` 须与 `kind` 一起提供，先按字面量逗号切分，再逐项 URL 解码。

每次最多 100 项，每项 1 至 200 个 Unicode 码点。空项、首尾空白或重复传入 `target_ids` 参数返回 400。重复 ID 只查询一次，并与其他筛选取交集。

`Service.CourseReferences` 接受 1 至 100 个目标 ID，返回本人收藏中匹配的课程目标 ID，结果有序且去重。

`GET /api/v1/favorites/export` 提供本人收藏夹和收藏快照。每页最多返回 `limit` 个收藏夹和 `limit` 条收藏，默认 200，上限 500。收藏项附带收藏夹名称、规范目标 ID 及路径，可按收藏夹和种类筛选。导出游标绑定账号与筛选条件。

每页在一个只读事务中读取两组数据，使用同一续页游标。持续读取到 `next` 缺省为止，可取得筛选范围内的收藏夹（含空夹）和匹配收藏。

## 写入与保留

POST 请求须发送 `Content-Type: application/json` 和 `Idempotency-Key`，键长 1 至 128 个非空白可打印 ASCII 字符。写入按账号串行，业务变化、审计和幂等回执同事务提交。同键不同内容及修订冲突返回 409。

新增收藏先在事务外读取目标，超时上限 3 秒。读取失败返回错误且无收藏写入，已有记录和有效幂等回执可在目标来源故障时按原规则处理。错误响应提供 `code` 和可选 `reason`，供调用方区分准入不足、修订冲突等情况。

幂等回执保留 24 小时。到期重试重新执行业务校验，继续遵循账号归属和唯一性。旧回执缺少完成时间时，从迁移起保留完整的 24 小时。

`CloseOwner` 在同一事务删除本人收藏、收藏夹和幂等回执，并封存账号。重复调用沿用关闭结果，并发请求无法写回已清除的数据。

维护进程通过 `postgres.Repository.PruneExpired(ctx, limit)` 清理过期回执、确认满 90 天的 Outbox 与创建满 12 个月的审计。每类最多 1000 条，同事务提交并返回三类删除总数。待确认事件继续保留。

## 运行与检查

使用独立迁移身份调用 `postgres.Migrate`。运行身份应具备 `favorites` schema 与标识序列的 USAGE、各表的必要操作权限。测试授权见 [grants.go](postgres/testdb/grants.go)。

回执、Outbox 和审计的清理身份需要 `requests`、`outbox`、`events` 的 SELECT 和 DELETE。历史清理身份可读取 `activity_buckets`，可读取和删除 `activity_history`。

`Ready` 校验九张表的操作权限、两个序列及活动保留版本行。`CheckRuntime` 额外检查角色和破坏性权限。

backend 以 `STUHELPER_FAVORITES_CONFIG` 指向私有 JSON 文件的绝对路径，字段为 `database_dsn_file` 和 `connections`（2 至 32）。配置和 DSN 可采用 0600 普通文件或由 systemd 凭据目录交付。

准备本地工具与隔离 PostgreSQL，执行以下命令。可用 `STUHELPER_TEST_PG_BIN` 指定 PostgreSQL 二进制目录。

```sh
make setup
bash scripts/check-with-postgres.sh
```

脚本启动只接受本机 Unix socket 的临时数据库，设置 `STUHELPER_FAVORITES_TEST_DSN` 并执行 `make check`。已有隔离测试库时，可直接设置该环境变量后执行 `make check`。接口变更使用 `make generate` 更新生成结果。

[模块清单](module.yaml) 的 `scenario_tests` 登记收藏、按目标查询、热门活动和账号注销的测试。检查范围与交付状态见 [收藏文档](docs/README.md)。
