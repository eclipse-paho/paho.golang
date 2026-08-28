package topicaliases

import (
	"fmt"
	"sync"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"
)

// inbound_topic_alias handles aliases on received PUBLISH packets.

// Note: This is currently a duplicate of code in `paho`. This version will become the master pre V1.0.
type topicAlias struct {
	sync.Mutex
	aliases map[uint16]string // Holds current aliases
	client  *paho.Client      // Used to detect disconnects

	debug log.Logger
}

// NewTopicAliasHandler returns an OnPublishReceived callback that resolves inbound
// topic aliases. Add it to ClientConfig.OnPublishReceived at the point where later
// callbacks should begin seeing resolved topics. The handler resets its mappings
// when AutoPaho supplies a new Client after reconnection.
func NewTopicAliasHandler() func(paho.PublishReceived) (bool, error) {
	t := &topicAlias{
		aliases: make(map[uint16]string),
		debug:   log.NOOPLogger{},
	}
	return t.OnPublishReceived
}

// OnPublishReceived mutates the PUBLISH packet to resolve an alias.
func (t *topicAlias) OnPublishReceived(pr paho.PublishReceived) (bool, error) {
	if pr.Packet == nil {
		return false, nil
	}
	if pr.Packet.Properties == nil || pr.Packet.Properties.TopicAlias == nil {
		// "It is a Protocol Error if the Topic Name is zero length and there is no Topic Alias."
		if pr.Packet.Topic == "" {
			return false, topicAliasDisconnectError(
				packets.DisconnectProtocolError,
				fmt.Errorf("topic name must not be empty when topic alias is absent"),
			)
		}
		return false, nil // No alias in packet so nothing for us to do
	}
	alias := *pr.Packet.Properties.TopicAlias
	if alias == 0 { // MQTT-3.3.2-8
		return false, topicAliasDisconnectError(packets.DisconnectTopicAliasInvalid, fmt.Errorf("topic alias must be greater than zero"))
	}
	if pr.Client != nil && alias > pr.Client.ClientProps().TopicAliasMaximum { // MQTT-3.3.2-11
		return false, topicAliasDisconnectError(packets.DisconnectTopicAliasInvalid, fmt.Errorf("topic alias %d exceeds maximum %d", alias, pr.Client.ClientProps().TopicAliasMaximum))
	}
	t.Lock()
	defer t.Unlock()
	// Possible the connection has changed, meaning we need to clear the map [MQTT-3.3.2-7].
	if pr.Client != nil && t.client != pr.Client {
		clear(t.aliases)
		t.client = pr.Client
	}

	// "Topic Alias mapping by including a non-zero-length Topic Name and a Topic Alias in the PUBLISH packet"
	if pr.Packet.Topic != "" {
		t.debug.Printf("registering new topic alias '%d' for topic '%s'", alias, pr.Packet.Topic)
		t.aliases[alias] = pr.Packet.Topic
		return false, nil
	}

	// pr.Packet.Topic not set, so we need to substitute the alias
	if sa, ok := t.aliases[alias]; ok {
		t.debug.Printf("aliased topic '%d' translates to '%s'", alias, sa)
		pr.Packet.Topic = sa
		return false, nil
	}

	// An unmapped alias with an empty Topic Name is a Protocol Error (MQTT 5.0 section 3.3.4).
	return false, topicAliasDisconnectError(packets.DisconnectProtocolError, fmt.Errorf("topic alias %d not found", alias))
}

// topicAliasDisconnectError implements handlerDisconnectError, which will cause the client to disconnect.
func topicAliasDisconnectError(reasonCode byte, err error) *handlerDisconnectError {
	return &handlerDisconnectError{
		Packet: &paho.Disconnect{
			ReasonCode: reasonCode,
			Properties: &paho.DisconnectProperties{ReasonString: err.Error()},
		},
		Err: err,
	}
}

// handlerDisconnectError may be returned by an OnPublishReceived callback to stop dispatching the current
// PUBLISH and disconnect with the supplied packet. The current PUBLISH is not acknowledged.
type handlerDisconnectError struct {
	Packet *paho.Disconnect
	Err    error
}

func (e *handlerDisconnectError) Error() string {
	if e == nil {
		return "handler requested disconnect"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Packet != nil {
		return fmt.Sprintf("handler requested disconnect with reason code %d", e.Packet.ReasonCode)
	}
	return "handler requested disconnect"
}

func (e *handlerDisconnectError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Disconnect implements disconnector.
func (e *handlerDisconnectError) Disconnect() *paho.Disconnect {
	return e.Packet
}
