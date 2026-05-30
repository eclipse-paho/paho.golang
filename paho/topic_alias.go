package paho

import (
	"fmt"
	"sync"

	"github.com/eclipse/paho.golang/paho/log"
)

// topic_alias handles aliases on received PUBLISH packets
// It provides a `PublishReceived` function that will mutate the PUBLISH packet to resolve any aliases

// Limitation - The alias cache should be reset before reconnecting. Currently, there is no mechanism to achieve
// this, and I don't think it's a big issue (the broker should resend any aliases before using them).

type topicAlias struct {
	sync.Mutex
	aliases map[uint16]string

	debug log.Logger
}

// OnPublishReceived should be added to `client.OnPublishReceived` before any handlers that rely upon the topic
// Note that this never `handles` the message; instead it may alter the message body.
func (t *topicAlias) OnPublishReceived(pr PublishReceived) (bool, error) {
	if pr.Packet.Properties.TopicAlias == nil {
		return false, nil
	}
	alias := *pr.Packet.Properties.TopicAlias
	t.Lock()
	defer t.Unlock()

	// "Topic Alias mapping by including a non-zero length Topic Name and a Topic Alias in the PUBLISH packet"
	if pr.Packet.Topic != "" {
		t.debug.Printf("registering new topic alias '%d' for topic '%s'", alias, pr.Packet.Topic)
		t.aliases[alias] = pr.Packet.Topic
		return false, nil
	}

	// pr.Packet.Topic not set so we need to substitute the alias
	if sa, ok := t.aliases[alias]; ok {
		t.debug.Printf("aliased topic '%d' translates to '%s'", alias, pr.Packet.Topic)
		pr.Packet.Topic = sa
		return false, nil
	}

	// This is a protocol error and should result in the connection being dropped
	return false, fmt.Errorf("topic alias %d not found", alias)
}
