# Tuya IPC Terminal

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.26%2B-blue.svg)](https://golang.org/)
[![Build Status](https://img.shields.io/badge/Build-Passing-brightgreen.svg)]()

A powerful CLI tool to connect Tuya Smart Cameras to RTSP clients through reverse-engineered Tuya browser client APIs. Stream your Tuya cameras directly to any RTSP-compatible media player or home automation system.

> [!NOTE]
> **This is a maintained fork of [seydx/tuya-ipc-terminal](https://github.com/seydx/tuya-ipc-terminal).**
> The original project has had no commits since May 2025 and its open pull requests have not been reviewed, so I continue it here. The fork includes my own fixes plus useful pull requests from the original repository, with credit to their authors (see [What's different in this fork](#-whats-different-in-this-fork)). All credit for the original work goes to [@seydx](https://github.com/seydx). If the original project becomes active again, I'll gladly contribute these changes back upstream (see [seydx/tuya-ipc-terminal#38](https://github.com/seydx/tuya-ipc-terminal/issues/38)).

## 🍴 What's different in this fork

Changes per release are listed in [CHANGELOG.md](CHANGELOG.md).

### RTSP server fixes

- **No more stream lockups:** The server no longer holds locks while it waits for the Tuya cloud or tears down a stream, so new clients get their `OPTIONS`/`DESCRIBE` answers while a camera is still starting, and one slow client no longer stalls the others.
- **Correct codec per stream:** `DESCRIBE` advertises the codec of the stream that was actually requested (`/hd` or `/sd`), which matters for cameras that use H.265 for HD and H.264 for SD.
- **HEVC playback:** RTP packets are forwarded with the payload type the SDP advertises (96), so FFmpeg, VLC and go2rtc no longer drop HEVC video from the camera's data channel.
- **UDP clients on other hosts:** RTP over UDP is sent to the address of the RTSP client instead of `localhost`.
- **Case-insensitive camera paths:** `rtsp://host:8554/frontdoor` finds the camera `FrontDoor` when there is no exact match, for clients such as VLC that change the case of the path.
- **Dead clients are cleaned up:** A client that disconnected, stopped reading (2 s write timeout on TCP) or closed its UDP port is dropped, and its RTSP session is closed so the camera stream can shut down.
- **Race fixes:** Forwarder state is synchronized (`go test -race` passes), and stopping a stream no longer races with clients joining or leaving.

### WebRTC bridge fixes

- **ICE candidates that arrive early:** Tuya can deliver a camera's ICE candidate before its answer. The candidate used to abort the stream start; it is now held until the answer is applied.
- **Start timeout:** A camera that never completes the handshake now fails the start after 30 s instead of keeping the stream "starting" forever. Stopping the server (Ctrl+C or SIGTERM) no longer hangs on such a stream or on pending Tuya API requests.
- **No leaked connections:** A failed start or a camera disconnect now always closes the WebRTC and MQTT connections, so nothing keeps pulling video from Tuya in the background.
- **Clients reconnect after a failure:** When a stream fails, its RTSP sessions are closed, so players reconnect and get a fresh stream instead of waiting for packets forever.
- **Errors are not lost:** A failed connection could be reported as a success because of a race in the internal waiter.
- **Two-way audio without a data race:** The camera's audio track for talkback is now stored atomically, so an RTSP client sending audio while the track is set up no longer races.
- **Talkback is audible:** The camera speaker stays muted until it is told otherwise. The bridge now unmutes it with the first talkback packet it forwards (once, even with several talking clients; a failed command is retried at most every 5 seconds) and mutes it again when the last talking client leaves or the stream stops.

### Command line fixes

- **`rtsp stop`, `rtsp status` and `--daemon` work:** `start` writes a PID file per port, so `stop` and `status` (which run in their own process) can find the server, and `--daemon` really keeps the server running in the background.
- **Up-to-date dependencies:** pion/webrtc 4.2, pion/dtls 3.1, paho MQTT 1.5.1 and current `golang.org/x` modules, which fixes the known vulnerabilities `govulncheck` reported in code paths this tool uses. Building now needs Go 1.26 or newer.

### Incorporated pull requests from the original repository

| Original PR | Author | Change |
|-------------|--------|--------|
| [#28](https://github.com/seydx/tuya-ipc-terminal/pull/28) | [@oeretana](https://github.com/oeretana) | `/sd` streams on H.264 (non-HEVC) cameras no longer hang forever on startup (fixes [#27](https://github.com/seydx/tuya-ipc-terminal/issues/27)) |
| [#26](https://github.com/seydx/tuya-ipc-terminal/pull/26) | [@muminkoykiran](https://github.com/muminkoykiran) | Remove dead clients instead of logging every failed packet (reworked: error types instead of message text, UDP and TCP timeouts covered, RTSP session closed too) |
| [#30](https://github.com/seydx/tuya-ipc-terminal/pull/30) | [@melankh](https://github.com/melankh) | Lock fix for clearing clients when a stream stops |
| [#34](https://github.com/seydx/tuya-ipc-terminal/pull/34) | [@mpereiradev](https://github.com/mpereiradev) | `brazil` region and `--country-code` flag for password login |

Several fixes from [#5](https://github.com/seydx/tuya-ipc-terminal/pull/5), [#24](https://github.com/seydx/tuya-ipc-terminal/pull/24), [#26](https://github.com/seydx/tuya-ipc-terminal/pull/26) and [#30](https://github.com/seydx/tuya-ipc-terminal/pull/30) were already covered by the RTSP server fixes above, so they were not merged separately.

### Home Assistant

This fork powers the **Tuya IPC Bridge** app for Home Assistant in [YaannnTech/HA-Apps](https://github.com/YaannnTech/HA-Apps).

## ✨ Features

- 🔐 **Dual Authentication**: QR code scanning OR email/password login
- 🌍 **Multi-Region Support**: Support for all Tuya regions (EU, US, China, India)
- 👥 **Multi-User Management**: Handle multiple Tuya Smart accounts simultaneously
- 📡 **Session Persistence**: Automatic session management with validation
- 🎥 **Camera Discovery**: Automatic detection of all cameras from authenticated accounts
- 🎬 **RTSP Server**: WebRTC-to-RTSP bridge for universal compatibility
- 🚀 **Real-time Streaming**: Direct camera access with minimal latency
- 🔄 **Multi-Client Support**: Multiple RTSP clients per camera stream
- 🎯 **H265/HEVC Ready**: Advanced codec support for optimal performance
- 🎤 **Two-Way Audio**: Bidirectional audio for compatible cameras

## 🚀 Quick Start

### Installation

From source:

```bash
git clone https://github.com/YaannnTech/tuya-ipc-terminal.git
cd tuya-ipc-terminal
chmod +x build.sh
./build.sh
```

Each tagged release also provides prebuilt Linux binaries (amd64, arm64, armv7) with a `SHA256SUMS` file on the [Releases page](https://github.com/YaannnTech/tuya-ipc-terminal/releases), and a multi-arch container image:

```bash
docker run --rm -it -v ./tuya-data:/data/.tuya-data ghcr.io/yaannntech/tuya-ipc-terminal:latest --help
```

The image runs as a non-root user (uid 1000) in `/data` and keeps sessions in `/data/.tuya-data`, so mount a directory there that uid 1000 can write. For the RTSP server, publish its port (`-p 8554:8554`).

### 1-Minute Setup

```bash
# 1. Add your Tuya account (interactive)
./tuya-ipc-terminal auth add eu-central user@example.com

# 2. Discover your cameras
./tuya-ipc-terminal cameras refresh

# 3. Start streaming
./tuya-ipc-terminal rtsp start --port 8554

# 4. Watch your cameras
ffplay rtsp://localhost:8554/MyCamera
```

## 📖 Complete Documentation

### 🔐 Authentication Management

#### Two Authentication Methods

**🎯 QR Code**
```bash
./tuya-ipc-terminal auth add eu-central user@example.com
# Scan QR code with Tuya Smart/Smart Life app
```

**🔑 Email/Password**
```bash
./tuya-ipc-terminal auth add eu-central user@example.com --password
# Enter password when prompted
```

Accounts are tied to the phone country code they were registered with. If the
login fails, pass it explicitly with `--country-code` (see
`auth show-country-codes`):

```bash
./tuya-ipc-terminal auth add us-west user@example.com --password --country-code 55
```

Brazilian accounts can use the `brazil` shortcut instead, which targets the
`us-west` data center with country code `55`:

```bash
./tuya-ipc-terminal auth add brazil user@example.com --password
```

#### QR or password login

QR login is the recommended method. Password login is known to be
unreliable for some accounts while QR login works for the same account:

- The login can fail with `账号或密码错误！` (wrong account or password)
  although the password is correct.
- The login can succeed, but the session sees 0 homes or cameras and
  requests fail with `内部错误` (internal error). The password session then
  has a different user ID than the QR session of the same account.
- OEM accounts (such as LittleElf and many others) do not work with
  password login. In a test, a LittleElf account registered in Germany
  (region `eu-central`) failed with `账号或密码错误！` with the default
  country code and with `--country-code 49`, while QR login with the
  LittleElf app works. Use QR login with OEM accounts: `auth add`
  without `--password`, then scan the code with the OEM app you use.

Password login does work for other accounts: In a test, a Smart Life
account (region `us-west`) logged in with its password and found all of
its cameras.

Known Tuya error messages are printed with an English explanation next to
the original text. When a password login finds no cameras, a warning
points to QR login; the login itself is not rejected.

To report a login problem, run the login again with `TUYA_DEBUG=1`, once
with QR and once with `--password`, and paste the output into the issue:

```bash
TUYA_DEBUG=1 ./tuya-ipc-terminal auth add eu-central user@example.com --password
```

It prints a fingerprint of the user ID, the region and API host fields,
whether a session ID is present, and the number of homes and devices. The
password, tokens and cookies are never printed, and the session ID, client
ID and ecode only as their first 4 characters and their length. The
fingerprint is the first 8 hex characters of the SHA-256 of the user ID,
so the full user ID is never printed; it is enough to see whether a QR
session and a password session belong to the same user.

#### User Management Commands

```bash
# List all authenticated users
./tuya-ipc-terminal auth list

# Remove user authentication
./tuya-ipc-terminal auth remove eu-central user@example.com

# Refresh expired session
./tuya-ipc-terminal auth refresh eu-central user@example.com

# Test session validity
./tuya-ipc-terminal auth test eu-central user@example.com
```

#### 🌍 Regional Information

```bash
# Show all available regions
./tuya-ipc-terminal auth show-regions

# Show country codes for password authentication
./tuya-ipc-terminal auth show-country-codes
./tuya-ipc-terminal auth show-country-codes --search germany
```

**Available Regions:**
| Region | Endpoint | Description |
|--------|----------|-------------|
| `eu-central` | protect-eu.ismartlife.me | Central Europe |
| `eu-east` | protect-we.ismartlife.me | East Europe |
| `us-west` | protect-us.ismartlife.me | West America |
| `us-east` | protect-ue.ismartlife.me | East America |
| `china` | protect.ismartlife.me | China |
| `india` | protect-in.ismartlife.me | India |
| `brazil` | protect-us.ismartlife.me | Brazil (West America data center) |

### 📹 Camera Management

```bash
# List all discovered cameras
./tuya-ipc-terminal cameras list

# List cameras for specific user
./tuya-ipc-terminal cameras list --user eu-central_user_at_example_com

# Refresh camera discovery
./tuya-ipc-terminal cameras refresh

# Get detailed camera information
./tuya-ipc-terminal cameras info [camera-id-or-name]
```

### 📡 RTSP Server Management

```bash
# Start RTSP server
./tuya-ipc-terminal rtsp start --port 8554

# Limit RTSP access to this host (default: all interfaces)
./tuya-ipc-terminal rtsp start --port 8554 --address 127.0.0.1

# Start in the background (log: .tuya-data/rtsp-8554.log)
./tuya-ipc-terminal rtsp start --port 8554 --daemon

# Stop all servers started from this directory, or only one port
./tuya-ipc-terminal rtsp stop
./tuya-ipc-terminal rtsp stop --port 8554

# Show running servers and whether they answer
./tuya-ipc-terminal rtsp status

# List all available camera endpoints
./tuya-ipc-terminal rtsp list-endpoints
```

The RTSP server has no authentication. By default it listens on all interfaces;
use `--address 127.0.0.1` to restrict access to the local machine.

## 🎬 RTSP Streaming Guide

### 📺 Connecting to Camera Streams

Camera streams are available at:
```
rtsp://localhost:[port]/[camera-name]
rtsp://localhost:[port]/[camera-name]/hd      # Main stream
rtsp://localhost:[port]/[camera-name]/sd      # Sub-stream (lower quality)
rtsp://localhost:[port]/[camera-name]/hd-lan  # Experimental LAN HEVC
```

`/hd-lan` is an opt-in workaround for cameras that start an H.264 SD
stream locally and switch it to HEVC HD on request. Tuya cloud signaling is
still required. The selected ICE peer must have a private or link-local
address; the stream fails instead of switching on a public or relay peer.
The server waits for HEVC parameters and a keyframe, and repacketizes the
camera's H.264-style framing for HEVC RTSP clients. This mode was reported
working on a `ksdjml` peephole camera by [@sintech](https://github.com/sintech);
other cameras may not support it, and intermittent artifacts are still under
investigation. The default, `/hd`, and `/sd` modes are unchanged.

### ⏱️ Video Timestamp Correction

Some cameras send video RTP timestamps that advance about 1000 times too fast,
which breaks recordings (NVRs, HomeKit Secure Video) while live viewing works.
The server detects this automatically and corrects it. Detection holds back
video for about half a second at stream start, or up to two seconds if frames
are sparse; audio is not held back. Set `TUYA_VIDEO_TS_DIVISOR=1000` when
starting the server to apply the correction immediately, or set it to `1` to
disable detection and correction. Other positive integer divisors up to
4294967295 are also supported. The setting applies to all cameras in that
server process. If the detection buffer reaches its limit, timestamps are left
unchanged.


### 🏠 Home Automation Integration

**Home Assistant**
```yaml
camera:
  - platform: generic
    stream_source: rtsp://localhost:8554/LivingRoomCamera
    name: "Living Room Camera"
    
  - platform: generic
    stream_source: rtsp://localhost:8554/FrontDoor
    name: "Front Door Camera"
```

**Go2RTC Integration**
```yaml
streams:
  living_room:
    - rtsp://localhost:8554/LivingRoomCamera
  front_door:
    - rtsp://localhost:8554/FrontDoor
```


## 🏗️ Advanced Usage

### 🔧 Running as System Service

**Create systemd service:**
```bash
sudo tee /etc/systemd/system/tuya-rtsp.service > /dev/null <<EOF
[Unit]
Description=Tuya IPC Terminal RTSP Server
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=$PWD
ExecStart=$PWD/tuya-ipc-terminal rtsp start --port 8554
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable tuya-rtsp
sudo systemctl start tuya-rtsp
```

### 🔀 Multiple Server Instances

```bash
# Different ports for different purposes
./tuya-ipc-terminal rtsp start --port 8554 --daemon  # Main cameras
./tuya-ipc-terminal rtsp start --port 8555 --daemon  # Backup/secondary

# Stop only the second one
./tuya-ipc-terminal rtsp stop --port 8555
```

`stop` and `status` find the servers through PID files in `.tuya-data/`, so run them from the same directory as `start`.

### 👥 Multi-User Setup Example

```bash
# Add accounts from different regions
./tuya-ipc-terminal auth add eu-central home@example.com      # European account
./tuya-ipc-terminal auth add --password us-west work@company.com  # US business account
./tuya-ipc-terminal auth add china personal@example.cn        # Chinese account

# Discover all cameras
./tuya-ipc-terminal cameras refresh

# List everything
./tuya-ipc-terminal auth list
./tuya-ipc-terminal cameras list
```

## 🔄 Data Flow

1. **Authentication** → Tuya Cloud API
2. **Discovery** → MQTT + Tuya API
3. **WebRTC Connection** → Direct camera stream
4. **RTP Processing** → Video/audio packet handling
5. **RTSP Server** → Standard protocol output

## 💾 Data Storage

```
.tuya-data/
├── user_eu-central_user_at_example_com.json    # User sessions
├── user_us-west_business_at_company_com.json   # Multiple accounts
└── cameras.json                                # Camera registry
```

## 🛠️ Technical Details

### 🎥 Supported Features

| Feature | Status | Notes |
|---------|--------|-------|
| H.264 Video | ✅ | Universal support |
| H.265/HEVC | ✅ | Better compression |
| PCMU Audio | ✅ | Standard quality |
| PCMA Audio | ✅ | Alternative codec |
| HD Streams | ✅ | Main camera stream |
| SD Streams | ✅ | Sub-stream, lower bandwidth |
| Multi-client | ✅ | Multiple viewers per camera |
| Two-way Audio | ✅ | Camera dependent |

### 🎯 Supported Camera Types

| Category | Description | Compatibility |
|----------|-------------|---------------|
| `sp` | Smart cameras | ✅ Full support |
| `dghsxj` | Additional camera type | ✅ Full support |
| `cdsxj` | Additional camera type | ⚠️ Discovery supported; stream compatibility may vary |
| `sp_wnq` | Smart bird feeder cameras | ⚠️ Discovery supported; stream compatibility may vary |
| `ksdjml` | Video peephole cameras | ⚠️ Discovery supported; stream compatibility may vary |
| Others | Other Tuya camera categories | ❌ Not discovered |

## 🐛 Troubleshooting

### 🔐 Authentication Issues

**Problem**: Login fails
```bash
# 1. Check session status
./tuya-ipc-terminal auth test eu-central user@example.com

# 2. Try refreshing
./tuya-ipc-terminal auth refresh eu-central user@example.com

# 3. Re-authenticate if needed
./tuya-ipc-terminal auth remove eu-central user@example.com
./tuya-ipc-terminal auth add eu-central user@example.com
```

**Problem**: Country code not found (password login)
```bash
# Search for your country
./tuya-ipc-terminal auth show-country-codes --search germany
./tuya-ipc-terminal auth show-country-codes --search "united states"
```

### 📹 Camera Discovery Issues

**Problem**: No cameras found
```bash
# 1. Verify authentication
./tuya-ipc-terminal auth list

# 2. Force refresh
./tuya-ipc-terminal cameras refresh

# 3. Check if cameras are online in Tuya Smart app
./tuya-ipc-terminal cameras list --verbose
```

### 📡 RTSP Connection Issues

**Problem**: Can't connect to stream
```bash
# 1. Check server status
./tuya-ipc-terminal rtsp status

# 2. List available streams
./tuya-ipc-terminal rtsp list-endpoints

# 3. Test with simple player
ffplay rtsp://localhost:8554/CameraName
```

**Problem**: Poor stream quality
```bash
# Try sub-stream for lower bandwidth
ffplay rtsp://localhost:8554/CameraName/sd

# Check camera capabilities
./tuya-ipc-terminal cameras info "Camera Name"
```

### Development Setup

```bash
git clone https://github.com/YaannnTech/tuya-ipc-terminal.git
cd tuya-ipc-terminal
go mod download
go run main.go --help
```

## ⚠️ Limitations & Considerations

### Technical Limitations
- 🌐 Requires active internet connection to Tuya servers
- ☁️ Dependent on Tuya Cloud availability
- 📱 QR authentication requires mobile app
- 🔧 Some advanced camera features may not be supported

### Security Considerations
- 🔒 Sessions stored locally in JSON files
- 🛡️ No additional encryption beyond Tuya's implementation
- 🔥 Firewall configuration recommended for external access
- 🔐 Consider network security for RTSP streams

## 📄 License

MIT License - See [LICENSE](LICENSE) for details.

## ⚠️ Disclaimer

This tool is created through reverse engineering of Tuya's web client APIs. Use responsibly and ensure compliance with:

- Tuya's Terms of Service
- Local privacy laws and regulations
- Camera usage regulations in your jurisdiction

The authors are not responsible for any issues arising from the use of this software.

## 🆘 Support

Having issues? Here's how to get help:

1. 📖 Check this README for common solutions
2. 🐛 Search existing [GitHub Issues](https://github.com/YaannnTech/tuya-ipc-terminal/issues)
3. 💬 Create a new issue with:
   - Your operating system
   - Go version (`go version`)
   - Error messages
   - Steps to reproduce

---

**⭐ If this project helps you, please consider giving it a star on GitHub!**
