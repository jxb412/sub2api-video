//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMinimaxVideoAdmissionRequiresCapabilityAndDurableStorage(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		h, slots, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
		account := service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{24},
			Credentials: map[string]any{"api_key": "secret", "base_url": "https://video.example"}}
		if enabled {
			account.Credentials["openai_capabilities"] = []string{"minimax_video"}
		}
		repo := openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}
		cfg := &config.Config{RunMode: config.RunModeSimple}
		h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, bindings, cfg, nil,
			service.NewConcurrencyService(slots), nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
		service.SetVideoTaskDB(nil)
		c, w := grokMediaSlotContext(context.Background(), true)
		key, _ := middleware.GetAPIKeyFromContext(c)
		key.Group.Platform = service.PlatformMiniMax
		price := 0.1
		key.Group.VideoPrice720P = &price
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"MiniMax-H3","prompt":"waves"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.MinimaxVideoGeneration(c)
		if enabled {
			require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "Failed to create video task")
			require.Positive(t, slots.acquired, "MiniMax groups must select compatible OpenAI relay accounts")
		} else {
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		}
		require.Zero(t, upstream.calls, "must never submit billable work without a durable task")
		slots.assertReleased(t)
	}
}

func TestMinimaxVideoInflightReservationUsesBillingSnapshot(t *testing.T) {
	for _, multiplier := range []float64{0, 1, 1.5} {
		task := service.MinimaxVideoTask{
			Model: "MiniMax-H3", Resolution: "768p", DurationSeconds: 5,
			PriceConfigured: true, PricePerSecond: 0.1, RateMultiplier: multiplier,
			BillingMode: service.VideoBillingModeLocalPrice, BillingSnapshot: &service.MinimaxVideoBillingSnapshot{},
		}
		cfg := &config.Config{}
		cfg.Billing.InflightReservation.Enabled = true
		cache := &cachedBillingReservationCache{balance: 100}
		billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
		t.Cleanup(billing.Stop)
		key := &service.APIKey{ID: 1, User: &service.User{ID: 2}, Quota: 10}
		ctx, release, err := reserveMinimaxVideoCharge(context.Background(), billing, nil, key, nil, task, service.InflightEstimateRequest{})
		require.NoError(t, err)
		require.Equal(t, 0.5*multiplier, service.InflightReservationFromContext(ctx).Amount())
		release()
		require.Equal(t, float64(100), cache.balance, "reservation must never debit the balance")
	}
}

func TestMinimaxVideoFixedChargeRejectsInsufficientPayerHeadroom(t *testing.T) {
	for _, payer := range []string{"balance", "key", "subscription"} {
		t.Run(payer, func(t *testing.T) {
			cache := &cachedBillingReservationCache{balance: 100, sub: &service.SubscriptionCacheData{
				Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour), DailyUsage: 0.9,
			}}
			key := &service.APIKey{ID: 1, User: &service.User{ID: 2}}
			var subscription *service.UserSubscription
			var expected error
			switch payer {
			case "balance":
				cache.balance = 0.1
				expected = service.ErrInsufficientBalance
			case "key":
				key.Quota, key.QuotaUsed = 1, 0.9
				expected = service.ErrAPIKeyQuotaExhausted
			case "subscription":
				limit := 1.0
				key.Group = &service.Group{ID: 3, SubscriptionType: service.SubscriptionTypeSubscription, DailyLimitUSD: &limit}
				subscription = &service.UserSubscription{ID: 4}
				expected = service.ErrDailyLimitExceeded
			}
			cfg := &config.Config{}
			cfg.Billing.InflightReservation.Enabled = true
			billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			task := service.MinimaxVideoTask{PriceConfigured: true, PricePerSecond: 0.1, DurationSeconds: 5, RateMultiplier: 1, BillingMode: service.VideoBillingModeLocalPrice}
			_, release, err := reserveMinimaxVideoCharge(context.Background(), billing, nil, key, subscription, task, service.InflightEstimateRequest{})
			require.ErrorIs(t, err, expected)
			release()
			for _, scope := range cache.reserved {
				require.Empty(t, scope, "rejected admission must release all reservations")
			}
		})
	}
}
