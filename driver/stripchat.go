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

const stripchatDefaultDomain = "https://stripchat.com/"
const stripchatCDN = "https://edge-hls.sacdnssedge.com"

func init() {
	stream.Register(&StripChat{})
}

// StripChat implements the Driver interface for stripchat.com streams.
type StripChat struct{}

func (s *StripChat) Name() string          { return "stripchat" }
func (s *StripChat) DefaultDomain() string { return stripchatDefaultDomain }
func (s *StripChat) FileExtension() string { return "ts" }

func (s *StripChat) CheckStream(ctx context.Context, opts stream.StreamOpts) (*stream.StreamInfo, error) {
	domain := opts.Domain
	if domain == "" {
		domain = s.DefaultDomain()
	}
	if !strings.HasSuffix(domain, "/") {
		domain += "/"
	}

	client := stream.NewHTTPClient(opts.Cookies, opts.UserAgent)

	apiURL := domain + "api/front/v2/models/username/" + opts.Source + "/cam"
	body, err := client.Get(ctx, apiURL)
	if err != nil {
		return nil, fmt.Errorf("fetch stream info: %w", err)
	}

	var resp struct {
		Cam struct {
			IsCamAvailable bool   `json:"isCamAvailable"`
			StreamName     string `json:"streamName"`
			ViewServers    struct {
				FlashphoneHost string `json:"flashphoner-hls"`
			} `json:"viewServers"`
		} `json:"cam"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("parse API response: %w", err)
	}

	if !resp.Cam.IsCamAvailable {
		return nil, stream.ErrOffline
	}
	if resp.Cam.StreamName == "" {
		return nil, fmt.Errorf("stream is live but no streamName in response")
	}

	// Build the master playlist URL
	cdn := stripchatCDN
	if resp.Cam.ViewServers.FlashphoneHost != "" {
		cdn = "https://" + resp.Cam.ViewServers.FlashphoneHost
	}
	masterURL := fmt.Sprintf("%s/hls/%s/master/%s_auto.m3u8",
		cdn, resp.Cam.StreamName, resp.Cam.StreamName)

	// Try to parse the master playlist for resolution selection
	return s.resolvePlaylist(ctx, client, masterURL, opts.Resolution, opts.Framerate)
}

func (s *StripChat) resolvePlaylist(ctx context.Context, client *stream.HTTPClient, masterURL string, resolution, framerate int) (*stream.StreamInfo, error) {
	body, err := client.Get(ctx, masterURL)
	if err != nil {
		// If we can't fetch the master, return the master URL itself and let ffmpeg handle it
		return &stream.StreamInfo{URL: masterURL, Resolution: resolution, Framerate: framerate}, nil
	}

	p, _, err := m3u8.DecodeFrom(strings.NewReader(body), true)
	if err != nil {
		return &stream.StreamInfo{URL: masterURL, Resolution: resolution, Framerate: framerate}, nil
	}

	master, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		// Got a media playlist directly — use it as-is
		return &stream.StreamInfo{URL: masterURL, Resolution: resolution, Framerate: framerate}, nil
	}

	url, actualRes, actualFPS, err := stream.PickVariant(master, masterURL, resolution, framerate)
	if err != nil {
		// Fallback to master URL (auto selection)
		return &stream.StreamInfo{URL: masterURL, Resolution: resolution, Framerate: framerate}, nil
	}

	return &stream.StreamInfo{
		URL:        url,
		Resolution: actualRes,
		Framerate:  actualFPS,
	}, nil
}

func (s *StripChat) ClassifyError(err error) stream.InterruptionType {
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
