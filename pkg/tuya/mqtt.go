package tuya

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/utils"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTClient struct {
	mqtt           mqtt.Client
	uid            string
	subscribeTopic string
	cameras        map[string]*MQTTCameraClient // sessionId -> camera
	closed         atomic.Bool
	hasConnected   atomic.Bool
	onState        func(string, error)
	Connected      utils.Waiter
}

type MqttFrameHeader struct {
	Type          string `json:"type"`
	From          string `json:"from"`
	To            string `json:"to"`
	SubDevID      string `json:"sub_dev_id"`
	SessionID     string `json:"sessionid"`
	MotoID        string `json:"moto_id"`
	TransactionID string `json:"tid"`
	Seq           int    `json:"seq"`
	Rtx           int    `json:"rtx"`
}

type MqttFrame struct {
	Header  MqttFrameHeader `json:"header"`
	Message json.RawMessage `json:"msg"`
}

type MqttMessage struct {
	Protocol int       `json:"protocol"`
	Pv       string    `json:"pv"`
	T        int64     `json:"t"`
	Data     MqttFrame `json:"data"`
}

// NewMqttClient connects to the MQTT broker and reports connection changes.
func NewMqttClient(
	clientId, mobileMqttsUrl string, mqttConfig *MQTConfig, onState func(string, error),
) (*MQTTClient, error) {
	client := &MQTTClient{
		uid:            mqttConfig.Msid,
		subscribeTopic: fmt.Sprintf("/av/u/%s", mqttConfig.Msid),
		Connected:      utils.Waiter{},
		onState:        onState,
	}

	wssUrl := fmt.Sprintf("wss://%s/mqtt", mobileMqttsUrl)
	username := fmt.Sprintf("web_%s", mqttConfig.Msid)
	password := mqttConfig.Password

	opts := mqtt.NewClientOptions()
	opts.AddBroker(wssUrl)
	opts.SetClientID(username)
	opts.SetUsername(username)
	opts.SetPassword(password)
	// opts.SetDefaultPublishHandler(messageHandler)
	opts.SetOnConnectHandler(client.onConnect)
	opts.SetConnectionLostHandler(client.onDisconnect)
	opts.SetAutoReconnect(true)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)

	client.mqtt = mqtt.NewClient(opts)
	if token := client.mqtt.Connect(); token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	return client, nil
}

// Stop disconnects the MQTT client.
func (c *MQTTClient) Stop() {
	if c.mqtt != nil {
		c.closed.Store(true)
		c.mqtt.Disconnect(250)
	}
}

func (c *MQTTClient) AddCameraClient(sessionId string, cameraClient *MQTTCameraClient) {
	if c.cameras == nil {
		c.cameras = make(map[string]*MQTTCameraClient)
	}
	c.cameras[sessionId] = cameraClient
}

func (c *MQTTClient) RemoveCameraClient(sessionId string) {
	if c.cameras == nil {
		return
	}
	delete(c.cameras, sessionId)
}

func (c *MQTTClient) onConnect(client mqtt.Client) {
	if c.onState != nil {
		state := "connected"
		if c.hasConnected.Swap(true) {
			state = "reconnected"
		}
		c.onState(state, nil)
	}
	core.Logger.Trace().Msgf("Connected to mqtt broker")
	core.Logger.Trace().Msgf("Subscribing to topic: %s", c.subscribeTopic)

	if token := client.Subscribe(c.subscribeTopic, 1, c.consume); token.Wait() && token.Error() != nil {
		c.Connected.Done(token.Error())
		return
	}

	core.Logger.Trace().Msgf("Subscribed")
	c.Connected.Done(nil)
}

func (c *MQTTClient) onDisconnect(client mqtt.Client, err error) {
	if c.onState != nil {
		c.onState("connection_lost", err)
	}
	if err != nil {
		c.Connected.Done(err)
	} else {
		c.Connected.Done(errors.New("mqtt client disconnected"))
	}

	c.closed.Store(true)
}

func (c *MQTTClient) consume(client mqtt.Client, msg mqtt.Message) {
	var rmqtt MqttMessage
	if err := json.Unmarshal(msg.Payload(), &rmqtt); err != nil {
		core.Logger.Error().Err(err).Msg("Failed to unmarshal mqtt message: %s")
		return
	}

	sessionId := rmqtt.Data.Header.SessionID
	cameraClient, ok := c.cameras[sessionId]
	if !ok {
		core.Logger.Warn().Msgf("No camera client found for sessionId: %s", sessionId)
		return
	}

	switch rmqtt.Data.Header.Type {
	case "answer":
		cameraClient.onMqttAnswer(&rmqtt)
	case "candidate":
		cameraClient.onMqttCandidate(&rmqtt)
	case "disconnect":
		cameraClient.onMqttDisconnect()
	}
}
