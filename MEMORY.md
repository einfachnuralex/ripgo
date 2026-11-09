# ripgo — Entwicklungs-Notizen

## Aktueller Stand (2026-04-14)

Branch `tui`. Letzte Arbeiten:
- Bubbletea TUI mit drei Panels (Rip / Encode / Overall)
- Cache-Verzeichnis-Management: Zeitstempel-Unterordner, `--keep-cache`, `--force-clean`, Cleanup nach Erfolg
- Konfigurierbare Encoding-Profile (`--profile`, `config.EncodingProfile` → `encoder.VideoProfile`)
- VAAPI-Support: optionale Felder, `hw_device`, `extra_args`, Quality-Flag-Auto-Erkennung
- Config-Feedback beim Start: `config.Load()` gibt Pfad zurück, druckt `→ Config: <pfad>` in Phase 1
- `--dry-run` (2026-10-14): Phase 1 echt, danach Befehle (Rip-/Probe-/Encode-Befehle) als kopierbares Shell-Skript statt Rip/Encode
- `--ask` (2026-10-18): nach dem Scan (Phase 1) Rückfragen nach Titel, Titles/Playlists (Komma-Liste von Title-IDs) und Audio-Tracks (Stream-Indizes); Enter = Default. Code in `internal/ripper/ask.go`
- MakeMKV entfernt (2026-11-09): nur noch ffmpeg/ffprobe als Abhängigkeit. Disc-Struktur liest ripgo selbst (`internal/disc`: UDF-Leser, MPLS-Parser, VIDEO_TS.IFO), Rip = ffmpeg-Remux (`-c copy`) in `title.mkv` im Cache. Gegen echte BD (Harry Potter 2, VC-1, 9 Playlists) verifiziert: Scan identisch zu `bd_list_titles -l`, Rip von 00122 mit Sprachen + Kapiteln ok. **DVD noch nicht gegen echte Disc getestet.**

---

## Architektur

### Pakete und Verantwortlichkeiten

```
cmd/ripgo/main.go
  - Flag-Parsing (parseArgs)
  - config.Load() → bei Fehler differenzierte Behandlung (ErrNoConfig = still, sonst Warnung)
  - checkPrerequisites() für ffmpeg / ffprobe (Dry Run: nur ffprobe)
  - Ripper erstellen und Run() aufrufen

internal/config/config.go
  - rawConfig: 1:1-Spiegelung der YAML-Struktur
  - Config: Runtime-Struct (alle Felder flach)
  - Load() → (*Config, string, error) — gibt Dateipfad zurück, ErrNoConfig als Sentinel
  - Defaults() → eingebautes h264_crf22_slow-Profil
  - EncodingProfile: Name, Codec, CRF, Preset, PixFmt, VideoFilter, HWDevice, ExtraArgs

internal/disc/ (disc.go, udf.go, mpls.go, bluray.go, dvd.go)
  - Scan(source, discType): Quelle = Laufwerk/ISO (UDF-Leser) oder Verzeichnis → DiscInfo{Source, Name, Type, Titles, Warnings}
  - BD: alle BDMV/PLAYLIST/*.mpls in Go (STN → Streams mit PID/Sprache, Marks → Kapitel), kein ffprobe
  - DVD: TT_SRPT aus VIDEO_TS.IFO + `ffprobe -f dvdvideo -title N` pro Titel
  - Rip()/RipArgs(): ffmpeg-Remux, mappt genau Title.Streams (Stream.Source) → Stream.Index = Index in title.mkv
  - FilterTitles(), DetectContentType(): unverändert aus dem alten makemkv-Paket

internal/ffmpeg/ffmpeg.go
  - Run(): ffmpeg mit -progress pipe:2, Progress{Frac, Speed, Size}, letzte 10 stderr-Zeilen im Fehler

internal/encoder/encoder.go
  - Analyze(): ffprobe JSON → FileInfo{Duration, Streams}
  - Encode(): ffprobe + ffmpeg, progressFn(0..1), debug-Flag
  - selectStreams(): bestes Video (max Pixel), Audio per Sprache/Kanäle, Subtitles optional
  - buildArgs(): alle Video-Felder optional (CRF=0 → weglassen, Preset/PixFmt="" → weglassen)
  - qualityFlagForCodec(): *_vaapi → "-global_quality", sonst → "-crf"
  - VideoProfile: Name, Codec, CRF, Preset, PixFmt, VideoFilter, HWDevice, ExtraArgs

internal/metadata/metadata.go
  - Lookup(): TMDB → OMDB → TVDB Fallback-Kette
  - EpisodeTitle(): TVDB für TV-Episode-Titel
  - Gesteuert über Config: TMDBKey, OMDBKey, TVDBKey, MetadataEnabled

internal/ripper/ripper.go
  - Ripper{cfg, opts}: orchestriert alles
  - Run():
      Phase 1 (plain stdout): ScanDisc, resolveContentType, metadata.Lookup, resolveProfile
      Phase 2 (bubbletea TUI): runParallel oder runSequential
  - runParallel(): Rip-Goroutine → buffered channel (cap 1) → Encode-Goroutine (überlappend)
  - runSequential(): erst alle rippen, dann alle encoden
  - encodeOne(): config.EncodingProfile → encoder.VideoProfile, Dateiname bauen, encoder.Encode()
  - prepareTempDir(): user-TempDir → Zeitstempel-Unterordner; sonst os.MkdirTemp (auto-cleanup)
  - resolveProfile(): Name-Lookup, leer = erstes Profil
  - Alle TUI-Typen hier: uiModel, liveBar, msgXxx, renderPanel, progressBar
```

### Datenfluss

```
CLI-Args
  → parseArgs() → cliOptions
  → config.Load() → Config (TempDir, DiscPath, Languages, EncodingProfiles, …)
  → Ripper.Run()
      → disc.Scan → DiscInfo
      → resolveContentType → isTV bool
      → metadata.Lookup → *metadata.Info
      → resolveProfile → config.EncodingProfile
      → tea.NewProgram startet TUI
      → goroutine: runParallel / runSequential
          → disc.Rip → tempDir/title_NN/title.mkv (+ chapters.txt bei BD)
          → encoder.Encode → .mkv in outputDir/
      → TUI beendet sich bei msgQuit
      → cleanup tempDir (wenn kein Fehler + kein --keep-cache)
```

### Phase-Trennung (wichtig)

`fmt.Scanln` (TV/Movie-Prompt) muss **vor** `tea.NewProgram` laufen, weil bubbletea das Terminal in den Raw-Mode versetzt. Deshalb zwei Phasen:
- **Phase 1**: plain stdout, interaktive Eingaben möglich
- **Phase 2**: TUI aktiv, alle Log-Ausgaben über `tea.Println()` / `uiProg.Send(msgLog{})`

### Import-Reihenfolge (kein Zyklus)

```
main → config, ripper
ripper → config, disc, encoder, ffmpeg, metadata, shellquote
encoder → ffmpeg
disc → ffmpeg, shellquote
ffmpeg → shellquote
config → (nur stdlib + yaml)
shellquote → (nur stdlib)
metadata → (nur stdlib)
```

`config.EncodingProfile` und `encoder.VideoProfile` sind bewusst getrennte Typen. Konvertierung in `ripper.encodeSettings()` (genutzt von `encodeOne` und Dry Run). Ebenso `disc.Stream` → `encoder.Stream` (Dry Run, `--ask`).

---

## Schlüssel-Entscheidungen

| Entscheidung | Begründung |
|---|---|
| Kein Alt-Screen | `tea.WithOutput(os.Stdout)` — Phase-1-Logs bleiben im Scrollback sichtbar |
| Bubbletea Inline-Mode | Log-Zeilen scrollen über die TUI-Panels, kein Bildschirm-Clear |
| Permille-Dedup | Progress-Events nur bei Änderung ≥ 0,1 % senden, sonst Event-Loop-Flut |
| Eigener UDF/MPLS-Parser statt Disc-Tool | ffmpeg `bluray:` liefert keine Sprachen, keine Kapitel, falsche Dauer; m2ts-Clips enthalten mehr PIDs als die Playlist nutzt. `bd_list_titles` verworfen (keine PIDs, zusätzliches Programm). Vom User so entschieden |
| Rip bleibt eigener Schritt (Remux) | Pipeline, Cache, ffprobe auf der MKV bleiben unverändert. Vom User so gewählt |
| Sprachcodes B ↔ T | DVD liefert `ger`, BD-MPLS `deu`; `encoder.altLanguage` gleicht ab |
| `config.ErrNoConfig` Sentinel | Unterscheidet "keine Datei" (leise Defaults) von "Datei fehlerhaft" (Warnung) |
| Zeitstempel-Unterordner | Verhindert, dass zwei gleichzeitige Programmstarts dieselben Roh-MKVs überschreiben |
| Audio-Copy-Strategie | AAC/AC3/EAC3 stream-copy, alles andere → AAC 160k transcode |
| `_vaapi`-Quality-Flag | Auto-Erkennung: `*_vaapi` → `-global_quality`, sonst `-crf` |
| Dry-Run-Streams aus dem Scan | Gerippte MKV existiert im Dry Run noch nicht → `-map` aus Scan-Daten; seit dem ffmpeg-Rip exakt (Rip mappt genau diese Streams), ffprobe-Befehl zusätzlich ausgegeben |
| `--ask` wählt per Title-ID | DVD: Titelnummer (1-basiert), BD: Playlist-Nummer. Tabelle zeigt auch Audio-Sprachen. Eingabereihenfolge = Verarbeitungs-/Episodenreihenfolge. Audio-Indizes gelten für alle gewählten Titles (Anzeige anhand des ersten). `resolveProfile` läuft dafür vor den Fragen |
| Ein Arg-Builder pro Befehl | `DVDProbeArgs`/`RipArgs`/`ProbeArgs`/`BuildArgs` werden vom echten Lauf und vom Dry Run genutzt → ausgegebene Befehle können nicht auseinanderlaufen |
| `-progress pipe:2` nicht in den Arg-Buildern | Nur ripgo braucht es; `ffmpeg.Run` stellt es voran, Dry Run stellt `-stats` voran |

---

## Bekannte Probleme / Offene Punkte

### DVD-Pfad noch nicht gegen echte Disc geprüft

`scanDVD` (TT_SRPT + ffprobe dvdvideo) und der DVD-Rip sind nur mit Unit-Tests geprüft. Beim ersten DVD: `RIPGO_TEST_DISC=/dev/sr0 go test -run ScanImage -v ./internal/disc/`, dann `--dry-run --ask` und Rip eines kurzen Titels. Prüfen: Dauer aus `format.duration`, Sprach-Tags (B-Codes erwartet), Kapitel ohne `-preindex` (genähert).

### Grenzen des ffmpeg-Rips

UHD (AACS 2.0) kann libaacs meist nicht entschlüsseln. BD-Multi-Angle: nur Winkel 1. Windows: kein Raw-Device-Zugriff im UDF-Leser (ISO/Verzeichnis gehen).

### VAAPI-Profile brauchen explizite Konfiguration

`*_vaapi`-Codec ohne `hw_device` und `video_filter: "format=nv12,hwupload"` schlägt mit `exit status 218` fehl. ffmpeg erhält CPU-Frames, der Hardware-Encoder erwartet VAAPI-Frames. Geräte-Pfad prüfen: `ls /dev/dri/`.

---

## Nützliche Befehle

```sh
# Bauen
go build ./...
go build -o ripgo ./cmd/ripgo

# Debug-Lauf (sieht ffprobe/ffmpeg-Kommandozeilen + komplettes stderr)
./ripgo --output /tmp/out --debug

# Profil prüfen
./ripgo --output /tmp/out --profile hevc_vaapi_qp23 --debug
```
