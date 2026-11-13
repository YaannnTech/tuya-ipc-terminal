package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	pion "github.com/pion/webrtc/v4"
	"golang.org/x/net/publicsuffix"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
	"tuya-ipc-terminal/pkg/utils"
	"tuya-ipc-terminal/pkg/webrtc"

	"github.com/pion/rtp"
)

type WebRTCBridge struct {
	camera         *storage.CameraInfo
	resolution     string
	user           *storage.UserSession
	storageManager *storage.StorageManager
	rtpForwarder   *RTPForwarder

	connected       bool
	stopped         bool
	closing         atomic.Bool
	closeOnce       sync.Once
	shutdownLogOnce sync.Once
	mutex           sync.RWMutex
	renewalMutex    sync.Mutex
	createdAt       time.Time
	startedAt       atomic.Pointer[time.Time]
	ctx             context.Context
	cancel          context.CancelFunc
	session         atomic.Pointer[bridgeSession]
	setupSession    func(*bridgeSession) error
	renewalDelay    func(context.Context, time.Duration) error

	OnVideoPacket func(packet *rtp.Packet)
	OnAudioPacket func(packet *rtp.Packet)
	OnError       func(error)
}

type bridgeSession struct {
	*WebRTCBridge
	streamType    int
	isHEVC        bool
	lanHDReady    utils.Waiter
	lanHDRequest  sync.Once
	lanHDSwitched atomic.Bool

	peerConnection                *pion.PeerConnection
	dataChannel                   *pion.DataChannel
	dataChannelParseErrorLogged   sync.Once
	dataChannelSSRCMismatchLogged sync.Once
	mqttClient                    *tuya.MQTTClient
	cameraClient                  *tuya.MQTTCameraClient
	waiter                        utils.Waiter
	// The camera's ICE candidates can arrive over MQTT before its answer.
	remoteCandidates            *remoteCandidateQueue
	ctx                         context.Context
	cancel                      context.CancelFunc
	closeOnce                   sync.Once
	closing                     atomic.Bool
	stateMutex                  sync.Mutex
	ready                       bool
	failure                     error
	mediaMutex                  sync.RWMutex
	videoSSRC                   atomic.Uint32
	audioSSRC                   atomic.Uint32
	renewing                    bool
	videoTrack                  *pion.TrackRemote
	audioTrack                  *pion.TrackRemote
	backchannel                 atomic.Pointer[pion.TrackLocalStaticRTP]
	backchannelWriteErrorLogged sync.Once
	speaker                     speakerState
}

func NewWebRTCBridge(camera *storage.CameraInfo, streamResolution string, user *storage.UserSession, storageManager *storage.StorageManager) *WebRTCBridge {
	ctx, cancel := context.WithCancel(context.Background())

	wb := &WebRTCBridge{
		camera:         camera,
		resolution:     streamResolution,
		user:           user,
		rtpForwarder:   NewRTPForwarder(),
		storageManager: storageManager,
		connected:      false,
		ctx:            ctx,
		cancel:         cancel,
		createdAt:      time.Now(),
	}

	wb.rtpForwarder.OnBackchannelAudio = wb.ForwardBackchannelAudioPacket
	wb.rtpForwarder.OnClientRemoved = func(id string) {
		if session := wb.session.Load(); session != nil {
			session.speaker.clientLeft(id)
		}
	}
	wb.session.Store(wb.newSession())
	wb.setupSession = (*bridgeSession).start
	wb.renewalDelay = waitRenewalDelay

	return wb
}

func (wb *WebRTCBridge) newSession() *bridgeSession {
	ctx, cancel := context.WithCancel(wb.ctx)
	s := &bridgeSession{WebRTCBridge: wb, ctx: ctx, cancel: cancel}
	s.remoteCandidates = newRemoteCandidateQueue(s.addRemoteCandidate)
	return s
}

func (wb *bridgeSession) active() bool {
	return !wb.closing.Load() && wb.ctx.Err() == nil && wb.WebRTCBridge.session.Load() == wb
}

// startTimeout bounds each wait in Start (MQTT connect, camera handshake), so a
// camera that never completes the handshake cannot keep a stream "starting".
var startTimeout = 30 * time.Second

// waitFor waits for w, but gives up after startTimeout or when Stop is called.
func (wb *bridgeSession) waitFor(w *utils.Waiter, what string) error {
	ch := w.WaitChan()
	if ch == nil {
		return w.Wait() // already finished
	}

	timer := time.NewTimer(startTimeout)
	defer timer.Stop()

	var err error
	select {
	case err = <-ch:
		return err
	case <-wb.ctx.Done():
		err = errors.New("bridge stopped")
	case <-timer.C:
		err = fmt.Errorf("timed out after %s waiting for %s", startTimeout, what)
	}

	// Release the goroutine behind WaitChan.
	w.Done(err)
	<-ch
	return err
}

// Start connects the bridge and waits for the camera stream to become ready.
func (wb *WebRTCBridge) Start() (err error) {
	wb.mutex.Lock()
	defer wb.mutex.Unlock()

	if wb.connected {
		return errors.New("bridge already connected")
	}
	if wb.stopped {
		return errors.New("bridge stopped")
	}
	started := time.Now()
	wb.startedAt.Store(&started)

	core.Logger.Info().Msgf("Starting WebRTC bridge for camera: %s", wb.camera.DeviceName)
	s := wb.session.Load()
	if err = wb.setupSession(s); err == nil {
		err = s.markReady()
	}
	if err != nil {
		wb.logShutdown("start", err)
		wb.closeResources()
		return err
	}
	if wb.ctx.Err() != nil {
		wb.closeResources()
		return errors.New("bridge stopped")
	}
	wb.connected = true
	core.Logger.Info().Msgf("WebRTC bridge started successfully for camera: %s", wb.camera.DeviceName)
	return nil
}

func (wb *bridgeSession) start() (err error) {
	if err := wb.ctx.Err(); err != nil {
		return err
	}
	// Create HTTP client with session
	httpClient := wb.createHTTPClient()
	if httpClient == nil {
		return errors.New("failed to create HTTP client")
	}

	// Get app info
	appInfo, err := tuya.GetAppInfo(wb.ctx, httpClient, wb.user.SessionData.ServerHost)
	if err != nil {
		return fmt.Errorf("failed to get app info: %v", err)
	}

	// Get MQTT config
	mqttConfig, err := tuya.GetMQTTConfig(wb.ctx, httpClient, wb.user.SessionData.ServerHost)
	if err != nil {
		return fmt.Errorf("failed to get MQTT config: %v", err)
	}
	if err := wb.ctx.Err(); err != nil {
		return err
	}

	// Connect to MQTT broker
	wb.mqttClient, err = tuya.NewMqttClient(
		appInfo.Result.ClientId,
		wb.user.SessionData.LoginResult.Domain.MobileMqttsUrl,
		&mqttConfig.Result,
		func(state string, err error) {
			if wb.active() {
				wb.logEvent("mqtt state=%s error=%q", state, errorText(err))
			}
		},
	)

	if err != nil {
		return fmt.Errorf("failed to connect to MQTT: %v", err)
	}
	if err := wb.ctx.Err(); err != nil {
		return err
	}

	if err = wb.waitFor(&wb.mqttClient.Connected, "the MQTT connection"); err != nil {
		return fmt.Errorf("MQTT connection failed: %v", err)
	}

	// Get WebRTC configuration
	webRTCConfig, err := tuya.GetWebRTCConfig(wb.ctx, httpClient, wb.user.SessionData.ServerHost, wb.camera.DeviceID)
	if err != nil {
		return fmt.Errorf("failed to get WebRTC config: %v", err)
	}
	fetched := time.Now().UTC().Format(time.RFC3339Nano)
	for _, server := range webRTCConfig.Result.P2PConfig.Ices {
		wb.logEvent("ice_server %s fetched_at=%s", iceServerDiagnostic(server), fetched)
	}
	if len(webRTCConfig.Result.P2PConfig.Ices) == 0 {
		wb.logEvent("ice_server none fetched_at=%s", fetched)
	}

	// Parse skill information
	var skill tuya.Skill
	if err := json.Unmarshal([]byte(webRTCConfig.Result.Skill), &skill); err != nil {
		return fmt.Errorf("failed to parse skill info: %v", err)
	}

	// Determine stream settings
	wb.streamType = tuya.GetStreamType(&skill, wb.upstreamResolution())
	wb.isHEVC = tuya.IsHEVC(&skill, wb.streamType)
	if wb.renewing && !wb.isHEVC {
		return errors.New("renewed HD session is not HEVC")
	}
	if wb.resolution == lanHDResolution && wb.isHEVC {
		return errors.New("/hd-lan requires an H.264 SD stream")
	}

	core.Logger.Info().Msgf("Stream settings - Resolution: %s, Type: %d, HEVC: %v", wb.resolution, wb.streamType, wb.isHEVC)

	// Setup WebRTC peer connection
	if err := wb.ctx.Err(); err != nil {
		return err
	}
	if err := wb.setupPeerConnection(&webRTCConfig.Result); err != nil {
		return fmt.Errorf("failed to setup peer connection: %v", err)
	}

	// Setup MQTT camera client
	if err := wb.ctx.Err(); err != nil {
		return err
	}
	wb.setupMQTTCameraClient(&webRTCConfig.Result)

	// Create and send offer
	if err := wb.ctx.Err(); err != nil {
		return err
	}
	if err := wb.createAndSendOffer(); err != nil {
		return fmt.Errorf("failed to create offer: %v", err)
	}

	if err = wb.waitFor(&wb.waiter, "the camera stream"); err != nil {
		return fmt.Errorf("failed to establish connection: %v", err)
	}
	if wb.resolution == lanHDResolution {
		if err = wb.waitFor(&wb.lanHDReady, "HEVC parameters and a keyframe"); err != nil {
			return fmt.Errorf("failed to start /hd-lan: %w", err)
		}
	}

	return nil
}

func (wb *WebRTCBridge) Stop() {
	wb.logShutdown("Stop", nil)
	// Cancel before taking the lock: a Start that is still waiting holds it.
	wb.cancel()

	wb.renewalMutex.Lock()
	defer wb.renewalMutex.Unlock()
	wb.mutex.Lock()
	defer wb.mutex.Unlock()

	if wb.stopped {
		return
	}
	wb.stopped = true
	wb.connected = false

	core.Logger.Info().Msgf("Stopping WebRTC bridge for camera: %s", wb.camera.DeviceName)
	wb.closeResources()
	if wb.rtpForwarder != nil {
		wb.rtpForwarder.Stop()
	}
	core.Logger.Info().Msgf("WebRTC bridge stopped for camera: %s", wb.camera.DeviceName)
}

// closeResources releases the current session, including after a failed Start.
func (wb *WebRTCBridge) closeResources() {
	wb.closeOnce.Do(func() {
		wb.closing.Store(true)
		wb.cancel()
		if s := wb.session.Load(); s != nil {
			s.close()
		}
	})
}

func (wb *bridgeSession) close() {
	wb.closeOnce.Do(func() {
		wb.closing.Store(true)
		wb.cancel()
		wb.mediaMutex.Lock()
		wb.mediaMutex.Unlock()
		// Before SendDisconnect and the MQTT shutdown, which end the session.
		wb.speaker.stop()
		if wb.cameraClient != nil {
			if err := wb.cameraClient.SendDisconnect(); err != nil {
				core.Logger.Warn().Err(err).Msg("Failed to send camera disconnect")
			}
		}
		if wb.peerConnection != nil {
			wb.peerConnection.Close()
		}
		if wb.mqttClient != nil {
			wb.mqttClient.Stop()
		}
	})
}

func (wb *WebRTCBridge) IsConnected() bool {
	wb.mutex.RLock()
	defer wb.mutex.RUnlock()
	return wb.connected
}

func (wb *WebRTCBridge) ForwardBackchannelAudioPacket(sessionID string, packet *rtp.Packet) {
	session := wb.session.Load()
	if session != nil && !session.closing.Load() {
		session.forwardBackchannelAudioPacket(sessionID, packet)
	}
}

func (wb *bridgeSession) forwardBackchannelAudioPacket(sessionID string, packet *rtp.Packet) {
	if track := wb.backchannel.Load(); track != nil {
		if err := track.WriteRTP(packet); err == nil {
			wb.speaker.packetForwarded(sessionID)
		} else {
			wb.backchannelWriteErrorLogged.Do(func() {
				core.Logger.Warn().Err(err).Msg("Failed to forward backchannel audio")
			})
		}
	}
}

func (wb *bridgeSession) setBackchannel(track *pion.TrackLocalStaticRTP) {
	wb.backchannel.Store(track)
}

func (wb *bridgeSession) setupPeerConnection(webRTCConfig *tuya.WebRTCConfig) error {
	// Convert ICE servers
	iceServerBytes, err := json.Marshal(webRTCConfig.P2PConfig.Ices)
	if err != nil {
		return fmt.Errorf("failed to marshal ICE servers: %v", err)
	}

	iceServers, err := webrtc.UnmarshalICEServers(iceServerBytes)
	if err != nil {
		return fmt.Errorf("failed to unmarshal ICE servers: %v", err)
	}
	if wb.resolution == lanHDResolution {
		iceServers = nil
	}

	// Create peer connection configuration
	conf := pion.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: pion.ICETransportPolicyAll,
		BundlePolicy:       pion.BundlePolicyMaxBundle,
	}

	// Create WebRTC API
	api, err := webrtc.NewAPI()
	if err != nil {
		return fmt.Errorf("failed to create WebRTC API: %v", err)
	}

	// Create peer connection
	wb.peerConnection, err = api.NewPeerConnection(conf)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %v", err)
	}
	wb.lanHDICETransport().OnSelectedCandidatePairChange(func(pair *pion.ICECandidatePair) {
		if !wb.active() {
			return
		}
		if pair != nil && pair.Local != nil && pair.Remote != nil {
			wb.logEvent("ice_pair local_type=%s local_protocol=%s remote_type=%s remote_protocol=%s remote_address=%s",
				pair.Local.Typ, pair.Local.Protocol, pair.Remote.Typ, pair.Remote.Protocol,
				net.JoinHostPort(pair.Remote.Address, fmt.Sprint(pair.Remote.Port)))
		} else {
			wb.logEvent("ice_pair unavailable")
		}
		if wb.resolution == lanHDResolution {
			if err := requireLANHDPeer(pair); err != nil {
				// Closing the peer connection inside its ICE callback can deadlock.
				go wb.handleError("ice_pair", err)
			}
		}
	})
	wb.peerConnection.OnICEConnectionStateChange(func(state pion.ICEConnectionState) {
		if !wb.active() {
			return
		}
		wb.logEvent("ice_state=%s", state)
	})

	// On HEVC, use DataChannel to receive video/audio
	if wb.isHEVC {
		maxRetransmits := uint16(5)
		ordered := true

		wb.dataChannel, err = wb.peerConnection.CreateDataChannel("fmp4Stream", &pion.DataChannelInit{
			MaxRetransmits: &maxRetransmits,
			Ordered:        &ordered,
		})
		if err != nil {
			return fmt.Errorf("failed to create data channel: %w", err)
		}

		wb.dataChannel.OnMessage(func(msg pion.DataChannelMessage) {
			wb.mediaMutex.RLock()
			defer wb.mediaMutex.RUnlock()
			if !wb.active() {
				return
			}
			if msg.IsString {
				if connected, err := wb.probe(msg); err != nil {
					wb.handleError("data_channel_message", err)
				} else if connected {
					wb.waiter.Done(nil)
				}
			} else {
				packet := &rtp.Packet{}
				if err := packet.Unmarshal(msg.Data); err != nil {
					wb.dataChannelParseErrorLogged.Do(func() {
						core.Logger.Warn().Err(err).Msgf("Could not parse HEVC data-channel packet (%d bytes) as RTP", len(msg.Data))
					})
					return
				}

				videoSSRC := wb.videoSSRC.Load()
				audioSSRC := wb.audioSSRC.Load()
				switch packet.SSRC {
				case videoSSRC:
					wb.rtpForwarder.ForwardVideoPacket(packet)
				case audioSSRC:
					wb.rtpForwarder.ForwardAudioPacket(packet)
				default:
					wb.dataChannelSSRCMismatchLogged.Do(func() {
						core.Logger.Warn().Msgf("Dropping HEVC data-channel RTP packet with SSRC %d (video %d, audio %d), PT %d, sequence %d, %d bytes",
							packet.SSRC, videoSSRC, audioSSRC,
							packet.PayloadType, packet.SequenceNumber, len(msg.Data))
					})
				}
			}
		})

		wb.dataChannel.OnError(func(err error) {
			wb.handleError("data_channel_error", err)
		})

		wb.dataChannel.OnClose(func() {
			if !wb.active() {
				return
			}
			wb.logEvent("data_channel state=closed")
			wb.handleError("data_channel_close", errors.New("datachannel: closed"))
		})

		wb.dataChannel.OnOpen(func() {
			if !wb.active() {
				return
			}
			wb.logEvent("data_channel state=open")
			codecRequest, _ := json.Marshal(tuya.DataChannelMessage{
				Type: "codec",
				Msg:  "",
			})

			if err := wb.sendMessageToDataChannel(codecRequest); err != nil {
				wb.handleError("data_channel_open", fmt.Errorf("failed to send codec request: %w", err))
			}
		})
	}

	// Setup connection state handler
	wb.peerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if !wb.active() {
			return
		}
		wb.logEvent("peer_state=%s", state)
		if state == pion.PeerConnectionStateFailed || state == pion.PeerConnectionStateClosed {
			wb.handleError("peer_"+state.String(), errors.New("WebRTC connection failed/closed"))
		}

		if state == pion.PeerConnectionStateConnected {
			core.Logger.Info().Msgf("WebRTC connection established")

			if !wb.isHEVC {
				if wb.resolution == "hd" {
					if err := wb.cameraClient.SendResolution(0); err != nil {
						core.Logger.Warn().Err(err).Msg("Failed to request the HD stream")
					}
				}
				wb.waiter.Done(nil)
			}
		}
	})

	// Setup track handler for incoming media if not HEVC
	wb.peerConnection.OnTrack(func(track *pion.TrackRemote, receiver *pion.RTPReceiver) {
		if !wb.active() {
			return
		}
		codec := track.Codec()
		core.Logger.Trace().Msgf("Received track: %s, PayloadType: %d", codec.MimeType, codec.PayloadType)

		if track.Kind() == pion.RTPCodecTypeVideo {
			wb.videoTrack = track

			if !wb.isHEVC {
				go wb.handleVideoTrack(track)
			}
		} else if track.Kind() == pion.RTPCodecTypeAudio {
			wb.audioTrack = track

			for _, tr := range wb.peerConnection.GetTransceivers() {
				if tr.Receiver() == receiver && tr.Kind() == pion.RTPCodecTypeAudio {
					if tr.Direction() == pion.RTPTransceiverDirectionSendrecv || tr.Direction() == pion.RTPTransceiverDirectionSendonly {
						localTrack, err := pion.NewTrackLocalStaticRTP(
							pion.RTPCodecCapability{MimeType: track.Codec().MimeType},
							"audio-backchannel", "pion",
						)
						if err != nil {
							core.Logger.Warn().Err(err).Msg("Failed to create backchannel track")
							break
						}
						if err := tr.Sender().ReplaceTrack(localTrack); err != nil {
							core.Logger.Warn().Err(err).Msg("Failed to attach backchannel track")
							break
						}
						wb.setBackchannel(localTrack)
						core.Logger.Trace().Msgf("Setup backchannel track")
						break
					}
				}
			}

			if !wb.isHEVC {
				go wb.handleAudioTrack(track)
			}
		}
	})

	return nil
}

func (wb *bridgeSession) setupMQTTCameraClient(webRTCConfig *tuya.WebRTCConfig) {
	device := &tuya.Device{
		DeviceId:   wb.camera.DeviceID,
		DeviceName: wb.camera.DeviceName,
		Category:   wb.camera.Category,
		ProductId:  wb.camera.ProductID,
		Uuid:       wb.camera.UUID,
	}

	// Create MQTT camera client
	wb.cameraClient = tuya.NewMqttCameraClient(wb.mqttClient, device, webRTCConfig)
	wb.speaker.setSender(wb.cameraClient.SendSpeaker)

	// Setup handlers
	wb.cameraClient.HandleAnswer = func(answer tuya.AnswerFrame) {
		if !wb.active() {
			return
		}
		core.Logger.Trace().Msgf("Received WebRTC answer")

		err := wb.remoteCandidates.SetReady(func() error {
			desc := pion.SessionDescription{
				Type: pion.SDPTypePranswer,
				SDP:  answer.Sdp,
			}

			if err := wb.peerConnection.SetRemoteDescription(desc); err != nil {
				return err
			}

			return webrtc.SetAnswer(wb.peerConnection, answer.Sdp)
		})
		if err != nil {
			wb.handleError("mqtt_answer", err)
		}
	}

	wb.cameraClient.HandleCandidate = func(candidate tuya.CandidateFrame) {
		if !wb.active() {
			return
		}
		candidateStr := strings.TrimSpace(candidate.Candidate)
		core.Logger.Trace().Msgf("Received ICE candidate: %s", candidateStr)

		if candidateStr != "" {
			// Remove "a=" prefix if present
			if strings.HasPrefix(candidateStr, "a=") {
				candidateStr = candidateStr[2:]
			}

			// Ensure candidate ends with CRLF
			if !strings.HasSuffix(candidateStr, "\r\n") && !strings.HasSuffix(candidateStr, "\n") {
				candidateStr = candidateStr + "\r\n"
			}

			wb.remoteCandidates.Add(strings.TrimSpace(candidateStr))
		}
	}

	wb.cameraClient.HandleError = func(err error) {
		wb.handleError("mqtt_camera_error", err)
	}

	wb.cameraClient.HandleDisconnect = func() {
		// Stop resets the state; setting connected here (without the lock)
		// made Stop skip the cleanup and leak the connection.
		wb.handleError("mqtt_camera_disconnect", errors.New("camera client disconnected"))
	}
	wb.mqttClient.AddCameraClient(wb.cameraClient.SessionId, wb.cameraClient)
}

// addRemoteCandidate adds one of the camera's ICE candidates. A candidate that
// pion rejects is not fatal: ICE fails on its own if no candidate pair works.
func (wb *bridgeSession) addRemoteCandidate(candidate string) error {
	core.Logger.Trace().Msgf("Adding ICE candidate: %s", candidate)

	err := wb.peerConnection.AddICECandidate(pion.ICECandidateInit{Candidate: candidate})
	if err != nil {
		core.Logger.Warn().Err(err).Msgf("Ignoring ICE candidate from camera %s", wb.camera.DeviceName)
	}
	return err
}

func (wb *bridgeSession) createAndSendOffer() error {
	wb.peerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		if !wb.active() {
			return
		}
		if candidate != nil {
			core.Logger.Trace().Msgf("Generated ICE candidate: %s", candidate.ToJSON().Candidate)

			if err := wb.cameraClient.SendCandidate(wb.ctx, "a="+candidate.ToJSON().Candidate); err != nil &&
				wb.ctx.Err() == nil {
				core.Logger.Error().Err(err).Msg("Error sending ICE candidate")
			}
		}
	})

	medias := []*utils.Media{
		{Kind: utils.KindAudio, Direction: utils.DirectionSendRecv},
		{Kind: utils.KindVideo, Direction: utils.DirectionRecvonly},
	}

	// Create offer
	offer, err := webrtc.CreateOffer(wb.peerConnection, medias)
	if err != nil {
		return fmt.Errorf("failed to create offer: %v", err)
	}

	// Remove extmap lines to reduce payload size (device limitation)
	re := regexp.MustCompile(`\r\na=extmap[^\r\n]*`)
	offer = re.ReplaceAllString(offer, "")

	core.Logger.Trace().Msgf("Sending WebRTC offer")

	// Send offer
	ctx, cancel := context.WithTimeout(wb.ctx, startTimeout)
	defer cancel()
	if err := wb.cameraClient.SendOffer(ctx, offer, wb.upstreamResolution(), wb.streamType, wb.isHEVC); err != nil {
		return fmt.Errorf("failed to send offer: %v", err)
	}

	return nil
}

// Allow time for NACK retransmission through a relay (late frames up to
// about 0.8 s were measured on a real camera) while bounding latency and
// the number of packets retained during a gap.
var videoReorderWait = time.Second
var videoReorderWindow uint16 = 1024

type videoRTPTrack interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(time.Time) error
}

func forwardVideoRTPTrack(ctx context.Context, track videoRTPTrack, forward func(*rtp.Packet)) {
	var reorder rtpReorder
	var deadline time.Time
	for ctx.Err() == nil {
		nextDeadline := reorder.deadline(videoReorderWait)
		if !nextDeadline.Equal(deadline) {
			if err := track.SetReadDeadline(nextDeadline); err != nil {
				core.Logger.Warn().Err(err).Msg("Could not set video RTP read deadline")
				return
			}
			deadline = nextDeadline
		}
		packet, _, err := track.ReadRTP()
		now := time.Now()
		for _, output := range reorder.drain(now, videoReorderWait, videoReorderWindow, "RTP video") {
			forward(output)
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && !deadline.IsZero() {
				continue
			}
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "closed") ||
				strings.Contains(err.Error(), "EOF") ||
				strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			core.Logger.Warn().Err(err).Msg("Unexpected error reading video RTP packet")
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, output := range reorder.push(packet, now, videoReorderWait, videoReorderWindow, "RTP video") {
			forward(output)
		}
	}
}

func (wb *bridgeSession) handleVideoTrack(track *pion.TrackRemote) {
	core.Logger.Trace().Msgf("Starting video track handler")
	if wb.resolution != lanHDResolution {
		forwardVideoRTPTrack(wb.ctx, track, wb.rtpForwarder.ForwardVideoPacket)
		return
	}
	lanVideo := newLANHDVideo()
	wb.lanHDRequest.Do(func() { go wb.requestLANHD() })

	for {
		select {
		case <-wb.ctx.Done():
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}
				// Check if it's a known close error
				if strings.Contains(err.Error(), "closed") ||
					strings.Contains(err.Error(), "EOF") ||
					strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				core.Logger.Warn().Err(err).Msg("Unexpected error reading video RTP packet")
				time.Sleep(10 * time.Millisecond)
				continue
			}

			if wb.lanHDSwitched.Load() {
				wasReady := lanVideo.ready
				packets := lanVideo.push(packet, time.Now())
				if !wasReady && lanVideo.ready {
					wb.lanHDReady.Done(nil)
					core.Logger.Info().Msg("/hd-lan: received HEVC parameters and a keyframe")
				}
				for _, output := range packets {
					wb.rtpForwarder.ForwardVideoPacket(output)
				}
			}
		}
	}
}

func (wb *bridgeSession) handleAudioTrack(track *pion.TrackRemote) {
	core.Logger.Trace().Msgf("Starting audio track handler")

	for {
		select {
		case <-wb.ctx.Done():
			return
		default:
			packet, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					return
				}
				if strings.Contains(err.Error(), "closed") ||
					strings.Contains(err.Error(), "EOF") ||
					strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				core.Logger.Warn().Err(err).Msg("Unexpected error reading audio RTP packet")
				time.Sleep(10 * time.Millisecond)
				continue
			}

			wb.rtpForwarder.ForwardAudioPacket(packet)
		}
	}
}

func (wb *WebRTCBridge) createHTTPClient() *http.Client {
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil
	}

	if wb.user.SessionData != nil && len(wb.user.SessionData.Cookies) > 0 {
		serverURL, _ := url.Parse(fmt.Sprintf("https://%s", wb.user.SessionData.ServerHost))

		var httpCookies []*http.Cookie
		for _, cookie := range wb.user.SessionData.Cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				Domain:   cookie.Domain,
				Path:     cookie.Path,
				Expires:  cookie.Expires,
				Secure:   cookie.Secure,
				HttpOnly: cookie.HttpOnly,
			})
		}

		jar.SetCookies(serverURL, httpCookies)
	}

	return &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
	}
}

func (wb *bridgeSession) probe(msg pion.DataChannelMessage) (bool, error) {
	var message tuya.DataChannelMessage
	if err := json.Unmarshal([]byte(msg.Data), &message); err != nil {
		return false, err
	}

	switch message.Type {
	case "codec":
		frameRequest, _ := json.Marshal(tuya.DataChannelMessage{
			Type: "start",
			Msg:  "frame",
		})

		err := wb.sendMessageToDataChannel(frameRequest)
		if err != nil {
			return false, err
		}

	case "recv":
		var recvMessage tuya.RecvMessage
		if err := json.Unmarshal([]byte(message.Msg), &recvMessage); err != nil {
			return false, err
		}

		wb.videoSSRC.Store(recvMessage.Video.SSRC)
		wb.audioSSRC.Store(recvMessage.Audio.SSRC)

		completeMsg, _ := json.Marshal(tuya.DataChannelMessage{
			Type: "complete",
			Msg:  "",
		})

		err := wb.sendMessageToDataChannel(completeMsg)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (wb *bridgeSession) sendMessageToDataChannel(message []byte) error {
	if wb.dataChannel != nil {
		return wb.dataChannel.Send(message)
	}

	return nil
}

func (wb *bridgeSession) handleError(source string, err error) {
	if wb.closing.Load() || wb.WebRTCBridge.ctx.Err() != nil ||
		wb.WebRTCBridge.closing.Load() ||
		wb.WebRTCBridge.session.Load() != wb {
		return
	}
	if err == nil {
		err = errors.New("WebRTC session failed")
	}
	wb.stateMutex.Lock()
	if wb.failure != nil {
		wb.stateMutex.Unlock()
		return
	}
	wb.failure = err
	ready := wb.ready
	wb.stateMutex.Unlock()
	wb.waiter.Done(err)
	wb.lanHDReady.Done(err)
	if !ready {
		return
	}
	if wb.resolution == "hd" && wb.isHEVC &&
		(source == "data_channel_error" || source == "data_channel_close" ||
			source == "peer_failed" || source == "peer_closed") {
		go wb.WebRTCBridge.renew(wb, source, err)
		return
	}
	go wb.WebRTCBridge.reportSessionFailure(wb, source, err)
}

func (wb *bridgeSession) markReady() error {
	wb.stateMutex.Lock()
	defer wb.stateMutex.Unlock()
	if wb.failure != nil {
		return wb.failure
	}
	if err := wb.ctx.Err(); err != nil {
		return err
	}
	wb.ready = true
	return nil
}

func waitRenewalDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (wb *WebRTCBridge) reportSessionFailure(s *bridgeSession, source string, err error) {
	if wb.ctx.Err() != nil || wb.session.Load() != s {
		return
	}
	wb.logShutdown(source, err)
	if wb.OnError != nil {
		wb.OnError(err)
	}
}

func (wb *WebRTCBridge) renew(old *bridgeSession, source string, cause error) {
	wb.renewalMutex.Lock()
	failed, err := wb.renewLocked(old, cause)
	wb.renewalMutex.Unlock()
	if failed != nil {
		wb.reportSessionFailure(failed, source, err)
	}
}

func (wb *WebRTCBridge) renewLocked(old *bridgeSession, cause error) (*bridgeSession, error) {
	wb.mutex.RLock()
	connected := wb.connected
	wb.mutex.RUnlock()
	if wb.ctx.Err() != nil || wb.session.Load() != old || !connected {
		return nil, nil
	}
	if wb.rtpForwarder.GetClientCount() == 0 {
		return old, cause
	}
	old.close()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := wb.renewalDelay(wb.ctx, time.Second<<uint(attempt-1)); err != nil {
			return nil, nil
		}
		if wb.ctx.Err() != nil {
			return nil, nil
		}
		if wb.rtpForwarder.GetClientCount() == 0 {
			return wb.session.Load(), cause
		}
		wb.logEvent("renewal attempt=%d result=started error=%q", attempt, "")
		s := wb.newSession()
		s.renewing = true
		if wb.ctx.Err() != nil {
			s.close()
			return nil, nil
		}
		wb.session.Store(s)
		wb.rtpForwarder.RenewSource(true)
		err := wb.ctx.Err()
		if err == nil {
			err = wb.setupSession(s)
		}
		if err == nil {
			err = s.markReady()
		}
		if err == nil && wb.ctx.Err() == nil {
			wb.logEvent("renewal attempt=%d result=ok error=%q", attempt, "")
			return nil, nil
		}
		s.close()
		if wb.ctx.Err() != nil {
			return nil, nil
		}
		wb.logEvent("renewal attempt=%d result=failed error=%q", attempt, errorText(err))
		cause = err
	}
	return wb.session.Load(), cause
}
