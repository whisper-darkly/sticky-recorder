package logger

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level represents a log severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// ParseLevel converts a string to a Level. Case-insensitive. Defaults to LevelInfo.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	case "fatal":
		return LevelFatal
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "???"
	}
}

// Logger provides leveled, dual-output logging.
//
// Without a log file (file == nil):
//   - DEBUG/INFO messages → stdout
//   - WARN/ERROR/FATAL messages → stderr
//   - Event messages → stdout
//
// With a log file:
//   - All messages (at or above level) → file
//   - Event messages additionally → stdout
//   - WARN/ERROR/FATAL additionally → stderr
type Logger struct {
	level Level
	file  io.Writer // nil if no log file
	mu    sync.Mutex
}

// New creates a Logger at the given level with no file output.
func New(level Level) *Logger {
	return &Logger{level: level}
}

// SetFile sets the log file writer. Pass nil to disable file logging.
func (l *Logger) SetFile(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file = w
}

// HasFile reports whether a log file is configured.
func (l *Logger) HasFile() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file != nil
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(format string, args ...any) { l.emit(LevelDebug, format, args...) }

// Info logs at INFO level.
func (l *Logger) Info(format string, args ...any) { l.emit(LevelInfo, format, args...) }

// Warn logs at WARN level.
func (l *Logger) Warn(format string, args ...any) { l.emit(LevelWarn, format, args...) }

// Error logs at ERROR level.
func (l *Logger) Error(format string, args ...any) { l.emit(LevelError, format, args...) }

// Fatal logs at FATAL level then exits.
func (l *Logger) Fatal(format string, args ...any) {
	l.emit(LevelFatal, format, args...)
	os.Exit(1)
}

// Event emits a lifecycle event (session/recording/segment). Events always emit
// regardless of log level.
//
// Without a log file: printed to stdout.
// With a log file: printed to both the file and stdout.
func (l *Logger) Event(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006/01/02 15:04:05")
	line := fmt.Sprintf("%s [EVENT] %s", ts, msg)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		fmt.Fprintln(l.file, line)
		fmt.Fprintln(os.Stdout, line)
	} else {
		fmt.Fprintln(os.Stdout, line)
	}
}

// Writer returns an io.Writer that logs each line at the given level.
// Useful for capturing subprocess output (e.g. ffmpeg stderr).
func (l *Logger) Writer(level Level) io.Writer {
	return &writerAdapter{logger: l, level: level}
}

func (l *Logger) emit(level Level, format string, args ...any) {
	if level < l.level {
		return
	}

	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006/01/02 15:04:05")
	line := fmt.Sprintf("%s [%s] %s", ts, level, msg)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		// File gets everything
		fmt.Fprintln(l.file, line)
		// WARN+ also to stderr
		if level >= LevelWarn {
			fmt.Fprintln(os.Stderr, line)
		}
	} else {
		// No file: INFO and below to stdout, WARN+ to stderr
		if level >= LevelWarn {
			fmt.Fprintln(os.Stderr, line)
		} else {
			fmt.Fprintln(os.Stdout, line)
		}
	}
}

type writerAdapter struct {
	logger *Logger
	level  Level
}

func (w *writerAdapter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n\r")
	if msg != "" {
		w.logger.emit(w.level, "%s", msg)
	}
	return len(p), nil
}
