package stream

import "errors"

var (
	ErrOffline           = errors.New("source is offline")
	ErrPrivateStream     = errors.New("stream is private or forbidden")
	ErrCloudflareBlocked = errors.New("blocked by Cloudflare; try with -cookies and -user-agent")
	ErrAgeVerification   = errors.New("age verification required; try with -cookies and -user-agent")
	ErrNotFound          = errors.New("source not found (404)")
)
