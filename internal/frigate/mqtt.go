package frigate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Subscriber delivers parsed Frigate MQTT payloads.
// Re-subscribes on every (re)connect; paho handles reconnect/backoff itself.
type Subscriber struct {
	client mqtt.Client
	ready  func(bool)
	logger zerolog.Logger
}

type Options struct {
	Broker      string
	ClientID    string
	Username    string
	Password    string
	TopicPrefix string
	// OnReady: true on connect+subscribe, false on lost connection.
	OnReady func(bool)
}

// Handlers routes each subscribed topic to a callback.
type Handlers struct {
	Review              func(ReviewEvent)
	TrackedObjectUpdate func(TrackedObjectUpdate)
}

// NewSubscriber connects and dispatches parsed payloads to handlers.
// Blocks until the initial connection succeeds or ctx is canceled.
func NewSubscriber(ctx context.Context, opts Options, handlers Handlers) (*Subscriber, error) {
	s := &Subscriber{
		ready:  opts.OnReady,
		logger: log.With().Str("logger", "mqtt").Logger(),
	}

	subscriptions := map[string]mqtt.MessageHandler{
		opts.TopicPrefix + "/reviews":               decodeInto(s, handlers.Review),
		opts.TopicPrefix + "/tracked_object_update": decodeInto(s, handlers.TrackedObjectUpdate),
	}

	mqttOpts := mqtt.NewClientOptions().
		AddBroker(opts.Broker).
		SetClientID(opts.ClientID).
		SetUsername(opts.Username).
		SetPassword(opts.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(60 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			for topic, handler := range subscriptions {
				if token := c.Subscribe(topic, 1, handler); token.Wait() && token.Error() != nil {
					s.logger.Error().Err(token.Error()).Str("topic", topic).Msg("failed to subscribe")
					s.ready(false)
					return
				}
				s.logger.Info().Str("topic", topic).Msg("subscribed to frigate MQTT topic")
			}
			s.ready(true)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			s.logger.Warn().Err(err).Msg("mqtt connection lost")
			s.ready(false)
		}).
		SetReconnectingHandler(func(_ mqtt.Client, _ *mqtt.ClientOptions) {
			s.logger.Info().Msg("mqtt reconnecting")
		})

	s.client = mqtt.NewClient(mqttOpts)

	s.logger.Info().Str("broker", opts.Broker).Msg("connecting to mqtt broker")

	token := s.client.Connect()
	select {
	case <-token.Done():
		if token.Error() != nil {
			return nil, fmt.Errorf("mqtt connect: %w", token.Error())
		}
	case <-ctx.Done():
		s.client.Disconnect(0)
		return nil, ctx.Err()
	}

	return s, nil
}

func (s *Subscriber) Close() {
	s.client.Disconnect(250)
}

// decodeInto adapts a typed handler to paho's untyped one.
func decodeInto[T any](s *Subscriber, handle func(T)) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var payload T
		if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
			s.logger.Error().Err(err).Str("topic", msg.Topic()).Msg("failed to unmarshal frigate payload")
			return
		}
		handle(payload)
	}
}
