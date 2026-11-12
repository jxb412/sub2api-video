package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVideoBillingSettingsGroupOverridesEnvironment(t *testing.T) {
	t.Setenv(envVideoBillingMode, VideoBillingModeUpstreamCost)
	t.Setenv(envVideoTiers, "768p:1080p,1024p:1080p,broken:4k")
	group := &Group{
		VideoBillingMode:     VideoBillingModeLocalPrice,
		VideoResolutionTiers: map[string]string{" 768P ": "HD"},
	}
	settings := VideoBillingSettingsFor(group)
	require.Equal(t, VideoBillingModeLocalPrice, settings.Mode)
	require.Equal(t, "720p", settings.Tiers["768p"])
	require.Equal(t, "1080p", settings.Tiers["1024p"])
	require.NotContains(t, settings.Tiers, "broken")
	require.Equal(t, VideoBillingModeUpstreamCost, VideoBillingSettingsFor(nil).Mode)
	require.Equal(t, map[string]string{" 768P ": "HD"}, group.VideoResolutionTiers)
}

func TestVideoResolutionTierDefaultAndOverride(t *testing.T) {
	prices := map[string]map[string]float64{"minimax-h3": {"480p": 0.03, "720p": 0.10, "1080p": 0.20}}
	price := LookupVideoModelPriceWithTiers(prices, nil, "MiniMax-H3", "768p")
	require.NotNil(t, price)
	require.Equal(t, 0.10, *price)
	price = LookupVideoModelPriceWithTiers(prices, map[string]string{"768p": "1080p"}, "MiniMax-H3", "768p")
	require.Equal(t, 0.20, *price)
}

func TestNormalizeVideoResolutionTiers(t *testing.T) {
	tiers, err := NormalizeVideoResolutionTiers(map[string]string{" 768P ": "HD", "1024p": "fhd"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"768p": "720p", "1024p": "1080p"}, tiers)
	for _, input := range []map[string]string{
		{"768p": "4k"},
		{"": "720p"},
		{"768P": "720p", "768p": "1080p"},
	} {
		_, err := NormalizeVideoResolutionTiers(input)
		require.Error(t, err)
	}
	cleared, err := NormalizeVideoResolutionTiers(map[string]string{})
	require.NoError(t, err)
	require.Empty(t, cleared)
}

func TestGroupVideoBillingRejectsInvalidConfigurationBeforePersistence(t *testing.T) {
	svc := &adminServiceImpl{}
	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{VideoBillingMode: "typo"})
	require.ErrorContains(t, err, "video_billing_mode")
	_, err = svc.UpdateGroup(context.Background(), 1, &UpdateGroupInput{
		VideoResolutionTiers: map[string]string{"768p": "1080i"},
	})
	require.ErrorContains(t, err, "invalid video billing tier")
	mode, _, err := normalizeGroupVideoBilling("local_price", nil)
	require.NoError(t, err)
	require.Equal(t, VideoBillingModeLocalPrice, mode)
}

func TestGroupVideoBillingSurvivesAuthCacheAndDuplication(t *testing.T) {
	group := &Group{ID: 5, VideoBillingMode: VideoBillingModeUpstreamCost, VideoResolutionTiers: map[string]string{"768p": "1080p"}}
	key := &APIKey{User: &User{ID: 1}, Group: group}
	svc := &APIKeyService{}
	snapshot := svc.snapshotFromAPIKey(context.Background(), key)
	restored := svc.snapshotToAPIKey("sk-test", snapshot)
	require.Equal(t, group.VideoBillingMode, restored.Group.VideoBillingMode)
	require.Equal(t, group.VideoResolutionTiers, restored.Group.VideoResolutionTiers)
	restored.Group.VideoResolutionTiers["768p"] = "480p"
	require.Equal(t, "1080p", snapshot.Group.VideoResolutionTiers["768p"])

	copy := cloneGroupForDuplicate(group, "test-copy")
	require.Equal(t, group.VideoBillingMode, copy.VideoBillingMode)
	require.Equal(t, group.VideoResolutionTiers, copy.VideoResolutionTiers)
	copy.VideoResolutionTiers["768p"] = "720p"
	require.Equal(t, "1080p", group.VideoResolutionTiers["768p"])
}
