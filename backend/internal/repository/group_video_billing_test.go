package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupVideoBillingPersistsAndHydratesAPIKey(t *testing.T) {
	keyRepo, client := newAPIKeyRepoSQLite(t)
	repo := newGroupRepositoryWithSQL(client, keyRepo.sql)
	ctx := context.Background()
	group := &service.Group{
		Name: "video-billing", Platform: service.PlatformOpenAI,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier: 1, VideoRateMultiplier: 1, DefaultValidityDays: 30,
		VideoBillingMode:     service.VideoBillingModeUpstreamCost,
		VideoResolutionTiers: map[string]string{"768p": "1080p"},
	}
	require.NoError(t, repo.Create(ctx, group))
	loaded, err := repo.GetByIDLite(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, group.VideoBillingMode, loaded.VideoBillingMode)
	require.Equal(t, group.VideoResolutionTiers, loaded.VideoResolutionTiers)

	user := mustCreateAPIKeyRepoUser(t, ctx, client, "video-billing@test.com")
	key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "sk-video-billing-test", Name: "Video", Status: service.StatusActive}
	require.NoError(t, keyRepo.Create(ctx, key))
	keyForAuth, err := keyRepo.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, group.VideoBillingMode, keyForAuth.Group.VideoBillingMode)
	require.Equal(t, group.VideoResolutionTiers, keyForAuth.Group.VideoResolutionTiers)

	loaded.VideoBillingMode = service.VideoBillingModeLocalPrice
	loaded.VideoResolutionTiers = map[string]string{}
	require.NoError(t, repo.Update(ctx, loaded))
	updated, err := repo.GetByIDLite(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, service.VideoBillingModeLocalPrice, updated.VideoBillingMode)
	require.Empty(t, updated.VideoResolutionTiers)
}
