package stream

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/grafov/m3u8"
)

// PickVariant selects the best matching variant from a master playlist based on
// desired resolution (height) and framerate. Falls back to the next-best resolution
// below the requested one if an exact match is unavailable.
func PickVariant(master *m3u8.MasterPlaylist, baseURL string, resolution, framerate int) (playlistURL string, actualRes, actualFPS int, err error) {
	type variant struct {
		url       string
		width     int
		height    int
		framerate int
	}

	var variants []variant
	for _, v := range master.Variants {
		if v == nil || v.Resolution == "" {
			continue
		}
		parts := strings.Split(v.Resolution, "x")
		if len(parts) != 2 {
			continue
		}
		height, e := strconv.Atoi(parts[1])
		if e != nil {
			continue
		}
		width, _ := strconv.Atoi(parts[0])
		fps := 30
		if strings.Contains(v.Name, "FPS:60.0") || v.FrameRate > 50 {
			fps = 60
		}
		variants = append(variants, variant{url: v.URI, width: width, height: height, framerate: fps})
	}

	if len(variants) == 0 {
		return "", 0, 0, fmt.Errorf("no variants found in master playlist")
	}

	// Group by height
	byHeight := map[int][]variant{}
	for _, v := range variants {
		byHeight[v.height] = append(byHeight[v.height], v)
	}

	// Find best resolution: exact match, or highest below requested
	targetHeight := 0
	if _, ok := byHeight[resolution]; ok {
		targetHeight = resolution
	} else {
		for h := range byHeight {
			if h < resolution && h > targetHeight {
				targetHeight = h
			}
		}
		// If nothing below, pick the lowest available
		if targetHeight == 0 {
			for h := range byHeight {
				if targetHeight == 0 || h < targetHeight {
					targetHeight = h
				}
			}
		}
	}

	candidates := byHeight[targetHeight]

	// Pick matching framerate, or first available
	var picked variant
	for _, c := range candidates {
		if c.framerate == framerate {
			picked = c
			break
		}
	}
	if picked.url == "" {
		picked = candidates[0]
	}

	// Build full URL: handle relative URIs and query params from base
	fullURL := resolvePlaylistURL(baseURL, picked.url)
	return fullURL, picked.height, picked.framerate, nil
}

// resolvePlaylistURL constructs the full variant URL from the base master playlist URL
// and the relative variant URI, preserving query parameters.
func resolvePlaylistURL(baseURL, variantURI string) string {
	// If variant URI is already absolute, use it directly
	if strings.HasPrefix(variantURI, "http://") || strings.HasPrefix(variantURI, "https://") {
		return variantURI
	}

	// Split base URL into path and query parts
	queryPart := ""
	pathPart := baseURL
	if idx := strings.Index(baseURL, "?"); idx != -1 {
		pathPart = baseURL[:idx]
		queryPart = baseURL[idx:]
	}

	// Replace the last path segment with the variant URI
	if lastSlash := strings.LastIndex(pathPart, "/"); lastSlash != -1 {
		return pathPart[:lastSlash+1] + variantURI + queryPart
	}
	return variantURI + queryPart
}
