package tuya

import (
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

func (t *speakerTestToken) WaitTimeout(timeout time.Duration) bool {
	t.timeout = timeout
	return false
}

type speakerTestClient struct {
	mqtt.Client
	token        *speakerTestToken
	onDisconnect func()
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
