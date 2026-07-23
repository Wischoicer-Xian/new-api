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
-- 记下 4 列当前的 nullability/default（postcheck 对比）
SHOW CREATE TABLE users;
-- 4 个钱字段全量留档（COUNT/MIN/MAX/SUM，与 postcheck 同口径比对——证 ALTER 全量无损，
-- 比 COUNT + 样本行更强）。int4→bigint 是无损加宽，这些聚合值 ALTER 前后必须完全一致。
SELECT COUNT(*) AS rows_total,
       MIN(quota) AS min_quota, MAX(quota) AS max_quota, SUM(quota) AS sum_quota,
       MIN(used_quota) AS min_used, MAX(used_quota) AS max_used, SUM(used_quota) AS sum_used,
       MIN(aff_quota) AS min_aff, MAX(aff_quota) AS max_aff, SUM(aff_quota) AS sum_aff,
       MIN(aff_history) AS min_aff_hist, MAX(aff_history) AS max_aff_hist, SUM(aff_history) AS sum_aff_hist
  FROM users;
-- 确认无用户值溢出 int32（WIS-561 前理论上不该有；脏数据留神——若 MAX > int32 max，
-- 反向降级路径彻底关闭，见下方「禁止盲降级」）
SELECT id, quota, used_quota, aff_quota, aff_history FROM users
  WHERE quota > 2147483647 OR used_quota > 2147483647
     OR aff_quota > 2147483647 OR aff_history > 2147483647;
```

### postcheck（ALTER 后）
```sql
-- 确认 4 列已 bigint + nullability/default 保留（与 precheck 对比）
SHOW CREATE TABLE users;
-- 4 个钱字段全量比对：COUNT/MIN/MAX/SUM 必须与 precheck 完全一致（证 ALTER 无丢失/截断）
SELECT COUNT(*) AS rows_total,
       MIN(quota) AS min_quota, MAX(quota) AS max_quota, SUM(quota) AS sum_quota,
       MIN(used_quota) AS min_used, MAX(used_quota) AS max_used, SUM(used_quota) AS sum_used,
       MIN(aff_quota) AS min_aff, MAX(aff_quota) AS max_aff, SUM(aff_quota) AS sum_aff,
       MIN(aff_history) AS min_aff_hist, MAX(aff_history) AS max_aff_hist, SUM(aff_history) AS sum_aff_hist
  FROM users;
```

## 禁止盲降级（one-way door）+ 安全回滚

`int4 → bigint` 是**安全加宽（无损）**。但**反向 `bigint → int4` 是 one-way door**：一旦有任何用户值 > `2147483647`（int32 max），反向会**静默截断**数据。

**⚠️ 对称反事实（R2 已修正）**：旧 binary（`type:int` tag）启动**同样**会发 `bigint → int`——锁定 GORM `v1.25.12` 的 `MigrateColumn` 对列类型差异**不区分加宽/缩窄**，发现 `bigint→int` 不同即置 `alterColumn=true` 发 `MODIFY COLUMN`。**即「直接 revert 到旧 binary」不是透明回滚**，会正踩 one-way door（>int32 值还会让 ALTER 失败 / 危险转换）。

### DDL 后的安全回滚规则

DDL（4 列已 bigint）完成后，**不得直接部署带 `type:int` 的旧 binary**。业务回退只能：

1. **用保留 4 个 `type:bigint` tag 的 rollback build**（revert 业务逻辑但保留 schema tag，或留本 binary + 关业务开关），**不要** revert schema tag；或
2. 显式关掉该实例的 `AutoMigrate`（若 new-api 提供 gate）再评估是否部署旧 binary。

回退前**先把 `WISCHOICER_MAX_USER_QUOTA` 调小**（回到 int32 顶以下的业务上限），避免回退态余额越界。

### 一旦出现 `>int32` 值：只走 forward-fix

若 precheck/postcheck 的 `MAX(quota/used_quota/aff_quota/aff_history)` 查到任何 `> 2147483647`：**只走 forward-fix**（保留 bigint、修业务逻辑），**禁止** schema 缩窄或部署 `type:int` 旧 binary——任何 `bigint→int` 尝试都会截断或失败。

### 反向 ALTER 的硬条件（仅极端核账场景）

`bigint → int4` 反向 ALTER **仅当** 4 字段 `MAX(...) <= 2147483647` 且经人工核账确认、在受控窗口执行；否则禁止。

## 范围外

- test-env（SQLite）：不需本指南。
- 退款搅动幂等 follow-up：另记（WIS-561 不含；记星 R2 已附记，非本 fix 引入）。
