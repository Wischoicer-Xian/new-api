package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---------------------------------------------------------------------------
// WischoicerRechargeCredit — quota capacity reservation + idempotent credit
// ---------------------------------------------------------------------------

// 该表同时承载 Wischoicer 微信充值的「支付前容量预留」与「支付后幂等入账凭据」，
// 不替代 billing 订单，也不进入现有 topup 列表 API。详见
// iterations/billing-recharge/方案.md §8.3。
type WischoicerRechargeCredit struct {
	Id                    int     `json:"id"`
	OrderNo               string  `json:"order_no" gorm:"type:varchar(32);not null;uniqueIndex"`
	NewAPIUserId          int     `json:"new_api_user_id" gorm:"type:int;not null;index:idx_wis_credit_user_status,priority:1"`
	Quota                 int     `json:"quota" gorm:"type:int;not null"`
	AmountCents           int64   `json:"amount_cents" gorm:"type:bigint;not null"`
	Currency              string  `json:"currency" gorm:"type:varchar(8);not null"`
	PaymentProvider       string  `json:"payment_provider" gorm:"type:varchar(32);not null"`
	ExternalTransactionId *string `json:"external_transaction_id,omitempty" gorm:"type:varchar(64);uniqueIndex"`
	Status                int     `json:"status" gorm:"type:int;not null;index:idx_wis_credit_user_status,priority:2"`
	CacheStatus           int     `json:"cache_status" gorm:"type:int;not null;index:idx_wis_credit_cache_due,priority:1"`
	CacheNextRetryAt      int64   `json:"cache_next_retry_at" gorm:"type:bigint;not null;index:idx_wis_credit_cache_due,priority:2"`
	PaidTime              int64   `json:"paid_time" gorm:"type:bigint;not null"`
	ReleaseReason         string  `json:"release_reason" gorm:"type:varchar(64);not null"`
	CreateTime            int64   `json:"create_time" gorm:"not null;autoCreateTime"`
	UpdateTime            int64   `json:"update_time" gorm:"not null;autoUpdateTime"`
}

func (WischoicerRechargeCredit) TableName() string {
	return "wischoicer_recharge_credits"
}

// status 枚举
const (
	WischoicerCreditStatusReserved = 0 // RESERVED — quota 容量已预留
	WischoicerCreditStatusSuccess  = 1 // SUCCESS — 预留已转为实际 quota
	WischoicerCreditStatusReleased = 2 // RELEASED — 预留已释放，未入账
)

// cache_status 枚举
const (
	WischoicerCacheStatusPending       = 0 // PENDING — 首次缓存删除待执行
	WischoicerCacheStatusVerifyPending = 1 // VERIFY_PENDING — 首次删除完成，等待二次删除
	WischoicerCacheStatusSuccess       = 2 // SUCCESS — 二次删除完成，缓存已收敛
)

// 错误哨兵：Controller 层据此映射 HTTP code（方案 §10）。
var (
	ErrWischoicerCreditNotFound        = errors.New("wischoicer recharge credit not found")
	ErrWischoicerReservationConflict   = errors.New("RESERVATION_CONFLICT")
	ErrWischoicerCreditConflict        = errors.New("CREDIT_CONFLICT")
	ErrWischoicerReservationReleased   = errors.New("RESERVATION_RELEASED")
	ErrWischoicerCreditUserUnavailable = errors.New("CREDIT_USER_UNAVAILABLE")
	ErrWischoicerQuotaCapacityExceeded = errors.New("QUOTA_CAPACITY_EXCEEDED")
	ErrWischoicerQuotaOverflow         = errors.New("QUOTA_OVERFLOW")
	ErrWischoicerInvalidArgument       = errors.New("INVALID_ARGUMENT")
)

// ---------------------------------------------------------------------------
// 请求 / 结果 DTO（model 层定义，供 controller 与 service 复用）
// ---------------------------------------------------------------------------

type ReserveExternalRechargeRequest struct {
	OrderNo         string `json:"orderNo"`
	NewApiUserId    int    `json:"newApiUserId"`
	Quota           int    `json:"quota"`
	AmountCents     int64  `json:"amountCents"`
	Currency        string `json:"currency"`
	PaymentProvider string `json:"paymentProvider"`
}

type ReserveExternalRechargeResult struct {
	Reserved  bool `json:"reserved"`
	Duplicate bool `json:"duplicate"`
}

type CreditExternalRechargeRequest struct {
	OrderNo         string `json:"orderNo"`
	NewApiUserId    int    `json:"newApiUserId"`
	Quota           int    `json:"quota"`
	AmountCents     int64  `json:"amountCents"`
	Currency        string `json:"currency"`
	PaymentProvider string `json:"paymentProvider"`
	TransactionId   string `json:"transactionId"`
	PaidAt          int64  `json:"paidAt"` // UTC Unix 秒
}

type CreditExternalRechargeResult struct {
	Credited  bool   `json:"credited"`
	Duplicate bool   `json:"duplicate"`
	OrderNo   string `json:"orderNo,omitempty"`
}

// ---------------------------------------------------------------------------
// 容量守卫：CreditUserQuotaTx
// ---------------------------------------------------------------------------

// CreditUserQuotaTx 是所有正向额度增加的唯一 model 层入口。
//
// 这里守卫的是「新正向额度的预约门槛（软上限）」（方案 §3.2）：
//
//	currentUserQuota + activeReservedQuota + delta <= WISCHOICER_MAX_USER_QUOTA
//
// WischoicerMaxUserQuota 不是物理硬界——退款降级直写（RefundUserQuota）会为了「退款
// 必到账」突破它，已付款 RESERVED 凭据的消费（consumeQuotaForCreditTx）也不再检查它。
// 真正的物理硬界是 user.quota 列的存储宽度（bigint 后 int64，见 maxUserBalanceForStorage），
// 由 RefundUserQuota 降级路径的存储溢出 CAS 守住。本函数守卫的软上限只 gate 新预约和新正向加额（admin
// 加额、签到等），对已付款的 RESERVED 消费 provably safe（Reserve 阶段已 gate）。
//
// 调用前必须已持有 tx（事务）；本函数在同一事务内锁 users 行、汇总该用户
// RESERVED 状态的预留 quota 总和、校验容量后再更新 quota。
func CreditUserQuotaTx(ctx context.Context, tx *gorm.DB, userID int, delta int) error {
	_ = ctx
	if delta <= 0 {
		return ErrWischoicerInvalidArgument
	}
	if userID <= 0 {
		return ErrWischoicerCreditUserUnavailable
	}

	// 锁 users 行，确保汇总与更新之间无并发写入。
	var user User
	if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrWischoicerCreditUserUnavailable
		}
		return err
	}

	reservedSum, err := sumActiveReservedQuotaTx(tx, userID)
	if err != nil {
		return err
	}

	// int64 checked 相加：余额接近 MaxInt64 时直接相加会 wrap 成负数绕过软上限，
	// 故分步相加并检测每步溢出。
	cur := int64(user.Quota)
	sum := cur + int64(reservedSum)
	projected := sum + int64(delta)
	if sum < cur || projected < sum || projected > common.WischoicerMaxUserQuota {
		common.SysError(
			"wischoicer quota capacity exceeded: " +
				quotaCapacityMsg(user.Id, user.Quota, reservedSum, delta, common.WischoicerMaxUserQuota),
		)
		return ErrWischoicerQuotaCapacityExceeded
	}

	result := tx.Model(&User{}).
		Where("id = ?", userID).
		Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrWischoicerCreditUserUnavailable
	}
	return nil
}

// quotaCapacityMsg 构造容量超限的审计日志消息。
func quotaCapacityMsg(userID int, currentQuota int, reservedSum int, delta int, limit int64) string {
	return fmt.Sprintf("user=%d current=%d reserved=%d delta=%d limit=%d",
		userID, currentQuota, reservedSum, delta, limit)
}

// sumActiveReservedQuotaTx 汇总指定用户 RESERVED 状态的预留 quota 总和。
func sumActiveReservedQuotaTx(tx *gorm.DB, userID int) (int, error) {
	var sum int
	err := tx.Model(&WischoicerRechargeCredit{}).
		Where("new_api_user_id = ? AND status = ?", userID, WischoicerCreditStatusReserved).
		Select("COALESCE(SUM(quota), 0)").
		Scan(&sum).Error
	if err != nil {
		return 0, err
	}
	return sum, nil
}

// CreditUserQuota 是 CreditUserQuotaTx 的事务包装，供未持有事务句柄的调用方使用。
//
// 返回的 error 为容量守卫或数据库错误；调用方负责后续缓存失效/增量。
func CreditUserQuota(userID int, delta int) error {
	if delta <= 0 {
		return ErrWischoicerInvalidArgument
	}
	return runWischoicerTx(func(tx *gorm.DB) error {
		return CreditUserQuotaTx(nil, tx, userID, delta)
	})
}

// ---------------------------------------------------------------------------
// Reserve / Release / Credit 公开入口
// ---------------------------------------------------------------------------

// ReserveExternalRecharge 以 orderNo 为幂等键预留 quota 容量。
//
// 首次创建 RESERVED 凭据；相同 orderNo 且不可变字段一致返回 duplicate=true；
// 字段不同返回 ErrWischoicerReservationConflict；用户不存在或容量不足返回
// ErrWischoicerQuotaCapacityExceeded。
func ReserveExternalRecharge(ctx context.Context, req ReserveExternalRechargeRequest) (*ReserveExternalRechargeResult, error) {
	_ = ctx
	if err := validateReserveRequest(req); err != nil {
		return nil, err
	}

	var duplicate bool
	err := runWischoicerTx(func(tx *gorm.DB) error {
		// 锁 user 行并校验容量（但不真正增加 quota，只是预留）。
		var user User
		if err := lockForUpdate(tx).Where("id = ?", req.NewApiUserId).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWischoicerCreditUserUnavailable
			}
			return err
		}

		// 检查现有同 orderNo 凭据。
		var existing WischoicerRechargeCredit
		findErr := tx.Where("order_no = ?", req.OrderNo).First(&existing).Error
		if findErr == nil {
			// 已存在：核对不可变字段。
			if !reservationFieldsMatch(&existing, req) {
				return ErrWischoicerReservationConflict
			}
			if existing.Status == WischoicerCreditStatusReleased {
				// 已释放的预留不允许重新创建。
				return ErrWischoicerReservationConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		// 软上限校验（新预约门槛）：current + reserved + newQuota <= limit。
		// limit 是软上限——退款降级直写或已付款 RESERVED 消费会突破它，但新预约始终受 gate。
		reservedSum, err := sumActiveReservedQuotaTx(tx, req.NewApiUserId)
		if err != nil {
			return err
		}
		// int64 checked 相加防余额接近 MaxInt64 时 wrap（同 CreditUserQuotaTx）。
		cur := int64(user.Quota)
		sum := cur + int64(reservedSum)
		projected := sum + int64(req.Quota)
		if sum < cur || projected < sum || projected > common.WischoicerMaxUserQuota {
			return ErrWischoicerQuotaCapacityExceeded
		}

		// 创建 RESERVED 凭据；并发时唯一索引兜底，DoNothing 后回读核对。
		credit := &WischoicerRechargeCredit{
			OrderNo:         req.OrderNo,
			NewAPIUserId:    req.NewApiUserId,
			Quota:           req.Quota,
			AmountCents:     req.AmountCents,
			Currency:        req.Currency,
			PaymentProvider: req.PaymentProvider,
			Status:          WischoicerCreditStatusReserved,
			CacheStatus:     WischoicerCacheStatusPending,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(credit).Error; err != nil {
			return err
		}
		if credit.Id == 0 {
			// 唯一冲突（并发预留）：回读核对。
			var reread WischoicerRechargeCredit
			if err := tx.Where("order_no = ?", req.OrderNo).First(&reread).Error; err != nil {
				return err
			}
			if !reservationFieldsMatch(&reread, req) {
				return ErrWischoicerReservationConflict
			}
			if reread.Status == WischoicerCreditStatusReleased {
				return ErrWischoicerReservationConflict
			}
			duplicate = true
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ReserveExternalRechargeResult{Reserved: true, Duplicate: duplicate}, nil
}

// ReleaseExternalRecharge 将 RESERVED 凭据转为 RELEASED，释放占用的容量。
//
// 幂等：已是 RELEASED 直接成功；已 SUCCESS 的凭据不允许 release（资金已入账）。
// release 只知道 orderNo，先无锁读取 user ID，再在事务内按统一锁顺序处理。
func ReleaseExternalRecharge(ctx context.Context, orderNo string, reason string) error {
	_ = ctx
	if orderNo == "" {
		return ErrWischoicerInvalidArgument
	}

	return runWischoicerTx(func(tx *gorm.DB) error {
		credit := &WischoicerRechargeCredit{}
		if err := tx.Where("order_no = ?", orderNo).First(credit).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 幂等：release 的语义是「确保该 orderNo 不再占用 RESERVED 容量」。
				// 记录不存在即无占用，等同已释放，返回 nil。这覆盖 billing 侧
				// reserve 响应丢失后兜底 release 的场景（本地 NOT_RESERVED 但不确定
				// new-api 是否已建预约），避免 release worker 因 NotFound 永久重试。
				return nil
			}
			return err
		}
		if credit.Status == WischoicerCreditStatusSuccess {
			return ErrWischoicerReservationConflict
		}
		if credit.Status == WischoicerCreditStatusReleased {
			return nil // 幂等
		}

		// 锁 user 行（统一锁顺序 users → credits），再做状态 CAS。
		var user User
		if err := lockForUpdate(tx).Where("id = ?", credit.NewAPIUserId).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWischoicerCreditUserUnavailable
			}
			return err
		}

		result := tx.Model(&WischoicerRechargeCredit{}).
			Where("order_no = ? AND status = ?", orderNo, WischoicerCreditStatusReserved).
			Updates(map[string]interface{}{
				"status":         WischoicerCreditStatusReleased,
				"release_reason": truncateReason(reason),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			// 并发：凭据状态已变。重新读取核对——SUCCESS 的凭据禁止 release。
			var reread WischoicerRechargeCredit
			if err := tx.Where("order_no = ?", orderNo).First(&reread).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrWischoicerCreditNotFound
				}
				return err
			}
			if reread.Status == WischoicerCreditStatusSuccess {
				return ErrWischoicerReservationConflict
			}
			// RELEASED → 幂等成功。
			return nil
		}
		return nil
	})
}

// ConsumeReservedQuotaTx 在同一事务内消费 RESERVED 凭据：增加 quota 并把凭据
// 转为 SUCCESS。供 CreditExternalRecharge 的核心事务使用。
func ConsumeReservedQuotaTx(ctx context.Context, tx *gorm.DB, req CreditExternalRechargeRequest) (*CreditExternalRechargeResult, error) {
	_ = ctx
	if err := validateCreditRequest(req); err != nil {
		return nil, err
	}

	// 统一锁顺序：users → wischoicer_recharge_credits。
	var user User
	if err := lockForUpdate(tx).Where("id = ?", req.NewApiUserId).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWischoicerCreditUserUnavailable
		}
		return nil, err
	}

	credit := &WischoicerRechargeCredit{}
	if err := lockForUpdate(tx).Where("order_no = ?", req.OrderNo).First(credit).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWischoicerCreditNotFound
		}
		return nil, err
	}

	switch credit.Status {
	case WischoicerCreditStatusSuccess:
		// 幂等：核对 transaction_id 与 paid_time，区分 duplicate 与 conflict。
		return handleAlreadySuccess(credit, req)
	case WischoicerCreditStatusReleased:
		return nil, ErrWischoicerReservationReleased
	case WischoicerCreditStatusReserved:
		// 继续消费流程。
	default:
		return nil, ErrWischoicerCreditConflict
	}

	// 不可变字段核对。
	if !creditFieldsMatch(credit, req) {
		return nil, ErrWischoicerCreditConflict
	}

	// transactionId 预检：防止同一微信交易关联不同订单（唯一索引兜底竞争）。
	if req.TransactionId != "" {
		conflict, err := findTransactionConflictTx(tx, req.TransactionId, req.OrderNo)
		if err != nil {
			return nil, err
		}
		if conflict {
			return nil, ErrWischoicerCreditConflict
		}
	}

	// 先 CAS 凭据 RESERVED → SUCCESS（要求 RowsAffected==1）。
	// 把凭据状态翻转作为「本事务赢得入账权」的门控：只有 CAS 成功才增加 quota，
	// 保证 quota 最多增加一次。并发落败者不会走到 quota 更新，事务回滚后由 billing
	// 重试，重试时凭据已 SUCCESS → 直接返回 duplicate（方案 §5.4 步骤 5/6/7）。
	updates := map[string]interface{}{
		"status":                  WischoicerCreditStatusSuccess,
		"cache_status":            WischoicerCacheStatusPending,
		"external_transaction_id": req.TransactionId,
		"paid_time":               req.PaidAt,
	}
	result := tx.Model(&WischoicerRechargeCredit{}).
		Where("order_no = ? AND status = ?", req.OrderNo, WischoicerCreditStatusReserved).
		Updates(updates)
	if result.Error != nil {
		// transaction_id 唯一冲突（并发入账同一微信交易到不同订单）统一返回 CREDIT_CONFLICT，
		// 不依赖数据库错误文本（gorm TranslateError 翻译为 ErrDuplicatedKey）。
		if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			return nil, ErrWischoicerCreditConflict
		}
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		// 并发落败：另一事务已消费该凭据。本事务未增加 quota（CAS 在 quota 更新前），
		// 回读核对返回 duplicate/conflict；billing 重试时走 switch 已 SUCCESS 分支。
		var reread WischoicerRechargeCredit
		if err := tx.Where("order_no = ?", req.OrderNo).First(&reread).Error; err != nil {
			return nil, err
		}
		if reread.Status == WischoicerCreditStatusSuccess {
			return handleAlreadySuccess(&reread, req)
		}
		return nil, ErrWischoicerCreditConflict
	}

	// CAS 成功（本事务赢得入账权）：增加 quota。任一失败整体回滚（凭据也回到 RESERVED）。
	if err := consumeQuotaForCreditTx(tx, &user, req.Quota); err != nil {
		return nil, err
	}

	return &CreditExternalRechargeResult{Credited: true, Duplicate: false, OrderNo: req.OrderNo}, nil
}

// handleAlreadySuccess 处理凭据已 SUCCESS 的幂等核对。
func handleAlreadySuccess(credit *WischoicerRechargeCredit, req CreditExternalRechargeRequest) (*CreditExternalRechargeResult, error) {
	existingTx := ""
	if credit.ExternalTransactionId != nil {
		existingTx = *credit.ExternalTransactionId
	}
	if existingTx == req.TransactionId && credit.PaidTime == req.PaidAt {
		return &CreditExternalRechargeResult{Credited: false, Duplicate: true, OrderNo: req.OrderNo}, nil
	}
	return nil, ErrWischoicerCreditConflict
}

// consumeQuotaForCreditTx 把 RESERVED 凭据的 quota 转为实际 user.quota。
//
// 这里不做软上限校验。该凭据的 quota 已在 ReserveExternalRecharge 阶段计入
// activeReservedQuota 并通过 `current + activeReserved + newQuota <= limit` 守卫；
// 消费只是把 reserved 占用转为 actual 占用，`current + reserved` 净额不变，数学上
// provably safe——不破坏软上限（limit 不支持热更新，见方案 §3.2）。软上限只 gate 新预约。
//
// 即便 user.quota 已被 RefundUserQuota 降级直写推过软上限，这里仍允许消费：退款必须
// 到账，已付款的 RESERVED 凭据不能因退款突破而被永久拒绝（否则用户付了钱到不了账，
// billing 只能重试/死信）。此时新 reservation 会被 ReserveExternalRecharge 的容量检查
// 正确拒绝，软上限对新预留仍然生效；CreditUserQuotaTx（其他正向加额）也仍守卫容量。
// 存储硬界（bigint 后 int64）由应用层在持有 users 行锁时显式检查，不依赖 MySQL/PostgreSQL 的
// 列类型映射或隐式 cast；越界会让整个事务回滚，凭据仍保持 RESERVED。
func consumeQuotaForCreditTx(tx *gorm.DB, user *User, delta int) error {
	cur := int64(user.Quota)
	projected := cur + int64(delta)
	if projected < cur || projected > maxUserBalanceForStorage() {
		common.SysError(fmt.Sprintf(
			"reserved quota consumption rejected: would overflow storage hard cap (int64): user=%d current=%d delta=%d",
			user.Id, user.Quota, delta,
		))
		return ErrWischoicerQuotaCapacityExceeded
	}
	result := tx.Model(&User{}).
		Where("id = ?", user.Id).
		Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrWischoicerCreditUserUnavailable
	}
	return nil
}

// findTransactionConflictTx 检查同一 transactionId 是否已被其他 orderNo 使用。
func findTransactionConflictTx(tx *gorm.DB, transactionId string, orderNo string) (bool, error) {
	if transactionId == "" {
		return false, nil
	}
	var count int64
	err := tx.Model(&WischoicerRechargeCredit{}).
		Where("external_transaction_id = ? AND order_no != ?", transactionId, orderNo).
		Count(&count).Error
	if err != nil {
		// external_transaction_id 为 NULL 的行不会被 `= ?` 匹配，无需排除 NULL。
		return false, err
	}
	return count > 0, nil
}

// CreditExternalRecharge 是幂等入账的事务包装：内部调用 ConsumeReservedQuotaTx，
// 成功后执行两阶段缓存删除。
func CreditExternalRecharge(ctx context.Context, req CreditExternalRechargeRequest) (*CreditExternalRechargeResult, error) {
	_ = ctx
	if err := validateCreditRequest(req); err != nil {
		return nil, err
	}

	var result *CreditExternalRechargeResult
	var credited bool
	err := runWischoicerTx(func(tx *gorm.DB) error {
		r, err := ConsumeReservedQuotaTx(ctx, tx, req)
		if err != nil {
			return err
		}
		result = r
		credited = r.Credited
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 事务提交成功后，执行两阶段缓存删除。失败由后台扫描任务重试。
	if credited && result != nil {
		startTwoPhaseCacheInvalidation(req.NewApiUserId, req.OrderNo)
	}
	return result, nil
}

// GetWischoicerRechargeCredit 按 orderNo 只读核对。
func GetWischoicerRechargeCredit(ctx context.Context, orderNo string) (*WischoicerRechargeCredit, error) {
	_ = ctx
	if orderNo == "" {
		return nil, ErrWischoicerInvalidArgument
	}
	var credit WischoicerRechargeCredit
	if err := DB.Where("order_no = ?", orderNo).First(&credit).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWischoicerCreditNotFound
		}
		return nil, err
	}
	return &credit, nil
}

// HasActiveWischoicerReservation 检查用户是否存在 RESERVED 状态的预留记录。
// 用于用户删除保护：存在预留时禁止删除。
func HasActiveWischoicerReservation(userID int) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	var count int64
	err := DB.Model(&WischoicerRechargeCredit{}).
		Where("new_api_user_id = ? AND status = ?", userID, WischoicerCreditStatusReserved).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// hasActiveReservedQuotaTx 是 HasActiveWischoicerReservation 的事务内版本，
// 供 DeleteUserById/HardDeleteUserById 在锁住 user 行后重查预留，消除 TOCTOU。
func hasActiveReservedQuotaTx(tx *gorm.DB, userID int) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	var count int64
	err := tx.Model(&WischoicerRechargeCredit{}).
		Where("new_api_user_id = ? AND status = ?", userID, WischoicerCreditStatusReserved).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ---------------------------------------------------------------------------
// 校验与辅助
// ---------------------------------------------------------------------------

func validateReserveRequest(req ReserveExternalRechargeRequest) error {
	if req.OrderNo == "" {
		return ErrWischoicerInvalidArgument
	}
	if req.NewApiUserId <= 0 {
		return ErrWischoicerCreditUserUnavailable
	}
	if req.Quota <= 0 || req.Quota > common.MaxQuota {
		return ErrWischoicerInvalidArgument
	}
	if req.AmountCents <= 0 {
		return ErrWischoicerInvalidArgument
	}
	if req.Currency == "" || req.PaymentProvider == "" {
		return ErrWischoicerInvalidArgument
	}
	return nil
}

func validateCreditRequest(req CreditExternalRechargeRequest) error {
	if req.OrderNo == "" {
		return ErrWischoicerInvalidArgument
	}
	if req.NewApiUserId <= 0 {
		return ErrWischoicerCreditUserUnavailable
	}
	if req.Quota <= 0 || req.Quota > common.MaxQuota {
		return ErrWischoicerInvalidArgument
	}
	if req.AmountCents <= 0 {
		return ErrWischoicerInvalidArgument
	}
	if req.Currency == "" || req.PaymentProvider == "" {
		return ErrWischoicerInvalidArgument
	}
	if req.TransactionId == "" {
		return ErrWischoicerInvalidArgument
	}
	if req.PaidAt <= 0 {
		return ErrWischoicerInvalidArgument
	}
	return nil
}

func reservationFieldsMatch(credit *WischoicerRechargeCredit, req ReserveExternalRechargeRequest) bool {
	return credit.NewAPIUserId == req.NewApiUserId &&
		credit.Quota == req.Quota &&
		credit.AmountCents == req.AmountCents &&
		credit.Currency == req.Currency &&
		credit.PaymentProvider == req.PaymentProvider
}

func creditFieldsMatch(credit *WischoicerRechargeCredit, req CreditExternalRechargeRequest) bool {
	return credit.NewAPIUserId == req.NewApiUserId &&
		credit.Quota == req.Quota &&
		credit.AmountCents == req.AmountCents &&
		credit.Currency == req.Currency &&
		credit.PaymentProvider == req.PaymentProvider
}

func truncateReason(reason string) string {
	if len(reason) > 64 {
		return reason[:64]
	}
	return reason
}

// runWischoicerTx 执行一个事务，SQLite 遇到 BUSY/LOCKED 时重试整个事务。
//
// SQLite 没有 FOR UPDATE，依赖单写模型：锁冲突会让其中一个事务失败。
// 遇到 BUSY/LOCKED 错误时不能沿用冲突前的读取值，必须重跑整个事务。
func runWischoicerTx(fn func(tx *gorm.DB) error) error {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := DB.Transaction(fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			return err
		}
		if !isSQLiteBusyOrLockedError(err) {
			return err
		}
		// SQLite BUSY/LOCKED：短暂退避后重试整个事务。
		time.Sleep(time.Duration(50<<(attempt)) * time.Millisecond)
	}
	return lastErr
}

// isSQLiteBusyOrLockedError 检测 SQLite 写锁冲突错误。
//
// 这是 SQLite 单写模型下的重试触发条件，与唯一约束冲突的错误文本检测是两回事：
// 唯一约束用 clause.OnConflict + 回读处理，绝不依赖错误文本。
func isSQLiteBusyOrLockedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "cannot start a transaction within a transaction")
}
