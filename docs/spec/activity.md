---
type: spec
status: Accepted
updated: 2026-11-09
---

# 热门活动来源

## 协议与数据归属

活动采用 search 9c56ad3 的 discovery-activity.schema.json，版本 1.0.0。contracts 保存原始 Schema、来源提交和 SHA-256。测试核对固定摘要，再逐条验证 outbox 事件，拒绝未拥有的指标字段。

favorites 只发送 favorites。事件类型为 discovery.activity.v1，source 固定为 favorites，target.source 固定为 catalog。活动 ID 为 kind/base64url(规范对象 ID)/UTC 日期，base64url 不带填充，subject 等于活动 ID。

每个对象日维护独立单调修订。事件完整替换日快照，零值显式发送；零计数墓碑不会被迟到事件重新计数。事件、计数和修订历史在同一业务事务提交，并调用 pg_notify('favorites_outbox', '')。

日快照只含目标、日期、修订、可见性和本模块指标，不含账号、正文、身份原值或支付信息。每项公开计数为 0 至 1000000000，越界会返回错误并回滚本次计数写入。

## 收藏净新增与跨日取消

favorites 等于当日新增数减当日取消数，对外下限为零。内部保留有符号净额，因此当天先取消后新增与先新增后取消得到相同结果。重复新增、移动收藏和删除收藏夹时的移动不重复计数。

跨日取消归属于取消操作当天，不重写原新增日。例：周一新增一条，周二取消，周一为 1，周二为 0；周二随后新增一条仍为 0，再新增一条才为 1。账号注销删除收藏时也计入注销当天，重复注销无额外效果。

只统计课程与教师收藏，资料收藏不生成活动。来源 Resolve 返回 ResolvedTarget{ID, Kind, Title}，其中 ID 必须为当前规范 ID，且必须执行请求人的当前授权。backend 来源适配器需要返回所属模块提供的真实规范 ID。

本人记录保留原 TargetID，用独立 canonical_target_id 保存统计目标。新增与取消使用同一统计目标，活动事件不包含本人记录或账号。CurrentActivity 重新调用来源 Resolve，并拒绝已变为别名的旧统计目标。

favorites.events 继续用于本模块审计，新的 favorites.outbox 专门保存可投递的活动封套。新增、取消和注销的计数或 outbox 写入失败时，业务操作同事务回滚，不能留下与收藏记录不一致的成功结果。

backend 的目录适配器必须优先使用 catalog.Object.CanonicalID，非别名时使用 ID。reviews.Catalog.Target.ID 与 favorites.ResolvedTarget.ID 均表示规范 ID，不能直接把旧别名作为统计目标返回。

## 公开接口与一致性

postgres.Repository.ActivitySnapshot(ctx, fromDay, toDay, cursor, limit) 返回 []Activity、next、error。日期必须为 UTC 零点，窗口左闭右开，limit 为 1 至 100。首轮 cursor 为空，next 为空表示结束。

分页绑定窗口、上限和 PostgreSQL 可见性快照，读取不可变历史中的首轮最新修订，包含零计数和不可见墓碑。分页期间的新增、修改及延迟提交不能进入旧快照。游标有效五分钟，窗口或上限变化会被拒绝。

Service.CurrentActivity(ctx, principal, activityID) 返回 Activity、bool、error。请求人由可信认证提供，每次检查当前目标权限；明确不可读或不存在返回 false，数据库和来源依赖故障返回错误，不表示来源未启用。

零计数快照仍可返回 true，Visible=false 表示该快照不贡献热度。当前查询不读取其他模块表；catalog 通过自己的公开读规则判断，其他来源通过所属模块公开接口取得当前目标事实。

统计目标按写入时的规范 ID 绑定。目录合并不搬运旧目标的历史计数，当前查询拒绝已经变成别名的旧目标。后续新增使用新的规范 ID，避免把不可证明的历史贡献复制到另一个对象。

OutboxChannel 为 favorites_outbox。PendingOutbox(ctx, limit, excludeSubjects) 每批最多 100 条，最多排除 1000 个主题，排除在数据库 LIMIT 前完成。ConfirmOutbox(ctx, id) 在消费者持久提交后调用，重复确认保留首次 sent_at。

内容事件和活动事件共用同一 outbox，relay 按 type 分发。backend 负责 LISTEN、启动和重连补读、60 秒兜底、退避及待处理主题；模块不扫描业务表寻找待投递变化。

## 清理、权限与验证

PruneActivityHistory(ctx, beforeDay, limit) 每次最多删除 1000 条旧修订，保留每个对象日的最新修订和墓碑。清理在同一事务提高保留修订号，已有分页游标失效，调用方须中止重建。

运行身份需要 activity_buckets 的读写、activity_history 的读取和追加、activity_retention 的读取，以及 outbox 的读写确认权限。历史删除权限只授予负责清理的身份。当前日快照和最新墓碑不自动删除。

接口均为公开 Go 端口，OpenAPI 路径和生成客户端没有变化。GOWORK=off 的隔离 PostgreSQL 检查覆盖修订单调、事务回滚无通知、Schema、固定快照、墓碑、并发和有界清理。backend 装配及实际热门页面组合另行验证。

| 场景 | 验证条件 |
| --- | --- |
| FAV-ACTIVITY-01 | 本来源计数、权限、并发、回滚、事件 Schema、固定分页、墓碑和清理上限 |
