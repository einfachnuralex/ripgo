# ripgo

Go-based disc ripping and encoding pipeline. Wraps **MakeMKV** (rip) and **FFmpeg** (encode) into a single CLI with a bubbletea TUI progress display.

## Project structure

```
cmd/ripgo/main.go              CLI entry point, flag parsing
internal/config/config.go      YAML config loading + defaults
internal/makemkv/makemkv.go    MakeMKV wrapper (scan + rip)
internal/encoder/encoder.go    FFmpeg wrapper (probe + encode)
internal/metadata/metadata.go  TMDB / OMDB / TVDB lookups
internal/ripper/ripper.go      Orchestration, pipeline, TUI
```

## Prerequisites

Three external binaries must be in PATH:
- `makemkvcon` — MakeMKV CLI
- `ffmpeg` — encoding
- `ffprobe` — stream analysis (part of FFmpeg)

Checked at startup via `checkPrerequisites()` in `cmd/ripgo/main.go`.

## Pipeline

Two phases to avoid a conflict between `fmt.Scanln` (interactive prompt) and bubbletea (raw terminal mode):

**Phase 1 — plain stdout** (before TUI starts):
1. Scan disc (`makemkvcon -r --robot info`)
2. Auto-detect content type (TV / movie) or prompt user
3. Metadata lookup (TMDB → OMDB → TVDB fallback chain)

**Phase 2 — bubbletea TUI** (rip + encode):
- **Parallel mode** (default): rip goroutine feeds a buffered channel → encode goroutine consumes it. Rip and encode of adjacent titles overlap.
- **Sequential mode** (`--sequential`): rip all titles first, then encode all.

Work runs in a separate goroutine; results are sent to the TUI via `tea.Program.Send()`. The TUI exits when it receives `msgQuit`.

## TUI

`internal/ripper/ripper.go` contains all bubbletea/lipgloss code.

Three lipgloss panels rendered via `lipgloss.JoinVertical`:
- **Ripping** — green border (`"2"`)
- **Encoding** — cyan border (`"6"`)
- **Overall Progress** — yellow border (`"3"`)

Each panel shows: task label, Unicode block bar (`█░`), percentage (1 decimal), elapsed time, ETA, and up to 3 caption lines (ring buffer from MakeMKV status messages).

Progress updates use permille (0–1000) deduplication to avoid flooding the event loop with identical values.

`tea.Println()` is used for permanent log lines that scroll above the live panels.

## Cache / temp directory

If `--temp` / `cfg.TempDir` is set (user-specified cache):
- A timestamped subdirectory (`20060102-150405`) is created inside it on each run, preventing two concurrent runs from sharing files.
- At startup the base directory is checked: if not empty, an error is returned unless `--force-clean` is set.
- On **successful** completion the timestamped subdirectory is deleted unless `--keep-cache` is set.
- On **failure** the directory is kept for debugging.

If no `--temp` is given, `os.MkdirTemp` creates a throw-away directory that is always cleaned up via `defer os.RemoveAll`.

## Configuration file

Search order (first match wins):
- Linux/macOS: `$XDG_CONFIG_HOME/ripgo/config.yaml`, `~/.ripgo.yaml`, `./ripgo.yaml`, `./config.yaml`
- Windows: `%APPDATA%\ripgo\config.yaml`, `~\.ripgo.yaml`

Full example:

```yaml
disc:
  default_path: "disc:0"
  default_temp_dir: "/tmp/ripgo-cache"

output:
  movies_dir: "~/Videos/Movies"
  tv_dir: "~/Videos/TV"

encoding:
  include_subtitles: true
  include_stereo_audio: true
  include_surround_audio: true
  languages:
    - "eng"
    - "ger"
  min_movie_duration_seconds: 1800    # 30 min
  min_episode_duration_seconds: 1200  # 20 min
  max_episode_duration_seconds: 3600  # 60 min
  profiles:
    - name: h264_crf22_slow
      codec: libx264
      crf: 22
      preset: slow
      pix_fmt: yuv420p
      video_filter: "bwdif=mode=send_frame:parity=auto:deint=interlaced"
    - name: h265_crf24_medium
      codec: libx265
      crf: 24
      preset: medium
      pix_fmt: yuv420p10le
    - name: hevc_vaapi_qp23
      codec: hevc_vaapi
      crf: 23
    - name: hevc_vaapi_qp23_explicit
      codec: hevc_vaapi
      crf: 23
      hw_device: /dev/dri/renderD128
      video_filter: "format=nv12,hwupload"

metadata:
  lookup_enabled: true
  tmdb_api_key: ""   # or set TMDB_API_KEY env var
  omdb_api_key: ""   # or set OMDB_API_KEY env var
  tvdb_api_key: ""   # or set TVDB_API_KEY env var
```

Environment variables `TMDB_API_KEY`, `OMDB_API_KEY`, `TVDB_API_KEY` override the config file values.

If no config file is found, `config.Defaults()` is used. The built-in default profile is `h264_crf22_slow` (libx264, CRF 22, slow preset, yuv420p, bwdif deinterlace).

## Encoding profiles

Profiles are defined in the config file under `encoding.profiles`. Each profile has:

| Field | YAML key | Description |
|-------|----------|-------------|
| Name | `name` | Identifier used with `--profile` |
| Codec | `codec` | FFmpeg video codec (e.g. `libx264`, `libx265`, `hevc_vaapi`) |
| CRF | `crf` | Quality value; `0` = omit quality flag entirely |
| Preset | `preset` | Encoder speed/quality preset; empty = omit `-preset` |
| PixFmt | `pix_fmt` | Pixel format (e.g. `yuv420p`); empty = omit `-pix_fmt` |
| VideoFilter | `video_filter` | Value passed to `-vf`; empty = no filter |
| HWDevice | `hw_device` | Hardware device path (e.g. `/dev/dri/renderD128`); produces `-vaapi_device` before `-i` |
| ExtraArgs | `extra_args` | List of arbitrary extra ffmpeg flags appended after codec args |

**Quality flag auto-detection:** codecs ending in `_vaapi` (e.g. `hevc_vaapi`, `h264_vaapi`) use `-global_quality` instead of `-crf`. All other codecs use `-crf`.

Without `--profile`, the first defined profile is used. Unknown profile names produce an error listing available names.

The type chain: `config.EncodingProfile` (config package) → converted to `encoder.VideoProfile` (encoder package) inside `ripper.encodeOne()`. This avoids an import cycle between `config` and `encoder`.

## CLI flags

```
--output, -o <path>        Output directory (required)
--mode <mode>              auto | movie | tv  (default: auto)
--disc <path>              Disc path (default: disc:0)
--temp <path>              Cache directory for ripped files
--title <text>             Override title for metadata lookup
--year <year>              Release year
--season <n>               Season number (default: 1)
--episode-start <n>        Starting episode number (default: 1)
--disc-type <type>         dvd | bd | uhd
--sequential               Disable parallel rip+encode pipeline
--keep-cache               Don't delete cache after successful rip+encode
--force-clean              Clean cache directory if not empty at startup
--profile <name>           Encoding profile name (defined in config file)
--debug                    Verbose output: MakeMKV lines + ffmpeg command
-v, --version              Show version
-h, --help                 Show help
```

## Build

```sh
go build ./...
go build -o ripgo ./cmd/ripgo
```

No code generation. No external test fixtures needed for `go build`.

## Key design decisions

- **No alt-screen**: bubbletea runs with `tea.WithOutput(os.Stdout)` (inline mode) so log lines printed before the TUI remain visible in the terminal scrollback.
- **Two-phase split**: `fmt.Scanln` for the TV/movie prompt must run before `tea.NewProgram` takes the terminal into raw mode.
- **Import cycle prevention**: `config` defines `EncodingProfile`; `encoder` defines `VideoProfile`. Conversion happens in `ripper` which imports both.
- **makemkvcon exit code**: it returns non-zero even on success; the scan only fails if both `disc.Name` and `titles` are empty after parsing.
- **Audio codec handling**: AAC, AC3, EAC3 are stream-copied; everything else is transcoded to AAC 160k. Stream selection is per-language and per-channel-count (stereo ≤2ch, surround >2ch).
