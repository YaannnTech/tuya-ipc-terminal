# Changelog

All notable changes to this fork are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). Versions before 0.1.0
are releases of the original project,
[seydx/tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal).

## [Unreleased]

### Added

- Per-stream ICE, WebRTC and MQTT lifecycle diagnostics for investigating
  TURN refresh failures (#11).

## [0.2.0] - 2026-11-11

### Added

- Password login diagnostics (diagnostics part of #13): Known Chinese Tuya
  error messages get an English explanation next to the original text,
  failed password logins and password sessions without cameras point to
  QR login (password login does not work for OEM accounts such as
  LittleElf and many others; it worked in a test with a Smart Life
  account), and `TUYA_DEBUG=1` prints the login result fields (redacted)
  for comparing QR and password sessions (#29).

### Changed

- Runtime errors (for example a failed login) no longer print the command
  usage text after the error message (#29).

### Fixed

- A rejected login token request no longer ends in a nil pointer panic
  during password login; the Tuya error is returned instead (#29).

## [0.1.0] - 2026-11-11

First release of the fork, based on the original project's 0.0.6.

### Added

- Prebuilt Linux binaries (amd64, arm64, armv7) with `SHA256SUMS` on each
  release, and a multi-arch container image on
  `ghcr.io/yaannntech/tuya-ipc-terminal` (#8).
- Experimental opt-in `/hd-lan` path for cameras that start an H.264 SD
  stream locally and switch it to HEVC HD on request (LAN peers only),
  reported working on a `ksdjml` peephole camera by @sintech (#21,
  PR #23).
- `rtsp start --address` to choose the RTSP listen address; the default
  stays all interfaces (#6, PR #19).
- Discovery of `cdsxj`, `sp_wnq` (PR #18) and `ksdjml` (video peephole,
  PR #22) cameras.
- `brazil` region and `--country-code` flag for password login (original
  PR #34 by @mpereiradev).
- CI: gofmt, `go mod tidy`, vet, race-enabled tests and multi-arch builds
  on every push and pull request (PR #17).

### Fixed

- RTSP server: No more stream lockups while a camera starts or stops, the
  correct codec per stream (`/hd` or `/sd`), HEVC playback in FFmpeg, VLC
  and go2rtc, RTP over UDP to clients on other hosts, case-insensitive
  camera paths, dead clients are dropped, and data races fixed.
- `/sd` streams on H.264 cameras no longer hang on startup (original PR #28
  by @oeretana).
- WebRTC bridge: Early ICE candidates are held until the answer arrives, a
  30 s start timeout, no leaked WebRTC/MQTT connections after a failure,
  RTSP clients reconnect after a stream fails, and errors are no longer
  reported as success.
- Stopping the server no longer hangs on pending Tuya API requests (#9,
  PR #16).
- Talkback: The camera speaker is unmuted with the first talkback packet
  and muted again when the last talking client leaves, and the talkback
  track no longer has a data race (#1, #4, PR #24).
- Video RTP timestamps that some cameras send 1000x too fast are
  corrected, with `TUYA_VIDEO_TS_DIVISOR` as an override (#12, PR #25).
- `rtsp stop`, `rtsp status` and `--daemon` work.
- `--version` reports the release version; `build.sh` passed a version
  flag the binary did not use (#8).

### Changed

- Updated dependencies (pion/webrtc 4.2, pion/dtls 3.1, paho MQTT 1.5.1,
  current `golang.org/x` modules), which fixes the vulnerabilities
  `govulncheck` reported. Building needs Go 1.26 or newer.

[Unreleased]: https://github.com/YaannnTech/tuya-ipc-terminal/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/YaannnTech/tuya-ipc-terminal/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/YaannnTech/tuya-ipc-terminal/releases/tag/v0.1.0
