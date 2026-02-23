package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/whisper-darkly/sticky-recorder/cookies"
	_ "github.com/whisper-darkly/sticky-recorder/driver" // register drivers
	"github.com/whisper-darkly/sticky-recorder/logger"
	"github.com/whisper-darkly/sticky-recorder/recorder"
	"github.com/whisper-darkly/sticky-recorder/stream"
	"github.com/whisper-darkly/sticky-recorder/units"
)

// Set via ldflags at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	// Extract find-style --exec args from os.Args before pflag parses them.
	// Supports: --exec cmd arg1 {} arg2 \;
	//           -e cmd arg1 {} arg2 \;
	// If no terminating ";" is found, falls back to pflag single-string parsing.
	var execArgs []string
	execArgs, os.Args = extractExecArgs(os.Args)

	// CLI flags: --long-name / -s shorthand
	driverName := flag.StringP("driver", "d", envOrDefault("STICKY_DRIVER", "chaturbate"), "Driver name (chaturbate, stripchat)")
	source := flag.StringP("source", "s", "", "Source/channel name (required)")
	resolution := flag.StringP("resolution", "r", "", "Target video resolution height (default 720)")
	framerate := flag.StringP("framerate", "f", "", "Target framerate (default 30)")
	segmentLength := flag.StringP("segment-length", "l", "", "Max segment duration (0=disabled, e.g. 60, 1h, 01:00:00)")
domain := flag.String("domain", "", "Override platform domain URL")
	cookies := flag.StringP("cookies", "c", envOrDefault("STICKY_COOKIES", ""), "HTTP cookies (format: key=value; key2=value2)")
	userAgent := flag.StringP("user-agent", "a", envOrDefault("STICKY_USER_AGENT", ""), "Custom User-Agent header")
	outPattern := flag.StringP("out", "o", envOrDefault("STICKY_OUT", ""), "Output file path template (required, extension added by driver)")
	logPattern := flag.String("log", envOrDefault("STICKY_LOG", ""), "Log file path template (empty=stdout only)")
	logLevel := flag.String("log-level", envOrDefault("STICKY_LOG_LEVEL", "info"), "Log level: debug, info, warn, error, fatal")
	outputFormat := flag.String("output-format", envOrDefault("STICKY_OUTPUT_FORMAT", "normal"), "Output format: normal, json")
	retryDelay := flag.String("retry-delay", "", "Delay between retry attempts (default 00:00:05, e.g. 5s, 00:00:05)")
	retryJitter := flag.String("retry-jitter", "", "Max random jitter added to each retry delay (0=disabled, e.g. 2s)")
	segmentTimeout := flag.String("segment-timeout", "", "Same-recording reconnect window (default 00:05:00, e.g. 5m)")
	recordingTimeout := flag.String("recording-timeout", "", "New-recording reconnect window (default 00:30:00, e.g. 30m)")
	checkInterval := flag.StringP("check-interval", "i", "", "Watch interval when offline (0=exit, e.g. 5m, 00:05:00)")
	sleepJitter := flag.String("sleep-jitter", "", "Max random jitter added to check-interval sleep (0=disabled, e.g. 30s)")
	heartbeatInterval := flag.String("heartbeat-interval", "", "Interval for HEARTBEAT JSON events during active segments (0=disabled, e.g. 30s)")

	// This flag exists only for --help output. The actual parsing is done by
	// extractExecArgs above. If find-style was not used (no ";"), pflag will
	// capture a single-string value here as a fallback.
	execFallback := flag.StringP("exec", "e", "", "Command to run on finished segments (like find -exec: {} \\;)")

	showVersion := flag.BoolP("version", "V", false, "Print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sticky-recorder %s\n\n", version)
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <source> [output-template]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Record live streams. Exits with code 2 if offline.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nDurations: hh:mm:ss | 1h30m | plain minutes.\n")
		fmt.Fprintf(os.Stderr, "Exit codes: 0=ok  1=error  2=offline  3=blocked\n")
	}

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(0)
	}

	flag.Parse()

	if *showVersion {
		fmt.Println("sticky-recorder", version)
		os.Exit(0)
	}

	// Support positional arguments: [source] [output-template]
	// First positional is source, second (if present) is output template.
	if flag.NArg() > 0 {
		if *source == "" {
			*source = flag.Arg(0)
		}
		if *outPattern == "" && flag.NArg() > 1 {
			*outPattern = flag.Arg(flag.NArg() - 1)
		}
	}

	// If find-style exec wasn't used, check for pflag single-string fallback
	if len(execArgs) == 0 && *execFallback != "" {
		execArgs = tokenize(*execFallback)
	}

	// Create logger early so all validation messages use it
	log := logger.New(logger.ParseLevel(*logLevel))
	log.SetFormat(logger.ParseFormat(*outputFormat))

	if *source == "" {
		log.Fatal("--source is required")
	}
	if *outPattern == "" {
		log.Fatal("--out (output template) is required")
	}

	*driverName = normalizeDriverName(*driverName)

	drv, err := stream.Get(*driverName)
	if err != nil {
		log.Fatal("%v", err)
	}

	// Handle graceful shutdown (before cookie pool init, which may start goroutines)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Warn("received %v, shutting down...", sig)
		cancel()
	}()

	cookiePool, err := initCookiePool(ctx, *cookies, *driverName, *source, log)
	if err != nil {
		log.Fatal("cookie pool: %v", err)
	}

	cfg := recorder.Config{
		Driver:            drv,
		Source:            *source,
		Domain:            *domain,
		Resolution:        intVal(*resolution, "STICKY_RESOLUTION", 720),
		Framerate:         intVal(*framerate, "STICKY_FRAMERATE", 30),
		CookiePool:        cookiePool,
		UserAgent:         *userAgent,
		OutPattern:        *outPattern,
		LogPattern:        *logPattern,
		ExecArgs:          execArgs,
		ExecFatal:         os.Getenv("STICKY_EXEC_FATAL") != "",
		SegmentLength:     durationVal(*segmentLength, "STICKY_SEGMENT_LENGTH", 0, log),
		RetryDelay:  durationVal(*retryDelay, "STICKY_RETRY_DELAY", 5*time.Second, log),
		RetryJitter: durationVal(*retryJitter, "STICKY_RETRY_JITTER", 0, log),
		SegmentTimeout:    durationVal(*segmentTimeout, "STICKY_SEGMENT_TIMEOUT", 5*time.Minute, log),
		RecordingTimeout:  durationVal(*recordingTimeout, "STICKY_RECORDING_TIMEOUT", 30*time.Minute, log),
		CheckInterval:     durationVal(*checkInterval, "STICKY_CHECK_INTERVAL", 0, log),
		SleepJitter:       durationVal(*sleepJitter, "STICKY_SLEEP_JITTER", 0, log),
		HeartbeatInterval: durationVal(*heartbeatInterval, "STICKY_HEARTBEAT_INTERVAL", 0, log),
		Log:               log,
	}

	rec := recorder.New(cfg)
	os.Exit(rec.Run(ctx))
}

func initCookiePool(ctx context.Context, raw, driverName, source string, log *logger.Logger) (*cookies.Pool, error) {
	if raw == "" {
		return cookies.NewPool(nil), nil
	}

	cfg := cookies.SourceConfig{
		ExternalEnabled: os.Getenv("STICKY_EXTERNAL_COOKIES") != "",
		SafeDomains:     os.Getenv("STICKY_COOKIES_SAFE_DOMAINS"),
		JSONMode:        os.Getenv("STICKY_COOKIES_JSON") != "",
		Driver:          driverName,
		Source:          source,
	}

	if v := os.Getenv("STICKY_COOKIES_REFRESH"); v != "" {
		d, err := units.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid STICKY_COOKIES_REFRESH: %w", err)
		}
		cfg.RefreshInterval = d
	}

	src, err := cookies.NewSource(raw, cfg)
	if err != nil {
		return nil, err
	}

	initial, err := src.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cookies: %w", err)
	}

	pool := cookies.NewPool(initial)
	src.StartRefresh(ctx, pool, log)
	return pool, nil
}

// extractExecArgs scans os.Args for --exec/-e and extracts all tokens up to
// a terminating ";" argument, just like find(1) -exec.
//
// Returns the extracted command tokens (without --exec and ;) and the remaining
// os.Args with the exec portion removed.
//
// If --exec is not found, or no terminating ";" exists, returns nil and the
// original args unchanged (pflag will handle it as a single-string flag).
func extractExecArgs(args []string) (execTokens []string, remaining []string) {
	for i := 1; i < len(args); i++ {
		a := args[i]

		// Match --exec or -e (but not --exec=val or -e=val, which pflag handles)
		isExec := a == "--exec" || a == "-e"
		if !isExec {
			continue
		}

		// Scan forward for the terminating ";"
		semicolonIdx := -1
		for j := i + 1; j < len(args); j++ {
			if args[j] == ";" {
				semicolonIdx = j
				break
			}
		}

		if semicolonIdx == -1 {
			// No ";' found — let pflag parse --exec as a single-value flag
			return nil, args
		}

		// Extract the command tokens between --exec and ;
		execTokens = args[i+1 : semicolonIdx]

		// Rebuild args without the --exec ... ; portion
		remaining = make([]string, 0, len(args)-(semicolonIdx-i+1))
		remaining = append(remaining, args[:i]...)
		remaining = append(remaining, args[semicolonIdx+1:]...)
		return execTokens, remaining
	}

	return nil, args
}

// tokenize splits a string into tokens, respecting single and double quotes.
// Used as a fallback when --exec is passed as a single string (e.g. -e 'cmd {} arg').
func tokenize(s string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	quoteChar := rune(0)

	for _, r := range s {
		switch {
		case (r == '"' || r == '\'') && !inQuote:
			inQuote = true
			quoteChar = r
		case r == quoteChar && inQuote:
			inQuote = false
			quoteChar = 0
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// normalizeDriverName handles common aliases.
func normalizeDriverName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cb", "ctb", "chaturbate":
		return "chaturbate"
	case "sc", "st", "stc", "stripchat":
		return "stripchat"
	default:
		return strings.ToLower(name)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// durationVal resolves a time.Duration from: CLI string (if non-empty) → ENV → default.
// Uses units.ParseDuration for flexible format support (hh:mm:ss, Go-style, plain minutes).
func durationVal(cliVal, envKey string, def time.Duration, log *logger.Logger) time.Duration {
	if cliVal != "" {
		d, err := units.ParseDuration(cliVal)
		if err != nil {
			log.Fatal("invalid duration for %s: %v", envKey, err)
		}
		return d
	}
	if v := os.Getenv(envKey); v != "" {
		d, err := units.ParseDuration(v)
		if err != nil {
			log.Fatal("invalid duration in %s: %v", envKey, err)
		}
		return d
	}
	return def
}

// intVal resolves an integer from: CLI string (if non-empty) → ENV → default.
func intVal(cliVal, envKey string, def int) int {
	if cliVal != "" {
		var n int
		if _, err := fmt.Sscanf(cliVal, "%d", &n); err == nil {
			return n
		}
	}
	if v := os.Getenv(envKey); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
