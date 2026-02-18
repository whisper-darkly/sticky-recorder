package stream

import (
	"context"
	"fmt"
)

// StreamInfo contains the resolved stream details for recording.
type StreamInfo struct {
	URL        string // Media playlist URL to pass to ffmpeg
	Resolution int    // Actual resolution obtained
	Framerate  int    // Actual framerate obtained
}

// StreamOpts holds the parameters for stream discovery.
type StreamOpts struct {
	Source     string
	Domain     string
	Resolution int
	Framerate  int
	Cookies    string
	UserAgent  string
}

// InterruptionType classifies what happened when recording stops.
type InterruptionType int

const (
	// Ended means the stream went offline normally.
	Ended InterruptionType = iota
	// Blocked means access was denied (cloudflare, age verification, private).
	Blocked
	// TransientError means a temporary failure (CDN error, network blip).
	TransientError
	// Fatal means an unrecoverable error (context cancelled, bad config).
	Fatal
)

// Driver defines the interface that platform-specific implementations must satisfy.
type Driver interface {
	// Name returns the driver identifier (e.g., "chaturbate", "stripchat").
	Name() string

	// DefaultDomain returns the base URL for this platform.
	DefaultDomain() string

	// FileExtension returns the output file extension without the dot (e.g., "ts").
	FileExtension() string

	// CheckStream determines if the source is live and returns the stream URL
	// at the best matching resolution/framerate.
	CheckStream(ctx context.Context, opts StreamOpts) (*StreamInfo, error)

	// ClassifyError determines the type of interruption from an error.
	ClassifyError(err error) InterruptionType
}

var registry = map[string]Driver{}

// Register adds a driver to the global registry.
func Register(d Driver) {
	registry[d.Name()] = d
}

// Get returns a registered driver by name.
func Get(name string) (Driver, error) {
	d, ok := registry[name]
	if !ok {
		names := make([]string, 0, len(registry))
		for n := range registry {
			names = append(names, n)
		}
		return nil, fmt.Errorf("unknown driver %q (available: %v)", name, names)
	}
	return d, nil
}
