---
type: spec
status: Accepted
updated: 2026-11-26
---

# 热门活动来源

## 协议与数据归属

活动采用 search 的 discovery-activity Schema，版本 2.0.0。来源提交和 SHA-256 由 [来源记录](https://github.com/StuHelper/stuhelper-favorites/blob/develop/contracts/discovery-activity.source.json) 固定。测试核对摘要，并按 Schema 验证 Outbox 事件及指标归属。

事件类型为 `discovery.activity.v2`，`source` 为 `favorites`，`target.source` 为 `catalog`，唯一指标是 `favorites`。活动 ID 由种类、base64url 编码的规范目标 ID 和 北京时间日期组成，与 `subject` 相同，总长最多 512 字符。目标 ID 最多 200 个码点，拒绝控制字符。

每个对象日维护独立单调修订。事件完整替换日快照，零值显式发送；零计数墓碑不会被迟到事件重新计数。事件、计数和修订历史在同一业务事务提交，并调用 pg_notify('favorites_outbox', '')。

日快照只含来源、活动 ID、目标、日期、修订、可见性及本模块指标。每项公开计数为 0 至 1000000000，越界会返回错误并回滚本次计数写入。

## 收藏净新增与跨日取消

favorites 等于当日新增数减当日取消数，对外下限为零。内部保留有符号净额，因此当天先取消后新增与先新增后取消得到相同结果。重复新增、移动收藏和删除收藏夹时的移动不重复计数。

跨日取消归属于取消操作当天，不重写原新增日。例：周一新增一条，周二取消，周一为 1，周二为 0；周二随后新增一条仍为 0，再新增一条才为 1。账号注销删除收藏时也计入注销当天，重复注销无额外效果。

注销使用服务时钟确定 北京时间日期。仓储按完整活动 ID 的字节顺序，每批处理最多 256 个统计目标。全部扣减、修订历史、Outbox 和账号封存在同一事务提交。产生事件时通知 Relay 一次。

活动指标只统计课程与教师收藏。`Resolve` 检查请求人的当前权限，返回 `ResolvedTarget{ID, Kind, Title}`，其中 `ID` 使用所属模块提供的规范 ID。

本人收藏保留原 `TargetID`，`canonical_target_id` 保存规范 ID。目录适配器优先返回 `catalog.Object.CanonicalID`，其为空时使用 `ID`。同一账号、种类和规范目标只保留一条收藏。

迁移用原目标 ID 回填尚未设置的规范 ID。已有重复时，保留创建时间最早的收藏及原收藏夹、标题和修订。时间相同按 ID 字节序取首条。删除重复收藏对课程及教师的净新增扣减计入迁移当天，已发布的历史修订保留。

`favorites.events` 记录本模块审计，`favorites.outbox` 保存可投递活动。新增、取消和注销的业务记录、活动计数与 Outbox 在同一事务完成。

## 公开接口与一致性

postgres.Repository.ActivitySnapshot(ctx, fromDay, toDay, cursor, limit) 返回 []Activity、next、error。开始及结束时间取北京时间零点，窗口左闭右开，覆盖 1 至 366 个日期。limit 为 1 至 100。首轮 cursor 为空，next 为空表示结束。

分页绑定窗口、上限和 PostgreSQL 可见性快照，读取不可变历史中的首轮最新修订，包含零计数和不可见墓碑。分页期间的新增、修改及延迟提交不能进入旧快照。游标有效五分钟，窗口或上限变化会被拒绝。

活动游标校验 PostgreSQL 可见性快照的两个事务编号边界。它们必须大于零，下界不能超过上界。非法编号返回输入错误。

`Service.CurrentActivity(ctx, principal, activityID)` 返回活动、是否存在及错误。调用方提供经认证的请求人。查询时复核目标权限，明确不可读或不存在时返回 `false`，数据库或来源故障时返回错误。

零计数快照仍可返回 true，Visible=false 表示该快照不贡献热度。当前查询不读取其他模块表；catalog 通过自己的公开读规则判断，其他来源通过所属模块公开接口取得当前目标事实。

统计目标按写入时的规范 ID 绑定。目录合并不搬运旧目标的历史计数，当前查询拒绝已经变成别名的旧目标。后续新增使用新的规范 ID，避免把不可证明的历史贡献复制到另一个对象。

OutboxChannel 为 favorites_outbox。PendingOutbox(ctx, limit, excludeSubjects) 每批最多 100 条，最多排除 1000 个主题，排除在数据库 LIMIT 前完成。ConfirmOutbox(ctx, id) 在消费者持久提交后调用，重复确认保留首次 sent_at。

Relay 先排除退避主题，再按可见待投递记录的 seq 升序取一批。较晚入队的课程事件不会挤掉已在等待的教师事件。同一主题内，活动桶行锁保证修订顺序。

OutboxLag(ctx) 只读返回未确认事件积压条数、最老未确认时间与年龄，供宿主导出监控指标与积压巡检。

收藏模块向 Outbox 写入活动事件。backend 的通用 Relay 按类型分发，负责 LISTEN、启动和重连补读、60 秒兜底轮询，以及失败主题的退避。

## 既有 UTC 日计数迁移

迁移身份在事务内根据保留事件的原始时刻及可确认的修订净额重新归日。仍存在的收藏可补充有可靠证据的新增时刻。每个目标的日净额中，无法判定时刻的余量按 UTC 日已过去时段与北京时间日期的重叠时长分配。

完整 UTC 日约三分之一未知余量移到次日，按最近整数取整。首次切换时刻决定当日分配比例，UTC 16:00 前的未知余量全部留在当地同日。迁移保持有符号总量，历史单日公开计数与窗口热度可能变动。

切换与回退前停止活动写入、清理和 Relay。迁移刷新受影响的快照及递增修订，使既有分页游标失效。旧协议待发事件保留载荷并退出待发队列。Search 从提供方最新快照重建排名。

使用迁移身份调用 `postgres.RollbackActivityCalendar(ctx, db)` 可恢复 UTC 日计数。内部按目标和 UTC 日保存净转移额，包括切换后产生的变动。回退保留新写入、发布 UTC 版修订，并支持再次迁移。

## 清理、权限与验证

PruneActivityHistory(ctx, beforeDay, limit) 使用北京时间零点作为清理截止，每次最多删除 1000 条旧修订，保留每个对象日的最新修订和墓碑。清理在同一事务提高保留修订号，已有分页游标失效，调用方须中止重建。

一条 UPDATE 或 DELETE 语句实际影响历史记录时，保留版本只增加一次。空清理保留当前版本。固定快照使用 REPEATABLE READ，日期范围查询可使用现有的 (day, id, revision) 索引。

运行身份需要 activity_buckets 的读写、activity_history 的读取和追加、activity_retention 的读取，以及 outbox 的读写确认权限。历史删除权限只授予负责清理的身份。当前日快照和最新墓碑不自动删除。

上述接口供可信进程内调用。已登记测试及独立模块检查见 [验证记录](../development/verification.md)。

| 场景 | 验证条件 |
| --- | --- |
| FAV-ACTIVITY-01 | 本来源计数、权限、并发、回滚、事件 Schema、固定分页、墓碑和清理上限 |
| FAV-ACTIVITY-02 | UTC 日归档重新分桶、近似余量分配、幂等切换及保留后续写入的回退 |

## 目录批量读取

`DirectoryActivitySnapshot(ctx, kind)` 为可信 Go 装配返回课程或教师的日净新增汇总。按对象累计全部可见日汇总，每日净新增的下限为零。最多返回十万个目标，超过上限时返回服务不可用。

消费者须先检查当前学校内容准入，再按 catalog 的当前可见目录限制结果。该端口仅返回目标 ID 和汇总计数。

每次请求使用一个聚合 SELECT，无跨请求缓存。浏览、评价与权重由所属模块和宿主分别负责，favorites 不复制其他模块统计。

| 场景 | 验证条件 |
| --- | --- |
| FAV-UI-01 | 跨年度累计、每日净新增、零值、非法类型及北京时间换日 |
