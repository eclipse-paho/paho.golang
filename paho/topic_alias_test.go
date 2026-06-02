/*
 * Copyright (c) 2024 Contributors to the Eclipse Foundation
 *
 *  All rights reserved. This program and the accompanying materials
 *  are made available under the terms of the Eclipse Public License v2.0
 *  and Eclipse Distribution License v1.0 which accompany this distribution.
 *
 * The Eclipse Public License is available at
 *    https://www.eclipse.org/legal/epl-2.0/
 *  and the Eclipse Distribution License is available at
 *    http://www.eclipse.org/org/documents/edl-v10.php.
 *
 *  SPDX-License-Identifier: EPL-2.0 OR BSD-3-Clause
 */

package paho

import (
	"sync"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTopicAliasForTest() *topicAlias {
	return &topicAlias{
		aliases: make(map[uint16]string),
		debug:   log.NOOPLogger{},
	}
}

func TestTopicAliasOnPublishReceived(t *testing.T) {
	ta := newTopicAliasForTest()

	register := &Publish{
		Topic:      "test/topic",
		Properties: &PublishProperties{TopicAlias: Uint16(1)},
	}
	handled, err := ta.OnPublishReceived(PublishReceived{Packet: register})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/topic", ta.aliases[1])

	aliased := &Publish{
		Properties: &PublishProperties{TopicAlias: Uint16(1)},
		Payload:    []byte("aliased payload"),
	}
	handled, err = ta.OnPublishReceived(PublishReceived{Packet: aliased})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/topic", aliased.Topic)
}

func TestTopicAliasOnPublishReceivedNoAlias(t *testing.T) {
	ta := newTopicAliasForTest()
	p := &Publish{
		Topic:      "test/no-alias",
		Properties: &PublishProperties{},
	}

	handled, err := ta.OnPublishReceived(PublishReceived{Packet: p})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/no-alias", p.Topic)
	assert.Empty(t, ta.aliases)
}

func TestTopicAliasOnPublishReceivedUnknownAlias(t *testing.T) {
	ta := newTopicAliasForTest()

	handled, err := ta.OnPublishReceived(PublishReceived{
		Packet: &Publish{Properties: &PublishProperties{TopicAlias: Uint16(99)}},
	})
	require.EqualError(t, err, "topic alias 99 not found")
	assert.False(t, handled)
}

// TestTopicAliasWithClient confirms that topicAlias works with Client
func TestTopicAliasWithClient(t *testing.T) {
	ta := newTopicAliasForTest()
	r := NewStandardRouter()

	// Create a new client that uses the above handles (TODO: will need to update after the client is changed to use topic_alias)
	c := NewClient(ClientConfig{
		Router:            r,
		OnPublishReceived: []func(PublishReceived) (bool, error){ta.OnPublishReceived},
	})

	var routed []*Publish
	r.RegisterHandler("test/topic", func(p *Publish) {
		routed = append(routed, p)
	})

	var routeWg sync.WaitGroup
	c.publishPackets = make(chan *packets.Publish, 2)
	routeWg.Go(func() { c.routePublishPackets() })

	// Set alias
	c.publishPackets <- &packets.Publish{
		Payload:    []byte("routed payload"),
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: Uint16(7)},
	}

	// Use alias
	c.publishPackets <- &packets.Publish{
		Payload:    []byte("routed payload 2"),
		Properties: &packets.Properties{TopicAlias: Uint16(7)},
	}

	close(c.publishPackets)
	routeWg.Wait()

	require.Len(t, routed, 2)
	assert.Equal(t, "test/topic", routed[0].Topic)
	assert.Equal(t, []byte("routed payload"), routed[0].Payload)
	assert.Equal(t, "test/topic", routed[1].Topic)
	assert.Equal(t, []byte("routed payload 2"), routed[1].Payload)
}
