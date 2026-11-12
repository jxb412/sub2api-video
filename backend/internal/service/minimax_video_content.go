package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Download availability is independent of generation and billing state.
var (
	ErrVideoContentExpired     = errors.New("video content has expired")
	ErrVideoContentUnavailable = errors.New("video content is unavailable")
)

// MinimaxVideoRequestURL applies the same operator URL policy as other providers.
func (s *OpenAIGatewayService) MinimaxVideoRequestURL(account *Account) (string, error) {
	protocol, err := MinimaxVideoProtocol(account)
	if err != nil {
		return "", err
	}
	base, err := s.validateUpstreamBaseURL(minimaxProtocolBaseURL(account, protocol))
	if err != nil {
		return "", err
	}
	return buildMinimaxProtocolURL(protocol, base, MinimaxVideoEndpointCreate, "")
}

func minimaxVideoSameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return port(a) == port(b)
}

func (s *OpenAIGatewayService) minimaxVideoContentURL(raw, base string) (*url.URL, *url.URL, error) {
	origin, err := url.Parse(base)
	if err != nil || origin.Host == "" || origin.User != nil {
		return nil, nil, fmt.Errorf("invalid video origin")
	}
	reference, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("invalid video URL")
	}
	target := origin.ResolveReference(reference)
	if err := s.validateMinimaxVideoContentURL(target, origin); err != nil {
		return nil, nil, err
	}
	return target, origin, nil
}

func (s *OpenAIGatewayService) validateMinimaxVideoContentURL(target, origin *url.URL) error {
	if target == nil || target.User != nil || target.Fragment != "" {
		return fmt.Errorf("invalid video URL")
	}
	if _, err := s.validateOutboundURL(target.String()); err != nil {
		return fmt.Errorf("video URL rejected by security policy")
	}
	allowPrivate := minimaxVideoSameOrigin(target, origin) && s.cfg != nil && s.cfg.Security.URLAllowlist.AllowPrivateHosts
	if !allowPrivate && urlvalidator.IsBlockedHost(target.Hostname()) {
		return fmt.Errorf("private video host is not allowed")
	}
	return nil
}

// Resolve at dial time and connect to the validated IP, preventing DNS rebinding.
func minimaxVideoContentDialer(origin *url.URL, allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !(allowPrivate && strings.EqualFold(host, origin.Hostname())) && (!ip.IP.IsGlobalUnicast() || urlvalidator.IsBlockedHost(ip.IP.String())) {
				return nil, fmt.Errorf("private video address is not allowed")
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("video host has no addresses")
		}
		return nil, lastErr
	}
}

// StreamMinimaxVideoContent authenticates only to the configured provider origin.
// CDN URLs and redirects never receive the provider bearer token.
func (s *OpenAIGatewayService) StreamMinimaxVideoContent(c *gin.Context, task *MinimaxVideoTask, baseURL, token string) error {
	if s == nil || c == nil || task == nil || strings.TrimSpace(task.VideoURL) == "" {
		return fmt.Errorf("video content is unavailable")
	}
	if task.UpstreamBaseURL != "" {
		baseURL = task.UpstreamBaseURL
	}
	if task.UpstreamProtocol == MinimaxVideoProtocolLL2 {
		// LL2 publishes public video URLs. Never attach its API key, even to its own origin.
		token = ""
	} else if task.Provider != VideoProviderSeedance && task.AccountID > 0 {
		account, boundBase, err := s.minimaxBoundTaskAccount(c.Request.Context(), task)
		if err != nil {
			return err
		}
		baseURL, token = boundBase, account.GetCredential("api_key")
	}
	ll2 := task.UpstreamProtocol == MinimaxVideoProtocolLL2
	if ll2 {
		if err := validateLL2MinimaxPublicHTTPSURL(task.VideoURL); err != nil {
			return err
		}
	}
	target, origin, err := s.minimaxVideoContentURL(task.VideoURL, baseURL)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = minimaxVideoContentDialer(origin, !ll2 && s.cfg != nil && s.cfg.Security.URLAllowlist.AllowPrivateHosts)
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 10 * time.Minute, Transport: transport}
	authenticate := func(req *http.Request) {
		setMinimaxVideoDownloadAuthorization(req, origin, token, ll2)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many video redirects")
		}
		if ll2 {
			if err := validateLL2MinimaxPublicHTTPSURL(req.URL.String()); err != nil {
				return err
			}
		}
		if err := s.validateMinimaxVideoContentURL(req.URL, origin); err != nil {
			return err
		}
		authenticate(req)
		return nil
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	authenticate(req)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusGone {
			return ErrVideoContentExpired
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict {
			// Getoken's content endpoint returns 409 even after the file expires.
			// A read-only status check distinguishes that from a temporary failure.
			// Do not change the completed task, replay generation, or touch billing.
			_ = resp.Body.Close()
			if task.Provider != VideoProviderSeedance && !ll2 && minimaxVideoContentExpired(c.Request.Context(), client, task, baseURL, token) {
				return ErrVideoContentExpired
			}
			return ErrVideoContentUnavailable
		}
		return fmt.Errorf("video upstream returned %d", resp.StatusCode)
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") && contentType != "application/octet-stream" {
		return fmt.Errorf("upstream did not return video content")
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": task.ID + ".mp4"}))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, err = io.Copy(c.Writer, resp.Body)
	return err
}

func minimaxVideoContentExpired(ctx context.Context, client *http.Client, task *MinimaxVideoTask, baseURL, token string) bool {
	if task.UpstreamID == "" {
		return false
	}
	target, err := BuildMinimaxVideoURL(baseURL, MinimaxVideoEndpointStatus, task.UpstreamID)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	queryClient := *client
	// Status credentials may only be sent to the bound provider URL.
	queryClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := queryClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 || !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	id := strings.TrimSpace(root.Get("request_id").String())
	if id == "" {
		id = strings.TrimSpace(root.Get("id").String())
	}
	return id == task.UpstreamID && strings.EqualFold(strings.TrimSpace(root.Get("status").String()), "expired")
}

func setMinimaxVideoDownloadAuthorization(req *http.Request, origin *url.URL, token string, public bool) {
	req.Header.Del("Authorization")
	if !public && minimaxVideoSameOrigin(req.URL, origin) && strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}
