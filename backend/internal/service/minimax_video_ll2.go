package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

// BuildLL2MinimaxVideoURL preserves an operator's proxy prefix while replacing
// only a trailing API version. LL2 exposes no task content download endpoint.
func BuildLL2MinimaxVideoURL(base string, endpoint GrokMediaEndpoint, taskID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(base, "#") {
		return "", fmt.Errorf("invalid LL2 MiniMax base URL")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("invalid LL2 MiniMax base URL port")
		}
	}
	prefix := strings.TrimRight(parsed.EscapedPath(), "/")
	if strings.HasSuffix(prefix, "/v1") || strings.HasSuffix(prefix, "/v2") {
		prefix = prefix[:len(prefix)-3]
	}
	suffix := "/v2/video_generation"
	switch endpoint {
	case MinimaxVideoEndpointCreate:
	case MinimaxVideoEndpointStatus:
		id := strings.TrimSpace(taskID)
		if id == "" {
			return "", fmt.Errorf("LL2 MiniMax task ID is required")
		}
		if err := validateUpstreamPathSegment("LL2 MiniMax task ID", id); err != nil {
			return "", err
		}
		suffix = "/v2/query/video_generation/" + id
	default:
		return "", fmt.Errorf("unsupported LL2 MiniMax endpoint: %s", endpoint)
	}
	parsed.Path, err = url.PathUnescape(prefix + suffix)
	if err != nil {
		return "", fmt.Errorf("invalid LL2 MiniMax base URL path")
	}
	parsed.RawPath = prefix + suffix
	return parsed.String(), nil
}

type ll2MinimaxContentItem struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *ll2MinimaxMediaURL `json:"image_url,omitempty"`
	VideoURL *ll2MinimaxMediaURL `json:"video_url,omitempty"`
	AudioURL *ll2MinimaxMediaURL `json:"audio_url,omitempty"`
	Role     string              `json:"role,omitempty"`
}

type ll2MinimaxMediaURL struct {
	URL string `json:"url"`
}

// NormalizeLL2MinimaxVideoRequest validates the complete content contract before
// account admission. Content is authoritative when supplied; compatibility
// prompt/image_url requests are translated once into the same representation.
// Media probing (dimensions, duration, codecs, DNS and redirects) belongs to the
// upstream downloader; this pure validator checks URL syntax and literal hosts.
func NormalizeLL2MinimaxVideoRequest(info MinimaxVideoRequestInfo) (MinimaxVideoRequestInfo, error) {
	if info.DurationSeconds < 1 || info.DurationSeconds > 15 {
		return info, fmt.Errorf("LL2 MiniMax duration must be an integer between 1 and 15")
	}
	if info.Resolution != "" && !strings.EqualFold(info.Resolution, "768p") {
		return info, fmt.Errorf("LL2 MiniMax supports only 768P resolution")
	}
	info.Resolution = "768P"
	if info.Ratio == "" {
		info.Ratio = "16:9"
	}
	switch info.Ratio {
	case "21:9", "16:9", "4:3", "1:1", "3:4", "9:16", "adaptive":
	default:
		return info, fmt.Errorf("unsupported LL2 MiniMax ratio")
	}

	content := info.Content
	if len(content) == 0 {
		items := []ll2MinimaxContentItem{{Type: "text", Text: info.Prompt}}
		for _, imageURL := range info.InputImageURLs {
			role := "reference_image"
			if len(info.InputImageURLs) == 1 {
				role = "first_frame"
			}
			items = append(items, ll2MinimaxContentItem{Type: "image_url", ImageURL: &ll2MinimaxMediaURL{URL: imageURL}, Role: role})
		}
		content, _ = json.Marshal(items)
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(content, &rawItems); err != nil || len(rawItems) == 0 || len(rawItems) > 13 {
		return info, fmt.Errorf("LL2 MiniMax content must contain one text item and at most 12 media items")
	}
	items := make([]ll2MinimaxContentItem, 0, len(rawItems))
	textCount, imageCount, visualCount := 0, 0, 0
	roles := make(map[string]int)
	info.InputImageURLs = nil
	for index, raw := range rawItems {
		fields, err := ll2MinimaxJSONObject(raw)
		if err != nil {
			return info, fmt.Errorf("invalid LL2 MiniMax content item %d: %w", index, err)
		}
		typeName, err := ll2MinimaxRequiredString(fields, "type")
		if err != nil {
			return info, fmt.Errorf("LL2 MiniMax content item %d requires a string type", index)
		}
		item := ll2MinimaxContentItem{Type: typeName}
		allowed := map[string]bool{"type": true}
		if typeName == "text" {
			allowed["text"] = true
			text, err := ll2MinimaxRequiredString(fields, "text")
			if err != nil || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 7000 {
				return info, fmt.Errorf("LL2 MiniMax text must contain 1 to 7000 Unicode characters")
			}
			item.Text = text
			info.Prompt = text
			textCount++
		} else {
			allowed[typeName], allowed["role"] = true, true
			switch typeName {
			case "image_url", "video_url", "audio_url":
			default:
				return info, fmt.Errorf("unsupported LL2 MiniMax content type")
			}
			media, err := ll2MinimaxJSONObject(fields[typeName])
			if err != nil || len(media) != 1 {
				return info, fmt.Errorf("LL2 MiniMax %s must contain only a URL string", typeName)
			}
			mediaURL, err := ll2MinimaxRequiredString(media, "url")
			if err != nil || validateLL2MinimaxPublicHTTPSURL(mediaURL) != nil {
				return info, fmt.Errorf("LL2 MiniMax %s requires a public HTTPS URL", typeName)
			}
			if _, exists := fields["role"]; exists {
				item.Role, err = ll2MinimaxRequiredString(fields, "role")
				if err != nil || item.Role == "" {
					return info, fmt.Errorf("LL2 MiniMax media role must be a nonempty string")
				}
			}
			switch typeName {
			case "image_url":
				if item.Role != "" && item.Role != "first_frame" && item.Role != "last_frame" && item.Role != "reference_image" {
					return info, fmt.Errorf("invalid LL2 MiniMax image role")
				}
				item.ImageURL = &ll2MinimaxMediaURL{URL: mediaURL}
				imageCount++
				visualCount++
				info.InputImageURLs = append(info.InputImageURLs, mediaURL)
			case "video_url":
				if item.Role != "reference_video" {
					return info, fmt.Errorf("LL2 MiniMax video role must be reference_video")
				}
				item.VideoURL = &ll2MinimaxMediaURL{URL: mediaURL}
				visualCount++
			case "audio_url":
				if item.Role != "reference_audio" {
					return info, fmt.Errorf("LL2 MiniMax audio role must be reference_audio")
				}
				item.AudioURL = &ll2MinimaxMediaURL{URL: mediaURL}
			}
			roles[item.Role]++
		}
		for field := range fields {
			if !allowed[field] {
				return info, fmt.Errorf("unsupported LL2 MiniMax content field %q", field)
			}
		}
		items = append(items, item)
	}
	if textCount != 1 {
		return info, fmt.Errorf("LL2 MiniMax content requires exactly one text item")
	}
	if roles[""] > 0 {
		if imageCount != 1 {
			return info, fmt.Errorf("LL2 MiniMax requests with multiple images require explicit roles")
		}
		for index := range items {
			if items[index].Type == "image_url" && items[index].Role == "" {
				items[index].Role = "first_frame"
				roles["first_frame"]++
			}
		}
	}
	if roles["first_frame"] > 1 || roles["last_frame"] > 1 {
		return info, fmt.Errorf("LL2 MiniMax permits at most one first frame and one last frame")
	}
	referenceCount := roles["reference_image"] + roles["reference_video"] + roles["reference_audio"]
	if roles["reference_image"] > 9 || roles["reference_video"] > 3 || roles["reference_audio"] > 3 || referenceCount > 12 {
		return info, fmt.Errorf("LL2 MiniMax reference limits are 9 images, 3 videos, 3 audio clips and 12 total")
	}
	if (roles["first_frame"] > 0 || roles["last_frame"] > 0) && referenceCount > 0 {
		return info, fmt.Errorf("LL2 MiniMax keyframes cannot be combined with reference media")
	}
	if info.Ratio == "adaptive" && visualCount == 0 {
		return info, fmt.Errorf("LL2 MiniMax adaptive ratio requires an image or video")
	}
	info.Content, _ = json.Marshal(items)
	return info, nil
}

func BuildLL2MinimaxVideoUpstreamBody(info MinimaxVideoRequestInfo, upstreamModel string) ([]byte, error) {
	info, err := NormalizeLL2MinimaxVideoRequest(info)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = strings.TrimSpace(info.Model)
	}
	if model != "MiniMax-H3" {
		return nil, fmt.Errorf("LL2 MiniMax upstream model must be MiniMax-H3")
	}
	return json.Marshal(struct {
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
		Resolution string          `json:"resolution"`
		Duration   int             `json:"duration"`
		Ratio      string          `json:"ratio"`
	}{model, info.Content, info.Resolution, info.DurationSeconds, info.Ratio})
}

// ParseLL2MinimaxVideoUpstreamResponse distinguishes creation receipts from
// status envelopes. HTTP request_id is never interpreted as a generation ID.
func ParseLL2MinimaxVideoUpstreamResponse(body []byte) (MinimaxVideoTask, error) {
	var task MinimaxVideoTask
	root, err := ll2MinimaxJSONObject(body)
	if err != nil {
		return task, fmt.Errorf("invalid LL2 MiniMax response: %w", err)
	}
	if value := root["error"]; len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return task, fmt.Errorf("LL2 MiniMax upstream returned an error")
	}
	if value := root["success"]; bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
		return task, fmt.Errorf("LL2 MiniMax upstream rejected the request")
	}
	rawTask, querying := root["task"]
	if !querying {
		task.UpstreamID, err = ll2MinimaxRequiredString(root, "task_id")
		if err != nil || strings.TrimSpace(task.UpstreamID) == "" {
			return task, fmt.Errorf("LL2 MiniMax creation response is missing task_id")
		}
		task.UpstreamID = strings.TrimSpace(task.UpstreamID)
		if err := validateUpstreamPathSegment("LL2 MiniMax task ID", task.UpstreamID); err != nil {
			return task, err
		}
		task.Status = MinimaxVideoStatusQueued
		return task, nil
	}
	fields, err := ll2MinimaxJSONObject(rawTask)
	if err != nil {
		return task, fmt.Errorf("invalid LL2 MiniMax task envelope")
	}
	task.UpstreamID, err = ll2MinimaxRequiredString(fields, "id")
	if err != nil || strings.TrimSpace(task.UpstreamID) == "" {
		return task, fmt.Errorf("LL2 MiniMax query response is missing task.id")
	}
	task.UpstreamID = strings.TrimSpace(task.UpstreamID)
	if err := validateUpstreamPathSegment("LL2 MiniMax task ID", task.UpstreamID); err != nil {
		return task, err
	}
	status, err := ll2MinimaxRequiredString(fields, "status")
	if err != nil {
		return task, fmt.Errorf("LL2 MiniMax query response is missing task.status")
	}
	switch status {
	case "queued":
		task.Status = MinimaxVideoStatusQueued
	case "running":
		task.Status = MinimaxVideoStatusProcessing
	case "succeeded":
		task.Status = MinimaxVideoStatusDone
	case "failed", "cancelled":
		task.Status = MinimaxVideoStatusError
	default:
		return task, fmt.Errorf("unknown LL2 MiniMax task status")
	}
	for name, destination := range map[string]*string{"model": &task.Model, "resolution": &task.Resolution, "ratio": &task.Ratio} {
		if _, exists := fields[name]; exists {
			*destination, err = ll2MinimaxRequiredString(fields, name)
			if err != nil {
				return task, fmt.Errorf("invalid LL2 MiniMax task.%s", name)
			}
		}
	}
	if duration, exists := fields["duration"]; exists {
		if err := json.Unmarshal(duration, &task.DurationSeconds); err != nil || task.DurationSeconds < 1 || task.DurationSeconds > 15 {
			return task, fmt.Errorf("invalid LL2 MiniMax task.duration")
		}
	}
	if usageRaw, exists := fields["usage"]; exists && !bytes.Equal(bytes.TrimSpace(usageRaw), []byte("null")) {
		usage, err := ll2MinimaxJSONObject(usageRaw)
		if err != nil {
			return task, fmt.Errorf("invalid LL2 MiniMax task.usage")
		}
		if seconds, exists := usage["output_seconds"]; exists {
			var outputSeconds int
			if err := json.Unmarshal(seconds, &outputSeconds); err != nil || outputSeconds < 0 || outputSeconds > 15 || (outputSeconds == 0 && task.Status == MinimaxVideoStatusDone) {
				return task, fmt.Errorf("invalid LL2 MiniMax task.usage.output_seconds")
			}
			if outputSeconds > 0 {
				task.DurationSeconds = outputSeconds
				task.OutputSecondsReported = true
			}
		}
	}
	if contentRaw, exists := fields["content"]; exists && !bytes.Equal(bytes.TrimSpace(contentRaw), []byte("null")) {
		content, err := ll2MinimaxJSONObject(contentRaw)
		if err != nil {
			return task, fmt.Errorf("invalid LL2 MiniMax task.content")
		}
		if _, exists := content["url"]; exists {
			task.VideoURL, err = ll2MinimaxRequiredString(content, "url")
			if err != nil || validateLL2MinimaxPublicHTTPSURL(task.VideoURL) != nil {
				return task, fmt.Errorf("invalid LL2 MiniMax video URL")
			}
		}
	}
	if errorRaw, exists := fields["error"]; exists && !bytes.Equal(bytes.TrimSpace(errorRaw), []byte("null")) {
		if task.Status != MinimaxVideoStatusError {
			return task, fmt.Errorf("LL2 MiniMax response has contradictory task status and error")
		}
		var message string
		if json.Unmarshal(errorRaw, &message) == nil {
			task.Error = strings.TrimSpace(message)
		} else if errorFields, err := ll2MinimaxJSONObject(errorRaw); err == nil {
			code, _ := ll2MinimaxRequiredString(errorFields, "code")
			message, _ := ll2MinimaxRequiredString(errorFields, "message")
			task.Error = strings.TrimSpace(strings.TrimSpace(code) + ": " + strings.TrimSpace(message))
			task.Error = strings.Trim(task.Error, ": ")
		} else {
			return task, fmt.Errorf("invalid LL2 MiniMax task.error")
		}
	}
	if task.Status == MinimaxVideoStatusError && task.Error == "" {
		task.Error = "LL2 MiniMax task " + status
	}
	if task.Status == MinimaxVideoStatusDone && task.VideoURL == "" {
		return task, fmt.Errorf("completed LL2 MiniMax response is missing task.content.url")
	}
	return task, nil
}

// Decode objects without silently accepting duplicate keys, null or trailing
// JSON. This is shared by content validation and task envelope parsing.
func ll2MinimaxJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON object")
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("invalid JSON field")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate JSON field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid JSON value")
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after JSON object")
	}
	return fields, nil
}

func ll2MinimaxRequiredString(fields map[string]json.RawMessage, name string) (string, error) {
	value, exists := fields[name]
	if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return "", fmt.Errorf("missing string field %s", name)
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", fmt.Errorf("invalid string field %s", name)
	}
	return decoded, nil
}

func validateLL2MinimaxPublicHTTPSURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || raw != strings.TrimSpace(raw) || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || strings.Contains(raw, "#") {
		return fmt.Errorf("invalid public HTTPS URL")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("invalid HTTPS URL port")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if urlvalidator.IsBlockedHost(host) {
		return fmt.Errorf("private media host is not allowed")
	}
	if address := net.ParseIP(host); address != nil {
		if !address.IsGlobalUnicast() {
			return fmt.Errorf("nonpublic media address is not allowed")
		}
		return nil
	}
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return fmt.Errorf("nonpublic media host is not allowed")
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid media hostname")
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return fmt.Errorf("invalid media hostname")
			}
		}
	}
	// Numeric and hexadecimal address alternatives must not bypass IP checks.
	lastLabel := labels[len(labels)-1]
	if _, err := strconv.ParseUint(lastLabel, 10, 64); err == nil || strings.HasPrefix(lastLabel, "0x") {
		return fmt.Errorf("noncanonical media IP address is not allowed")
	}
	return nil
}
