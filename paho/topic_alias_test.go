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
	"context"
	"net"
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

// TestTopicAliasOnPublishReceivedEmptyTopicWithoutAlias must have an alias or a topic
func TestTopicAliasOnPublishReceivedEmptyTopicWithoutAlias(t *testing.T) {
	ta := newTopicAliasForTest()

	handled, err := ta.OnPublishReceived(PublishReceived{
		Packet: &Publish{Properties: &PublishProperties{}},
	})

	require.EqualError(t, err, "topic name must not be empty when topic alias is absent")
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, err, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectProtocolError), disconnectErr.Packet.ReasonCode)
	assert.False(t, handled)
	assert.Empty(t, ta.aliases)
}

func TestTopicAliasOnPublishReceivedUnknownAlias(t *testing.T) {
	ta := newTopicAliasForTest()
	c := &Client{clientProps: CommsProperties{TopicAliasMaximum: 100}}

	handled, err := ta.OnPublishReceived(PublishReceived{
		Packet: &Publish{Properties: &PublishProperties{TopicAlias: Uint16(99)}}, Client: c,
	})
	require.EqualError(t, err, "topic alias 99 not found")
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, err, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectTopicAliasInvalid), disconnectErr.Packet.ReasonCode)
	assert.False(t, handled)
}

func TestTopicAliasOnPublishReceivedInvalidAlias(t *testing.T) {
	tests := []struct {
		name  string
		alias uint16
		max   uint16
		err   string
	}{
		{name: "zero", alias: 0, max: 10, err: "topic alias must be greater than zero"},
		{name: "above maximum", alias: 11, max: 10, err: "topic alias 11 exceeds maximum 10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ta := newTopicAliasForTest()
			c := &Client{clientProps: CommsProperties{TopicAliasMaximum: tt.max}}
			handled, err := ta.OnPublishReceived(PublishReceived{
				Packet: &Publish{Topic: "test/topic", Properties: &PublishProperties{TopicAlias: Uint16(tt.alias)}},
				Client: c,
			})

			require.EqualError(t, err, tt.err)
			var disconnectErr *handlerDisconnectError
			require.ErrorAs(t, err, &disconnectErr)
			assert.Equal(t, byte(packets.DisconnectTopicAliasInvalid), disconnectErr.Packet.ReasonCode)
			assert.False(t, handled)
			assert.Empty(t, ta.aliases)
		})
	}
}

// TestTopicAliasWithClient confirms that topicAlias works with Client
func TestTopicAliasWithClient(t *testing.T) {
	r := NewStandardRouter()

	c := NewClient(ClientConfig{Router: r})
	c.clientProps.TopicAliasMaximum = 7

	var routed []*Publish
	r.RegisterHandler("test/topic", func(p *Publish) {
		routed = append(routed, p)
	})

	var routeWg sync.WaitGroup
	c.publishPackets = make(chan *packets.Publish, 2)
	routeWg.Add(1)
	go func() {
		defer routeWg.Done()
		c.routePublishPackets()
	}()

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

func TestTopicAliasHandlerOrdering(t *testing.T) {
	var beforeAlias, afterAlias []string
	aliasHandler := newTopicAliasHandler()
	c := NewClient(ClientConfig{
		OnPublishReceived: []func(PublishReceived) (bool, error){
			func(pr PublishReceived) (bool, error) {
				beforeAlias = append(beforeAlias, pr.Packet.Topic)
				return false, nil
			},
			aliasHandler,
			func(pr PublishReceived) (bool, error) {
				afterAlias = append(afterAlias, pr.Packet.Topic)
				return false, nil
			},
		},
	})
	c.clientProps.TopicAliasMaximum = 1
	c.publishPackets = make(chan *packets.Publish, 2)
	c.publishPackets <- &packets.Publish{
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: Uint16(1)},
	}
	c.publishPackets <- &packets.Publish{
		Properties: &packets.Properties{TopicAlias: Uint16(1)},
	}
	close(c.publishPackets)

	c.routePublishPackets()

	assert.Equal(t, []string{"test/topic", ""}, beforeAlias)
	assert.Equal(t, []string{"test/topic", "test/topic"}, afterAlias)
}

func TestTopicAliasHandlerResetsForNewClient(t *testing.T) {
	handler := newTopicAliasHandler()
	c1 := &Client{clientProps: CommsProperties{TopicAliasMaximum: 1}}
	c2 := &Client{clientProps: CommsProperties{TopicAliasMaximum: 1}}

	_, err := handler(PublishReceived{
		Packet: &Publish{Topic: "old/topic", Properties: &PublishProperties{TopicAlias: Uint16(1)}},
		Client: c1,
	})
	require.NoError(t, err)

	_, err = handler(PublishReceived{
		Packet: &Publish{Properties: &PublishProperties{TopicAlias: Uint16(1)}},
		Client: c2,
	})
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, err, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectTopicAliasInvalid), disconnectErr.Packet.ReasonCode)
}

func TestTopicAliasInvalidDisconnectsBeforeHandlers(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	handlerCalled := false
	errorReceived := make(chan error, 1)
	aliasHandler := newTopicAliasHandler()
	c := NewClient(ClientConfig{
		Conn: clientConn,
		OnPublishReceived: []func(PublishReceived) (bool, error){
			aliasHandler,
			func(PublishReceived) (bool, error) {
				handlerCalled = true
				return false, nil
			},
		},
		OnClientError: func(err error) { errorReceived <- err },
	})
	c.clientProps.TopicAliasMaximum = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.done = done
	var cancelOnce sync.Once
	c.cancelFunc = func() {
		cancel()
		cancelOnce.Do(func() { close(done) })
	}
	c.publishPackets = make(chan *packets.Publish, 1)
	c.publishPackets <- &packets.Publish{
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: Uint16(2)},
	}
	close(c.publishPackets)

	routeDone := make(chan struct{})
	go func() {
		c.routePublishPackets()
		close(routeDone)
	}()

	received, err := packets.ReadPacket(serverConn)
	require.NoError(t, err)
	require.Equal(t, byte(packets.DISCONNECT), received.Type)
	disconnect := received.Content.(*packets.Disconnect)
	assert.Equal(t, byte(packets.DisconnectTopicAliasInvalid), disconnect.ReasonCode)

	<-routeDone
	assert.False(t, handlerCalled)
	assert.ErrorContains(t, <-errorReceived, "topic alias 2 exceeds maximum 1")
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
