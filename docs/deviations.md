# Known deviations from the design spec

The design spec ([docs/superpowers/specs/2026-10-13-convertflow-design.md](superpowers/specs/2026-10-13-convertflow-design.md)) describes the intended product. The implementation deviates from it in the places below. They were accepted deliberately; none is a defect to be fixed silently later.

| Spec says | What ConvertFlow does | Why / consequence |
|---|---|---|
| A VP8 preset is offered next to VP9 | There is **no VP8 preset**. WebM output is VP9 or AV1 only. | VP9 is the better default for the same container; one fewer encoder to test. VP8 inputs are still decoded. |
| MKV and MP4 output containers | The `lgpl` flavor (default) produces **WebM, Ogg, FLAC, MP3, WAV only, no MKV and no MP4**. MP4/M4A output exists only in `full` (`video-h264`, `video-h265`, `audio-aac`). | The only MP4 presets need H.264/H.265/AAC encoders, which the `lgpl` flavor withholds for patent reasons ([licensing.md](licensing.md)). MKV output was never implemented as a preset. |
| Workers probe CPU instruction sets (for example AVX2) | **CPU-instruction probing is not implemented.** Software encoders are chosen by a short **trial encode** per encoder at worker start (then hourly and after a failed job). | A trial encode tests what actually works, including illegal-instruction crashes, without maintaining a CPU feature table. |
| A web UI built with a framework | The UI is **plain TypeScript** bundled with esbuild, no framework. | Fewer dependencies in the air-gapped supply chain; the UI is small. |
| Uploaded files are kept for the retention period | The **input file is deleted right after processing** (as soon as the job is done, failed with a final error, or canceled). Only the result stays for the retention period. | Minimizes stored data. A job requeued after a worker crash keeps its input until it finished. |
| Hardware encoders (NVENC, QSV, VAAPI) are used when detected | **Hardware encoders are inert in the default image**: FFmpeg is built with `--disable-autodetect`, so `av1_nvenc`/`av1_qsv`/`av1_vaapi` are never present. Using them needs a custom FFmpeg build and a compose override ([operations.md](operations.md)). | The default image must be reproducible and free of driver dependencies. |
| (not mentioned) | **libaom is bundled but no preset uses it.** `libaom-av1` is on the `lgpl` allow-list and built into FFmpeg, but the AV1 presets use SVT-AV1 (fallback VP9). | Kept as a ready fallback encoder; it adds image size without being reachable today. |
