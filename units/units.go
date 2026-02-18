package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses a flexible duration string. Accepted formats:
//   - hh:mm:ss (e.g. "01:30:00")
//   - Go-style duration (e.g. "1h30m", "5m", "30s")
//   - Plain number as minutes (e.g. "90", "0.5")
//
// Negative values are rejected.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	// Try hh:mm:ss
	if strings.Count(s, ":") == 2 {
		parts := strings.SplitN(s, ":", 3)
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		sec, err3 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil && err3 == nil {
			d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
			if d < 0 {
				return 0, fmt.Errorf("negative duration: %s", s)
			}
			return d, nil
		}
	}

	// Try Go-style duration (e.g. "1h30m5s", "5m", "30s")
	if d, err := time.ParseDuration(strings.ToLower(s)); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration: %s", s)
		}
		return d, nil
	}

	// Try plain number as minutes
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: must be hh:mm:ss, Go duration (1h30m), or minutes", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("negative duration: %s", s)
	}
	return time.Duration(f * float64(time.Minute)), nil
}

// FormatDuration formats a duration as hh:mm:ss, truncated to seconds.
func FormatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

const (
	megabyte = 1024 * 1024
	gigabyte = 1024 * 1024 * 1024
	terabyte = 1024 * 1024 * 1024 * 1024
)

// ParseSize parses a flexible size string into bytes. Accepted formats:
//   - Number with suffix: "500MB", "1.5GB", "2TB" (case-insensitive)
//   - Plain number as MB: "500", "1.5"
//
// Negative values are rejected. Nothing smaller than MB is accepted.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	upper := strings.ToUpper(s)

	// Try suffix-based parsing
	type suffix struct {
		tag  string
		mult float64
	}
	suffixes := []suffix{
		{"TB", terabyte},
		{"GB", gigabyte},
		{"MB", megabyte},
	}
	for _, sf := range suffixes {
		if strings.HasSuffix(upper, sf.tag) {
			numStr := strings.TrimSpace(s[:len(s)-len(sf.tag)])
			f, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q: bad number before %s", s, sf.tag)
			}
			if f < 0 {
				return 0, fmt.Errorf("negative size: %s", s)
			}
			return int64(math.Round(f * sf.mult)), nil
		}
	}

	// Plain number → MB
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: must be {n}TB/GB/MB or plain number (MB)", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("negative size: %s", s)
	}
	return int64(math.Round(f * megabyte)), nil
}

// FormatSize formats bytes into a human-readable string, picking TB/GB/MB automatically.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= terabyte:
		return fmt.Sprintf("%.1fTB", float64(bytes)/terabyte)
	case bytes >= gigabyte:
		return fmt.Sprintf("%.1fGB", float64(bytes)/gigabyte)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/megabyte)
	}
}
