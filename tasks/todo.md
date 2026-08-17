# Custom Issue Status — 7-Category 模型（MUL-6243）

## 拍板结论（来自 issue 讨论）

- 7 个 category 与 7 个内置 status **一一对应**；category 的值就是 canonical key
- 不要 `behaves_as`，不要 flag
- `issue.status` 继续是权威 TEXT；**没有** `status_id`、没有回填、没有 double-write
- category / key 创建后不可改
- 归档自定义 status 前，必须先把存量 issue 迁走
- **内置 status 不允许修改名字和颜色**（v1 锁死）
- 目录管理权限：workspace owner / admin

## 完成情况

### 数据层
- [x] 332 migration：`issue_status` 表（无外键）
- [x] 333/334 migration：2 个 `CREATE INDEX CONCURRENTLY`，各自独立文件
- [x] 335/336 migration：drop 旧 enum CHECK，加格式 CHECK（NOT VALID → VALIDATE）
- [x] 337 migration：为存量 workspace 幂等补 7 条系统行
- [x] `cmd/migrate/main.go` 注册 2 个并发索引的 cleanup hook
- [x] sqlc queries + `sqlc generate`

### 行为层
- [x] `internal/issuestatus` 包：`Ensure` / `Effective` / `Resolve` / `ActiveKeys`
- [x] `Effective()` 接入 15 个判断点（内置 key 恒等且**不查库**）
- [x] workspace 创建时 seed，删除时清理（并补 deletion manifest）

### API / CLI
- [x] 目录 CRUD API（owner/admin gate；内置禁改名/颜色、禁归档）
- [x] issue 写路径（create / update / batch）对目录校验
- [x] CLI 改为校验 key 格式，成员资格交给服务端

### 前端
- [x] 类型 + zod schema + API client（纯新增）
- [ ] 状态选择器 / 看板列 / 设置页 UI（后续 PR）

## Review

### 关键设计
`Effective()` 对 7 个内置 key 是**恒等函数且不产生任何数据库查询**，这是"已有功能不损坏 + 默认用户无感"的技术保证：现存所有 `status == "todo"` 判断语义不变，也不新增一次 IO。只有自定义 key 才查目录，而这个集合在管理员建之前是空的。

### 过程中发现并修掉的一个真实缺陷
第一版 `Resolve()` 要求目录里必须有行才算合法 status。跑 DB 集成测试时 30+ 个既有用例失败，报错自相矛盾：
`invalid status "in_progress"; valid values: backlog, todo, in_progress, ...`

根因不是测试夹具问题，而是**生产缺陷**：任何目录行缺失的 workspace（旧 pod 在滚动发布期间创建的、或 337 迁移还没跑到的）将完全无法创建/更新 issue。

修法：目录是对内置 status 的**扩展**而非定义，7 个内置 key 无论有没有目录行都合法；非内置 key 仍然必须有行。fail-open 的范围精确等于"这个功能出现之前就合法的集合"。已加回归测试
`TestUnseededWorkspaceStillAcceptsBuiltInStatuses`。

### 已知限制（明确留给后续）
issue-table 的分组/筛选仍只认 7 个内置 key（`validIssueStatuses`），自定义 status 暂不能作为表格分组值。已在代码注释标注。这样做是为了保证没有自定义 status 的 workspace 表格视图零变化。

### 验证
- `go build ./...` / `go vet` / `gofmt` 干净
- 迁移在真实 Postgres 17 (pgvector) 上全量跑通；索引全部 valid；旧 enum 约束已移除、格式约束 validated
- `internal/handler`(含 8 个新增用例) / `internal/service` / `cmd/server` / `internal/issuestatus` / `internal/migrations` / `cmd/migrate` 全绿
- `@multica/core` 1408 个测试通过；core + views typecheck 干净
- 未通过项：`internal/daemon` 若干用例失败，已用 `git stash` 在干净树上复现，为环境性既有失败，与本次改动无关
