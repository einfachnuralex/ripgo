# ripgo

Go-based disc ripping and encoding pipeline. Reads the DVD/Blu-ray structure itself (UDF, IFO, MPLS) and uses **FFmpeg** for both rip (lossless remux) and encode, in a single CLI with a bubbletea TUI progress display.

## Project structure

```
cmd/ripgo/main.go              CLI entry point, flag parsing
internal/config/config.go      YAML config loading + defaults
internal/disc/disc.go          Disc types, Scan, Rip/RipArgs (ffmpeg remux), title filter, TV detection
internal/disc/udf.go           Read-only UDF reader (drive / ISO) for the navigation files
internal/disc/mpls.go          Blu-ray playlist parser (durations, STN streams with PID + language, chapters)
internal/disc/bluray.go        Blu-ray scan (MPLS, bdmt_*.xml title, KEYDB check)
internal/disc/dvd.go           DVD scan (VIDEO_TS.IFO title table + ffprobe dvdvideo per title)
internal/ffmpeg/ffmpeg.go      Runs ffmpeg with -progress, parses progress, keeps stderr tail for errors
internal/encoder/encoder.go    FFmpeg wrapper (probe + encode)
internal/metadata/metadata.go  TMDB / OMDB / TVDB lookups
internal/ripper/ripper.go      Orchestration, pipeline, TUI, dry run
internal/ripper/ask.go         --ask mode: interactive questions (title, titles/playlists, audio)
internal/shellquote/           POSIX shell quoting for printed commands
```

## Prerequisites

Only the FFmpeg tools must be in PATH:
- `ffmpeg` — rip (remux) and encoding
- `ffprobe` — DVD title scan and stream analysis

Checked at startup via `checkPrerequisites()` in `cmd/ripgo/main.go`. `disc.CheckSupport` checks after the type detection that ffmpeg was built with the `dvdvideo` demuxer (libdvdnav/libdvdread) or the `bluray` protocol (libbluray).

Decryption is done by libraries ffmpeg loads, not by ripgo: CSS via libdvdcss, AACS via libaacs + `~/.config/aacs/KEYDB.cfg` (missing KEYDB only produces a warning), BD+ via libbdplus. UHD (AACS 2.0) usually fails at the rip.

## Pipeline

Two phases to avoid a conflict between `fmt.Scanln` (interactive prompt) and bubbletea (raw terminal mode):

**Phase 1 — plain stdout** (before TUI starts):
1. Scan disc (`disc.Scan`): source = drive, ISO or directory (`disc:N` → `/dev/srN`, `file:X` → `X` for old configs). Type from `BDMV/index.bdmv` (`0300` → uhd) or `VIDEO_TS/VIDEO_TS.IFO`; `--disc-type` overrides.
   - **Blu-ray**: all `BDMV/PLAYLIST/*.mpls` parsed in Go (playlists < 120 s skipped). Title ID = playlist number. Streams = STN table of the first play item (primary video, primary audio, PG; main-path entries only) — the m2ts clips contain more PIDs (other language versions) than a playlist uses. No ffprobe, since the `bluray:` protocol exposes neither languages nor chapters and reports a wrong duration. Name from `BDMV/META/DL/bdmt_eng.xml`, else UDF label.
   - **DVD**: title count + chapters from the TT_SRPT in `VIDEO_TS.IFO`, then `ffprobe -f dvdvideo -title N` per title (duration, streams, languages). Title ID = DVD title number (1-based).
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

Each panel shows: task label, Unicode block bar (`█░`), percentage (1 decimal), elapsed time, ETA, and one caption line with ffmpeg speed and size (`4.2x · 1.3 GB`, from `-progress`).

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
  default_path: "/dev/sr0"   # drive, ISO image or directory with VIDEO_TS/BDMV
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
    # VAAPI: hw_device + video_filter are required (see "VAAPI notes" below)
    - name: hevc_vaapi_qp23
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

At startup, `main.go` calls `config.Load()` which returns `(*Config, string, error)` and a sentinel `config.ErrNoConfig` for the "not found" case:
- Config found and valid → prints `→ Config: <path>` before scanning
- No config file anywhere → silently uses `config.Defaults()` (no output)
- Config file found but broken → prints a warning on stderr, then uses `config.Defaults()`

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

**VAAPI notes:** `hw_device` and `video_filter: "format=nv12,hwupload"` are **required** for VAAPI encoders to work. Without `hwupload`, ffmpeg receives CPU-resident frames and cannot pass them to the VAAPI encoder — resulting in `exit status 218`. Available devices: `ls /dev/dri/`.

Without `--profile`, the first defined profile is used. Unknown profile names produce an error listing available names.

The type chain: `config.EncodingProfile` (config package) → converted to `encoder.VideoProfile` (encoder package) inside `ripper.encodeOne()`. This avoids an import cycle between `config` and `encoder`.

## CLI flags

```
--output, -o <path>        Output directory (required)
--mode <mode>              auto | movie | tv  (default: auto)
--disc <path>              Drive, ISO image or directory with VIDEO_TS/BDMV (default: /dev/sr0)
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
--audio-tracks <i,j,...>   Audio stream indices (ffprobe index, e.g. 1,3); overrides language/channel selection
--ask                      Ask for title, titles/playlists and audio tracks after the disc scan
--debug                    Verbose output: ffprobe/ffmpeg commands and their full stderr
--dry-run                  Scan + metadata only; print all commands instead of running them
-v, --version              Show version
-h, --help                 Show help
```

## Ask mode (`--ask`)

Part of Phase 1 (plain stdout, before the TUI). After the disc scan and TV/movie detection, `Run` asks three questions via a shared `prompter` (`internal/ripper/ask.go`). Enter accepts the default shown in brackets; invalid input is asked again; EOF on stdin accepts the defaults.

1. **Title** — default `--title`, else disc name. The answer is used for the metadata lookup.
2. **Titles (DVD) / Playlists (Blu-ray)** — comma-separated title IDs (DVD title number, Blu-ray playlist number). All titles are listed with duration, chapters, playlist (BD) and audio languages (`*` = passed the duration filter, which is the default). The input order is kept and defines the episode order in TV mode.
3. **Audio tracks** — comma-separated stream indices, listed from the first selected title (default: `--audio-tracks`, else what `encoder.SelectStreams` would pick). Applies to all selected titles; the answer replaces `Options.AudioTracks`. Only audio streams are accepted. Empty answer with no default keeps the automatic selection.

`--ask` works together with `--dry-run`. `resolveProfile` runs before the questions so a bad `--profile` fails early.

## Dry run (`--dry-run`)

Runs Phase 1 for real (disc scan, TV/movie detection incl. prompt, metadata + episode titles, profile), then prints the commands a real run would execute as a commented shell script on stdout (`Ripper.printDryRun`). Nothing is created, deleted, ripped or encoded; no TUI. A non-empty cache dir only produces a warning. Only `ffprobe` is required in PATH.

- Command args come from the same functions the real run uses: `disc.DVDProbeArgs` / `disc.RipArgs` / `disc.ChapterMetadata` (printed as heredoc), `encoder.ProbeArgs` / `encoder.BuildArgs`. All are quoted with `shellquote.Join`.
- `disc.RipArgs` and `encoder.BuildArgs` exclude the progress flags; `ffmpeg.Run` prepends `-progress pipe:2 -nostats`, the dry run prepends `-stats` instead.
- **Stream indices are exact**: the rip maps exactly `Title.Streams` in order (`Stream.Source` = `0:<idx>` for DVD, `0:i:<PID>` for Blu-ray), so `Stream.Index` is the index in the ripped `title.mkv`. Blu-ray languages are written with `-metadata:s:N language=…`, chapters from `chapters.txt` (FFMETADATA, 45 kHz timebase) via `-map_chapters 1`. The encoder still selects from ffprobe on the ripped file; the ffprobe command is printed too.

## Build

```sh
go build ./...
go build -o ripgo ./cmd/ripgo
```

No code generation. No external test fixtures needed for `go build`. Unit tests: `go test ./...` (shellquote, MPLS/IFO/UDF parsing, rip/ffmpeg args, progress parsing, ask mode). `RIPGO_TEST_DISC=/dev/sr0 go test -run ScanImage -v ./internal/disc/` scans a real drive, ISO or directory.

## Key design decisions

- **No alt-screen**: bubbletea runs with `tea.WithOutput(os.Stdout)` (inline mode) so log lines printed before the TUI remain visible in the terminal scrollback.
- **Two-phase split**: `fmt.Scanln` for the TV/movie prompt must run before `tea.NewProgram` takes the terminal into raw mode.
- **Import cycle prevention**: `config` defines `EncodingProfile`; `encoder` defines `VideoProfile`. Conversion happens in `ripper` which imports both.
- **Own disc parsing instead of a disc tool**: ffmpeg's `bluray:` protocol only delivers the transport stream (no languages, no chapters, wrong duration) and cannot read files from the disc. The UDF reader reads only the small, unencrypted navigation files with sector-aligned reads (works on raw `/dev/srN`); content is always read by ffmpeg.
- **Language codes**: DVDs report ISO 639-2/B (`ger`), Blu-ray playlists 639-2/T (`deu`); `encoder.SelectStreams` matches both forms (`altLanguage`).
- **Audio codec handling**: only AAC is stream-copied; everything else (incl. AC3/EAC3) is transcoded to AAC 160k. Stream selection is per-language and per-channel-count (stereo ≤2ch, surround >2ch). `--audio-tracks` selects streams by index instead (language/channel filters are skipped for audio); a requested index that is not an audio stream makes `Encode` fail, the dry run only warns.
- **ffmpeg error output**: `ffmpeg.Run` parses `-progress` from stderr and keeps the last 10 other lines; a failed rip/encode returns them in the error (`exit status 251: … | Error opening input files: …`). `--debug` additionally prints every stderr line.
