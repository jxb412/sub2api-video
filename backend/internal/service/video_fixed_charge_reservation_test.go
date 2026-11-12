//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type videoFixedChargeRenewCache struct {
	BillingCache
	mu      sync.Mutex
	amounts map[string]map[string]float64
	expires map[string]time.Time
}

func (c *videoFixedChargeRenewCache) GetSubscriptionCache(context.Context, int64, int64) (*SubscriptionCacheData, error) {
	return &SubscriptionCacheData{Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (c *videoFixedChargeRenewCache) ReserveInflightScope(_ context.Context, scope, id string, amount float64, ttl time.Duration) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.amounts[scope] == nil {
		c.amounts[scope] = make(map[string]float64)
	}
	var pending float64
	for other, cost := range c.amounts[scope] {
		if other != id {
			pending += cost
		}
	}
	c.amounts[scope][id] = amount
	c.expires[scope] = time.Now().Add(ttl)
	return pending, nil
}

func (c *videoFixedChargeRenewCache) ReleaseInflightScope(_ context.Context, scope, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.amounts[scope], id)
	return nil
}

func TestMinimaxVideoFixedChargeRenewsSubscriptionAndKeyUntilWorkerDone(t *testing.T) {
	cache := &videoFixedChargeRenewCache{amounts: make(map[string]map[string]float64), expires: make(map[string]time.Time)}
	cfg := &config.Config{}
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 1}
	billing := NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	limit := 1.0
	group := &Group{ID: 3, SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &limit}
	started := time.Now()
	reservation, err := billing.ReserveFixedCharge(context.Background(), &User{ID: 1}, &APIKey{ID: 2, Quota: 1}, group, &UserSubscription{ID: 4}, 0.5)
	require.NoError(t, err)
	require.NotNil(t, reservation)
	t.Cleanup(reservation.HandlerDone)
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if time.Since(started) < 1100*time.Millisecond || len(cache.expires) != 2 {
			return false
		}
		for scope, expiry := range cache.expires {
			if !expiry.After(time.Now()) || len(cache.amounts[scope]) != 1 {
				return false
			}
			for _, amount := range cache.amounts[scope] {
				if amount != 0.5 {
					return false
				}
			}
		}
		return true
	}, 3*time.Second, 20*time.Millisecond, "scope reservations must outlive their original TTL without adding a second charge")
	reservation.HandlerDone()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, amounts := range cache.amounts {
		require.Empty(t, amounts)
	}
}
