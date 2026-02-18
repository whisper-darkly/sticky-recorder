package recorder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/whisper-darkly/sticky-recorder/cookies"
	"github.com/whisper-darkly/sticky-recorder/logger"
	"github.com/whisper-darkly/sticky-recorder/stream"
	"github.com/whisper-darkly/sticky-recorder/units"
)

// ErrExecFatal is returned when --exec exits non-zero and STICKY_EXEC_FATAL is set.
var ErrExecFatal = errors.New("exec returned non-zero (STICKY_EXEC_FATAL)")

// Config holds all recording parameters.
type Config struct {
	Driver     stream.Driver
	Source     string
	Domain     string
	Resolution int
	Framerate  int
	CookiePool *cookies.Pool
	UserAgent  string

	OutPattern string   // Go template for output file path (without extension)
	LogPattern string   // Go template for log file path (empty = no file logging)
	ExecArgs   []string // Command + args to run on finished segments ({} = file path)
	ExecFatal  bool     // If true, non-zero exec exit kills the session

	SegmentLength time.Duration // Max segment duration (0 = no limit)
	SegmentSize   int64         // Max segment size in bytes (0 = no limit)

	RetryDelay       time.Duration // Delay between retry attempts
	SegmentTimeout   time.Duration // Reconnect within this = same recording, new segment
	RecordingTimeout time.Duration // Reconnect within this = new recording in same session
	CheckInterval    time.Duration // Watch mode interval (0 = one-shot, exit on offline/end)
	ShortPaths       bool          // If true, emit only filenames in events instead of full paths

	Log *logger.Logger
}

// Recorder manages the full session lifecycle: session → recordings → segments.
type Recorder struct {
	cfg Config
	log *logger.Logger

	// Session state
	sessionStart   time.Time
	recordingCount int

	// Current recording state
	recordingStart time.Time
	segmentCount   int
	segmentStart   time.Time

	// Cookie selected for the current connection
	activeCookies string

	// Log file handle (if any)
	logFile *os.File
}

// New creates a Recorder with the given config.
func New(cfg Config) *Recorder {
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 5 * time.Second
	}
	if cfg.SegmentTimeout <= 0 {
		cfg.SegmentTimeout = 5 * time.Minute
	}
	if cfg.RecordingTimeout <= 0 {
		cfg.RecordingTimeout = 30 * time.Minute
	}

	return &Recorder{
		cfg: cfg,
		log: cfg.Log,
	}
}

// Run executes the watch loop (or single-shot). Returns an exit code.
func (r *Recorder) Run(ctx context.Context) int {
	for {
		exitCode := r.runSession(ctx)
		if r.cfg.CheckInterval <= 0 {
			return exitCode // one-shot: pass through
		}
		if exitCode == 1 || exitCode == 3 || ctx.Err() != nil {
			return exitCode // fatal/blocked: stop watching
		}
		// offline or session ended: sleep and retry
		r.log.Event("SLEEP source=%s interval=%s",
			r.cfg.Source, units.FormatDuration(r.cfg.CheckInterval))
		select {
		case <-time.After(r.cfg.CheckInterval):
		case <-ctx.Done():
			return 0
		}
	}
}

// runSession executes a single session lifecycle. Returns an exit code.
func (r *Recorder) runSession(ctx context.Context) int {
	opts := r.streamOpts()

	// Initial stream check
	info, err := r.cfg.Driver.CheckStream(ctx, opts)
	if err != nil {
		itype := r.cfg.Driver.ClassifyError(err)
		if itype == stream.Blocked || itype == stream.TransientError {
			r.penalizeCookie()
		}
		switch itype {
		case stream.Ended:
			r.log.Warn("%s is offline: %v", r.cfg.Source, err)
			return 2
		case stream.Blocked:
			r.log.Error("access blocked: %v", err)
			return 3
		default:
			r.log.Error("%v", err)
			return 1
		}
	}

	// === Session starts ===
	r.sessionStart = time.Now()
	r.recordingCount = 0

	// Setup log file if pattern provided
	if r.cfg.LogPattern != "" {
		if err := r.openLogFile(); err != nil {
			r.log.Error("failed to open log file: %v", err)
			return 1
		}
		defer r.closeLogFile()
	}

	r.log.Event("SESSION START source=%s driver=%s resolution=%dp/%dfps",
		r.cfg.Source, r.cfg.Driver.Name(), info.Resolution, info.Framerate)

	exitCode := r.sessionLoop(ctx, info)

	r.log.Event("SESSION END source=%s recordings=%d duration=%s",
		r.cfg.Source, r.recordingCount, units.FormatDuration(time.Since(r.sessionStart)))

	return exitCode
}

func (r *Recorder) sessionLoop(ctx context.Context, initialInfo *stream.StreamInfo) int {
	info := initialInfo

	for {
		if ctx.Err() != nil {
			return 0
		}

		exitCode := r.runRecording(ctx, info)
		if exitCode >= 0 {
			return exitCode
		}
		// exitCode == -1 means: recording ended, try for a new one within session timeout

		// Attempt to start a new recording within recording-timeout
		deadline := time.Now().Add(r.cfg.RecordingTimeout)
		var newInfo *stream.StreamInfo
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return 0
			}

			r.log.Debug("waiting %s before checking for new recording...", r.cfg.RetryDelay)
			select {
			case <-time.After(r.cfg.RetryDelay):
			case <-ctx.Done():
				return 0
			}

			var err error
			newInfo, err = r.cfg.Driver.CheckStream(ctx, r.streamOpts())
			if err == nil {
				break
			}

			itype := r.cfg.Driver.ClassifyError(err)
			if itype == stream.Blocked {
				r.penalizeCookie()
				r.log.Error("%v (not retrying)", err)
				return 3
			}
			if itype == stream.Fatal {
				r.log.Error("%v (not retrying)", err)
				return 1
			}
			r.log.Debug("source not available: %v (retrying until %s)", err, deadline.Format("15:04:05"))
		}

		if newInfo == nil {
			r.log.Info("session timeout reached (%s), no new recording",
				units.FormatDuration(r.cfg.RecordingTimeout))
			return 0
		}

		info = newInfo
	}
}

// runRecording manages a single recording (one or more segments).
// Returns >=0 for a final exit code, or -1 to signal "try for a new recording."
func (r *Recorder) runRecording(ctx context.Context, info *stream.StreamInfo) int {
	r.recordingStart = time.Now()
	r.segmentCount = 0
	r.recordingCount++

	r.log.Event("RECORDING START recording=%d resolution=%dp/%dfps",
		r.recordingCount-1, info.Resolution, info.Framerate)

	for {
		if ctx.Err() != nil {
			return 0
		}

		_, segErr := r.runSegment(ctx, info)

		if segErr == nil {
			// Segment limit reached — next segment
			continue
		}

		// Exec killed the session
		if errors.Is(segErr, ErrExecFatal) {
			r.log.Event("RECORDING END recording=%d segments=%d duration=%s trigger=exec_fatal",
				r.recordingCount-1, r.segmentCount,
				units.FormatDuration(time.Since(r.recordingStart)))
			return 1
		}

		itype := r.cfg.Driver.ClassifyError(segErr)
		if itype == stream.Blocked || itype == stream.TransientError {
			r.penalizeCookie()
		}
		if itype == stream.Blocked {
			r.log.Error("blocked: %v", segErr)
			return 3
		}
		if itype == stream.Fatal {
			r.log.Error("fatal: %v", segErr)
			return 1
		}

		// Ended or TransientError: try reconnecting within segment-timeout
		deadline := time.Now().Add(r.cfg.SegmentTimeout)
		reconnected := false
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return 0
			}

			select {
			case <-time.After(r.cfg.RetryDelay):
			case <-ctx.Done():
				return 0
			}

			newInfo, err := r.cfg.Driver.CheckStream(ctx, r.streamOpts())
			if err == nil {
				info = newInfo
				reconnected = true
				break
			}

			it := r.cfg.Driver.ClassifyError(err)
			if it == stream.Blocked {
				r.penalizeCookie()
				r.log.Error("%v (not retrying)", err)
				return 3
			}
			if it == stream.Fatal {
				r.log.Error("%v (not retrying)", err)
				return 1
			}
			r.log.Debug("reconnecting: %v (until %s)", err, deadline.Format("15:04:05"))
		}

		if !reconnected {
			r.log.Event("RECORDING END recording=%d segments=%d duration=%s trigger=timeout",
				r.recordingCount-1, r.segmentCount,
				units.FormatDuration(time.Since(r.recordingStart)))
			return -1 // try for new recording within session
		}
		// Reconnected — continue with new segment
	}
}

// runSegment records a single segment via ffmpeg. Returns the output file path and any error.
func (r *Recorder) runSegment(ctx context.Context, info *stream.StreamInfo) (string, error) {
	r.segmentStart = time.Now()

	tmplData := NewTemplateData(
		r.cfg.Source, r.cfg.Driver.Name(),
		r.sessionStart, r.recordingStart, r.segmentStart,
		r.recordingCount-1, r.segmentCount,
	)

	outBase, err := RenderTemplate(r.cfg.OutPattern, tmplData)
	if err != nil {
		return "", fmt.Errorf("render output template: %w", err)
	}
	outFile := outBase + "." + r.cfg.Driver.FileExtension()

	if err := os.MkdirAll(filepath.Dir(outFile), 0755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	r.segmentCount++
	targetInfo := r.segmentTargetInfo()

	r.emitSegmentStart(outFile, targetInfo)

	ffmpegErr := r.runFFmpeg(ctx, info.URL, outFile)
	elapsed := time.Since(r.segmentStart)

	var fileSize int64
	if fi, err := os.Stat(outFile); err == nil {
		fileSize = fi.Size()
	}

	if fileSize == 0 {
		os.Remove(outFile)
	}

	trigger := "stream_end"
	if ffmpegErr != nil {
		if ctx.Err() != nil {
			trigger = "cancelled"
		} else {
			trigger = "error"
		}
	} else if r.shouldSplit(elapsed, fileSize) {
		trigger = "split"
	}

	r.emitSegmentFinish(outFile, targetInfo, elapsed, fileSize, trigger, ffmpegErr)

	// Run exec and check return code
	if fileSize > 0 && len(r.cfg.ExecArgs) > 0 {
		if execErr := r.runExec(outFile); execErr != nil {
			return outFile, execErr
		}
	}

	if ffmpegErr != nil && ctx.Err() == nil {
		return outFile, ffmpegErr
	}
	if ctx.Err() != nil {
		return outFile, ctx.Err()
	}
	if trigger == "split" {
		return outFile, nil
	}
	return outFile, stream.ErrOffline
}

// runFFmpeg launches ffmpeg and blocks until it exits or a split limit is reached.
func (r *Recorder) runFFmpeg(ctx context.Context, streamURL, outFile string) error {
	args := []string{
		"-y", "-hide_banner", "-loglevel", "warning",
		"-reconnect", "1",
		"-reconnect_on_network_error", "1",
		"-reconnect_on_http_error", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-tls_verify", "0",
	}

	var headers []string
	if r.activeCookies != "" {
		headers = append(headers, "Cookie: "+r.activeCookies)
	}
	if r.cfg.UserAgent != "" {
		headers = append(headers, "User-Agent: "+r.cfg.UserAgent)
	}
	if len(headers) > 0 {
		args = append(args, "-headers", strings.Join(headers, "\r\n")+"\r\n")
	}

	args = append(args,
		"-i", streamURL,
		"-c", "copy",
		"-copyts", "-start_at_zero",
		outFile,
	)

	r.log.Debug("ffmpeg %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = r.log.Writer(logger.LevelWarn)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	splitCtx, splitCancel := context.WithCancel(ctx)
	defer splitCancel()
	splitTriggered := make(chan struct{}, 1)

	if r.cfg.SegmentLength > 0 || r.cfg.SegmentSize > 0 {
		go r.monitorSplit(splitCtx, outFile, cmd, splitTriggered)
	}

	err := cmd.Wait()

	select {
	case <-splitTriggered:
		return nil
	default:
	}

	return err
}

// monitorSplit watches the output file and signals ffmpeg to stop when limits are reached.
func (r *Recorder) monitorSplit(ctx context.Context, outFile string, cmd *exec.Cmd, triggered chan<- struct{}) {
	start := time.Now()

	maxDuration := r.cfg.SegmentLength
	maxSize := r.cfg.SegmentSize

	// Poll interval: 1 second, or 1/10th of the max duration if that's shorter
	pollInterval := 1 * time.Second
	if maxDuration > 0 && maxDuration/10 < pollInterval {
		pollInterval = maxDuration / 10
		if pollInterval < 100*time.Millisecond {
			pollInterval = 100 * time.Millisecond
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			shouldSplit := false

			if maxDuration > 0 && time.Since(start) >= maxDuration {
				shouldSplit = true
			}
			if maxSize > 0 {
				if fi, err := os.Stat(outFile); err == nil && fi.Size() >= maxSize {
					shouldSplit = true
				}
			}

			if shouldSplit {
				r.log.Debug("segment limit reached, splitting")
				select {
				case triggered <- struct{}{}:
				default:
				}
				terminateProcess(cmd)
				return
			}
		}
	}
}

// terminateProcess sends SIGTERM then SIGKILL to the ffmpeg process group.
func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func (r *Recorder) shouldSplit(elapsed time.Duration, fileSize int64) bool {
	if r.cfg.SegmentLength > 0 && elapsed >= r.cfg.SegmentLength {
		return true
	}
	if r.cfg.SegmentSize > 0 && fileSize >= r.cfg.SegmentSize {
		return true
	}
	return false
}

// runExec executes the configured command with {} replaced by the segment file
// path in each argument token, matching find(1) -exec behavior:
//   - Direct execution (no shell)
//   - {} is replaced everywhere it appears in each argument (including substrings)
//
// Returns ErrExecFatal if the command exits non-zero and ExecFatal is enabled.
// Returns nil otherwise (non-zero exit is logged as a warning but not fatal).
func (r *Recorder) runExec(segmentFile string) error {
	args := make([]string, len(r.cfg.ExecArgs))
	for i, a := range r.cfg.ExecArgs {
		args[i] = strings.ReplaceAll(a, "{}", segmentFile)
	}

	r.log.Info("exec: %s", strings.Join(args, " "))

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = r.log.Writer(logger.LevelInfo)
	cmd.Stderr = r.log.Writer(logger.LevelWarn)

	if err := cmd.Run(); err != nil {
		if r.cfg.ExecFatal {
			r.log.Error("exec failed (STICKY_EXEC_FATAL): %s → %v", args[0], err)
			return ErrExecFatal
		}
		r.log.Warn("exec failed: %v", err)
	}
	return nil
}

// --- Segment event helpers ---

func (r *Recorder) segmentTargetInfo() string {
	var parts []string
	if r.cfg.SegmentLength > 0 {
		parts = append(parts, fmt.Sprintf("target_duration=%s",
			units.FormatDuration(r.cfg.SegmentLength)))
	}
	if r.cfg.SegmentSize > 0 {
		parts = append(parts, fmt.Sprintf("target_size=%s",
			units.FormatSize(r.cfg.SegmentSize)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (r *Recorder) displayPath(file string) string {
	if r.cfg.ShortPaths {
		return filepath.Base(file)
	}
	return file
}

func (r *Recorder) emitSegmentStart(file, targetInfo string) {
	msg := fmt.Sprintf("SEGMENT START file=%s segment=%d recording=%d",
		r.displayPath(file), r.segmentCount-1, r.recordingCount-1)
	if targetInfo != "" {
		msg += " " + targetInfo
	}
	r.log.Event(msg)
}

func (r *Recorder) emitSegmentFinish(file, targetInfo string, elapsed time.Duration, size int64, trigger string, ffmpegErr error) {
	msg := fmt.Sprintf("SEGMENT FINISH file=%s segment=%d recording=%d duration=%s size=%s trigger=%s",
		r.displayPath(file), r.segmentCount-1, r.recordingCount-1,
		units.FormatDuration(elapsed),
		formatBytes(size),
		trigger)
	if targetInfo != "" {
		msg += " " + targetInfo
	}
	if ffmpegErr != nil && trigger == "error" {
		msg += fmt.Sprintf(" error=%v", ffmpegErr)
	}
	r.log.Event(msg)
}

// --- Log file management ---

func (r *Recorder) openLogFile() error {
	tmplData := NewTemplateData(
		r.cfg.Source, r.cfg.Driver.Name(),
		r.sessionStart, r.sessionStart, r.sessionStart,
		0, 0,
	)

	logPath, err := RenderTemplate(r.cfg.LogPattern, tmplData)
	if err != nil {
		return fmt.Errorf("render log template: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	r.logFile = f
	r.log.SetFile(f)
	r.log.Info("logging to %s", logPath)
	return nil
}

func (r *Recorder) closeLogFile() {
	if r.logFile != nil {
		r.log.SetFile(nil)
		r.logFile.Close()
		r.logFile = nil
	}
}

func (r *Recorder) streamOpts() stream.StreamOpts {
	r.activeCookies = ""
	if r.cfg.CookiePool != nil {
		r.activeCookies = r.cfg.CookiePool.Select()
	}
	return stream.StreamOpts{
		Source:     r.cfg.Source,
		Domain:     r.cfg.Domain,
		Resolution: r.cfg.Resolution,
		Framerate:  r.cfg.Framerate,
		Cookies:    r.activeCookies,
		UserAgent:  r.cfg.UserAgent,
	}
}

// penalizeCookie deprioritizes the currently active cookie set in the pool.
func (r *Recorder) penalizeCookie() {
	if r.cfg.CookiePool != nil && r.activeCookies != "" {
		r.cfg.CookiePool.Penalize(r.activeCookies)
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1fGB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}
