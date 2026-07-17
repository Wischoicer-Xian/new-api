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
// setupWischoicerIntegrationDB lifts those scenarios onto real MySQL/PostgreSQL
// so FOR UPDATE, row locks, unique indexes and the int32 column width are
// actually exercised.
//
// Fail-closed (R9 P1-3): any container/connection failure is fatal (t.Fatalf),
// never skipped — a green `go test -tags integration ./model/...` must prove
// the DB semantics ran, not mask an unavailable Docker daemon as a pass. CI
// invokes the tag explicitly via .github/workflows/integration.yml.
package model

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"

	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	gormmysql "gorm.io/driver/mysql"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	// wisIntegrationMySQLImage matches the InnoDB semantics (FOR UPDATE, record
	// locks, error 1213 deadlock retry, strict int out-of-range) the integration
	// suite exists to prove.
	wisIntegrationMySQLImage    = "mysql:8.0"
	wisIntegrationPostgresImage = "postgres:16-alpine"

	// wisIntegrationStartup gives testcontainers enough room to pull the image on
	// a cold CI cache before the wait-strategy declares the container ready.
	wisIntegrationStartup = 3 * time.Minute
)

// setupWischoicerIntegrationDB starts a disposable MySQL or PostgreSQL
// testcontainer, migrates the tables the wischoicer feature touches, points the
// package-global DB at it, and switches common.MainDatabaseType so lockForUpdate emits real FOR UPDATE and
// UsingMainDatabase(SQLite) returns false. Cleanup restores the prior (SQLite)
// DB and type so non-integration tests are unaffected, then terminates the
// container.
//
// The existing TestMain (task_cas_test.go) opens an in-memory SQLite DB and
// remains active for the whole test binary; each integration test overrides the
// global for its own lifetime. Tests in this file must NOT call t.Parallel — DB
// is a process-wide global and concurrent swaps would clobber each other. Go
// runs tests sequentially within a package by default, which is sufficient.
func setupWischoicerIntegrationDB(t *testing.T) {
	t.Helper()

	startCtx, cancel := context.WithTimeout(context.Background(), wisIntegrationStartup)
	defer cancel()

	database := os.Getenv("WIS_INTEGRATION_DATABASE")
	if database == "" {
		database = string(common.DatabaseTypeMySQL)
	}
	var (
		dialector    gorm.Dialector
		databaseType common.DatabaseType
		terminate    func(context.Context) error
	)
	switch database {
	case string(common.DatabaseTypeMySQL):
		container, err := tcmysql.Run(startCtx, wisIntegrationMySQLImage, tcmysql.WithDatabase("newapi_test"))
		if err != nil {
			t.Fatalf("mysql testcontainer unavailable (integration gate is fail-closed): %v", err)
		}
		dsn, err := container.ConnectionString(startCtx, "parseTime=true", "loc=UTC")
		if err != nil {
			t.Fatalf("mysql testcontainer connection string: %v", err)
		}
		dialector = gormmysql.Open(dsn)
		databaseType = common.DatabaseTypeMySQL
		terminate = func(ctx context.Context) error { return container.Terminate(ctx) }
	case string(common.DatabaseTypePostgreSQL):
		container, err := tcpostgres.Run(startCtx, wisIntegrationPostgresImage,
			tcpostgres.WithDatabase("newapi_test"),
			tcpostgres.WithUsername("newapi"),
			tcpostgres.WithPassword("newapi_test"),
			// PostgreSQL initializes a temporary cluster and restarts once before
			// it is ready for client connections. Run does not install a wait
			// strategy by default, so opening GORM immediately can race that
			// restart and fail with connection reset by peer. The module's basic
			// strategy waits for both readiness log entries and the mapped port.
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("postgres testcontainer unavailable (integration gate is fail-closed): %v", err)
		}
		dsn, err := container.ConnectionString(startCtx, "sslmode=disable")
		if err != nil {
			t.Fatalf("postgres testcontainer connection string: %v", err)
		}
		dialector = gormpostgres.Open(dsn)
		databaseType = common.DatabaseTypePostgreSQL
		terminate = func(ctx context.Context) error { return container.Terminate(ctx) }
	default:
		t.Fatalf("unsupported WIS_INTEGRATION_DATABASE=%q; want mysql or postgres", database)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open %s gorm: %v", database, err)
	}

	// Migrate only the tables the wischoicer capacity/credit/Epay paths touch.
	// AutoMigrate respects each model's gorm tags (type:int for the 32-bit quota
	// column, uniqueIndex on order_no / trade_no / external_transaction_id).
	if err := db.AutoMigrate(
		&User{},
		&TopUp{},
		&WischoicerRechargeCredit{},
		&EpayPaymentAnomaly{},
		&Channel{},
		&ImageTaskExecution{},
		&TaskBillingLedger{},
		&ChannelRevision{},
		&Task{},
		// Token, UserSubscription, SubscriptionPlan back the §5.5/§5.6
		// image-task billing aggregate funding matrix: token deduction + funding
		// source choice requires these tables on every database. The default
		// subscription_first path queries user_subscriptions, so the integration
		// DB must migrate these for the concurrency matrix.
		&Token{},
		&UserSubscription{},
		&SubscriptionPlan{},
	); err != nil {
		t.Fatalf("auto-migrate %s schema: %v", database, err)
	}

	prevDB := DB
	prevType := common.MainDatabaseType()
	DB = db
	common.SetMainDatabaseType(databaseType)

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
		_ = terminate(termCtx)
	})
}
