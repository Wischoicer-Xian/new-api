package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TopUp struct {
	Id              int     `json:"id"`
	UserId          int     `json:"user_id" gorm:"index"`
	Amount          int64   `json:"amount"`
	Money           float64 `json:"money"`
	TradeNo         string  `json:"trade_no" gorm:"unique;type:varchar(255);index"`
	PaymentMethod   string  `json:"payment_method" gorm:"type:varchar(50)"`
	PaymentProvider string  `json:"payment_provider" gorm:"type:varchar(50);default:''"`
	CreateTime      int64   `json:"create_time"`
	CompleteTime    int64   `json:"complete_time"`
	Status          string  `json:"status"`
}

const (
	PaymentMethodStripe       = "stripe"
	PaymentMethodCreem        = "creem"
	PaymentMethodWaffo        = "waffo"
	PaymentMethodWaffoPancake = "waffo_pancake"
	PaymentMethodBalance      = "balance"
)

const (
	PaymentProviderEpay         = "epay"
	PaymentProviderStripe       = "stripe"
	PaymentProviderCreem        = "creem"
	PaymentProviderWaffo        = "waffo"
	PaymentProviderWaffoPancake = "waffo_pancake"
	PaymentProviderBalance      = "balance"
)

var (
	ErrPaymentMethodMismatch = errors.New("payment method mismatch")
	ErrTopUpNotFound         = errors.New("topup not found")
	ErrTopUpStatusInvalid    = errors.New("topup status invalid")
	ErrEpayMoneyMismatch     = errors.New("epay notify money mismatch")
)

// EpayMoneyMismatchError carries the details of a signature-valid epay notify
// whose amount does not equal the order's frozen amount. The channel signature
// passed, so the channel has confirmed a real fund movement that the local
// credit transaction refused to honor; the caller must persist this as a
// durable payment obligation (for manual reconciliation/refund) before ACKing.
// Is keeps errors.Is(err, ErrEpayMoneyMismatch) working for callers that only
// need the mismatch kind. See CompleteEpayTopUpTx (r9 P2-4).
type EpayMoneyMismatchError struct {
	TradeNo       string
	UserId        int
	ExpectedCents int64
	NotifyCents   int64
}

const (
	EpayAnomalyTypeMoneyMismatch = "MONEY_MISMATCH"
	EpayAnomalyStatusOpen        = "OPEN"
	EpayAnomalyStatusResolved    = "RESOLVED"
)

// EpayPaymentAnomaly 是签名验证通过、但本地无法自动入账的渠道资金事实。
// DedupKey 对同一订单和同一金额事实做幂等收敛；OccurrenceCount 保留渠道重试次数。
type EpayPaymentAnomaly struct {
	Id              int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	DedupKey        string `json:"dedup_key" gorm:"type:char(64);not null;uniqueIndex"`
	AnomalyType     string `json:"anomaly_type" gorm:"type:varchar(32);not null;index"`
	TradeNo         string `json:"trade_no" gorm:"type:varchar(255);not null;index"`
	UserId          int    `json:"user_id" gorm:"not null;index"`
	ExpectedCents   int64  `json:"expected_cents" gorm:"not null"`
	NotifyCents     int64  `json:"notify_cents" gorm:"not null"`
	CallerIp        string `json:"caller_ip" gorm:"type:varchar(64);not null;default:''"`
	Status          string `json:"status" gorm:"type:varchar(16);not null;default:OPEN;index"`
	OccurrenceCount int64  `json:"occurrence_count" gorm:"not null;default:1"`
	FirstSeenAt     int64  `json:"first_seen_at" gorm:"not null"`
	LastSeenAt      int64  `json:"last_seen_at" gorm:"not null"`
}

func (e *EpayMoneyMismatchError) Error() string {
	return fmt.Sprintf("epay notify money mismatch: trade_no=%s expected_cents=%d notify_cents=%d", e.TradeNo, e.ExpectedCents, e.NotifyCents)
}

func (e *EpayMoneyMismatchError) Is(target error) bool {
	return target == ErrEpayMoneyMismatch
}

// UpsertEpayMoneyMismatchAnomaly 在到账事务回滚后独立持久化人工补账/退款义务。
// 只有该写入提交后，调用方才可以 ACK 渠道并停止确定性失败的重复通知。
func UpsertEpayMoneyMismatchAnomaly(mmErr *EpayMoneyMismatchError, callerIp string) error {
	return upsertEpayMoneyMismatchAnomaly(DB, mmErr, callerIp)
}

func upsertEpayMoneyMismatchAnomaly(db *gorm.DB, mmErr *EpayMoneyMismatchError, callerIp string) error {
	if mmErr == nil {
		return errors.New("epay money mismatch error is nil")
	}
	fact := fmt.Sprintf("epay|%s|%s|%d|%d", EpayAnomalyTypeMoneyMismatch, mmErr.TradeNo, mmErr.ExpectedCents, mmErr.NotifyCents)
	sum := sha256.Sum256([]byte(fact))
	now := common.GetTimestamp()
	anomaly := &EpayPaymentAnomaly{
		DedupKey:        hex.EncodeToString(sum[:]),
		AnomalyType:     EpayAnomalyTypeMoneyMismatch,
		TradeNo:         mmErr.TradeNo,
		UserId:          mmErr.UserId,
		ExpectedCents:   mmErr.ExpectedCents,
		NotifyCents:     mmErr.NotifyCents,
		CallerIp:        callerIp,
		Status:          EpayAnomalyStatusOpen,
		OccurrenceCount: 1,
		FirstSeenAt:     now,
		LastSeenAt:      now,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "dedup_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"caller_ip":    callerIp,
			"last_seen_at": now,
			// Qualify the target column: PostgreSQL exposes both the target and
			// excluded rows inside ON CONFLICT and rejects the bare name as
			// ambiguous. The table-qualified expression is valid on all three
			// supported dialects.
			"occurrence_count": gorm.Expr("epay_payment_anomalies.occurrence_count + 1"),
		}),
	}).Create(anomaly).Error
}

func (topUp *TopUp) Insert() error {
	var err error
	err = DB.Create(topUp).Error
	return err
}

func (topUp *TopUp) Update() error {
	var err error
	err = DB.Save(topUp).Error
	return err
}

func GetTopUpById(id int) *TopUp {
	var topUp *TopUp
	var err error
	err = DB.Where("id = ?", id).First(&topUp).Error
	if err != nil {
		return nil
	}
	return topUp
}

func GetTopUpByTradeNo(tradeNo string) *TopUp {
	var topUp *TopUp
	var err error
	err = DB.Where("trade_no = ?", tradeNo).First(&topUp).Error
	if err != nil {
		return nil
	}
	return topUp
}

func UpdatePendingTopUpStatus(tradeNo string, expectedPaymentProvider string, targetStatus string) error {
	if tradeNo == "" {
		return errors.New("未提供支付单号")
	}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		topUp := &TopUp{}
		if err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(topUp).Error; err != nil {
			return ErrTopUpNotFound
		}
		if expectedPaymentProvider != "" && topUp.PaymentProvider != expectedPaymentProvider {
			return ErrPaymentMethodMismatch
		}
		if topUp.Status != common.TopUpStatusPending {
			return ErrTopUpStatusInvalid
		}

		topUp.Status = targetStatus
		return tx.Save(topUp).Error
	})
}

func Recharge(referenceId string, customerId string, callerIp string) (err error) {
	if referenceId == "" {
		return errors.New("未提供支付单号")
	}

	var quota float64
	topUp := &TopUp{}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(refCol+" = ?", referenceId).First(topUp).Error
		if err != nil {
			return errors.New("充值订单不存在")
		}

		if topUp.PaymentProvider != PaymentProviderStripe {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("充值订单状态错误")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		err = tx.Save(topUp).Error
		if err != nil {
			return err
		}

		quota = topUp.Money * common.QuotaPerUnit
		quotaInt := int(quota)
		if quotaInt <= 0 {
			return errors.New("无效的充值额度")
		}
		// 正向额度增加统一走已收款到账入口 CreditPaidTopUpTx：钱已确认支付，
		// 不能被「新售卖软上限」拒绝导致已付款却拿不到额度（方案 §3.2）。
		if err := CreditPaidTopUpTx(tx, topUp.UserId, quotaInt); err != nil {
			return err
		}
		// stripe_customer 仍需在同一事务内更新。
		if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).Update("stripe_customer", customerId).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		common.SysError("topup failed: " + err.Error())
		return errors.New("充值失败，请稍后重试")
	}

	RecordTopupLog(topUp.UserId, fmt.Sprintf("使用在线充值成功，充值金额: %v，支付金额：%d", logger.FormatQuota(int(quota)), topUp.Amount), callerIp, topUp.PaymentMethod, PaymentMethodStripe)

	return nil
}

// CompleteEpayTopUpTx 在同一数据库事务内原子完成 Epay 已收款到账：锁定 topup 行
// （MySQL/PostgreSQL FOR UPDATE 跨进程行锁，SQLite 单写串行）→ 校验 provider/status
// → 幂等（已 SUCCESS 直接返回 credited=false）→ 回调金额核对 → 同事务
// CreditPaidTopUpTx 增额 → 标记 SUCCESS。调用方必须已持有事务 tx。
//
// 与 Recharge（Stripe）/ RechargeCreem / RechargeWaffo / ManualCompleteTopUp 同一事务
// 模式：quota 增额与订单状态变更原子提交，任一步失败整体回滚。跨进程幂等由 trade_no
// 唯一行 + 行锁保证——重复通知命中已 SUCCESS 时 credited=false，不再增 quota（防双到账）。
// 返回 credited=true 仅在本次实际完成到账时，供调用方决定是否记录“充值成功”日志，
// 避免重复通知产生重复用户日志（r7 P1-2）。
//
// notifyMoneyCents 为渠道回调实际支付金额（最小货币单位，由 controller 从
// verifyInfo.Money 字符串经 common.MoneyToCents 解析），必须等于订单创建时冻结的预期
// 金额（topUp.Money 经 common.FloatMoneyToCents 换算到分——与 notify 共用唯一定点换算
// 路径）。不一致返回 *EpayMoneyMismatchError（errors.Is 命中 ErrEpayMoneyMismatch）触发
// 事务回滚——签名有效但金额异常（渠道配置错误/协议漂移）时不能按本地订单全额到账
// （r8 P1-1）。调用方应在此后事务外持久化 EpayPaymentAnomaly，因为渠道已确认一笔
// 资金事实，仅易失日志不足以保留人工补账/退款义务（r9 P2-4）。幂等路径
// （已 SUCCESS）跳过金额校验，因为首次到账已核对过金额且无法回滚。
//
// actualPaymentMethod 为渠道回调里的实际支付方式，与订单记录不同时同步到 PaymentMethod
// 字段（仅记录，不影响资金）。
func CompleteEpayTopUpTx(tx *gorm.DB, tradeNo string, quotaToAdd int, actualPaymentMethod string, notifyMoneyCents int64) (credited bool, err error) {
	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}
	topUp := &TopUp{}
	if err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(topUp).Error; err != nil {
		return false, errors.New("充值订单不存在")
	}
	if topUp.PaymentProvider != PaymentProviderEpay {
		return false, ErrPaymentMethodMismatch
	}
	// 幂等：已 SUCCESS 直接返回，重复通知不再增 quota（防双到账），且跳过金额校验——
	// 首次到账时已核对过金额，重复通知即使格式不同也无法回滚已到账额度。
	if topUp.Status == common.TopUpStatusSuccess {
		return false, nil
	}
	if topUp.Status != common.TopUpStatusPending {
		return false, errors.New("充值订单状态错误")
	}
	// 金额核对：回调实际支付金额必须等于订单冻结金额（按最小货币单位）。expected 与 notify
	// 共用 common.MoneyToCents 唯一定点换算路径：notify 侧直接解析渠道字符串，expected 侧
	// 把 topUp.Money（float64）按发给渠道的两位小数格式化后反解析，消除 float64 存储误差
	// （如 9.99 存成 9.989999... 两侧都得到 999 分）。expected 换算失败表示订单 Money 本身
	// 损坏，返回普通 error 触发回滚，不属于渠道侧异常（r9 P2-4）。
	expectedCents, err := common.FloatMoneyToCents(topUp.Money)
	if err != nil {
		return false, fmt.Errorf("epay order money invalid: trade_no=%s money=%f: %w", tradeNo, topUp.Money, err)
	}
	if notifyMoneyCents != expectedCents {
		return false, &EpayMoneyMismatchError{
			TradeNo:       tradeNo,
			UserId:        topUp.UserId,
			ExpectedCents: expectedCents,
			NotifyCents:   notifyMoneyCents,
		}
	}
	if quotaToAdd <= 0 {
		return false, errors.New("无效的充值额度")
	}
	if actualPaymentMethod != "" && topUp.PaymentMethod != actualPaymentMethod {
		topUp.PaymentMethod = actualPaymentMethod
	}
	topUp.CompleteTime = common.GetTimestamp()
	topUp.Status = common.TopUpStatusSuccess
	if err := tx.Save(topUp).Error; err != nil {
		return false, err
	}
	// 同事务内增 quota（已收款到账入口，不被“新售卖软上限”拒绝；int32 硬界溢出或 DB
	// 错误时返回 error 触发整体回滚，订单保持 Pending、quota 不变，等待重试或人工介入）。
	if err := CreditPaidTopUpTx(tx, topUp.UserId, quotaToAdd); err != nil {
		return false, err
	}
	return true, nil
}

// topUpQueryWindowSeconds 限制充值记录查询的时间窗口（秒）。
const topUpQueryWindowSeconds int64 = 30 * 24 * 60 * 60

// topUpQueryCutoff 返回允许查询的最早 create_time（秒级 Unix 时间戳）。
func topUpQueryCutoff() int64 {
	return common.GetTimestamp() - topUpQueryWindowSeconds
}

func GetUserTopUps(userId int, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	// Start transaction
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	cutoff := topUpQueryCutoff()

	// Get total count within transaction
	err = tx.Model(&TopUp{}).Where("user_id = ? AND create_time >= ?", userId, cutoff).Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Get paginated topups within same transaction
	err = tx.Where("user_id = ? AND create_time >= ?", userId, cutoff).Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Commit transaction
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return topups, total, nil
}

// GetAllTopUps 获取全平台的充值记录（管理员使用，不限制时间窗口）
func GetAllTopUps(pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err = tx.Model(&TopUp{}).Count(&total).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return topups, total, nil
}

// searchTopUpCountHardLimit 搜索充值记录时 COUNT 的安全上限，
// 防止对超大表执行无界 COUNT 触发 DoS。
const searchTopUpCountHardLimit = 10000

// SearchUserTopUps 按订单号搜索某用户的充值记录
func SearchUserTopUps(userId int, keyword string, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&TopUp{}).Where("user_id = ? AND create_time >= ?", userId, topUpQueryCutoff())
	if keyword != "" {
		pattern, perr := sanitizeLikePattern(keyword)
		if perr != nil {
			tx.Rollback()
			return nil, 0, perr
		}
		query = query.Where("trade_no LIKE ? ESCAPE '!'", pattern)
	}

	if err = query.Limit(searchTopUpCountHardLimit).Count(&total).Error; err != nil {
		tx.Rollback()
		common.SysError("failed to count search topups: " + err.Error())
		return nil, 0, errors.New("搜索充值记录失败")
	}

	if err = query.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		common.SysError("failed to search topups: " + err.Error())
		return nil, 0, errors.New("搜索充值记录失败")
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	return topups, total, nil
}

// SearchAllTopUps 按订单号搜索全平台充值记录（管理员使用，不限制时间窗口）
func SearchAllTopUps(keyword string, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&TopUp{})
	if keyword != "" {
		pattern, perr := sanitizeLikePattern(keyword)
		if perr != nil {
			tx.Rollback()
			return nil, 0, perr
		}
		query = query.Where("trade_no LIKE ? ESCAPE '!'", pattern)
	}

	if err = query.Limit(searchTopUpCountHardLimit).Count(&total).Error; err != nil {
		tx.Rollback()
		common.SysError("failed to count search topups: " + err.Error())
		return nil, 0, errors.New("搜索充值记录失败")
	}

	if err = query.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		common.SysError("failed to search topups: " + err.Error())
		return nil, 0, errors.New("搜索充值记录失败")
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	return topups, total, nil
}

// ManualCompleteTopUp 管理员手动完成订单并给用户充值
func ManualCompleteTopUp(tradeNo string, callerIp string) error {
	if tradeNo == "" {
		return errors.New("未提供订单号")
	}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	var userId int
	var quotaToAdd int
	var payMoney float64
	var paymentMethod string

	err := DB.Transaction(func(tx *gorm.DB) error {
		topUp := &TopUp{}
		// 行级锁，避免并发补单
		if err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(topUp).Error; err != nil {
			return errors.New("充值订单不存在")
		}

		// 幂等处理：已成功直接返回
		if topUp.Status == common.TopUpStatusSuccess {
			return nil
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("订单状态不是待支付，无法补单")
		}

		// 计算应充值额度：
		// - Stripe 订单：Money 代表经分组倍率换算后的美元数量，直接 * QuotaPerUnit
		// - 其他订单（如易支付）：Amount 为美元数量，* QuotaPerUnit
		if topUp.PaymentProvider == PaymentProviderStripe {
			dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
			quotaToAdd = int(decimal.NewFromFloat(topUp.Money).Mul(dQuotaPerUnit).IntPart())
		} else {
			dAmount := decimal.NewFromInt(topUp.Amount)
			dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
			quotaToAdd = int(dAmount.Mul(dQuotaPerUnit).IntPart())
		}
		if quotaToAdd <= 0 {
			return errors.New("无效的充值额度")
		}

		// 标记完成
		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Save(topUp).Error; err != nil {
			return err
		}

		// 增加用户额度：管理员补单是确认订单已实际支付，走已收款到账入口
		// CreditPaidTopUpTx，不能被「新售卖软上限」拒绝（方案 §3.2）。
		if err := CreditPaidTopUpTx(tx, topUp.UserId, quotaToAdd); err != nil {
			return err
		}

		userId = topUp.UserId
		payMoney = topUp.Money
		paymentMethod = topUp.PaymentMethod
		return nil
	})

	if err != nil {
		return err
	}

	// 事务外记录日志，避免阻塞
	RecordTopupLog(userId, fmt.Sprintf("管理员补单成功，充值金额: %v，支付金额：%f", logger.FormatQuota(quotaToAdd), payMoney), callerIp, paymentMethod, "admin")
	return nil
}
func RechargeCreem(referenceId string, customerEmail string, customerName string, callerIp string) (err error) {
	if referenceId == "" {
		return errors.New("未提供支付单号")
	}

	var quota int64
	topUp := &TopUp{}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(refCol+" = ?", referenceId).First(topUp).Error
		if err != nil {
			return errors.New("充值订单不存在")
		}

		if topUp.PaymentProvider != PaymentProviderCreem {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("充值订单状态错误")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		err = tx.Save(topUp).Error
		if err != nil {
			return err
		}

		// Creem 直接使用 Amount 作为充值额度（整数）
		quota = topUp.Amount
		if quota <= 0 {
			return errors.New("无效的充值额度")
		}

		// 如果有客户邮箱，尝试更新用户邮箱（仅当用户邮箱为空时）
		if customerEmail != "" {
			var user User
			if err := tx.Where("id = ?", topUp.UserId).First(&user).Error; err != nil {
				return err
			}
			if user.Email == "" {
				if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).Update("email", customerEmail).Error; err != nil {
					return err
				}
			}
		}

		// 正向额度增加走已收款到账入口 CreditPaidTopUpTx（方案 §3.2）
		if err := CreditPaidTopUpTx(tx, topUp.UserId, int(quota)); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		common.SysError("creem topup failed: " + err.Error())
		return errors.New("充值失败，请稍后重试")
	}

	RecordTopupLog(topUp.UserId, fmt.Sprintf("使用Creem充值成功，充值额度: %v，支付金额：%.2f", quota, topUp.Money), callerIp, topUp.PaymentMethod, PaymentMethodCreem)

	return nil
}

func RechargeWaffo(tradeNo string, callerIp string) (err error) {
	if tradeNo == "" {
		return errors.New("未提供支付单号")
	}

	var quotaToAdd int
	topUp := &TopUp{}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(topUp).Error
		if err != nil {
			return errors.New("充值订单不存在")
		}

		if topUp.PaymentProvider != PaymentProviderWaffo {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status == common.TopUpStatusSuccess {
			return nil // 幂等：已成功直接返回
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("充值订单状态错误")
		}

		dAmount := decimal.NewFromInt(topUp.Amount)
		dQuotaPerUnit := decimal.NewFromFloat(common.QuotaPerUnit)
		quotaToAdd = int(dAmount.Mul(dQuotaPerUnit).IntPart())
		if quotaToAdd <= 0 {
			return errors.New("无效的充值额度")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Save(topUp).Error; err != nil {
			return err
		}

		// 正向额度增加走已收款到账入口 CreditPaidTopUpTx（方案 §3.2）
		if err := CreditPaidTopUpTx(tx, topUp.UserId, quotaToAdd); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		common.SysError("waffo topup failed: " + err.Error())
		return errors.New("充值失败，请稍后重试")
	}

	if quotaToAdd > 0 {
		RecordTopupLog(topUp.UserId, fmt.Sprintf("Waffo充值成功，充值额度: %v，支付金额: %.2f", logger.FormatQuota(quotaToAdd), topUp.Money), callerIp, topUp.PaymentMethod, PaymentMethodWaffo)
	}

	return nil
}

func RechargeWaffoPancake(tradeNo string) (err error) {
	if tradeNo == "" {
		return errors.New("未提供支付单号")
	}

	var quotaToAdd int
	topUp := &TopUp{}

	refCol := "`trade_no`"
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		refCol = `"trade_no"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(refCol+" = ?", tradeNo).First(topUp).Error
		if err != nil {
			return errors.New("充值订单不存在")
		}

		if topUp.PaymentProvider != PaymentProviderWaffoPancake {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status == common.TopUpStatusSuccess {
			return nil
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("充值订单状态错误")
		}

		quotaToAdd = int(decimal.NewFromInt(topUp.Amount).Mul(decimal.NewFromFloat(common.QuotaPerUnit)).IntPart())
		if quotaToAdd <= 0 {
			return errors.New("无效的充值额度")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Save(topUp).Error; err != nil {
			return err
		}

		// 正向额度增加走已收款到账入口 CreditPaidTopUpTx（方案 §3.2）
		if err := CreditPaidTopUpTx(tx, topUp.UserId, quotaToAdd); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		common.SysError("waffo pancake topup failed: " + err.Error())
		return errors.New("充值失败，请稍后重试")
	}

	if quotaToAdd > 0 {
		RecordLog(topUp.UserId, LogTypeTopup, fmt.Sprintf("Waffo Pancake充值成功，充值额度: %v，支付金额: %.2f", logger.FormatQuota(quotaToAdd), topUp.Money))
	}

	return nil
}
