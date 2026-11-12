package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	MinimaxVideoProtocolGetoken = "getoken"
	MinimaxVideoProtocolLL2     = "ll2"
	DefaultLL2BaseURL           = "https://api.ll2.cn"
)

// Missing protocol is the original Getoken format. Never infer it from a URL.
func MinimaxVideoProtocol(account *Account) (string, error) {
	protocol := ""
	if account != nil {
		protocol = strings.TrimSpace(account.GetCredential("minimax_video_protocol"))
	}
	switch protocol {
	case "", MinimaxVideoProtocolGetoken:
		return MinimaxVideoProtocolGetoken, nil
	case MinimaxVideoProtocolLL2:
		return protocol, nil
	default:
		return "", fmt.Errorf("unsupported MiniMax video protocol")
	}
}

func minimaxProtocolBaseURL(account *Account, protocol string) string {
	if protocol != MinimaxVideoProtocolLL2 {
		return MinimaxVideoBaseURL(account)
	}
	base := strings.TrimSpace(account.GetCredential("base_url"))
	if base == "" {
		base = DefaultLL2BaseURL
	}
	return strings.TrimRight(base, "/")
}

func minimaxCredentialHash(account *Account) string {
	headers := make(http.Header)
	account.ApplyHeaderOverrides(headers)
	for name := range headers {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Idempotency-Key") {
			delete(headers, name)
		}
	}
	payload, _ := json.Marshal(struct {
		Key     string
		Headers http.Header
	}{strings.TrimSpace(account.GetCredential("api_key")), headers})
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

// Freeze the endpoint and credential identity, but never copy secrets into task JSON.
// Switching suppliers means adding a new account; old tasks retain their original account.
func (s *OpenAIGatewayService) SnapshotMinimaxVideoProtocol(task *MinimaxVideoTask, account *Account, body []byte) error {
	protocol, err := MinimaxVideoProtocol(account)
	if err != nil {
		return err
	}
	if protocol == MinimaxVideoProtocolLL2 && account.SupportsAccountQuota() && account.HasAnyQuotaLimit() {
		return fmt.Errorf("LL2 does not report monetary cost; account cost quotas are unsupported, use user/API-key quotas and the supplier budget instead")
	}
	base, err := s.validateUpstreamBaseURL(minimaxProtocolBaseURL(account, protocol))
	if err != nil {
		return err
	}
	task.UpstreamProtocol, task.UpstreamBaseURL = protocol, base
	task.UpstreamCredentialHash = minimaxCredentialHash(account)
	task.RequestedDurationSeconds = task.DurationSeconds
	if protocol == MinimaxVideoProtocolLL2 {
		task.UpstreamRequestBody = append([]byte(nil), body...)
	}
	return nil
}

func (s *OpenAIGatewayService) minimaxBoundTaskAccount(ctx context.Context, task *MinimaxVideoTask) (*Account, string, error) {
	if s == nil || s.accountRepo == nil || task == nil {
		return nil, "", fmt.Errorf("video account repository unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, task.AccountID)
	if err != nil {
		return nil, "", err
	}
	if account == nil || strings.TrimSpace(account.GetCredential("api_key")) == "" {
		return nil, "", fmt.Errorf("video account credential unavailable")
	}
	protocol, err := MinimaxVideoProtocol(account)
	if err != nil {
		return nil, "", err
	}
	expected := task.UpstreamProtocol
	if expected == "" {
		expected = MinimaxVideoProtocolGetoken
	}
	if expected != protocol {
		return nil, "", fmt.Errorf("video account protocol changed; retain the original account for pending tasks")
	}
	base, err := s.validateUpstreamBaseURL(minimaxProtocolBaseURL(account, protocol))
	if err != nil {
		return nil, "", err
	}
	if (task.UpstreamBaseURL != "" && task.UpstreamBaseURL != base) ||
		(task.UpstreamCredentialHash != "" && task.UpstreamCredentialHash != minimaxCredentialHash(account)) {
		return nil, "", fmt.Errorf("video account endpoint or credential changed; restore the original account to resume its tasks")
	}
	return account, base, nil
}

func (s *OpenAIGatewayService) MinimaxVideoRequest(account *Account, info MinimaxVideoRequestInfo, model string) (string, []byte, error) {
	protocol, err := MinimaxVideoProtocol(account)
	if err != nil {
		return "", nil, err
	}
	var body []byte
	if protocol == MinimaxVideoProtocolLL2 {
		body, err = BuildLL2MinimaxVideoUpstreamBody(info, model)
	} else {
		body, err = BuildMinimaxVideoUpstreamBody(info, model)
	}
	if err != nil {
		return "", nil, err
	}
	base, err := s.validateUpstreamBaseURL(minimaxProtocolBaseURL(account, protocol))
	if err != nil {
		return "", nil, err
	}
	target, err := buildMinimaxProtocolURL(protocol, base, MinimaxVideoEndpointCreate, "")
	return target, body, err
}

func buildMinimaxProtocolURL(protocol, base string, endpoint GrokMediaEndpoint, id string) (string, error) {
	if protocol == MinimaxVideoProtocolLL2 {
		return BuildLL2MinimaxVideoURL(base, endpoint, id)
	}
	if protocol != "" && protocol != MinimaxVideoProtocolGetoken {
		return "", fmt.Errorf("unsupported MiniMax video protocol")
	}
	return BuildMinimaxVideoURL(base, endpoint, id)
}
