# sticky-recorder

Record live streams from Chaturbate and Stripchat. Designed to be called at regular intervals (e.g. via cron); exits quickly with code 2 if the source is offline. Use `--check-interval` to watch persistently instead.

## Build

```
make
```

The binary is placed in `dist/sticky-recorder`.

## Install

```
sudo make install            # installs to /usr/local/bin
make install PREFIX=~/.local  # installs to ~/.local/bin
```

## Usage

```
sticky-recorder [flags] <source> [output-template]
```

### Duration & Size Formats

All duration flags accept flexible formats:
- `hh:mm:ss` — e.g. `01:30:00`
- Go-style — e.g. `1h30m`, `5m`, `30s`
- Plain number — interpreted as minutes (e.g. `90`)

Size flags accept:
- `{n}TB/GB/MB` — e.g. `500MB`, `1.5GB`, `2TB`
- Plain number — interpreted as MB (e.g. `500`)

### Examples

```bash
# Basic recording
sticky-recorder -s alice -o 'recordings/{{.Source}}_{{.Recording.Year}}-{{.Recording.Month}}-{{.Recording.Day}}'

# Post-process segments with ffmpeg
sticky-recorder -s alice -o /tmp/out --exec ffmpeg -i {} -c:v libx264 {}.mp4 \;

# Stripchat with segment splitting (1 hour segments)
sticky-recorder --driver stripchat -s bob --segment-length 1h -o 'rec/{{.Source}}/{{.Recording.Count}}_{{.Segment.Count}}'

# Watch mode: check every 5 minutes, record when online
sticky-recorder -i 5m -s alice -o 'rec/{{.Source}}/{{.Session.Year}}-{{.Session.Month}}-{{.Session.Day}}'
```

### Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--driver` | `-d` | `chaturbate` | Driver name (`chaturbate`, `stripchat`) |
| `--source` | `-s` | | Source/channel name (required) |
| `--out` | `-o` | | Output file path template (required) |
| `--resolution` | `-r` | `720` | Target video resolution height |
| `--framerate` | `-f` | `30` | Target framerate |
| `--segment-length` | `-l` | `0` | Max segment duration (0=disabled, e.g. `60`, `1h`, `01:00:00`) |
| `--segment-size` | `-z` | `0` | Max segment size (0=disabled, e.g. `500`, `500MB`, `1.5GB`) |
| `--cookies` | `-c` | | HTTP cookie set, `file://` path, or `http(s)://` URL |
| `--user-agent` | `-a` | | Custom User-Agent header |
| `--exec` | `-e` | | Command to run on finished segments (find-style: `{} \;`) |
| `--log` | | | Log file path template |
| `--log-level` | | `info` | Log level: `debug`, `info`, `warn`, `error`, `fatal` |
| `--retry-delay` | | `00:00:05` | Delay between retry attempts (e.g. `5s`, `00:00:05`) |
| `--segment-timeout` | | `00:05:00` | Reconnect within this = same recording (e.g. `5m`) |
| `--recording-timeout` | | `00:30:00` | Reconnect within this = new recording in session (e.g. `30m`) |
| `--check-interval` | `-i` | | Watch interval when offline (0=exit, e.g. `5m`, `00:05:00`) |
| `--version` | `-V` | | Print version and exit |

### Watch Mode

When `--check-interval` is set, the recorder becomes a persistent watcher:
- If the source is offline or a session ends, it sleeps for the given interval and retries.
- Fatal errors (exit code 1) and access blocks (exit code 3) still cause an immediate exit.
- A `SLEEP` event is emitted before each wait.

Without `--check-interval`, behavior is unchanged: the recorder exits immediately with code 2 when the source is offline.

### Environment Variables

CLI flags take priority over environment variables.

`STICKY_DRIVER`, `STICKY_RESOLUTION`, `STICKY_FRAMERATE`, `STICKY_SEGMENT_LENGTH`, `STICKY_SEGMENT_SIZE`, `STICKY_COOKIES`, `STICKY_USER_AGENT`, `STICKY_LOG`, `STICKY_LOG_LEVEL`, `STICKY_RETRY_DELAY`, `STICKY_SEGMENT_TIMEOUT`, `STICKY_RECORDING_TIMEOUT`, `STICKY_CHECK_INTERVAL`

Environment-only:
- `STICKY_EXEC_FATAL=1` — non-zero exec exit codes kill the session.
- `STICKY_SHORT_PATHS=1` — emit only filenames (not full paths) in segment events.

### Cookie Pool

The `--cookies` flag (or `STICKY_COOKIES`) accepts three source types:

- **Literal** — a plain cookie string (e.g. `key=val; key2=val2`). Used directly, no extra config needed.
- **File** — `file:///path/to/cookies.txt`. Requires `STICKY_EXTERNAL_COOKIES=1`.
- **URL** — `https://cookies.internal/api/get?driver={{.Driver}}&source={{.Source}}`. Requires `STICKY_EXTERNAL_COOKIES=1` and `STICKY_COOKIES_SAFE_DOMAINS`.

URL templates support `{{.Driver}}` and `{{.Source}}` placeholders.

When the source contains multiple cookie sets (via JSON mode), they are rotated using smooth weighted round-robin. Cookie sets that cause blocked or transient errors are automatically penalized and deprioritized.

| Env Var | Description |
|---------|-------------|
| `STICKY_EXTERNAL_COOKIES` | Set to `1` to enable `file://` and `http(s)://` cookie sources |
| `STICKY_COOKIES_SAFE_DOMAINS` | Comma-separated safe domains/CIDRs for URL sources (e.g. `10.0.0.0/8,cookies.internal`) |
| `STICKY_COOKIES_JSON` | Set to `1` to parse external cookie files/responses as JSON arrays of cookie strings |
| `STICKY_COOKIES_REFRESH` | Refresh interval for external sources (e.g. `5m`, `01:00:00`) |

### Template Variables

| Scope | Variables |
|-------|-----------|
| General | `{{.Source}}`, `{{.Driver}}` |
| Session | `{{.Session.Year}}`, `.Month`, `.Day`, `.Hour`, `.Minute`, `.Second`, `.Unix` |
| Recording | `{{.Recording.Year}}`, ... (same fields), `{{.Recording.Count}}` |
| Segment | `{{.Segment.Year}}`, ... (same fields), `{{.Segment.Count}}` |

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Clean exit |
| 1 | General error |
| 2 | Source offline (with `--check-interval`, retries instead of exiting) |
| 3 | Access blocked (Cloudflare, age verification, etc.) |

## Acknowledgements

Inspired by [chaturbate-dvr](https://github.com/teacat/chaturbate-dvr) and [ctbcap](https://github.com/KFERMercer/ctbcap). sticky-recorder is a ground-up rewrite with a different architecture, but those projects provided the initial spark and domain knowledge.
