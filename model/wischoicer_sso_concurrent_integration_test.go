//go:build integration

// Compiled ONLY with `go test -tags integration ./...`. Runs on real MySQL/PostgreSQL
// in CI (.github/workflows/integration.yml, database: [mysql, postgres]).
//
// Exact contract (记星 b8a7653c / 张驰 B decision): production MySQL/PostgreSQL row locks
// guarantee the concurrent loser blocks until winner commits → sees consumed_at → returns
// ErrAuthFlowConsumed. SQLite single-write-lock may return SQLITE_BUSY instead — that's a
// test artifact, not a safety gap (loser is still fail-closed: no session). The SQLite
// concurrent test in controller/wischoicer_sso_callback_test.go asserts the weaker but
// security-equivalent "one success + one non-success + one side effect"; THIS test pins
// the strict "one success + one ErrAuthFlowConsumed" on the production DB engine.
package model

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestConsumeAuthFlowWithAction_ConcurrentF2_ExactConsumedOnRowLockDB(t *testing.T) {
	setupWischoicerIntegrationDB(t)
	require.NoError(t, DB.AutoMigrate(&AuthFlow{}))

	tok, _, err := CreateAuthFlow(AuthFlowCreate{
		Purpose:   AuthFlowPurposeWischoicerSSOCode,
		Intent:    AuthFlowIntentLogin,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	require.NoError(t, err)

	allReady := sync.WaitGroup{}
	allReady.Add(2)
	start := make(chan struct{})
	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		successes      int
		consumedErrors int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allReady.Done()
			<-start
			_, e := ConsumeAuthFlowWithAction(tok, AuthFlowMatch{Purpose: AuthFlowPurposeWischoicerSSOCode}, func(tx *gorm.DB, f *AuthFlow) error {
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				successes++
			} else if errors.Is(e, ErrAuthFlowConsumed) {
				consumedErrors++
			}
		}()
	}
	allReady.Wait()
	close(start)
	wg.Wait()

	assert.Equal(t, 1, successes, "exactly one consume succeeds")
	assert.Equal(t, 1, consumedErrors, "exactly one ErrAuthFlowConsumed (row lock guarantees loser = consumed)")
}
