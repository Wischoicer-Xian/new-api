//go:build integration

// This file is compiled ONLY with `go test -tags integration ./...`. The
// default `go test ./...` (and `go build ./...`) never compiles it, so the
// testcontainers-go dependency does not affect local dev or the existing
// pr-check gate.
//
// Why this exists (第九轮 codex review P1-3 "资金正确性 CI/E2E 仍无一次真实绿色执行证据"):
// the wischoicer feature's money-critical invariants — reserve/release/credit
// idempotency, per-user quota capacity row lock, Epay atomic credit, int32
// hard-cap rollback — are exercised on in-memory SQLite by model/*_test.go.
// SQLite serializes every connection on a single write lock, so SELECT ... FOR
// UPDATE is a no-op (lockForUpdate skips the clause entirely), InnoDB record
// locks never engage, and the int column has no 32-bit boundary. SQLite green-lit
// the dual-prepay / capacity-oversell / overflow paths that the review flagged.
// setupWischoicerMySQLDB lifts those scenarios onto a real mysql:8.0 so FOR
// UPDATE, row locks, unique indexes and the int32 column width are actually
// exercised.
//
// Fail-closed (R9 P1-3): any container/connection failure is fatal (t.Fatalf),
// never skipped — a green `go test -tags integration ./model/...` must prove
// the DB semantics ran, not mask an unavailable Docker daemon as a pass. CI
// invokes the tag explicitly via .github/workflows/integration.yml.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// wisIntegrationMySQLImage matches the InnoDB semantics (FOR UPDATE, record
	// locks, error 1213 deadlock retry, strict int out-of-range) the integration
	// suite exists to prove.
	wisIntegrationMySQLImage = "mysql:8.0"

	// wisIntegrationStartup gives testcontainers enough room to pull the image on
	// a cold CI cache before the wait-strategy declares the container ready.
	wisIntegrationStartup = 3 * time.Minute
)

// setupWischoicerMySQLDB starts a disposable mysql:8.0 testcontainer, migrates
// the tables the wischoicer feature touches (users / top_ups /
// wischoicer_recharge_credits), points the package-global DB at it, and switches
// common.MainDatabaseType to MySQL so lockForUpdate emits real FOR UPDATE and
// UsingMainDatabase(SQLite) returns false. Cleanup restores the prior (SQLite)
// DB and type so non-integration tests are unaffected, then terminates the
// container.
//
// The existing TestMain (task_cas_test.go) opens an in-memory SQLite DB and
// remains active for the whole test binary; each integration test overrides the
// global for its own lifetime. Tests in this file must NOT call t.Parallel — DB
// is a process-wide global and concurrent swaps would clobber each other. Go
// runs tests sequentially within a package by default, which is sufficient.
func setupWischoicerMySQLDB(t *testing.T) {
	t.Helper()

	startCtx, cancel := context.WithTimeout(context.Background(), wisIntegrationStartup)
	defer cancel()

	container, err := tcmysql.Run(startCtx, wisIntegrationMySQLImage,
		tcmysql.WithDatabase("newapi_test"),
	)
	if err != nil {
		t.Fatalf("mysql testcontainer unavailable — Docker not running or image pull failed (integration tag requires Docker): %v", err)
	}

	// parseTime=true so DATETIME/TIMESTAMP columns round-trip into time.Time;
	// loc=UTC makes cross-goroutine time assertions deterministic regardless of
	// host tz. The GORMDeletedAt soft-delete column on User needs parseTime.
	dsn, err := container.ConnectionString(startCtx, "parseTime=true", "loc=UTC")
	if err != nil {
		t.Fatalf("mysql testcontainer connection string: %v", err)
	}

	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open mysql gorm: %v", err)
	}

	// Migrate only the tables the wischoicer capacity/credit/Epay paths touch.
	// AutoMigrate respects each model's gorm tags (type:int for the 32-bit quota
	// column, uniqueIndex on order_no / trade_no / external_transaction_id).
	if err := db.AutoMigrate(&User{}, &TopUp{}, &WischoicerRechargeCredit{}); err != nil {
		t.Fatalf("auto-migrate mysql schema: %v", err)
	}

	prevDB := DB
	prevType := common.MainDatabaseType()
	DB = db
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)

	t.Cleanup(func() {
		DB = prevDB
		common.SetMainDatabaseType(prevType)
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
		// Terminate with a fresh context: the startup ctx above is already
		// cancelled, and container shutdown must not be bounded by it.
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = container.Terminate(termCtx)
	})
}
