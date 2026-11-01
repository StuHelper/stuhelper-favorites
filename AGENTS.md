# 工程约定

遵循主工作区 AGENTS.md。收藏由本模块拥有，通过公开接口校验目标。接口先维护 OpenAPI，再生成 SDK。SQL 留在 postgres，使用真实 PostgreSQL 与独立运行身份验证归属、幂等、并发及迁移。不得提交真实账号或内容。
