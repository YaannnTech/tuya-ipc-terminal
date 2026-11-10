package tuya

import (
	"sync"
	"testing"
)

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
