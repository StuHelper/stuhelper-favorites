# StuHelper Favorites

独立拥有课程、教师、资料收藏与本人收藏夹，提供 Go 服务、PostgreSQL、HTTP 接口和生成 SDK。详情与文件仍由对应模块检查当前权限。

## 当前行为

新增收藏由目标模块检查当前内容准入和可见状态。本人列表、移动、取消和收藏夹管理只要求登录与归属。列表只返回名称快照、目标 ID、种类、收藏夹、修订与时间，不包含简介、评分、正文或文件。

每条收藏只属于一个收藏夹。同一目标重复新增返回已有记录，指定不同收藏夹时返回 409，移动使用独立接口。删除收藏夹将收藏原子地移入默认夹，并递增收藏修订。默认夹不能删除，可以改名。

名称去掉首尾 Unicode 空白，长度为 1 至 20 个 Unicode 码点。同一账号的名称区分大小写且不得重复，数据库唯一约束覆盖并发创建和改名。未命名的默认夹返回空名称，由客户端显示本地化默认标签。

读接口每页最多 100 条。收藏支持种类、收藏夹和名称筛选，按新增时间或名称排序，游标绑定筛选条件与账号。收藏夹返回当前总条数，条数不受列表筛选影响。

写操作按账号串行，业务变化、幂等结果与审计事件在一个事务提交。同键不同内容或过期修订返回 409。目标读取最多 3 秒，失败不保存收藏。幂等结果保存在本模块，前端不得复用已经使用过的键。

## 检查与运行

按锁文件执行 `make setup`，设置独立 PostgreSQL 的 `STUHELPER_FAVORITES_TEST_DSN` 后执行 `make check`。CI 使用最新稳定 PostgreSQL 元包。本模块没有访问真实账号或生产数据的脚本。

迁移调用 `postgres.Migrate`，运行身份只取得 favorites schema 的 USAGE、表 SELECT/INSERT/UPDATE/DELETE 和序列 USAGE；events 只允许 SELECT/INSERT，requests 另授予 DELETE 用于注销清理。

`CheckRuntime` 拒绝超级用户、建库建角色权限、schema CREATE 和审计记录的 UPDATE/DELETE 和幂等记录的 UPDATE 权限。

后端使用 `STUHELPER_FAVORITES_CONFIG` 指向 0600 JSON 文件，包含 `database_dsn_file` 和 2 至 32 的 `connections`。DSN 文件也必须为 0600。本人列表和收藏夹管理使用本模块数据，resources 不可用时只有新增资料收藏受影响。

| 场景 | 实际检查 |
| --- | --- |
| FAV-01 | 归属、当前准入、收藏夹唯一名称、移动删除、并发幂等、事务回滚与运行身份隔离 |
| FAV-02 | 旧记录分页失败后恢复、并发重试及取消后不重新导入 |

OpenAPI 位于 contracts，使用 `make generate` 更新 Go 和 TypeScript SDK。后端组合测试验证真实 catalog、reviews、resources 与本模块；真实账号、Web/Admin 接入与部署另行验收。

账号注销通过 CloseOwner 删除本人的收藏、收藏夹和幂等响应，并永久封存账号。删除与关闭标记在同一事务完成，重复调用不会重复审计，失败可重试，并发写入不能恢复已删除记录。
