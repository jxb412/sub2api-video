//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func ll2MinimaxTestInfo(content string) MinimaxVideoRequestInfo {
	info := MinimaxVideoRequestInfo{Model: "MiniMax-H3", Prompt: "A calm lake", DurationSeconds: 5, Resolution: "768p"}
	if content != "" {
		info.Content = json.RawMessage(content)
	}
	return info
}

func TestLL2MinimaxVideoURL(t *testing.T) {
	for _, test := range []struct{ base, prefix string }{
		{"https://hub.ll2.cn", "https://hub.ll2.cn"},
		{"https://hub.ll2.cn/", "https://hub.ll2.cn"},
		{"https://hub.ll2.cn/v1/", "https://hub.ll2.cn"},
		{"https://hub.ll2.cn/v2", "https://hub.ll2.cn"},
		{"https://proxy.example/prefix/v1", "https://proxy.example/prefix"},
		{"https://proxy.example/prefix/v2/", "https://proxy.example/prefix"},
		{"https://proxy.example/prefix", "https://proxy.example/prefix"},
		{"https://proxy.example/a%20b/v2", "https://proxy.example/a%20b"},
	} {
		create, err := BuildLL2MinimaxVideoURL(test.base, MinimaxVideoEndpointCreate, "")
		require.NoError(t, err)
		require.Equal(t, test.prefix+"/v2/video_generation", create)
		query, err := BuildLL2MinimaxVideoURL(test.base, MinimaxVideoEndpointStatus, "task_123")
		require.NoError(t, err)
		require.Equal(t, test.prefix+"/v2/query/video_generation/task_123", query)
	}
	for _, base := range []string{"", "hub.ll2.cn", "file:///tmp/x", "https://user:secret@hub.ll2.cn", "https://hub.ll2.cn?api_key=secret", "https://hub.ll2.cn?", "https://hub.ll2.cn#anchor", "https://hub.ll2.cn#", "https://hub.ll2.cn:99999"} {
		_, err := BuildLL2MinimaxVideoURL(base, MinimaxVideoEndpointCreate, "")
		require.Error(t, err, base)
	}
	for _, id := range []string{"", " ", "../secret", "a/b", "a?key=x", "a#x", "a%2fb"} {
		_, err := BuildLL2MinimaxVideoURL("https://hub.ll2.cn", MinimaxVideoEndpointStatus, id)
		require.Error(t, err, id)
	}
	_, err := BuildLL2MinimaxVideoURL("https://hub.ll2.cn", MinimaxVideoEndpointContent, "task_123")
	require.Error(t, err)
}

func TestLL2MinimaxVideoCompatibilityBody(t *testing.T) {
	info := ll2MinimaxTestInfo("")
	info.InputImageURLs = []string{"https://media.example/first.png"}
	body, err := BuildLL2MinimaxVideoUpstreamBody(info, "MiniMax-H3")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"MiniMax-H3","content":[{"type":"text","text":"A calm lake"},{"type":"image_url","image_url":{"url":"https://media.example/first.png"},"role":"first_frame"}],"resolution":"768P","duration":5,"ratio":"16:9"}`, string(body))
	info.InputImageURLs = append(info.InputImageURLs, "https://media.example/second.png")
	body, err = BuildLL2MinimaxVideoUpstreamBody(info, "")
	require.NoError(t, err)
	require.Equal(t, "reference_image", gjson.GetBytes(body, "content.1.role").String())
	require.Equal(t, "reference_image", gjson.GetBytes(body, "content.2.role").String())
	info.Model = "public-model-alias"
	body, err = BuildLL2MinimaxVideoUpstreamBody(info, "MiniMax-H3")
	require.NoError(t, err)
	require.Equal(t, "MiniMax-H3", gjson.GetBytes(body, "model").String())
	_, err = BuildLL2MinimaxVideoUpstreamBody(info, "unknown-model")
	require.Error(t, err)
}

func TestLL2MinimaxVideoMultimodalBodyPreservesRoles(t *testing.T) {
	content := `[{"type":"text","text":"Match <Picture 1> and <Video 1>, then <Audio 1>."},{"type":"image_url","image_url":{"url":"https://media.example/image.png?signature=a%2Bb"},"role":"reference_image"},{"type":"video_url","video_url":{"url":"https://media.example/video.mov"},"role":"reference_video"},{"type":"audio_url","audio_url":{"url":"https://media.example/audio.m4a"},"role":"reference_audio"}]`
	info := ll2MinimaxTestInfo(content)
	info.Ratio = "adaptive"
	normalized, err := NormalizeLL2MinimaxVideoRequest(info)
	require.NoError(t, err)
	require.Equal(t, "Match <Picture 1> and <Video 1>, then <Audio 1>.", normalized.Prompt)
	require.Equal(t, []string{"https://media.example/image.png?signature=a%2Bb"}, normalized.InputImageURLs)
	require.JSONEq(t, content, string(normalized.Content))
	body, err := BuildLL2MinimaxVideoUpstreamBody(normalized, "")
	require.NoError(t, err)
	require.JSONEq(t, content, gjson.GetBytes(body, "content").Raw)
	require.Equal(t, "adaptive", gjson.GetBytes(body, "ratio").String())

	for _, ratio := range []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"} {
		info := ll2MinimaxTestInfo("")
		info.Ratio = ratio
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.NoError(t, err)
	}
}

func TestLL2MinimaxVideoRequestRejectsInvalidContent(t *testing.T) {
	for _, content := range []string{
		`null`, `{}`, `[]`, `[null]`, `["text"]`,
		`[{"type":"text","text":"x"}] {}`,
		`[{"type":"text","text":""}]`,
		`[{"type":"text","text":"  "}]`,
		`[{"type":"text","text":12}]`,
		`[{"type":"text","text":null}]`,
		`[{"type":"text","text":"x","role":"text"}]`,
		`[{"type":"text","text":"x","unsupported":true}]`,
		`[{"type":"text","text":"x","text":"y"}]`,
		`[{"type":"text","text":"x"},{"type":"text","text":"y"}]`,
		`[{"type":"image_url","image_url":{"url":"https://media.example/a.png"}}]`,
		`[{"type":5,"text":"x"}]`,
		`[{"type":"TEXT","text":"x"}]`,
		`[{"type":"text","text":"x"},{"type":"file","url":"https://media.example/a"}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":"https://media.example/a.png"}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":123}}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png","detail":"high"}}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png"},"role":null}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png"},"role":"reference_video"}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png"},"video_url":{"url":"https://media.example/b.mp4"}}]`,
		`[{"type":"text","text":"x"},{"type":"video_url","video_url":{"url":"https://media.example/a.mp4"}}]`,
		`[{"type":"text","text":"x"},{"type":"audio_url","audio_url":{"url":"https://media.example/a.mp3"},"role":"first_frame"}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png"}},{"type":"image_url","image_url":{"url":"https://media.example/b.png"},"role":"last_frame"}]`,
		`[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://media.example/a.png"},"role":"first_frame"},{"type":"audio_url","audio_url":{"url":"https://media.example/a.mp3"},"role":"reference_audio"}]`,
	} {
		_, err := NormalizeLL2MinimaxVideoRequest(ll2MinimaxTestInfo(content))
		require.Error(t, err, content)
	}
	for _, duration := range []int{-1, 0, 16, 120} {
		info := ll2MinimaxTestInfo("")
		info.DurationSeconds = duration
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.Error(t, err, duration)
	}
	for _, resolution := range []string{"720p", "1080p", "768", " 768P "} {
		info := ll2MinimaxTestInfo("")
		info.Resolution = resolution
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.Error(t, err, resolution)
	}
	for _, ratio := range []string{"adaptive", "2:1", "16:9 ", "ADAPTIVE"} {
		info := ll2MinimaxTestInfo("")
		info.Ratio = ratio
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.Error(t, err, ratio)
	}
	info := ll2MinimaxTestInfo(`[{"type":"text","text":"x"},{"type":"audio_url","audio_url":{"url":"https://media.example/a.mp3"},"role":"reference_audio"}]`)
	info.Ratio = "adaptive"
	_, err := NormalizeLL2MinimaxVideoRequest(info)
	require.Error(t, err)
	info.Ratio = "9:16"
	_, err = NormalizeLL2MinimaxVideoRequest(info)
	require.NoError(t, err)
}

func TestLL2MinimaxVideoTextLengthUsesUnicodeCharacters(t *testing.T) {
	info := ll2MinimaxTestInfo("")
	info.Prompt = strings.Repeat("界", 7000)
	_, err := NormalizeLL2MinimaxVideoRequest(info)
	require.NoError(t, err)
	info.Prompt += "界"
	_, err = NormalizeLL2MinimaxVideoRequest(info)
	require.Error(t, err)
}

func TestLL2MinimaxVideoReferenceAndKeyframeLimits(t *testing.T) {
	for _, test := range []struct {
		images, videos, audios int
		valid                  bool
	}{
		{9, 0, 0, true}, {10, 0, 0, false}, {0, 3, 0, true}, {0, 4, 0, false},
		{0, 0, 3, true}, {0, 0, 4, false}, {6, 3, 3, true}, {7, 3, 3, false},
	} {
		var items []map[string]any
		items = append(items, map[string]any{"type": "text", "text": "x"})
		for _, media := range []struct {
			typeName, role string
			count          int
		}{{"image_url", "reference_image", test.images}, {"video_url", "reference_video", test.videos}, {"audio_url", "reference_audio", test.audios}} {
			for index := 0; index < media.count; index++ {
				items = append(items, map[string]any{"type": media.typeName, "role": media.role, media.typeName: map[string]string{"url": fmt.Sprintf("https://media.example/%d", index)}})
			}
		}
		content, err := json.Marshal(items)
		require.NoError(t, err)
		_, err = NormalizeLL2MinimaxVideoRequest(ll2MinimaxTestInfo(string(content)))
		if test.valid {
			require.NoError(t, err, test)
		} else {
			require.Error(t, err, test)
		}
	}
	for _, roles := range [][]string{{"first_frame"}, {"last_frame"}, {"first_frame", "last_frame"}, {"first_frame", "first_frame"}, {"last_frame", "last_frame"}} {
		items := []map[string]any{{"type": "text", "text": "x"}}
		for _, role := range roles {
			items = append(items, map[string]any{"type": "image_url", "role": role, "image_url": map[string]string{"url": "https://media.example/a.png"}})
		}
		content, err := json.Marshal(items)
		require.NoError(t, err)
		info := ll2MinimaxTestInfo(string(content))
		info.Ratio = "adaptive"
		_, err = NormalizeLL2MinimaxVideoRequest(info)
		if len(roles) == 2 && roles[0] == roles[1] {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestLL2MinimaxVideoMediaURLs(t *testing.T) {
	for _, mediaURL := range []string{
		"http://media.example/a", "data:image/png;base64,AAAA", "/relative.png", "https://localhost/a",
		"https://localhost./a", "https://a.localhost/a", "https://10.1.2.3/a", "https://127.0.0.1/a",
		"https://169.254.169.254/a", "https://[::1]/a", "https://[fd00::1]/a", "https://[::ffff:127.0.0.1]/a",
		"https://224.0.0.1/a", "https://0.0.0.0/a", "https://2130706433/a", "https://0177.0.0.1/a",
		"https://0x7f000001/a", "https://internal/a", "https://media.local/a", "https://media.internal/a",
		"https://user:password@media.example/a", "https://media.example/a#fragment", "https://media.example:0/a",
		"https://media.example:65536/a", " https://media.example/a", "https://media..example/a",
	} {
		info := ll2MinimaxTestInfo("")
		info.InputImageURLs = []string{mediaURL}
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.Error(t, err, mediaURL)
	}
	for _, mediaURL := range []string{"https://media.example/a?token=x%2By", "https://8.8.8.8/a", "https://[2606:4700:4700::1111]/a"} {
		info := ll2MinimaxTestInfo("")
		info.InputImageURLs = []string{mediaURL}
		_, err := NormalizeLL2MinimaxVideoRequest(info)
		require.NoError(t, err, mediaURL)
	}
}

func TestLL2MinimaxVideoResponseStatesAndOutputSeconds(t *testing.T) {
	created, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(`{"task_id":"task_123","request_id":"http-call-456"}`))
	require.NoError(t, err)
	require.Equal(t, "task_123", created.UpstreamID)
	require.Equal(t, MinimaxVideoStatusQueued, created.Status)
	require.False(t, created.OutputSecondsReported)
	for status, expected := range map[string]string{"queued": MinimaxVideoStatusQueued, "running": MinimaxVideoStatusProcessing, "succeeded": MinimaxVideoStatusDone, "failed": MinimaxVideoStatusError, "cancelled": MinimaxVideoStatusError} {
		response := fmt.Sprintf(`{"task":{"id":"task_123","status":%q,"content":{"url":"https://media.example/video.mp4"},"model":"MiniMax-H3","resolution":"768P","ratio":"16:9","duration":5,"usage":{"output_seconds":4,"total_seconds":999,"input_seconds":10,"input_image_count":2}}}`, status)
		task, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(response))
		require.NoError(t, err)
		require.Equal(t, expected, task.Status)
		require.Equal(t, "task_123", task.UpstreamID)
		require.Equal(t, "MiniMax-H3", task.Model)
		require.Equal(t, "768P", task.Resolution)
		require.Equal(t, "16:9", task.Ratio)
		require.Equal(t, 4, task.DurationSeconds)
		require.True(t, task.OutputSecondsReported)
		require.False(t, task.UsageReported)
		require.False(t, task.UpstreamCostReported)
		require.Zero(t, task.UpstreamCostUSD)
		require.Zero(t, task.InputTokens)
		require.Zero(t, task.OutputTokens)
	}
	for _, usage := range []string{"", `,"usage":null`, `,"usage":{"total_seconds":5,"input_seconds":2}`} {
		task, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(`{"task":{"id":"task_123","status":"succeeded","duration":5,"content":{"url":"https://media.example/result.mp4"}` + usage + `}}`))
		require.NoError(t, err)
		require.Equal(t, 5, task.DurationSeconds)
		require.False(t, task.OutputSecondsReported)
		require.False(t, task.UsageReported)
		require.False(t, task.UpstreamCostReported)
	}
	failed, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(`{"task":{"id":"task_123","status":"failed","error":{"code":"content_policy_violation","message":"Denied"},"usage":{"output_seconds":0}}}`))
	require.NoError(t, err)
	require.Equal(t, MinimaxVideoStatusError, failed.Status)
	require.Equal(t, "content_policy_violation: Denied", failed.Error)
	require.False(t, failed.OutputSecondsReported)
}

func TestLL2MinimaxVideoResponseRejectsMalformedOrErrorEnvelopes(t *testing.T) {
	for _, response := range []string{
		`{}`, `null`, `[]`, `{"task_id":"x"} {}`, `{"task_id":"x","task_id":"y"}`,
		`{"request_id":"http-call"}`, `{"id":"not-a-task-id"}`, `{"task_id":5}`, `{"task_id":null}`, `{"task_id":""}`,
		`{"task_id":"../bad"}`, `{"task_id":"x","error":{"message":"denied"}}`, `{"task_id":"x","success":false}`,
		`{"request_id":"http-call","error":{"message":"denied"}}`, `{"task":null}`, `{"task":[]}`,
		`{"task":{"status":"queued"}}`, `{"task":{"id":"" ,"status":"queued"}}`, `{"task":{"id":5,"status":"queued"}}`,
		`{"task":{"id":"x"}}`, `{"task":{"id":"x","status":"unknown"}}`, `{"task":{"id":"x","status":null}}`,
		`{"task":{"id":"x","status":"succeeded"}}`,
		`{"task":{"id":"x","status":"running","error":{"code":"denied"}}}`,
		`{"task":{"id":"x","status":"succeeded","content":{"url":"http://media.example/a"}}}`,
		`{"task":{"id":"x","status":"succeeded","content":{"url":"https://127.0.0.1/a"}}}`,
		`{"task":{"id":"x","status":"succeeded","content":{"url":12}}}`,
		`{"task":{"id":"x","status":"queued","duration":"5"}}`,
		`{"task":{"id":"x","status":"queued","usage":[]}}`,
	} {
		_, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(response))
		require.Error(t, err, response)
	}
	for _, seconds := range []string{`0`, `-1`, `16`, `1.5`, `"5"`, `null`, `true`, `{}`} {
		_, err := ParseLL2MinimaxVideoUpstreamResponse([]byte(`{"task":{"id":"x","status":"succeeded","content":{"url":"https://media.example/result.mp4"},"usage":{"output_seconds":` + seconds + `}}}`))
		require.Error(t, err, seconds)
	}
}
