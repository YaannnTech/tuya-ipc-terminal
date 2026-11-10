package rtsp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/rtsp"
	"tuya-ipc-terminal/pkg/storage"
)

var storageManager *storage.StorageManager
var rtspServer *rtsp.RTSPServer

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

func NewRTSPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rtsp",
		Short: "Manage RTSP server",
		Long:  "Commands to start, stop, and manage the RTSP server for camera streaming.",
	}

	cmd.AddCommand(newStartCmd())
	cmd.AddCommand(newStopCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newListEndpointsCmd())

	return cmd
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start RTSP server",
		Long:  "Start the RTSP server to provide camera streams.",
		RunE:  runStartServer,
	}

	cmd.Flags().IntP("port", "p", 8554, "RTSP server port")
	cmd.Flags().String("address", "", "RTSP server listen address (empty listens on all interfaces)")
	cmd.Flags().BoolP("daemon", "d", false, "Run as daemon (background)")

	return cmd
}

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop RTSP server",
		Long:  "Stop the RTSP servers started from this directory, or only the one on --port.",
		RunE:  runStopServer,
	}

	cmd.Flags().IntP("port", "p", 0, "Only stop the server on this port")

	return cmd
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show RTSP server status",
		Long:  "Show the RTSP servers started from this directory and whether they answer.",
		RunE:  runServerStatus,
	}
}

func newListEndpointsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-endpoints",
		Short: "List available RTSP endpoints",
		Long:  "Display all available RTSP camera endpoints.",
		RunE:  runListEndpoints,
	}

	cmd.Flags().BoolP("online-only", "o", false, "Show only online cameras")

	return cmd
}

func runStartServer(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetInt("port")
	address, _ := cmd.Flags().GetString("address")
	daemon, _ := cmd.Flags().GetBool("daemon")

	// Check if we have any authenticated users
	users, err := storageManager.ListUsers()
	if err != nil {
		return fmt.Errorf("failed to check users: %v", err)
	}

	if len(users) == 0 {
		core.Logger.Warn().Msg("No authenticated users found.")
		core.Logger.Warn().Msg("Please add users first:")
		core.Logger.Warn().Msg("  tuya-ipc-terminal auth add [region] [email]")
		return errors.New("no authenticated users")
	}

	// Check if we have any cameras
	cameras, err := storageManager.GetAllCameras()
	if err != nil {
		return fmt.Errorf("failed to check cameras: %v", err)
	}

	if len(cameras) == 0 {
		core.Logger.Warn().Msg("No cameras found.")
		core.Logger.Warn().Msg("Please refresh camera discovery:")
		core.Logger.Warn().Msg("  tuya-ipc-terminal cameras refresh")
		return errors.New("no cameras found")
	}

	dataDir := storageManager.GetDataDir()
	if rec, err := findServer(dataDir, port); err == nil && rec != nil {
		return fmt.Errorf("RTSP server on port %d is %w (PID %d)", port, errAlreadyRunning, rec.PID)
	}

	if daemon {
		rec, err := startDaemon(dataDir, port, address)
		if err != nil {
			return fmt.Errorf("failed to start RTSP server in the background: %v", err)
		}
		core.Logger.Info().Msgf("RTSP server running in the background on %s (PID %d)", net.JoinHostPort(rtsp.URLHostForAddress(rec.Address), strconv.Itoa(rec.Port)), rec.PID)
		core.Logger.Info().Msgf("Log: %s", rec.LogFile)
		core.Logger.Info().Msgf("Stop it with: tuya-ipc-terminal rtsp stop --port %d", rec.Port)
		return nil
	}

	// Create and start RTSP server
	rtspServer = rtsp.NewRTSPServer(address, port, storageManager)

	core.Logger.Info().Msgf("Starting RTSP server on %s...", net.JoinHostPort(rtsp.URLHostForAddress(address), strconv.Itoa(port)))

	if err := rtspServer.Start(); err != nil {
		return fmt.Errorf("failed to start RTSP server: %v", err)
	}

	pid := os.Getpid()
	if err := writeServerRecord(dataDir, serverRecord{PID: pid, Port: port, Address: address, Started: time.Now()}); err != nil {
		core.Logger.Warn().Err(err).
			Msg("Could not write the PID file; rtsp stop and rtsp status will not find this server")
	}
	defer removeServerRecord(dataDir, port, pid)

	// Wait for interrupt signal
	core.Logger.Info().Msgf("RTSP server is running. Press Ctrl+C to stop.")

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	<-signalChan

	core.Logger.Info().Msgf("Shutting down RTSP server...")
	if err := rtspServer.Stop(); err != nil {
		return fmt.Errorf("error stopping server: %v", err)
	}

	core.Logger.Info().Msgf("RTSP server stopped.")
	return nil
}

func runStopServer(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetInt("port")
	dataDir := storageManager.GetDataDir()

	servers, err := runningServers(dataDir)
	if err != nil {
		return fmt.Errorf("failed to look for running servers: %v", err)
	}

	stopped := 0
	var failed []string
	for _, rec := range servers {
		if port != 0 && rec.Port != port {
			continue
		}
		core.Logger.Info().Msgf("Stopping RTSP server on port %d (PID %d)...", rec.Port, rec.PID)
		if err := stopServer(dataDir, rec); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		stopped++
	}

	if len(failed) > 0 {
		return fmt.Errorf("could not stop every server: %s", strings.Join(failed, "; "))
	}
	if stopped == 0 {
		if port != 0 {
			core.Logger.Info().Msgf("No RTSP server running on port %d", port)
		} else {
			core.Logger.Info().Msg("No RTSP server running")
		}
		return nil
	}

	core.Logger.Info().Msgf("Stopped %d RTSP server(s).", stopped)
	return nil
}

func runServerStatus(cmd *cobra.Command, args []string) error {
	servers, err := runningServers(storageManager.GetDataDir())
	if err != nil {
		return fmt.Errorf("failed to look for running servers: %v", err)
	}

	if len(servers) == 0 {
		fmt.Println("RTSP Server Status: Not running")
		fmt.Println("Start it with: tuya-ipc-terminal rtsp start")
		return nil
	}

	fmt.Println("RTSP Server Status:")
	fmt.Println("==================")
	for _, rec := range servers {
		answering := "yes"
		if err := probeRTSP(rec.Address, rec.Port, 2*time.Second); err != nil {
			answering = fmt.Sprintf("no (%v)", err)
		}

		fmt.Printf("Port: %d\n", rec.Port)
		fmt.Printf("PID: %d\n", rec.PID)
		fmt.Printf("Running since: %s (%s)\n", rec.Started.Format("2006-01-02 15:04:05"),
			time.Since(rec.Started).Round(time.Second))
		fmt.Printf("Answering RTSP: %s\n", answering)
		if rec.LogFile != "" {
			fmt.Printf("Log: %s\n", rec.LogFile)
		}
		fmt.Printf("Access cameras via: rtsp://%s/[camera-path]\n\n",
			net.JoinHostPort(rtsp.URLHostForAddress(rec.Address), strconv.Itoa(rec.Port)))
	}

	return nil
}

func runListEndpoints(cmd *cobra.Command, args []string) error {
	cameras, err := storageManager.GetAllCameras()
	if err != nil {
		return fmt.Errorf("failed to get cameras: %v", err)
	}

	if len(cameras) == 0 {
		fmt.Println("No cameras found.")
		fmt.Println("Run 'tuya-ipc-terminal cameras refresh' to discover cameras.")
		return nil
	}

	if len(cameras) == 0 {
		fmt.Println("No online cameras found.")
		return nil
	}

	port := 8554
	address := ""
	if servers, err := runningServers(storageManager.GetDataDir()); err == nil && len(servers) == 1 {
		port = servers[0].Port
		address = servers[0].Address
	}
	endpoint := net.JoinHostPort(rtsp.URLHostForAddress(address), strconv.Itoa(port))

	fmt.Println("Available RTSP Endpoints:")
	fmt.Println("========================")

	for i, camera := range cameras {
		fmt.Printf("%d. %s\n", i+1, camera.DeviceName)
		fmt.Printf("   URL: rtsp://%s%s\n", endpoint, camera.RTSPPath)
		fmt.Printf("   Device ID: %s\n", camera.DeviceID)
		fmt.Printf("   User: %s\n", camera.UserKey)
		fmt.Println()
	}

	registry, err := storageManager.GetCameraRegistry()
	if err == nil && !registry.LastUpdated.IsZero() {
		fmt.Printf("Camera list last updated: %s\n", registry.LastUpdated.Format("2006-01-02 15:04:05"))

		if time.Since(registry.LastUpdated) > 24*time.Hour {
			fmt.Println("⚠️  Camera list is old. Consider running 'cameras refresh'")
		}
	}

	fmt.Printf("Example usage:\n")
	fmt.Printf("  ffplay rtsp://%s%s\n", endpoint, cameras[0].RTSPPath)
	fmt.Printf("  vlc rtsp://%s%s\n", endpoint, cameras[0].RTSPPath)

	return nil
}
