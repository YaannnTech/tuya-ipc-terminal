package rtsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	streamType     int
	isHEVC         bool
	lanHDReady     utils.Waiter
	lanHDRequest   sync.Once
	lanHDSwitched  atomic.Bool
	user           *storage.UserSession
	storageManager *storage.StorageManager

	// WebRTC components
	peerConnection                *pion.PeerConnection
	dataChannel                   *pion.DataChannel
	dataChannelParseErrorLogged   sync.Once
	dataChannelSSRCMismatchLogged sync.Once
	mqttClient                    *tuya.MQTTClient
	cameraClient                  *tuya.MQTTCameraClient
	rtpForwarder                  *RTPForwarder

	// State
	connected bool
	stopped   bool
	closing   atomic.Bool // set once the bridge shuts itself down
	closeOnce sync.Once
	waiter    utils.Waiter
	mutex     sync.RWMutex

	// The camera's ICE candidates can arrive over MQTT before its answer.
	remoteCandidates *remoteCandidateQueue

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// RTP forwarding
	videoTrack                  *pion.TrackRemote
	audioTrack                  *pion.TrackRemote
	backchannel                 atomic.Pointer[pion.TrackLocalStaticRTP]
	backchannelWriteErrorLogged sync.Once
	speaker                     speakerState

	// Callbacks
	OnVideoPacket func(packet *rtp.Packet)
	OnAudioPacket func(packet *rtp.Packet)
	OnError       func(error)
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
		waiter:         utils.Waiter{},
		ctx:            ctx,
		cancel:         cancel,
	}

	wb.rtpForwarder.OnBackchannelAudio = wb.ForwardBackchannelAudioPacket
	wb.rtpForwarder.OnClientRemoved = wb.speaker.clientLeft
	wb.remoteCandidates = newRemoteCandidateQueue(wb.addRemoteCandidate)

	return wb
}

// startTimeout bounds each wait in Start (MQTT connect, camera handshake), so a
// camera that never completes the handshake cannot keep a stream "starting".
var startTimeout = 30 * time.Second

// waitFor waits for w, but gives up after startTimeout or when Stop is called.
func (wb *WebRTCBridge) waitFor(w *utils.Waiter, what string) error {
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

	// A failed start must not leave the MQTT client or peer connection behind:
	// they would keep pulling the stream from Tuya with nobody to receive it.
	defer func() {
		if err != nil {
			wb.closeResources()
		}
	}()

	core.Logger.Info().Msgf("Starting WebRTC bridge for camera: %s", wb.camera.DeviceName)

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

	// Connect to MQTT broker
	wb.mqttClient, err = tuya.NewMqttClient(
		appInfo.Result.ClientId,
		wb.user.SessionData.LoginResult.Domain.MobileMqttsUrl,
		&mqttConfig.Result,
	)

	if err != nil {
		return fmt.Errorf("failed to connect to MQTT: %v", err)
	}

	if err = wb.waitFor(&wb.mqttClient.Connected, "the MQTT connection"); err != nil {
		return fmt.Errorf("MQTT connection failed: %v", err)
	}

	// Get WebRTC configuration
	webRTCConfig, err := tuya.GetWebRTCConfig(wb.ctx, httpClient, wb.user.SessionData.ServerHost, wb.camera.DeviceID)
	if err != nil {
		return fmt.Errorf("failed to get WebRTC config: %v", err)
	}

	// Parse skill information
	var skill tuya.Skill
	if err := json.Unmarshal([]byte(webRTCConfig.Result.Skill), &skill); err != nil {
		return fmt.Errorf("failed to parse skill info: %v", err)
	}

	// Determine stream settings
	wb.streamType = tuya.GetStreamType(&skill, wb.upstreamResolution())
	wb.isHEVC = tuya.IsHEVC(&skill, wb.streamType)
	if wb.resolution == lanHDResolution && wb.isHEVC {
		return errors.New("/hd-lan requires an H.264 SD stream")
	}

	core.Logger.Info().Msgf("Stream settings - Resolution: %s, Type: %d, HEVC: %v", wb.resolution, wb.streamType, wb.isHEVC)

	// Setup WebRTC peer connection
	if err := wb.setupPeerConnection(&webRTCConfig.Result); err != nil {
		return fmt.Errorf("failed to setup peer connection: %v", err)
	}

	// Setup MQTT camera client
	wb.setupMQTTCameraClient(&webRTCConfig.Result)

	// Create and send offer
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

	wb.connected = true
	core.Logger.Info().Msgf("WebRTC bridge started successfully for camera: %s", wb.camera.DeviceName)

	return nil
}

func (wb *WebRTCBridge) Stop() {
	// Cancel before taking the lock: a Start that is still waiting holds it.
	wb.cancel()

	wb.mutex.Lock()
	defer wb.mutex.Unlock()

	if wb.stopped {
		return
	}
	wb.stopped = true
	wb.connected = false

	core.Logger.Info().Msgf("Stopping WebRTC bridge for camera: %s", wb.camera.DeviceName)
	wb.closeResources()
	core.Logger.Info().Msgf("WebRTC bridge stopped for camera: %s", wb.camera.DeviceName)
}

// closeResources releases everything Start set up, at most once. It runs both
// after a failed Start and on Stop, whether or not the bridge ever connected.
func (wb *WebRTCBridge) closeResources() {
	wb.closeOnce.Do(func() {
		wb.closing.Store(true)
		wb.cancel()

		// Before SendDisconnect and the MQTT shutdown, which end the session.
		wb.speaker.stop()
		if wb.cameraClient != nil {
			wb.cameraClient.SendDisconnect()
		}
		if wb.peerConnection != nil {
			wb.peerConnection.Close()
		}
		if wb.mqttClient != nil {
			wb.mqttClient.Stop()
		}
		if wb.rtpForwarder != nil {
			wb.rtpForwarder.Stop()
		}
	})
}

func (wb *WebRTCBridge) IsConnected() bool {
	wb.mutex.RLock()
	defer wb.mutex.RUnlock()
	return wb.connected
}

func (wb *WebRTCBridge) ForwardBackchannelAudioPacket(sessionID string, packet *rtp.Packet) {
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

func (wb *WebRTCBridge) setBackchannel(track *pion.TrackLocalStaticRTP) {
	wb.backchannel.Store(track)
}

func (wb *WebRTCBridge) setupPeerConnection(webRTCConfig *tuya.WebRTCConfig) error {
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
	if wb.resolution == lanHDResolution {
		wb.lanHDICETransport().OnSelectedCandidatePairChange(func(pair *pion.ICECandidatePair) {
			if err := requireLANHDPeer(pair); err != nil {
				// Closing the peer connection inside its ICE callback can deadlock.
				go wb.handleError(err)
			}
		})
	}

	// On HEVC, use DataChannel to receive video/audio
	if wb.isHEVC {
		maxRetransmits := uint16(5)
		ordered := true

		wb.dataChannel, err = wb.peerConnection.CreateDataChannel("fmp4Stream", &pion.DataChannelInit{
			MaxRetransmits: &maxRetransmits,
			Ordered:        &ordered,
		})

		wb.dataChannel.OnMessage(func(msg pion.DataChannelMessage) {
			if msg.IsString {
				if connected, err := wb.probe(msg); err != nil {
					wb.handleError(err)
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

				videoSSRC := wb.rtpForwarder.videoSSRC.Load()
				audioSSRC := wb.rtpForwarder.audioSSRC.Load()
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
			wb.handleError(err)
		})

		wb.dataChannel.OnClose(func() {
			wb.handleError(errors.New("datachannel: closed"))
		})

		wb.dataChannel.OnOpen(func() {
			codecRequest, _ := json.Marshal(tuya.DataChannelMessage{
				Type: "codec",
				Msg:  "",
			})

			if err := wb.sendMessageToDataChannel(codecRequest); err != nil {
				wb.handleError(fmt.Errorf("failed to send codec request: %w", err))
			}
		})
	}

	// Setup connection state handler
	wb.peerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateFailed || state == pion.PeerConnectionStateClosed {
			wb.handleError(errors.New("WebRTC connection failed/closed"))
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

func (wb *WebRTCBridge) setupMQTTCameraClient(webRTCConfig *tuya.WebRTCConfig) {
	device := &tuya.Device{
		DeviceId:   wb.camera.DeviceID,
		DeviceName: wb.camera.DeviceName,
		Category:   wb.camera.Category,
		ProductId:  wb.camera.ProductID,
		Uuid:       wb.camera.UUID,
	}

	// Create MQTT camera client
	wb.cameraClient = tuya.NewMqttCameraClient(wb.mqttClient, device, webRTCConfig)
	wb.mqttClient.AddCameraClient(wb.cameraClient.SessionId, wb.cameraClient)
	wb.speaker.setSender(wb.cameraClient.SendSpeaker)

	// Setup handlers
	wb.cameraClient.HandleAnswer = func(answer tuya.AnswerFrame) {
		core.Logger.Trace().Msgf("Received WebRTC answer")
		core.Logger.Trace().Msgf("Answer SDP: %s", answer.Sdp)

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
			wb.handleError(err)
		}
	}

	wb.cameraClient.HandleCandidate = func(candidate tuya.CandidateFrame) {
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
		wb.handleError(err)
	}

	wb.cameraClient.HandleDisconnect = func() {
		// Stop resets the state; setting connected here (without the lock)
		// made Stop skip the cleanup and leak the connection.
		wb.handleError(errors.New("camera client disconnected"))
	}
}

// addRemoteCandidate adds one of the camera's ICE candidates. A candidate that
// pion rejects is not fatal: ICE fails on its own if no candidate pair works.
func (wb *WebRTCBridge) addRemoteCandidate(candidate string) error {
	core.Logger.Trace().Msgf("Adding ICE candidate: %s", candidate)

	err := wb.peerConnection.AddICECandidate(pion.ICECandidateInit{Candidate: candidate})
	if err != nil {
		core.Logger.Warn().Err(err).Msgf("Ignoring ICE candidate from camera %s", wb.camera.DeviceName)
	}
	return err
}

func (wb *WebRTCBridge) createAndSendOffer() error {
	wb.peerConnection.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			core.Logger.Trace().Msgf("Generated ICE candidate: %s", candidate.ToJSON().Candidate)

			if err := wb.cameraClient.SendCandidate("a=" + candidate.ToJSON().Candidate); err != nil {
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
	if err := wb.cameraClient.SendOffer(offer, wb.upstreamResolution(), wb.streamType, wb.isHEVC); err != nil {
		return fmt.Errorf("failed to send offer: %v", err)
	}

	return nil
}

func (wb *WebRTCBridge) handleVideoTrack(track *pion.TrackRemote) {
	core.Logger.Trace().Msgf("Starting video track handler")
	var lanVideo *lanHDVideo
	if wb.resolution == lanHDResolution {
		lanVideo = newLANHDVideo()
		wb.lanHDRequest.Do(func() { go wb.requestLANHD() })
	}

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

			if lanVideo == nil {
				wb.rtpForwarder.ForwardVideoPacket(packet)
			} else if wb.lanHDSwitched.Load() {
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

func (wb *WebRTCBridge) handleAudioTrack(track *pion.TrackRemote) {
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

func (wb *WebRTCBridge) probe(msg pion.DataChannelMessage) (bool, error) {
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

		wb.rtpForwarder.videoSSRC.Store(recvMessage.Video.SSRC)
		wb.rtpForwarder.audioSSRC.Store(recvMessage.Audio.SSRC)

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

func (wb *WebRTCBridge) sendMessageToDataChannel(message []byte) error {
	if wb.dataChannel != nil {
		return wb.dataChannel.Send(message)
	}

	return nil
}

func (wb *WebRTCBridge) handleError(err error) {
	// Errors after our own shutdown started are expected (e.g. the camera
	// confirming the disconnect we sent) and must not be reported.
	if wb.closing.Load() {
		return
	}
	if wb.resolution == lanHDResolution {
		wb.waiter.Done(err)
		wb.lanHDReady.Done(err)
	}
	if wb.OnError != nil {
		wb.waiter.Done(err)
		wb.OnError(err)
	}
}
