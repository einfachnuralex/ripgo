# ripgo — Entwicklungs-Notizen

## Aktueller Stand (2026-04-14)

Branch `tui`. Letzte Arbeiten:
- Bubbletea TUI mit drei Panels (Rip / Encode / Overall)
- Cache-Verzeichnis-Management: Zeitstempel-Unterordner, `--keep-cache`, `--force-clean`, Cleanup nach Erfolg
- Konfigurierbare Encoding-Profile (`--profile`, `config.EncodingProfile` → `encoder.VideoProfile`)
- VAAPI-Support: optionale Felder, `hw_device`, `extra_args`, Quality-Flag-Auto-Erkennung
- Config-Feedback beim Start: `config.Load()` gibt Pfad zurück, druckt `→ Config: <pfad>` in Phase 1

---

## Architektur

### Pakete und Verantwortlichkeiten

```
cmd/ripgo/main.go
  - Flag-Parsing (parseArgs)
  - config.Load() → bei Fehler differenzierte Behandlung (ErrNoConfig = still, sonst Warnung)
  - checkPrerequisites() für makemkvcon / ffmpeg / ffprobe
  - Ripper erstellen und Run() aufrufen

internal/config/config.go
  - rawConfig: 1:1-Spiegelung der YAML-Struktur
  - Config: Runtime-Struct (alle Felder flach)
  - Load() → (*Config, string, error) — gibt Dateipfad zurück, ErrNoConfig als Sentinel
  - Defaults() → eingebautes h264_crf22_slow-Profil
  - EncodingProfile: Name, Codec, CRF, Preset, PixFmt, VideoFilter, HWDevice, ExtraArgs

internal/makemkv/makemkv.go
  - ScanDisc(): makemkvcon -r --robot info → DiscInfo{Name, Type, Titles}
  - RipTitle(): makemkvcon mkv → ruft progressFn(frac, caption) auf
  - FilterTitles(): Dauer-Filterung (Movie: ≥minMovie, TV: minEp..maxEp)
  - DetectContentType(): Statistik auf Title-Dauern → Detection{IsTV, Confidence}
  - Achtung: makemkvcon gibt non-zero exit auch bei Erfolg zurück

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
      → ScanDisc → DiscInfo
      → resolveContentType → isTV bool
      → metadata.Lookup → *metadata.Info
      → resolveProfile → config.EncodingProfile
      → tea.NewProgram startet TUI
      → goroutine: runParallel / runSequential
          → makemkv.RipTitle → .mkv in tempDir/title_NN/
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
ripper → config, encoder, makemkv, metadata
encoder → (nur stdlib)
config → (nur stdlib + yaml)
makemkv → (nur stdlib)
metadata → (nur stdlib)
```

`config.EncodingProfile` und `encoder.VideoProfile` sind bewusst getrennte Typen. Konvertierung in `ripper.encodeOne()`.

---

## Schlüssel-Entscheidungen

| Entscheidung | Begründung |
|---|---|
| Kein Alt-Screen | `tea.WithOutput(os.Stdout)` — Phase-1-Logs bleiben im Scrollback sichtbar |
| Bubbletea Inline-Mode | Log-Zeilen scrollen über die TUI-Panels, kein Bildschirm-Clear |
| Permille-Dedup | Progress-Events nur bei Änderung ≥ 0,1 % senden, sonst Event-Loop-Flut |
| makemkvcon non-zero ignorieren | Gibt non-zero auch bei Erfolg zurück; Scan schlägt nur fehl wenn Name+Titles beide leer |
| `config.ErrNoConfig` Sentinel | Unterscheidet "keine Datei" (leise Defaults) von "Datei fehlerhaft" (Warnung) |
| Zeitstempel-Unterordner | Verhindert, dass zwei gleichzeitige Programmstarts dieselben Roh-MKVs überschreiben |
| Audio-Copy-Strategie | AAC/AC3/EAC3 stream-copy, alles andere → AAC 160k transcode |
| `_vaapi`-Quality-Flag | Auto-Erkennung: `*_vaapi` → `-global_quality`, sonst `-crf` |

---

## Bekannte Probleme / Offene Punkte

### ffmpeg-Fehlermeldungen werden verworfen

**Problem:** `encoder.Encode()` liest stderr für Progress-Zeilen (`out_time_us=`). Alle anderen Stderr-Zeilen (echte Fehlermeldungen) werden weggeworfen, wenn `--debug` nicht gesetzt ist. Der User sieht bei Encoding-Fehlern nur den Exit-Code (z.B. `exit status 218`).

**Ursache:** Progress (`-progress pipe:2`) und Fehlermeldungen (`-loglevel warning`) teilen sich dieselbe Stderr-Pipe. Der Scanner-Loop reagiert nur auf `out_time_us=`.

**Workaround:** `--debug` aktivieren.

**Saubere Lösung:** Nicht-Progress-Zeilen in einem Ring-Buffer sammeln, bei `cmd.Wait()`-Fehler die letzten N Zeilen an den zurückgegebenen Fehler anhängen.

### VAAPI-Profile brauchen explizite Konfiguration

`*_vaapi`-Codec ohne `hw_device` und `video_filter: "format=nv12,hwupload"` schlägt mit `exit status 218` fehl. ffmpeg erhält CPU-Frames, der Hardware-Encoder erwartet VAAPI-Frames. Geräte-Pfad prüfen: `ls /dev/dri/`.

---

## Nützliche Befehle

```sh
# Bauen
go build ./...
go build -o ripgo ./cmd/ripgo

# Debug-Lauf (sieht ffmpeg-Kommandozeile + alle makemkv-Ausgaben)
./ripgo --output /tmp/out --debug

# Profil prüfen
./ripgo --output /tmp/out --profile hevc_vaapi_qp23 --debug
```
