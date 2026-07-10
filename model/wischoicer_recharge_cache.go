package model

import (
	"context"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/bytedance/gopkg/util/gopool"
)

// ---------------------------------------------------------------------------
// 两阶段缓存删除 + 后台扫描任务
// ---------------------------------------------------------------------------

// 入账事务提交后，quota 已持久化但 user:{id} 缓存可能仍持有旧 quota。
// 第一次删除后，并发的 quota 更新（batch updater / relay settle）可能在
// 删除与下次读之间的窗口里把旧值重新写入缓存。因此先删除一次并标记
// VERIFY_PENDING，延迟后再删一次，成功后标记 SUCCESS。
//
// 任一阶段失败或进程崩溃，持久化的 cache_status 让后台扫描任务继续重试，
// 重试只删缓存、不再修改 quota（方案 §5.4 步骤 7、§9.1 缓存同步行）。

var wischoicerCacheInflight sync.Map // orderNo -> struct{} ：防止两阶段删除重复调度

// startTwoPhaseCacheInvalidation 启动两阶段缓存删除。
//
// 事务提交后调用：第一次同步删除 + 标记 VERIFY_PENDING，延迟后异步第二次删除。
// 失败由后台扫描任务兜底。
func startTwoPhaseCacheInvalidation(userID int, orderNo string) {
	if _, loaded := wischoicerCacheInflight.LoadOrStore(orderNo, struct{}{}); loaded {
		return
	}

	// 第一次删除。
	firstErr := invalidateUserCache(userID)
	if firstErr != nil {
		common.SysError("wischoicer credit first cache delete failed: orderNo=" + orderNo + " err=" + firstErr.Error())
		// 第一次失败：标记 PENDING + 设置重试时间，交给扫描任务。
		markCacheRetry(orderNo, WischoicerCacheStatusPending)
		wischoicerCacheInflight.Delete(orderNo)
		return
	}

	// 第一次成功：标记 VERIFY_PENDING。
	if err := markCacheRetry(orderNo, WischoicerCacheStatusVerifyPending); err != nil {
		common.SysError("wischoicer credit mark verify_pending failed: orderNo=" + orderNo + " err=" + err.Error())
		wischoicerCacheInflight.Delete(orderNo)
		return
	}

	delay := time.Duration(common.WischoicerCacheSecondDeleteDelay) * time.Second
	if delay <= 0 {
		delay = 2 * time.Second
	}

	gopool.Go(func() {
		defer wischoicerCacheInflight.Delete(orderNo)

		time.Sleep(delay)

		// 第二次删除。
		if err := invalidateUserCache(userID); err != nil {
			common.SysError("wischoicer credit second cache delete failed: orderNo=" + orderNo + " err=" + err.Error())
			// 保持 VERIFY_PENDING，扫描任务会重试。
			return
		}

		// 第二次成功：标记 SUCCESS。
		now := common.GetTimestamp()
		result := DB.Model(&WischoicerRechargeCredit{}).
			Where("order_no = ? AND cache_status = ?", orderNo, WischoicerCacheStatusVerifyPending).
			Updates(map[string]interface{}{
				"cache_status":       WischoicerCacheStatusSuccess,
				"cache_next_retry_at": now,
			})
		if result.Error != nil {
			common.SysError("wischoicer credit mark cache success failed: orderNo=" + orderNo + " err=" + result.Error.Error())
		}
	})
}

// markCacheRetry 更新凭据的 cache_status 并设置重试时间为「最近可重试」。
func markCacheRetry(orderNo string, status int) error {
	retryAt := common.GetTimestamp() + int64(common.WischoicerCacheRetryInterval)
	result := DB.Model(&WischoicerRechargeCredit{}).
		Where("order_no = ?", orderNo).
		Updates(map[string]interface{}{
			"cache_status":        status,
			"cache_next_retry_at": retryAt,
		})
	return result.Error
}

// RunWischoicerCacheScanOnce 扫描一次待重试的缓存凭据，返回处理的条数。
//
// 只处理 SUCCESS 状态但 cache_status 非 SUCCESS 的凭据：重试缓存删除，不修改 quota。
// 调用方按周期调用（service 层 ticker）。
func RunWischoicerCacheScanOnce(ctx context.Context, limit int) (int, error) {
	_ = ctx
	if limit <= 0 {
		limit = 100
	}

	now := common.GetTimestamp()
	var credits []*WischoicerRechargeCredit
	err := DB.Where("status = ? AND cache_status != ? AND cache_next_retry_at <= ?",
		WischoicerCreditStatusSuccess, WischoicerCacheStatusSuccess, now).
		Order("cache_next_retry_at asc").
		Limit(limit).
		Find(&credits).Error
	if err != nil {
		return 0, err
	}

	processed := 0
	for _, credit := range credits {
		processed++
		if err := retryCacheInvalidationForCredit(credit); err != nil {
			common.SysError("wischoicer cache scan retry failed: orderNo=" + credit.OrderNo + " err=" + err.Error())
		}
	}
	return processed, nil
}

// retryCacheInvalidationForCredit 对单条凭据执行对应阶段的缓存删除。
func retryCacheInvalidationForCredit(credit *WischoicerRechargeCredit) error {
	switch credit.CacheStatus {
	case WischoicerCacheStatusPending:
		// 第一阶段未完成：删除缓存，成功后推进到 VERIFY_PENDING。
		if err := invalidateUserCache(credit.NewAPIUserId); err != nil {
			return scheduleCacheRetry(credit.OrderNo, WischoicerCacheStatusPending)
		}
		return markCacheRetry(credit.OrderNo, WischoicerCacheStatusVerifyPending)

	case WischoicerCacheStatusVerifyPending:
		// 第二阶段：删除缓存，成功后推进到 SUCCESS。
		if err := invalidateUserCache(credit.NewAPIUserId); err != nil {
			return scheduleCacheRetry(credit.OrderNo, WischoicerCacheStatusVerifyPending)
		}
		now := common.GetTimestamp()
		result := DB.Model(&WischoicerRechargeCredit{}).
			Where("order_no = ? AND cache_status = ?", credit.OrderNo, WischoicerCacheStatusVerifyPending).
			Updates(map[string]interface{}{
				"cache_status":        WischoicerCacheStatusSuccess,
				"cache_next_retry_at": now,
			})
		return result.Error

	default:
		return nil
	}
}

// scheduleCacheRetry 在删除失败时推迟下一次重试时间。
func scheduleCacheRetry(orderNo string, status int) error {
	retryAt := common.GetTimestamp() + int64(common.WischoicerCacheRetryInterval)
	result := DB.Model(&WischoicerRechargeCredit{}).
		Where("order_no = ?", orderNo).
		Updates(map[string]interface{}{
			"cache_status":        status,
			"cache_next_retry_at": retryAt,
		})
	return result.Error
}

// wischoicerCacheScanRunning 防止扫描任务并发重叠执行。
var wischoicerCacheScanRunning atomicBool

type atomicBool struct {
	v bool
	m sync.Mutex
}

func (a *atomicBool) compareAndSwap(expected bool, desired bool) bool {
	a.m.Lock()
	defer a.m.Unlock()
	if a.v != expected {
		return false
	}
	a.v = desired
	return true
}

// StartWischoicerCacheScanTask 启动后台缓存扫描任务（仅 master 节点）。
func StartWischoicerCacheScanTask() {
	if !common.IsMasterNode {
		return
	}
	interval := common.WischoicerCacheRetryInterval
	if interval <= 0 {
		interval = 60
	}
	gopool.Go(func() {
		logger.LogInfo(context.Background(), "wischoicer recharge cache scan task started")
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if !wischoicerCacheScanRunning.compareAndSwap(false, true) {
				continue
			}
			_, _ = RunWischoicerCacheScanOnce(context.Background(), 200)
			wischoicerCacheScanRunning.compareAndSwap(true, false)
		}
	})
}
