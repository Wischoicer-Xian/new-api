# WIS-561: `users` 表 quota 列 int4 → bigint 生产迁移（R3 / one-way-door）

> 适用：WIS-561 Fix C（new-api quota 余额链路 int32→int64）的**生产 MySQL** rollout。
> **test-env（SQLite）不需本指南**——SQLite `INTEGER` affinity 不卡 int32，部署新 binary 即解 user69。

## 背景

WIS-561 把 `user` 的 4 个 quota 列（`quota` / `used_quota` / `aff_quota` / `aff_history`）从 `int`（int32）改为 `bigint`（int64），余额上限抬到 ¥1M（`5×10¹¹` 单位）。GORM tag 已改 `type:bigint;default:0`。

## ⚠️ 硬门禁：先 ALTER，后部署 binary（禁止反序）

**新 binary 启动 `AutoMigrate`（`model/main.go:280`）会对 `User` 跑列迁移**——锁定 GORM `v1.25.12` 的 `MigrateColumn` 发现 `int→bigint` 类型不同即置 `alterColumn=true`，MySQL driver `v1.4.3` 随即发 `ALTER TABLE users MODIFY COLUMN ...`。

**即生产部署新 binary 会在启动时自动对 4 列发 DDL**，绕过 R3 窗口。为把 schema 变更隔离到受控窗口，rollout 必须严格按序：

1. **R3 窗口**（Jirui 单独授权）：执行下方 forward SQL 手动 ALTER 4 列 + postcheck 复验。
2. **复验通过后**：部署新 binary。此时 `AutoMigrate` 发现列已是 `bigint`（与模型一致）→ no-op，不再发 DDL。
3. **禁止反序**（先部署 binary 后 ALTER）：反序会让 binary 启动时自动 ALTER，在非窗口期改钱表 schema；若该自动 ALTER 失败，binary 会 restart-loop。

## forward SQL

> **执行前先 `SHOW CREATE TABLE users;`** 确认 4 列当前的 nullability / default。下方 SQL 假设生产为 `NOT NULL DEFAULT 0`（模型期望）。若实际 nullable 或 default 不同，**必须保留现有属性**调整 SQL——MySQL `MODIFY` 会重写整个列定义，漏写 nullability/default 会丢属性。

```sql
ALTER TABLE users MODIFY COLUMN quota BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users MODIFY COLUMN used_quota BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users MODIFY COLUMN aff_quota BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users MODIFY COLUMN aff_history BIGINT NOT NULL DEFAULT 0;
```

### precheck（ALTER 前）
```sql
-- 记下 4 列当前的 nullability/default（postcheck 比对）
SHOW CREATE TABLE users;
-- 确认无用户值溢出 int32（WIS-561 前理论上不该有；脏数据留神）
SELECT id, quota, used_quota, aff_quota, aff_history FROM users
  WHERE quota > 2147483647 OR used_quota > 2147483647
     OR aff_quota > 2147483647 OR aff_history > 2147483647;
```

### postcheck（ALTER 后）
```sql
-- 确认 4 列已 bigint + nullability/default 保留（与 precheck 对比）
SHOW CREATE TABLE users;
-- 确认数据无丢失
SELECT COUNT(*) FROM users;
SELECT id, quota, used_quota, aff_quota, aff_history FROM users ORDER BY id LIMIT 5;
```

## 禁止盲降级（one-way door）

`int4 → bigint` 是**安全加宽（无损）**。但**反向 `bigint → int4` 是 one-way door**：一旦有任何用户值 > `2147483647`（int32 max），反向 ALTER 会**静默截断**数据。因此：

- 回滚代码（revert binary + `WISCHOICER_MAX_USER_QUOTA` env 设回小值）**不需要**反向 ALTER——bigint 列对旧 binary 透明（旧 binary gorm tag `type:int`，但 GORM 对更宽的 DB 列 no-op，不缩窄）。
- **禁止**执行 `bigint → int4` 反向 ALTER，除非已确认全表无 >int32 值（precheck 查询为空）。

## 范围外

- test-env（SQLite）：不需本指南。
- 退款搅动幂等 follow-up：另记（WIS-561 不含；记星 R2 已附记，非本 fix 引入）。
