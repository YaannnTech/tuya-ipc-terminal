package tuya

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type speakerTestToken struct {
	mqtt.Token
	timeout time.Duration
}

func (t *speakerTestToken) Wait() bool {
	return true
}

func (t *speakerTestToken) Error() error {
	return nil
}

func (t *speakerTestToken) WaitTimeout(timeout time.Duration) bool {
	t.timeout = timeout
	return false
}

type speakerTestClient struct {
	mqtt.Client
	token        *speakerTestToken
	onDisconnect func()
}

type cameraTestMessage struct {
	mqtt.Message
}

type cancellableTestToken struct {
	mqtt.Token
	done chan struct{}
	err  error
}

func (t *cancellableTestToken) Done() <-chan struct{} { return t.done }
func (t *cancellableTestToken) Error() error          { return t.err }

type cancellableTestClient struct {
	mqtt.Client
	token     *cancellableTestToken
	published chan struct{}
}

func (c *cancellableTestClient) Publish(string, byte, bool, interface{}) mqtt.Token {
	close(c.published)
	return c.token
}

func (cameraTestMessage) Payload() []byte {
	return []byte(`{"data":{"header":{"type":"candidate","sessionid":"session"},` +
		`"msg":{"candidate":"a=candidate"}}}`)
}

func (c *speakerTestClient) Publish(string, byte, bool, interface{}) mqtt.Token {
	return c.token
}

func (c *speakerTestClient) Disconnect(uint) {
	c.onDisconnect()
}

func TestSpeakerPublishTimesOut(t *testing.T) {
	token := &speakerTestToken{}
	client := &MQTTClient{mqtt: &speakerTestClient{token: token}}
	camera := &MQTTCameraClient{mqttClient: client}

	err := camera.SendSpeaker(1)
	if err == nil || !strings.Contains(err.Error(), "timed out") || token.timeout <= 0 {
		t.Fatalf("SendSpeaker error = %v, timeout = %s", err, token.timeout)
	}
}

func TestDisconnectPublishTimesOut(t *testing.T) {
	token := &speakerTestToken{}
	client := &MQTTClient{mqtt: &speakerTestClient{token: token}}
	camera := &MQTTCameraClient{mqttClient: client}

	err := camera.SendDisconnect()
	if err == nil || !strings.Contains(err.Error(), "timed out") || token.timeout <= 0 {
		t.Fatalf("SendDisconnect error = %v, timeout = %s", err, token.timeout)
	}
}

func TestMQTTStopMarksClosedBeforeDisconnect(t *testing.T) {
	client := &MQTTClient{}
	client.mqtt = &speakerTestClient{onDisconnect: func() {
		if !client.closed.Load() {
			t.Error("MQTT client was still open during disconnect")
		}
	}}
	client.Stop()
}

func TestResolutionRequestWhileMQTTDisconnects(t *testing.T) {
	client := &MQTTClient{}
	client.closed.Store(true)
	camera := &MQTTCameraClient{mqttClient: client}

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			client.onDisconnect(nil, nil)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			if err := camera.SendResolution(0); err == nil {
				t.Error("resolution request succeeded after MQTT disconnect")
				return
			}
		}
	}()
	workers.Wait()
}

func TestCameraRegistrationWhileMQTTConsumes(t *testing.T) {
	client := &MQTTClient{}
	camera := &MQTTCameraClient{HandleCandidate: func(CandidateFrame) {}}
	client.AddCameraClient("session", camera)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			client.AddCameraClient("session", camera)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			client.consume(nil, cameraTestMessage{})
		}
	}()
	workers.Wait()
}

func TestSignalingPublishStopsOnCancellation(t *testing.T) {
	for _, tt := range []struct {
		name string
		send func(context.Context, *MQTTCameraClient) error
	}{
		{"offer", func(ctx context.Context, c *MQTTCameraClient) error {
			return c.SendOffer(ctx, "sdp", "hd", 0, true)
		}},
		{"candidate", func(ctx context.Context, c *MQTTCameraClient) error {
			return c.SendCandidate(ctx, "candidate")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &cancellableTestClient{
				token:     &cancellableTestToken{done: make(chan struct{})},
				published: make(chan struct{}),
			}
			camera := &MQTTCameraClient{
				mqttClient:   &MQTTClient{mqtt: client},
				webrtcConfig: &WebRTCConfig{},
			}
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- tt.send(ctx, camera) }()
			select {
			case <-client.published:
			case <-time.After(5 * time.Second):
				t.Fatal("publish did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("publish error = %v, want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publish did not stop on cancellation")
			}
			if err := tt.send(ctx, camera); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled publish error = %v", err)
			}
			for _, publishErr := range []error{nil, errors.New("publish failed")} {
				token := &cancellableTestToken{done: make(chan struct{}), err: publishErr}
				close(token.done)
				camera.mqttClient.mqtt = &cancellableTestClient{
					token: token, published: make(chan struct{}),
				}
				if err := tt.send(context.Background(), camera); !errors.Is(err, publishErr) {
					t.Fatalf("completed publish error = %v, want %v", err, publishErr)
				}
			}
		})
	}
}
