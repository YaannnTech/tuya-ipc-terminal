package rtsp

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/tuya"
)

func iceServerDiagnostic(server tuya.ICEServer) string {
	scheme, rest, ok := strings.Cut(server.Urls, ":")
	switch scheme {
	case "stun", "stuns", "turn", "turns":
	default:
		ok = false
	}
	safeURL := "<invalid>"
	if ok {
		u, err := url.Parse(scheme + "://" + strings.TrimPrefix(rest, "//"))
		if err == nil && u.Hostname() != "" && u.Path == "" {
			safeURL = scheme + ":" + u.Host
		}
	}
	return fmt.Sprintf("url=%q ttl_raw=%d", safeURL, server.Ttl)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (wb *WebRTCBridge) logEvent(format string, args ...any) {
	elapsed := "not-started"
	if started := wb.startedAt.Load(); started != nil {
		elapsed = fmt.Sprintf("+%.3fs", time.Since(*started).Seconds())
	}
	core.Logger.Info().Msgf("stream_diag camera=%q resolution=%q stream_id=%s t=%s %s",
		wb.camera.DeviceName, wb.resolution, wb.createdAt.UTC().Format(time.RFC3339Nano),
		elapsed, fmt.Sprintf(format, args...))
}

func (wb *WebRTCBridge) logShutdown(source string, err error) {
	wb.shutdownLogOnce.Do(func() {
		wb.logEvent("shutdown source=%s error=%q", source, errorText(err))
	})
}
