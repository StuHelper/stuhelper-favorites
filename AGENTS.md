---
type: guide
status: Accepted
updated: 2026-11-21
---

# 工程约定

更新日期：2026-11-21。

遵循 [主工作区工程约定](https://github.com/StuHelper/stuhelper/blob/develop/AGENTS.md)。收藏回执、收藏夹及活动计数归本模块维护。新增收藏和刷新标题通过所属模块的公开接口验证当前准入和目标状态，SQL 留在 `postgres/`。

修改 HTTP 接口时先维护 [OpenAPI](contracts/favorites.openapi.json)，再运行 `make generate`。将已实现的业务场景和实际测试函数登记到 [模块清单](module.yaml) 的 `scenario_tests`。

使用隔离 PostgreSQL 与独立运行身份验证归属、幂等、并发及迁移。开发和 CI 使用合成账号与内容。
