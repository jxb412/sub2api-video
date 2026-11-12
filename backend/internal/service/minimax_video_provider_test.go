//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMinimaxVideoRateFollowsUserOverrideAndIndependentPolicy(t *testing.T) {
	rate := 1.7
	s := &OpenAIGatewayService{userGroupRateResolver: newUserGroupRateResolver(&userGroupRateResolverRepoStub{rate: &rate}, nil, time.Minute, nil, "test")}
	groupID := int64(12)
	key := &APIKey{UserID: 10, GroupID: &groupID, Group: &Group{RateMultiplier: 2, VideoRateMultiplier: 0.5}}
	require.Equal(t, 1.7, s.ResolveMinimaxVideoRateMultiplier(context.Background(), key))
	key.Group.VideoRateIndependent = true
	require.Equal(t, 0.5, s.ResolveMinimaxVideoRateMultiplier(context.Background(), key))
	key.Group.VideoRateMultiplier = 0
	require.Zero(t, s.ResolveMinimaxVideoRateMultiplier(context.Background(), key))
}

func TestMinimaxVideoRequestValidationAndImages(t *testing.T) {
	for _, body := range []string{
		`{"model":12,"prompt":"x"}`, `{"model":"MiniMax-H3","prompt":"x","duration":0}`,
		`{"model":"MiniMax-H3","prompt":"x","duration":1.5}`, `{"model":"MiniMax-H3","prompt":"x","duration":-1}`,
		`{"model":"MiniMax-H3","prompt":"x","resolution":"720p"}`, `{"model":"MiniMax-H3","image_url":{}}`,
	} {
		_, err := ParseMinimaxVideoRequest([]byte(body))
		require.Error(t, err, body)
	}
	info, err := ParseMinimaxVideoRequest([]byte(`{"model":"MiniMax-H3","image_url":["https://example.com/first.png","https://example.com/last.png"]}`))
	require.NoError(t, err)
	require.Equal(t, 5, info.DurationSeconds)
	require.Equal(t, "768p", info.Resolution)
	require.Len(t, info.InputImageURLs, 2)
	body, err := BuildMinimaxVideoUpstreamBody(info, "mapped-h3")
	require.NoError(t, err)
	require.Equal(t, "mapped-h3", gjson.GetBytes(body, "model").String())
	require.Len(t, gjson.GetBytes(body, "image_url").Array(), 2)
}

func TestMinimaxVideoCapabilityIsolation(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "secret", "base_url": "https://video.example", "openai_capabilities": []string{"minimax_video"},
	}}
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityMinimaxVideo))
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityGrokMediaGeneration))
	a.Platform = PlatformMiniMax
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityMinimaxVideo))
	a.Platform = PlatformGrok
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityMinimaxVideo))
	a.Platform = PlatformOpenAI
	delete(a.Credentials, "api_key")
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityMinimaxVideo))
}

func TestMinimaxVideoResponseStatesAndCosts(t *testing.T) {
	for input, want := range map[string]string{"completed": "done", "succeeded": "done", "failed": "error", "running": "processing"} {
		task, err := ParseMinimaxVideoUpstreamResponse([]byte(`{"status":"` + input + `","video":{"url":"https://video.example/result.mp4"},"getoken":{"cost_usd":"0.375"}}`))
		require.NoError(t, err)
		require.Equal(t, want, task.Status)
		require.Equal(t, 0.375, task.UpstreamCostUSD)
	}
	for _, cost := range []string{"NaN", "Inf", "-0.375", "0.375garbage"} {
		task, err := ParseMinimaxVideoUpstreamResponse([]byte(`{"getoken":{"cost_usd":"` + cost + `"}}`))
		require.NoError(t, err)
		require.Zero(t, task.UpstreamCostUSD)
	}
}

func TestMinimaxVideoContentURLPolicy(t *testing.T) {
	s := &OpenAIGatewayService{}
	target, origin, err := s.minimaxVideoContentURL("/v1/videos/id/content", "https://provider.example/proxy/v1")
	require.NoError(t, err)
	require.Equal(t, "https://provider.example/v1/videos/id/content", target.String())
	require.True(t, minimaxVideoSameOrigin(target, origin))
	cdn, _, err := s.minimaxVideoContentURL("https://cdn.example/video?signature=abc%2Fdef", origin.String())
	require.NoError(t, err)
	require.False(t, minimaxVideoSameOrigin(cdn, origin))
	require.Equal(t, "signature=abc%2Fdef", cdn.RawQuery)
	for _, raw := range []string{"http://provider.example/video", "https://127.0.0.1/video", "https://169.254.169.254/latest", "https://user:password@cdn.example/video", "file:///etc/passwd"} {
		_, _, err := s.minimaxVideoContentURL(raw, origin.String())
		require.Error(t, err, raw)
	}
	for _, raw := range []string{"https://provider.example:444/video", "https://sub.provider.example/video", "http://provider.example/video"} {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		require.False(t, minimaxVideoSameOrigin(u, origin))
	}
	u, _ := url.Parse("https://provider.example:443/video")
	require.True(t, minimaxVideoSameOrigin(u, origin))
	_, err = minimaxVideoContentDialer(origin, false)(context.Background(), "tcp", "127.0.0.1:80")
	require.ErrorContains(t, err, "private video address")
}

func TestMinimaxVideoContentStreamsAndRejectsUnsafeRedirect(t *testing.T) {
	var bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://127.0.0.2/private", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video-bytes"))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	s := &OpenAIGatewayService{cfg: cfg}
	for _, path := range []string{"/video", "/redirect"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/mmv_task/content", nil)
		err := s.StreamMinimaxVideoContent(c, &MinimaxVideoTask{ID: "mmv_task", VideoURL: path}, server.URL, "secret")
		require.Equal(t, "Bearer secret", bearer)
		if path == "/redirect" {
			require.Error(t, err)
			require.Empty(t, w.Body.String())
			continue
		}
		require.NoError(t, err)
		require.Equal(t, "video-bytes", w.Body.String())
		require.Contains(t, w.Header().Get("Content-Disposition"), "attachment")
		require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	}
}
