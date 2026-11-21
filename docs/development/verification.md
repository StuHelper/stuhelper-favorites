---
type: development
status: Accepted
updated: 2026-11-21
---

# 收藏验证记录

## 已实现

develop 提供本人收藏和收藏夹管理、按目标操作、批量移动与移除、刷新标题和有界导出。backend 负责装配接口、目标校验与活动投递。

## 已验证

develop 的独立模块检查通过。检查包含仓库和 SQL 边界、文档、生成一致性，以及使用隔离 PostgreSQL 验证并发、幂等、迁移和运行身份。[Favorites checks](https://github.com/StuHelper/stuhelper-favorites/actions/runs/44403651808)

`module.yaml` 登记 FAV-01、FAV-02、FAV-ACTIVITY-01、FAV-UI-01 与 PC-07。按目标查询、收藏夹排序与计数、批量操作和导出的测试均已登记。

StuHelper v0.1.31 的正式源码组合固定收藏模块 v0.1.1。以上 develop 的按目标写入、批量操作、标题刷新和导出接口尚待纳入正式组合。[StuHelper 发行组合](https://github.com/StuHelper/stuhelper/releases/download/v0.1.31/COMPONENTS.json)

## 待验证

以对应发行版本验证实际账号下的收藏、移动、移除和权限。待新版进入正式组合后，核验导出、批量操作及其 Web 集成。
