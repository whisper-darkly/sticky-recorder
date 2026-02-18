package cookies

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/whisper-darkly/sticky-recorder/logger"
)

// SourceConfig controls how cookie sources are loaded and refreshed.
type SourceConfig struct {
	ExternalEnabled bool          // STICKY_EXTERNAL_COOKIES
	SafeDomains     string        // STICKY_COOKIES_SAFE_DOMAINS
	JSONMode        bool          // STICKY_COOKIES_JSON
	RefreshInterval time.Duration // STICKY_COOKIES_REFRESH
	Driver          string        // for URL template {{.Driver}}
	Source          string        // for URL template {{.Source}}
}

type sourceKind int

const (
	kindLiteral sourceKind = iota
	kindFile
	kindURL
)

// Source represents a cookie source that can load and refresh cookie strings.
type Source struct {
	kind     sourceKind
	raw      string // original value (literal string, file path, or rendered URL)
	cfg      SourceConfig
	rendered string // for URL sources: template-rendered URL
}

// NewSource classifies the raw cookie string and validates access controls.
func NewSource(raw string, cfg SourceConfig) (*Source, error) {
	s := &Source{raw: raw, cfg: cfg}

	switch {
	case strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://"):
		s.kind = kindURL
	case strings.HasPrefix(raw, "file://"):
		s.kind = kindFile
		s.raw = strings.TrimPrefix(raw, "file://")
	default:
		s.kind = kindLiteral
	}

	// External sources require explicit opt-in
	if s.kind != kindLiteral && !cfg.ExternalEnabled {
		return nil, fmt.Errorf("external cookie sources require STICKY_EXTERNAL_COOKIES=1 (got %q)", raw)
	}

	// URL sources need safe domain validation
	if s.kind == kindURL {
		rendered, err := renderTemplate(raw, cfg)
		if err != nil {
			return nil, fmt.Errorf("render cookie URL template: %w", err)
		}
		s.rendered = rendered

		if err := validateSafeDomain(rendered, cfg.SafeDomains); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// Load fetches cookie strings from the source.
func (s *Source) Load(ctx context.Context) ([]string, error) {
	switch s.kind {
	case kindLiteral:
		return []string{s.raw}, nil
	case kindFile:
		return s.loadFile()
	case kindURL:
		return s.loadURL(ctx)
	default:
		return nil, fmt.Errorf("unknown source kind")
	}
}

// StartRefresh launches a background goroutine that periodically reloads the
// source and updates the pool. Only runs for non-literal sources with a
// positive refresh interval.
func (s *Source) StartRefresh(ctx context.Context, pool *Pool, log *logger.Logger) {
	if s.kind == kindLiteral || s.cfg.RefreshInterval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(s.cfg.RefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cookies, err := s.Load(ctx)
				if err != nil {
					log.Warn("cookie refresh failed: %v", err)
					continue
				}
				pool.Update(cookies)
				log.Debug("cookie pool refreshed: %d entries", pool.Count())
			}
		}
	}()
}

func (s *Source) loadFile() ([]string, error) {
	data, err := os.ReadFile(s.raw)
	if err != nil {
		return nil, fmt.Errorf("read cookie file %q: %w", s.raw, err)
	}
	return s.parseContent(data)
}

func (s *Source) loadURL(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, "GET", s.rendered, nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch cookies from %s: %w", s.rendered, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cookie URL returned %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read cookie response: %w", err)
	}

	return s.parseContent(body)
}

func (s *Source) parseContent(data []byte) ([]string, error) {
	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil, fmt.Errorf("empty cookie source")
	}

	if s.cfg.JSONMode {
		var cookies []string
		if err := json.Unmarshal([]byte(content), &cookies); err != nil {
			return nil, fmt.Errorf("parse cookie JSON: %w", err)
		}
		return cookies, nil
	}

	return []string{content}, nil
}

// renderTemplate applies {{.Driver}} and {{.Source}} to a URL template.
func renderTemplate(raw string, cfg SourceConfig) (string, error) {
	tmpl, err := template.New("cookie-url").Parse(raw)
	if err != nil {
		return "", err
	}

	data := struct {
		Driver string
		Source string
	}{
		Driver: cfg.Driver,
		Source: cfg.Source,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// validateSafeDomain checks that the URL's host resolves to an allowed
// domain or CIDR range.
func validateSafeDomain(rawURL, safeDomains string) error {
	if strings.TrimSpace(safeDomains) == "" {
		return fmt.Errorf("URL cookie sources require STICKY_COOKIES_SAFE_DOMAINS to be set")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse cookie URL: %w", err)
	}
	host := u.Hostname()

	tokens := strings.Split(safeDomains, ",")
	for i := range tokens {
		tokens[i] = strings.TrimSpace(tokens[i])
	}

	// Check plain domain matches first
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(token); err == nil {
			continue // CIDR, handled below
		}
		// Exact or subdomain match
		if host == token || strings.HasSuffix(host, "."+token) {
			return nil
		}
	}

	// Resolve host and check against CIDRs
	var cidrs []*net.IPNet
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(token); err == nil {
			cidrs = append(cidrs, cidr)
		}
	}

	if len(cidrs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return fmt.Errorf("resolve cookie host %q: %w", host, err)
		}

		for _, addr := range addrs {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			for _, cidr := range cidrs {
				if cidr.Contains(ip) {
					return nil
				}
			}
		}
	}

	return fmt.Errorf("cookie URL host %q is not in STICKY_COOKIES_SAFE_DOMAINS (%s)", host, safeDomains)
}
