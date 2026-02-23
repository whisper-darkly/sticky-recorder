package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/grafov/m3u8"
	"github.com/whisper-darkly/sticky-recorder/stream"
)

const chaturbateDefaultDomain = "https://chaturbate.com/"

func init() {
	stream.Register(&Chaturbate{})
}

// Chaturbate implements the Driver interface for chaturbate.com streams.
type Chaturbate struct{}

func (c *Chaturbate) Name() string          { return "chaturbate" }
func (c *Chaturbate) DefaultDomain() string { return chaturbateDefaultDomain }
func (c *Chaturbate) FileExtension() string { return "ts" }

func (c *Chaturbate) CheckStream(ctx context.Context, opts stream.StreamOpts) (*stream.StreamInfo, error) {
	domain := opts.Domain
	if domain == "" {
		domain = c.DefaultDomain()
	}
	if !strings.HasSuffix(domain, "/") {
		domain += "/"
	}

	client := stream.NewHTTPClient(opts.Cookies, opts.UserAgent)

	// Use HTML page scraping as the primary method. The API endpoint below is
	// more structured but sits behind stricter Cloudflare rules, causing a
	// disproportionate number of exit-code-3 (blocked) results in practice.
	// See: chaturbate-dvr legacy behaviour — page scraping is what it used exclusively.
	return c.checkStreamViaPage(ctx, client, domain, opts)

	// --- API endpoint (kept for reference) ---
	// The /api/chatvideocontext/ endpoint returns clean JSON but is more aggressively
	// Cloudflare-gated than the regular page, leading to frequent blocked exits.
	//
	// apiURL := domain + "api/chatvideocontext/" + opts.Source + "/"
	// body, err := client.Get(ctx, apiURL)
	// if err != nil {
	// 	return nil, fmt.Errorf("fetch stream info: %w", err)
	// }
	// var resp struct {
	// 	HLSSource  string `json:"hls_source"`
	// 	RoomStatus string `json:"room_status"`
	// }
	// if err := json.Unmarshal([]byte(body), &resp); err != nil {
	// 	// Fallback: try the page scraping method
	// 	return c.checkStreamViaPage(ctx, client, domain, opts)
	// }
	// if resp.HLSSource == "" {
	// 	if resp.RoomStatus == "private" || resp.RoomStatus == "hidden" {
	// 		return nil, stream.ErrPrivateStream
	// 	}
	// 	return nil, stream.ErrOffline
	// }
	// return c.resolvePlaylist(ctx, client, resp.HLSSource, opts.Resolution, opts.Framerate)
}

// checkStreamViaPage uses the HTML page scraping method (same approach as chaturbate-dvr).
// The page is less aggressively Cloudflare-gated than the API endpoint.
func (c *Chaturbate) checkStreamViaPage(ctx context.Context, client *stream.HTTPClient, domain string, opts stream.StreamOpts) (*stream.StreamInfo, error) {
	body, err := client.Get(ctx, domain+opts.Source)
	if err != nil {
		return nil, fmt.Errorf("fetch page: %w", err)
	}

	if !strings.Contains(body, "playlist.m3u8") {
		return nil, stream.ErrOffline
	}

	// Extract room dossier JSON
	const prefix = `window.initialRoomDossier = "`
	idx := strings.Index(body, prefix)
	if idx == -1 {
		return nil, fmt.Errorf("room dossier not found in page")
	}
	start := idx + len(prefix)
	end := strings.Index(body[start:], `"`)
	if end == -1 {
		return nil, fmt.Errorf("room dossier end quote not found")
	}
	raw := body[start : start+end]

	// Decode unicode escapes
	decoded := strings.ReplaceAll(raw, `\u0022`, `"`)
	decoded = strings.ReplaceAll(decoded, `\u0027`, `'`)
	decoded = strings.ReplaceAll(decoded, `\/`, `/`)
	decoded = strings.ReplaceAll(decoded, `\\`, `\`)

	var room struct {
		HLSSource string `json:"hls_source"`
	}
	if err := json.Unmarshal([]byte(decoded), &room); err != nil {
		return nil, fmt.Errorf("parse room dossier: %w", err)
	}
	if room.HLSSource == "" {
		return nil, stream.ErrOffline
	}

	return c.resolvePlaylist(ctx, client, room.HLSSource, opts.Resolution, opts.Framerate)
}

func (c *Chaturbate) resolvePlaylist(ctx context.Context, client *stream.HTTPClient, hlsSource string, resolution, framerate int) (*stream.StreamInfo, error) {
	body, err := client.Get(ctx, hlsSource)
	if err != nil {
		return nil, fmt.Errorf("fetch master playlist: %w", err)
	}

	p, _, err := m3u8.DecodeFrom(strings.NewReader(body), true)
	if err != nil {
		return nil, fmt.Errorf("decode master playlist: %w", err)
	}

	master, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return nil, fmt.Errorf("expected master playlist, got media playlist")
	}

	url, actualRes, actualFPS, err := stream.PickVariant(master, hlsSource, resolution, framerate)
	if err != nil {
		return nil, fmt.Errorf("pick variant: %w", err)
	}

	return &stream.StreamInfo{
		URL:        url,
		Resolution: actualRes,
		Framerate:  actualFPS,
	}, nil
}

func (c *Chaturbate) ClassifyError(err error) stream.InterruptionType {
	if err == nil {
		return stream.Ended
	}
	if errors.Is(err, context.Canceled) {
		return stream.Fatal
	}
	if errors.Is(err, stream.ErrOffline) || errors.Is(err, stream.ErrPrivateStream) {
		return stream.Ended
	}
	if errors.Is(err, stream.ErrCloudflareBlocked) || errors.Is(err, stream.ErrAgeVerification) {
		return stream.Blocked
	}
	if errors.Is(err, stream.ErrNotFound) {
		return stream.Ended
	}
	return stream.TransientError
}
