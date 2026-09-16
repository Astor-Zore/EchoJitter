# latency-jitter

End-to-end latency / jitter measurement tool for embedded audio systems.  
Measures the propagation delay from speaker playback to DMIC capture using FFT-accelerated normalized cross-correlation.

<p align="center">
  <img src="docs/screenshot.png" width="720" alt="Web UI Screenshot">
</p>

## Features

- **Single-binary, zero dependency** — Pure Go implementation (WAV parser, FFT, cross-correlation, HTTP server). No Python, no NumPy, no DLLs.
- **CLI mode** — Run analysis directly from the terminal, ideal for CI/automation.
- **Web UI** — Interactive browser-based interface with Chart.js visualizations, real-time parameter tuning, and per-playback result tables.
- **Test signal generator** — Built-in linear chirp generator with Hanning window tapers, configurable sample rate, bit depth, duration, gap, and frequency range.
- **CSV export** — One-click export of per-playback delays and statistical summary.
- **Robust to polarity inversion** — Cross-correlation takes absolute value, so DMIC signal polarity doesn't affect measurements.

## Algorithm

1. **Extract chirp template** from the reference (playback bus) signal via envelope energy detection.
2. **Sliding normalized cross-correlation** of the template over the entire reference to locate all chirp positions.
3. **Local fine-tuning** — for each chirp, search the DMIC recording in a window around the expected position and compute precise sub-sample delay.
4. **Statistics** — compute max, min, mean, standard deviation (jitter), and peak-to-peak (jitter) across all playback instances.

Cross-correlation uses FFT (`O(N log N)`), making it practical for multi-million sample recordings.

## Quick Start

### Download

Download the pre-built binary from [Releases](https://github.com/your-org/latency-jitter/releases) for your platform, or build from source:

```bash
git clone https://github.com/your-org/latency-jitter.git
cd latency-jitter
go build -o latency-jitter .
```

### Generate a test signal

```bash
# 16 kHz, 16-bit, 200ms chirp (500–8000 Hz), 2s silence gap, 10 repeats
latency-jitter --generate chirp_ref.wav

# Custom parameters
latency-jitter --generate ref.wav \
    --gen-sr 48000 --gen-bits 16 --gen-amp 0.9 \
    --gen-chirp-ms 150 --gen-gap-ms 1000 --gen-repeat 20 \
    --gen-f0 100 --gen-f1 10000
```

### Measure latency / jitter

```bash
# CLI mode — quick analysis
latency-jitter --ref chirp_ref.wav --rec dmic_capture.wav --no-web

# Web UI — interactive exploration
latency-jitter --ref chirp_ref.wav --rec dmic_capture.wav
# Open http://localhost:8080 in your browser
```

### Example output

```
=== Analysis (threshold=0.15, minGap=0.5s, margin=2000) ===
Template length: 1600 samples (100.0 ms)
Chirps detected in ref: 3
Valid delays found:     3

No.   ref_pos      rec_pos      delay(samp)    delay(ms)  corr
1     65720        66435        715            44.688     0.817
2     110766       111474       708            44.250     0.847
3     155535       156250       715            44.688     0.817

=== Statistics ===
Count (N)    : 3
Max delay    : 715 samples (44.688 ms)
Min delay    : 708 samples (44.250 ms)
Mean delay   : 712.7 samples (44.542 ms)
Std (jitter) : 4.04 samples (0.253 ms)
P2P (jitter) : 7 samples (0.438 ms)
```

## CLI Reference

```
Usage:
  latency-jitter                                    Start Web UI
  latency-jitter --generate output.wav              Generate chirp and exit
  latency-jitter --ref ref.wav --rec rec.wav --no-web
                                                    CLI mode analysis

Analysis options:
  --ref <path>        Reference WAV (playback bus signal)
  --rec <path>        Recording WAV (DMIC capture)
  --port N            Web server port (default: 8080)
  --no-web            CLI-only, no web server
  --threshold 0.15    Cross-correlation peak threshold [0–1]
  --chirp-ms 100      Chirp template duration in ms (0 = auto-detect)
  --margin 2000       Local search window in samples
  --min-gap 0.5       Minimum gap between chirps in seconds

Generation options (use with --generate):
  --gen-sr 16000      Sample rate (Hz)
  --gen-bits 16       Bit depth (16 or 32)
  --gen-amp 0.8       Amplitude (0.0 – 1.0)
  --gen-chirp-ms 200  Chirp duration (ms)
  --gen-gap-ms 2000   Silence gap between chirps (ms)
  --gen-repeat 10     Number of chirp repetitions
  --gen-f0 500        Chirp start frequency (Hz)
  --gen-f1 8000       Chirp end frequency (Hz)
```

## Web UI

Launch the Web UI to interactively explore your measurement data:

```bash
latency-jitter --ref ref.wav --rec rec.wav
```

<p align="center">
  <img src="docs/ui-layout.png" width="720" alt="UI Layout">
</p>

**Controls panel:**
- **Threshold** — Peak detection sensitivity; lower = more detections (may include false peaks).
- **Min Gap** — Minimum time between chirps; prevents double-counting the same chirp.
- **Margin** — Local search window; must exceed the expected maximum delay.
- **Chirp ms** — Template length for cross-correlation; typically 1/4 to 1/2 of the actual chirp duration.
- **BP** — Band-pass filter toggle (500–9000 Hz) to remove out-of-band noise.

**Charts:**
- **Cross-Correlation** — Normalized sliding cross-correlation over the reference signal; green markers show detected chirp positions.
- **Latency per Playback** — Scatter plot of delay values per playback instance.

**Export:**
- **Export CSV** — Downloads a CSV with per-playback details (ref_pos, rec_pos, delay_samples, delay_ms, correlation) plus a statistical summary section.

## CSV Format

```csv
playback,ref_pos,rec_pos,delay_samples,delay_ms,correlation
1,65720,66435,715,44.688,0.817
2,110766,111474,708,44.250,0.847
3,155535,156250,715,44.688,0.817

statistic,,,value_samples,value_ms,
count,,,,3,,
max,,,,715,44.688,
min,,,,708,44.250,
mean,,,,712.67,44.542,
std_jitter,,,,4.04,0.253,
p2p_jitter,,,,7,0.438,
```

## Parameter Tuning Guide

| Symptom | Action |
|---------|--------|
| Too few chirps detected | Lower `--threshold` (try 0.05 → 0.10 → 0.15) |
| False peaks (noise) | Raise `--threshold`, or enable BP filter |
| Erratic delay values | Increase `--margin` (must exceed max physical delay + jitter) |
| Low correlation values | Adjust `--chirp-ms` to match actual chirp length |
| Negative delays | Increase `--margin`; may indicate template/recording misalignment |

**Golden rule:** set `--margin` to 2–3× your expected maximum delay so the local search doesn't truncate the true correlation peak.

## Why Chirp Instead of Tone?

| | Single Tone (1 kHz) | Chirp (Sweep) |
|---|---|---|
| Cross-correlation peak | Multiple equal-height peaks (period ambiguity) | Single sharp peak |
| Jitter measurement | Up to 16 samples false jitter from period alias | True system jitter |
| Noise immunity | Poor (narrowband energy) | Good (broadband energy) |

## Building

```bash
# Native build
go build -o latency-jitter .

# Cross-compile for Windows
GOOS=windows GOARCH=amd64 go build -o latency-jitter.exe .

# Cross-compile for ARM Linux
GOOS=linux GOARCH=arm64 go build -o latency-jitter-arm64 .
```

Requires Go 1.21+.

## License

MIT