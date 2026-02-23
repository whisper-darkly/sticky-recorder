package recorder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

	SegmentLength time.Duration // Max segment duration (0 = no splitting)

	RetryDelay  time.Duration // Delay between retry attempts
	RetryJitter time.Duration // Max random jitter added to each retry delay (0 = disabled)
	SegmentTimeout   time.Duration // Reconnect within this = same recording, new segment
	RecordingTimeout time.Duration // Reconnect within this = new recording in same session
	CheckInterval    time.Duration // Watch mode interval (0 = one-shot, exit on offline/end)
	SleepJitter      time.Duration // Max random jitter added to CheckInterval sleep (0 = disabled)
	HeartbeatInterval time.Duration // Interval for HEARTBEAT events (0 = disabled)

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

	// Cookie selected for the current connection
	activeCookies string

	// Log file handle (if any)
	logFile *os.File
}

// segmentedResult holds the outcome of a single ffmpeg segment muxer run.
type segmentedResult struct {
	err       error
	execFatal bool
	segCount  int // total segments produced (completed + final in-progress)
}

// kv is a shorthand for logger.KV.
func kv(key, value string) logger.KV { return logger.KV{Key: key, Value: value} }

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

// retryDelay returns the configured retry delay plus a random jitter in [0, RetryJitter).
func (r *Recorder) retryDelay() time.Duration {
	delay := r.cfg.RetryDelay
	if r.cfg.RetryJitter > 0 {
		delay += time.Duration(rand.Int63n(int64(r.cfg.RetryJitter)))
	}
	return delay
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
		sleepDur := r.cfg.CheckInterval
		if r.cfg.SleepJitter > 0 {
			sleepDur += time.Duration(rand.Int63n(int64(r.cfg.SleepJitter)))
		}
		r.log.Event("SLEEP",
			kv("source", r.cfg.Source),
			kv("interval", units.FormatDuration(sleepDur)))
		select {
		case <-time.After(sleepDur):
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

	r.log.Event("SESSION START",
		kv("source", r.cfg.Source),
		kv("driver", r.cfg.Driver.Name()),
		kv("resolution", fmt.Sprintf("%dp/%dfps", info.Resolution, info.Framerate)))

	exitCode := r.sessionLoop(ctx, info)

	r.log.Event("SESSION END",
		kv("source", r.cfg.Source),
		kv("recordings", fmt.Sprintf("%d", r.recordingCount)),
		kv("duration", units.FormatDuration(time.Since(r.sessionStart))))

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

			delay := r.retryDelay()
			r.log.Debug("waiting %s before checking for new recording...", units.FormatDuration(delay))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return 0
			}

			var err error
			newInfo, err = r.cfg.Driver.CheckStream(ctx, r.streamOpts())
			if err == nil {
				break
			}

			itype := r.cfg.Driver.ClassifyError(err)
			if itype == stream.Fatal {
				r.log.Error("%v (not retrying)", err)
				return 1
			}
			// Blocked is treated as transient here: a Cloudflare challenge during a
			// between-recording check is often short-lived. Penalize the cookie and
			// keep retrying within the deadline rather than hard-exiting with code 3.
			if itype == stream.Blocked {
				r.penalizeCookie()
				r.log.Warn("access blocked, retrying: %v", err)
			} else {
				r.log.Debug("source not available: %v (retrying until %s)", err, deadline.Format("15:04:05"))
			}
		}

		if newInfo == nil {
			r.log.Info("session timeout reached (%s), no new recording",
				units.FormatDuration(r.cfg.RecordingTimeout))
			return 0
		}

		info = newInfo
	}
}

// runRecording manages a single recording.
// Returns >=0 for a final exit code, or -1 to signal "try for a new recording."
func (r *Recorder) runRecording(ctx context.Context, info *stream.StreamInfo) int {
	r.recordingStart = time.Now()
	r.recordingCount++

	r.log.Event("RECORDING START",
		kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
		kv("resolution", fmt.Sprintf("%dp/%dfps", info.Resolution, info.Framerate)))

	// Use ffmpeg segment muxer for time-based splitting
	if r.cfg.SegmentLength > 0 {
		return r.runSegmentedRecording(ctx, info)
	}

	return r.runSingleFile(ctx, info)
}

// runSingleFile records to a single file (no splitting).
func (r *Recorder) runSingleFile(ctx context.Context, info *stream.StreamInfo) int {
	segmentCount := 0
	for {
		if ctx.Err() != nil {
			return 0
		}

		segStart := time.Now()
		outFile, ffmpegErr := r.runSingleFFmpeg(ctx, info)
		segmentCount++

		// Emit segment events even in single-file mode
		var fileSize int64
		if outFile != "" {
			if fi, err := os.Stat(outFile); err == nil {
				fileSize = fi.Size()
			}
		}

		if outFile != "" {
			r.log.Event("SEGMENT START",
				kv("file", outFile),
				kv("segment", fmt.Sprintf("%d", segmentCount-1)),
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)))

			trigger := "stream_end"
			if ffmpegErr != nil {
				if ctx.Err() != nil {
					trigger = "cancelled"
				} else {
					trigger = "error"
				}
			}

			elapsed := time.Since(segStart)
			r.log.Event("SEGMENT FINISH",
				kv("file", outFile),
				kv("segment", fmt.Sprintf("%d", segmentCount-1)),
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("duration", units.FormatDuration(elapsed)),
				kv("size", formatBytes(fileSize)),
				kv("trigger", trigger))
		}

		// Run exec on the file
		if fileSize > 0 && len(r.cfg.ExecArgs) > 0 {
			if execErr := r.runExec(outFile); execErr != nil {
				r.log.Event("RECORDING END",
					kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
					kv("segments", fmt.Sprintf("%d", segmentCount)),
					kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
					kv("trigger", "exec_fatal"))
				return 1
			}
		}

		if ffmpegErr == nil {
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", segmentCount)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", "stream_end"))
			return -1
		}

		if ctx.Err() != nil {
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", segmentCount)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", "cancelled"))
			return 0
		}

		itype := r.cfg.Driver.ClassifyError(ffmpegErr)
		if itype == stream.Blocked || itype == stream.TransientError {
			r.penalizeCookie()
		}
		if itype == stream.Blocked {
			r.log.Error("blocked: %v", ffmpegErr)
			return 3
		}
		if itype == stream.Fatal {
			r.log.Error("fatal: %v", ffmpegErr)
			return 1
		}

		// Ended or TransientError: try reconnecting within segment-timeout
		info = r.reconnect(ctx, info)
		if info == nil {
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", segmentCount)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", "timeout"))
			return -1
		}
	}
}

// runSingleFFmpeg records to a single file and returns the output path and any error.
func (r *Recorder) runSingleFFmpeg(ctx context.Context, info *stream.StreamInfo) (string, error) {
	tmplData := NewTemplateData(
		r.cfg.Source, r.cfg.Driver.Name(),
		r.sessionStart, r.recordingStart, time.Now(),
		r.recordingCount-1, 0,
	)

	outBase, err := RenderTemplate(r.cfg.OutPattern, tmplData)
	if err != nil {
		r.log.Error("render output template: %v", err)
		return "", fmt.Errorf("render output template: %w", err)
	}
	outFile := outBase + "." + r.cfg.Driver.FileExtension()

	if err := os.MkdirAll(filepath.Dir(outFile), 0755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	r.log.Info("recording to %s", outFile)

	args := r.ffmpegInputArgs(info.URL, "warning")
	args = append(args,
		"-c", "copy",
		"-copyts", "-start_at_zero",
		"-muxdelay", "0", "-muxpreload", "0",
		outFile,
	)

	r.log.Debug("ffmpeg %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = r.log.Writer(logger.LevelDebug)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start ffmpeg: %w", err)
	}

	ffmpegErr := cmd.Wait()

	// Remove empty files
	if fi, err := os.Stat(outFile); err == nil && fi.Size() == 0 {
		os.Remove(outFile)
		return "", ffmpegErr
	}

	return outFile, ffmpegErr
}

// --- Segmented recording (ffmpeg -f segment muxer) ---

// runSegmentedRecording uses ffmpeg's segment muxer for glitch-free time-based splitting.
// One ffmpeg process handles all splits internally — the stream connection stays alive.
// Segment events are tracked in real time via stderr (SEGMENT START) and stdout (SEGMENT FINISH).
func (r *Recorder) runSegmentedRecording(ctx context.Context, info *stream.StreamInfo) int {
	totalSegments := 0

	for {
		if ctx.Err() != nil {
			return 0
		}

		// Render base path for this recording's segments
		tmplData := NewTemplateData(
			r.cfg.Source, r.cfg.Driver.Name(),
			r.sessionStart, r.recordingStart, time.Now(),
			r.recordingCount-1, 0,
		)
		outBase, err := RenderTemplate(r.cfg.OutPattern, tmplData)
		if err != nil {
			r.log.Error("render output template: %v", err)
			return 1
		}

		ext := r.cfg.Driver.FileExtension()
		if err := os.MkdirAll(filepath.Dir(outBase), 0755); err != nil {
			r.log.Error("create output directory: %v", err)
			return 1
		}

		// ffmpeg writes: outBase_00000.ext, outBase_00001.ext, ...
		segPattern := outBase + "_%05d." + ext

		result := r.runFFmpegSegmented(ctx, info.URL, segPattern, ext, totalSegments, outBase)
		totalSegments += result.segCount

		if result.execFatal {
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", totalSegments)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", "exec_fatal"))
			return 1
		}

		if result.err == nil || ctx.Err() != nil {
			trigger := "stream_end"
			if ctx.Err() != nil {
				trigger = "cancelled"
			}
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", totalSegments)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", trigger))
			if ctx.Err() != nil {
				return 0
			}
			return -1
		}

		// Error: classify and maybe reconnect
		itype := r.cfg.Driver.ClassifyError(result.err)
		if itype == stream.Blocked || itype == stream.TransientError {
			r.penalizeCookie()
		}
		if itype == stream.Blocked {
			r.log.Error("blocked: %v", result.err)
			return 3
		}
		if itype == stream.Fatal {
			r.log.Error("fatal: %v", result.err)
			return 1
		}

		// Try reconnecting
		info = r.reconnect(ctx, info)
		if info == nil {
			r.log.Event("RECORDING END",
				kv("recording", fmt.Sprintf("%d", r.recordingCount-1)),
				kv("segments", fmt.Sprintf("%d", totalSegments)),
				kv("duration", units.FormatDuration(time.Since(r.recordingStart))),
				kv("trigger", "timeout"))
			return -1
		}
		// Reconnected: loop back, start new ffmpeg with continued segment numbering
	}
}

// runFFmpegSegmented launches ffmpeg with -f segment and monitors segment progress
// in real time via stderr (Opening lines → SEGMENT START) and stdout (segment_list → SEGMENT FINISH).
func (r *Recorder) runFFmpegSegmented(ctx context.Context, streamURL, segPattern, ext string, startNumber int, outBase string) segmentedResult {
	args := r.ffmpegInputArgs(streamURL, "info")
	args = append(args,
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
		"-muxdelay", "0", "-muxpreload", "0",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%d", int(r.cfg.SegmentLength.Seconds())),
		"-segment_format", segmentFormat(ext),
		"-reset_timestamps", "1",
		"-segment_start_number", fmt.Sprintf("%d", startNumber),
		"-segment_list", "pipe:1",
		"-segment_list_type", "flat",
		segPattern,
	)

	r.log.Debug("ffmpeg %s", strings.Join(args, " "))

	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return segmentedResult{err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return segmentedResult{err: fmt.Errorf("stderr pipe: %w", err)}
	}

	if err := cmd.Start(); err != nil {
		return segmentedResult{err: fmt.Errorf("start ffmpeg: %w", err)}
	}

	// Graceful shutdown: SIGTERM → wait → SIGKILL (instead of Go's default instant SIGKILL)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			terminateProcess(cmd)
		case <-done:
		}
	}()

	// Shared state between goroutines
	var mu sync.Mutex
	segStarts := make(map[string]time.Time) // file path → start time
	var lastOpenedFile string
	var stderrSegCount int   // segments seen via stderr (Opening lines)
	var stdoutSegCount int   // segments completed via stdout (segment_list)
	var execFatal bool
	ffmpegStarted := time.Now()

	completedFiles := make(map[string]bool) // files reported by stdout

	var wg sync.WaitGroup

	// Expected base filename for filtering stderr Opening lines
	outBaseFile := filepath.Base(outBase)

	outDir := filepath.Dir(outBase)
	recNum := fmt.Sprintf("%d", r.recordingCount-1)
	targetDur := units.FormatDuration(r.cfg.SegmentLength)

	// Heartbeat goroutine: periodically emits HEARTBEAT events while a segment is active.
	if r.cfg.HeartbeatInterval > 0 {
		go func() {
			ticker := time.NewTicker(r.cfg.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					mu.Lock()
					file := lastOpenedFile
					segNum := startNumber + stderrSegCount - 1
					segStart := segStarts[file]
					mu.Unlock()
					if file == "" {
						continue
					}
					var bytesWritten int64
					if fi, err := os.Stat(file); err == nil {
						bytesWritten = fi.Size()
					}
					segDur := time.Duration(0)
					if !segStart.IsZero() {
						segDur = time.Since(segStart)
					}
					r.log.Event("HEARTBEAT",
						kv("file", file),
						kv("bytes_written", fmt.Sprintf("%d", bytesWritten)),
						kv("segment_duration", units.FormatDuration(segDur)),
						kv("session_duration", units.FormatDuration(time.Since(r.sessionStart))),
						kv("recording", recNum),
						kv("segment", fmt.Sprintf("%d", segNum)))
				}
			}
		}()
	}

	// stderr goroutine: parse [segment @] Opening lines → SEGMENT START
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		logWriter := r.log.Writer(logger.LevelDebug)
		for scanner.Scan() {
			line := scanner.Text()

			if file := parseSegmentOpening(line); file != "" && strings.Contains(filepath.Base(file), outBaseFile) {
				mu.Lock()
				segNum := startNumber + stderrSegCount
				segStarts[file] = time.Now()
				lastOpenedFile = file
				stderrSegCount++
				mu.Unlock()

				r.log.Event("SEGMENT START",
					kv("file", file),
					kv("segment", fmt.Sprintf("%d", segNum)),
					kv("recording", recNum),
					kv("target_duration", targetDur))
			} else {
				// Capture but only show at debug level
				fmt.Fprintln(logWriter, line)
			}
		}
	}()

	// stdout goroutine: completed segment filenames → SEGMENT FINISH + --exec
	// Note: ffmpeg segment_list outputs bare filenames (no directory),
	// so we resolve them to full paths using the output directory.
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			name := strings.TrimSpace(scanner.Text())
			if name == "" {
				continue
			}

			// Resolve to full path if not already absolute
			file := name
			if !filepath.IsAbs(file) {
				file = filepath.Join(outDir, file)
			}

			mu.Lock()
			segNum := startNumber + stdoutSegCount
			start, hasStart := segStarts[file]
			if !hasStart {
				start = ffmpegStarted
			}
			completedFiles[file] = true
			stdoutSegCount++
			mu.Unlock()

			elapsed := time.Since(start)
			var fileSize int64
			if fi, err := os.Stat(file); err == nil {
				fileSize = fi.Size()
			}

			trigger := "split"
			if ctx.Err() != nil {
				trigger = "cancelled"
			}

			r.log.Event("SEGMENT FINISH",
				kv("file", file),
				kv("segment", fmt.Sprintf("%d", segNum)),
				kv("recording", recNum),
				kv("duration", units.FormatDuration(elapsed)),
				kv("size", formatBytes(fileSize)),
				kv("trigger", trigger))

			if fileSize > 0 && len(r.cfg.ExecArgs) > 0 {
				if execErr := r.runExec(file); execErr != nil {
					mu.Lock()
					execFatal = true
					mu.Unlock()
					terminateProcess(cmd)
					return
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	wg.Wait()

	mu.Lock()
	lastFile := lastOpenedFile
	lastStart := segStarts[lastFile]
	isCompleted := completedFiles[lastFile]
	totalSeg := stderrSegCount
	fatal := execFatal
	mu.Unlock()

	// Finalize the last segment (the one being written when ffmpeg exited)
	if lastFile != "" && !isCompleted && !fatal {
		var fileSize int64
		if fi, err := os.Stat(lastFile); err == nil {
			fileSize = fi.Size()
		}

		if fileSize == 0 {
			os.Remove(lastFile)
		} else {
			trigger := "stream_end"
			if waitErr != nil {
				if ctx.Err() != nil {
					trigger = "cancelled"
				} else {
					trigger = "error"
				}
			}

			if lastStart.IsZero() {
				lastStart = ffmpegStarted
			}
			elapsed := time.Since(lastStart)
			segNum := startNumber + totalSeg - 1

			r.log.Event("SEGMENT FINISH",
				kv("file", lastFile),
				kv("segment", fmt.Sprintf("%d", segNum)),
				kv("recording", recNum),
				kv("duration", units.FormatDuration(elapsed)),
				kv("size", formatBytes(fileSize)),
				kv("trigger", trigger))

			if fileSize > 0 && len(r.cfg.ExecArgs) > 0 {
				if execErr := r.runExec(lastFile); execErr != nil {
					fatal = true
				}
			}
		}
	}

	return segmentedResult{
		err:       waitErr,
		execFatal: fatal,
		segCount:  totalSeg,
	}
}

// parseSegmentOpening extracts the filename from ffmpeg segment muxer log lines.
// Input:  [segment @ 0x56d74ca4ff00] Opening './test_00009.ts' for writing
// Output: ./test_00009.ts
func parseSegmentOpening(line string) string {
	const prefix = "] Opening '"
	const suffix = "' for writing"
	i := strings.Index(line, prefix)
	if i < 0 {
		return ""
	}
	rest := line[i+len(prefix):]
	j := strings.Index(rest, suffix)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// segmentFormat maps file extensions to ffmpeg segment format names.
func segmentFormat(ext string) string {
	if ext == "ts" {
		return "mpegts"
	}
	return ext
}

// --- Common helpers ---

// ffmpegInputArgs builds the common ffmpeg input arguments (before -c copy).
func (r *Recorder) ffmpegInputArgs(streamURL, loglevel string) []string {
	args := []string{
		"-y", "-hide_banner", "-loglevel", loglevel,
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

	args = append(args, "-i", streamURL)
	return args
}

// reconnect tries to reconnect within the segment timeout window.
// Returns new StreamInfo on success, nil on timeout.
func (r *Recorder) reconnect(ctx context.Context, _ *stream.StreamInfo) *stream.StreamInfo {
	deadline := time.Now().Add(r.cfg.SegmentTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil
		}

		delay := r.retryDelay()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}

		newInfo, err := r.cfg.Driver.CheckStream(ctx, r.streamOpts())
		if err == nil {
			return newInfo
		}

		it := r.cfg.Driver.ClassifyError(err)
		if it == stream.Fatal {
			r.log.Error("%v (not retrying)", err)
			return nil
		}
		// Blocked is treated as transient during reconnect: penalize the cookie
		// and keep retrying within the segment timeout window.
		if it == stream.Blocked {
			r.penalizeCookie()
			r.log.Warn("access blocked during reconnect, retrying: %v", err)
		} else {
			r.log.Debug("reconnecting: %v (delay %s, until %s)", err, units.FormatDuration(delay), deadline.Format("15:04:05"))
		}
	}
	return nil
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

// runExec executes the configured command with {} replaced by the file path.
func (r *Recorder) runExec(file string) error {
	args := make([]string, len(r.cfg.ExecArgs))
	for i, a := range r.cfg.ExecArgs {
		args[i] = strings.ReplaceAll(a, "{}", file)
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
