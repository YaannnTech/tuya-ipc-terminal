package tuya

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// OEMLoginHint is shown next to password login failures and next to a password
// session that finds no cameras.
const OEMLoginHint = "If the account was created in a Tuya OEM app (for example LittleElf), " +
	"password login may not work; try QR login (auth add without --password)"

// CameraCategories lists the Tuya device categories that discovery treats as
// cameras: sp = smart camera, dghsxj = another camera type, cdsxj = camera
// type from upstream PR #32, sp_wnq = smart bird feeder camera,
// ksdjml = video peephole camera (some were moved here from sp).
var CameraCategories = []string{"sp", "dghsxj", "cdsxj", "sp_wnq", "ksdjml"}

// IsCameraCategory reports whether category is one of CameraCategories.
func IsCameraCategory(category string) bool {
	return slices.Contains(CameraCategories, category)
}

// knownMessages maps error messages returned by the Tuya cloud (in Chinese)
// to an English explanation.
var knownMessages = map[string]string{
	"账号或密码错误！": "wrong account or password",
	"内部错误":     "internal error on the Tuya server, often a sign that the session is not usable for this account",
}

// ExplainMessage returns msg with an English explanation appended when msg is
// a known Tuya message. The original text is always kept.
func ExplainMessage(msg string) string {
	trimmed := strings.TrimSpace(msg)
	for known, explanation := range knownMessages {
		if strings.Contains(trimmed, strings.TrimRight(known, "！!")) {
			return fmt.Sprintf("%s (%s)", msg, explanation)
		}
	}
	return msg
}

// apiError turns the errorMsg of an unsuccessful API response into an error.
func apiError(msg string) error {
	return errors.New(ExplainMessage(msg))
}

// DebugEnabled reports whether the TUYA_DEBUG environment variable is set to
// a true value (1, true, yes, on).
func DebugEnabled() bool {
	switch strings.ToLower(os.Getenv("TUYA_DEBUG")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// RedactSecret describes a secret by its length and first 4 characters only.
func RedactSecret(secret string) string {
	switch {
	case secret == "":
		return "(empty)"
	case len(secret) <= 8:
		return fmt.Sprintf("(len %d)", len(secret))
	default:
		return fmt.Sprintf("%s... (len %d)", secret[:4], len(secret))
	}
}

// ListDevices returns the devices of all own and shared homes for which keep
// returns true (nil keeps all), without duplicates, and the number of own
// homes. Failing calls are skipped, so a broken session yields no devices.
func ListDevices(ctx context.Context, client *http.Client, serverHost string, keep func(Device) bool) (int, []Device) {
	var devices []Device
	add := func(device Device) {
		if keep != nil && !keep(device) {
			return
		}
		if !slices.ContainsFunc(devices, func(d Device) bool { return d.DeviceId == device.DeviceId }) {
			devices = append(devices, device)
		}
	}

	homeCount := 0
	homes, _ := GetHomeList(ctx, client, serverHost)
	if homes != nil {
		homeCount = len(homes.Result)
		for _, home := range homes.Result {
			roomList, err := GetRoomList(ctx, client, serverHost, strconv.Itoa(home.Gid))
			if err != nil {
				continue
			}
			for _, room := range roomList.Result {
				for _, device := range room.DeviceList {
					add(device)
				}
			}
		}
	}

	sharedHomes, _ := GetSharedHomeList(ctx, client, serverHost)
	if sharedHomes != nil {
		for _, sharedHome := range sharedHomes.Result.SecurityWebCShareInfoList {
			for _, device := range sharedHome.DeviceInfoList {
				add(device)
			}
		}
	}

	return homeCount, devices
}

// PrintLoginDebug writes the fields of a login result that help to compare a
// QR session with a password session. Secrets (sid, ecode) are redacted and
// the password, tokens and cookies are never part of the output.
func PrintLoginDebug(w io.Writer, result *LoginResult, serverHost string, homeCount, deviceCount, cameraCount int) {
	fmt.Fprintln(w, "TUYA_DEBUG login result (secrets redacted):")
	fmt.Fprintf(w, "  server host: %s\n", serverHost)
	if result == nil {
		fmt.Fprintln(w, "  login result: (none)")
		return
	}
	fmt.Fprintf(w, "  uid: %s\n", result.Uid)
	fmt.Fprintf(w, "  region code: %s\n", result.Domain.RegionCode)
	fmt.Fprintf(w, "  mobile api url: %s\n", result.Domain.MobileApiUrl)
	fmt.Fprintf(w, "  gw api url: %s\n", result.Domain.GwApiUrl)
	fmt.Fprintf(w, "  mobile mqtt url: %s\n", result.Domain.MobileMqttUrl)
	fmt.Fprintf(w, "  client id: %s\n", RedactSecret(result.ClientId))
	fmt.Fprintf(w, "  sid present: %t\n", result.Sid != "")
	fmt.Fprintf(w, "  sid: %s\n", RedactSecret(result.Sid))
	fmt.Fprintf(w, "  ecode: %s\n", RedactSecret(result.Ecode))
	fmt.Fprintf(w, "  user type: %d, reg from: %d, attribute: %d\n", result.UserType, result.RegFrom, result.Attribute)
	fmt.Fprintf(w, "  home count: %d\n", homeCount)
	fmt.Fprintf(w, "  device count: %d (cameras: %d)\n", deviceCount, cameraCount)
}

// CheckLoginSession counts the homes and devices visible to a fresh login
// session. With TUYA_DEBUG set it prints the login diagnostics to w; with
// warnNoCameras it prints a warning (with OEMLoginHint) when no camera is
// found. It never fails the login and makes no request when neither applies.
func CheckLoginSession(ctx context.Context, w io.Writer, client *http.Client, serverHost string, result *LoginResult, warnNoCameras bool) {
	debug := DebugEnabled()
	if !debug && !warnNoCameras {
		return
	}

	homeCount, devices := ListDevices(ctx, client, serverHost, nil)
	cameraCount := 0
	for _, device := range devices {
		if IsCameraCategory(device.Category) {
			cameraCount++
		}
	}

	if debug {
		PrintLoginDebug(w, result, serverHost, homeCount, len(devices), cameraCount)
	}
	if warnNoCameras && cameraCount == 0 {
		fmt.Fprintf(w, "\n! Warning: the login worked, but this session sees no cameras (%d home(s), %d device(s)).\n", homeCount, len(devices))
		fmt.Fprintf(w, "  %s.\n", OEMLoginHint)
		fmt.Fprintln(w, "  Set TUYA_DEBUG=1 and log in again to see details for a bug report.")
	}
}
