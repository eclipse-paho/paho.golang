package paho

import (
	"fmt"
	"sync"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/log"
)

// topic_alias handles aliases on received PUBLISH packets.

// Note: This is in paho to retain backwards compatability (the functionality was previously in `router.go` so retaining
// support for a default router would be challenging if it were moved). Prior to V1 support for the old router will be
// removed and this code moved to extensions/topicaliases. Because of this, the functions are not exported (want to avoid
// future breakage.

type topicAlias struct {
	sync.Mutex
	aliases map[uint16]string // Holds current aliases
	client  *Client           // Used to detect disconnects

	debug log.Logger
}

// newTopicAliasHandler returns an OnPublishReceived callback that resolves inbound
// topic aliases. Add it to ClientConfig.OnPublishReceived at the point where later
// callbacks should begin seeing resolved topics. The handler resets its mappings
// when AutoPaho supplies a new Client after reconnection.
// Note: If you wish to use this in your own code, please copy it. In the future it
// will move to extensions/topicaliases.
func newTopicAliasHandler() func(PublishReceived) (bool, error) {
	t := &topicAlias{
		aliases: make(map[uint16]string),
		debug:   log.NOOPLogger{},
	}
	return t.OnPublishReceived
}

// OnPublishReceived mutates the PUBLISH packet to resolve an alias.
func (t *topicAlias) OnPublishReceived(pr PublishReceived) (bool, error) {
	if pr.Packet == nil || pr.Packet.Properties == nil || pr.Packet.Properties.TopicAlias == nil {
		return false, nil
	}
	alias := *pr.Packet.Properties.TopicAlias
	if alias == 0 {
		return false, topicAliasDisconnectError(fmt.Errorf("topic alias must be greater than zero"))
	}
	if pr.Client != nil && alias > pr.Client.clientProps.TopicAliasMaximum {
		return false, topicAliasDisconnectError(fmt.Errorf("topic alias %d exceeds maximum %d", alias, pr.Client.clientProps.TopicAliasMaximum))
	}
	t.Lock()
	defer t.Unlock()
	// Possible the connection has changed, meaning we need to clear the map.
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

	// Client treats this as a protocol error and drops the connection.
	return false, topicAliasDisconnectError(fmt.Errorf("topic alias %d not found", alias))
}

// topicAliasDisconnectError implements handlerDisconnectError, which will cause the client to disconnect.
func topicAliasDisconnectError(err error) *handlerDisconnectError {
	return &handlerDisconnectError{
		Packet: &Disconnect{
			ReasonCode: packets.DisconnectTopicAliasInvalid,
			Properties: &DisconnectProperties{ReasonString: err.Error()},
		},
		Err: err,
	}
}

// handlerDisconnectError may be returned by an OnPublishReceived callback to stop dispatching the current
// PUBLISH and disconnect with the supplied packet. The current PUBLISH is not acknowledged.
type handlerDisconnectError struct {
	Packet *Disconnect
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
func (e *handlerDisconnectError) Disconnect() *Disconnect {
	return e.Packet
}
