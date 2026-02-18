package stream

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPClient wraps http.Client with cookie/user-agent injection and error detection.
type HTTPClient struct {
	client    *http.Client
	cookies   string
	userAgent string
}

// NewHTTPClient creates an HTTP client with TLS verification disabled and
// optional cookie/user-agent headers.
func NewHTTPClient(cookies, userAgent string) *HTTPClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	return &HTTPClient{
		client:    &http.Client{Transport: transport},
		cookies:   cookies,
		userAgent: userAgent,
	}
}

// Get fetches a URL and returns the body as a string.
func (h *HTTPClient) Get(ctx context.Context, url string) (string, error) {
	b, err := h.GetBytes(ctx, url)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GetBytes fetches a URL and returns the body as bytes.
func (h *HTTPClient) GetBytes(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if h.userAgent != "" {
		req.Header.Set("User-Agent", h.userAgent)
	}
	if h.cookies != "" {
		for _, pair := range strings.Split(h.cookies, ";") {
			parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(parts) == 2 {
				req.AddCookie(&http.Cookie{
					Name:  strings.TrimSpace(parts[0]),
					Value: strings.TrimSpace(parts[1]),
				})
			}
		}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	body := string(b)

	if strings.Contains(body, "<title>Just a moment...</title>") {
		return nil, ErrCloudflareBlocked
	}
	if strings.Contains(body, "Verify your age") {
		return nil, ErrAgeVerification
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrPrivateStream
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return b, nil
}
