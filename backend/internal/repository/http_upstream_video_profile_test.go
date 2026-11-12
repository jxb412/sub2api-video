//go:build unit

package repository

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamVideoProfilePreservesBlockingDeadlineAndPoolIsolation(t *testing.T) {
	svc := &httpUpstreamService{cfg: &config.Config{Gateway: config.GatewayConfig{ResponseHeaderTimeout: 300}}}
	settings := svc.resolvePoolSettings(config.ConnectionPoolIsolationAccountProxy, 7)
	videoSettings := svc.applyProfilePoolSettings(settings, service.HTTPUpstreamProfileVideo)
	videoMode := svc.resolveProtocolMode(service.HTTPUpstreamProfileVideo, "", nil)
	transport, err := buildUpstreamTransport(videoSettings, nil, videoMode)
	require.NoError(t, err)
	defer transport.CloseIdleConnections()
	require.Equal(t, 20*time.Minute, transport.ResponseHeaderTimeout)
	require.Equal(t, 7, transport.MaxConnsPerHost)
	require.True(t, transport.ForceAttemptHTTP2)
	ordinary := svc.applyProfilePoolSettings(settings, service.HTTPUpstreamProfileDefault)
	require.Equal(t, 300*time.Second, ordinary.responseHeaderTimeout)
	require.NotEqual(t, buildCacheKey(config.ConnectionPoolIsolationAccountProxy, "proxy", 1, videoMode), buildCacheKey(config.ConnectionPoolIsolationAccountProxy, "proxy", 1, upstreamProtocolModeDefault))
	req, err := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(t.Context(), service.HTTPUpstreamProfileVideo), http.MethodPost, "https://video.example", nil)
	require.NoError(t, err)
	require.Equal(t, service.HTTPUpstreamProfileVideo, service.HTTPUpstreamProfileFromContext(req.Context()))
}
