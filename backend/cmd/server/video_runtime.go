package main

import (
	"context"
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type videoTaskRuntime struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Keep the durable task worker in the Wire graph so regeneration cannot drop
// database initialization or recovery. Shutdown precedes closing DB/cache.
func provideVideoTaskRuntime(db *sql.DB, gateway *service.OpenAIGatewayService, billing *service.BillingCacheService, apiKeys *service.APIKeyService) *videoTaskRuntime {
	service.SetVideoTaskDB(db)
	service.SetVideoBillingCache(billing)
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &videoTaskRuntime{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(runtime.done)
		gateway.StartMinimaxVideoRecovery(ctx, apiKeys)
	}()
	return runtime
}

func (r *videoTaskRuntime) stop() {
	if r != nil {
		r.cancel()
		<-r.done
	}
}
