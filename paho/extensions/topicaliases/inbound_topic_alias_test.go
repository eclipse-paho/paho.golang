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

package topicaliases

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/internal/basictestserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
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

func newConnectedClientForTest(
	t *testing.T,
	topicAliasMaximum uint16,
	handlers []func(paho.PublishReceived) (bool, error),
) (*paho.Client, *basictestserver.TestServer) {
	t.Helper()
	return newConnectedClientWithConfigForTest(t, topicAliasMaximum, paho.ClientConfig{
		OnPublishReceived: handlers,
	})
}

func newConnectedClientWithConfigForTest(
	t *testing.T,
	topicAliasMaximum uint16,
	config paho.ClientConfig,
) (*paho.Client, *basictestserver.TestServer) {
	t.Helper()

	ts := basictestserver.New(log.NOOPLogger{})
	ts.SetResponse(packets.CONNACK, &packets.Connack{
		ReasonCode: packets.ConnackSuccess,
		Properties: &packets.Properties{},
	})
	go ts.Run()

	config.Conn = ts.ClientConn()
	c := paho.NewClient(config)
	_, err := c.Connect(context.Background(), &paho.Connect{
		ClientID:   t.Name(),
		CleanStart: true,
		Properties: &paho.ConnectProperties{TopicAliasMaximum: paho.Uint16(topicAliasMaximum)},
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		select {
		case <-c.Done():
		default:
			_ = c.Disconnect(&paho.Disconnect{})
		}
		ts.Stop()
	})

	return c, ts
}

func TestTopicAliasOnPublishReceived(t *testing.T) {
	ta := newTopicAliasForTest()

	register := &paho.Publish{
		Topic:      "test/topic",
		Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(1)},
	}
	handled, err := ta.OnPublishReceived(paho.PublishReceived{Packet: register})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/topic", ta.aliases[1])

	aliased := &paho.Publish{
		Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(1)},
		Payload:    []byte("aliased payload"),
	}
	handled, err = ta.OnPublishReceived(paho.PublishReceived{Packet: aliased})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/topic", aliased.Topic)
}

func TestTopicAliasOnPublishReceivedNoAlias(t *testing.T) {
	ta := newTopicAliasForTest()
	p := &paho.Publish{
		Topic:      "test/no-alias",
		Properties: &paho.PublishProperties{},
	}

	handled, err := ta.OnPublishReceived(paho.PublishReceived{Packet: p})
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, "test/no-alias", p.Topic)
	assert.Empty(t, ta.aliases)
}

// TestTopicAliasOnPublishReceivedEmptyTopicWithoutAlias must have an alias or a topic
func TestTopicAliasOnPublishReceivedEmptyTopicWithoutAlias(t *testing.T) {
	ta := newTopicAliasForTest()

	handled, err := ta.OnPublishReceived(paho.PublishReceived{
		Packet: &paho.Publish{Properties: &paho.PublishProperties{}},
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
	c, _ := newConnectedClientForTest(t, 100, nil)

	handled, err := ta.OnPublishReceived(paho.PublishReceived{
		Packet: &paho.Publish{Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(99)}}, Client: c,
	})
	require.EqualError(t, err, "topic alias 99 not found")
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, err, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectProtocolError), disconnectErr.Packet.ReasonCode)
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
			c, _ := newConnectedClientForTest(t, tt.max, nil)
			handled, err := ta.OnPublishReceived(paho.PublishReceived{
				Packet: &paho.Publish{Topic: "test/topic", Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(tt.alias)}},
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
	received := make(chan *paho.Publish, 2)
	_, ts := newConnectedClientForTest(t, 7, []func(paho.PublishReceived) (bool, error){
		NewTopicAliasHandler(),
		func(pr paho.PublishReceived) (bool, error) {
			received <- pr.Packet
			return true, nil
		},
	})

	// Set alias
	require.NoError(t, ts.SendPacket(&packets.Publish{
		Payload:    []byte("routed payload"),
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: paho.Uint16(7)},
	}))

	// Use alias
	require.NoError(t, ts.SendPacket(&packets.Publish{
		Payload:    []byte("routed payload 2"),
		Properties: &packets.Properties{TopicAlias: paho.Uint16(7)},
	}))

	first := receivePublish(t, received)
	second := receivePublish(t, received)
	assert.Equal(t, "test/topic", first.Topic)
	assert.Equal(t, []byte("routed payload"), first.Payload)
	assert.Equal(t, "test/topic", second.Topic)
	assert.Equal(t, []byte("routed payload 2"), second.Payload)
}

func receivePublish(t *testing.T, received <-chan *paho.Publish) *paho.Publish {
	t.Helper()
	select {
	case p := <-received:
		return p
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PUBLISH handler")
		return nil
	}
}

func TestTopicAliasHandlerOrdering(t *testing.T) {
	var beforeAlias, afterAlias []string
	aliasHandler := NewTopicAliasHandler()
	processed := make(chan struct{}, 2)
	_, ts := newConnectedClientForTest(t, 1,
		[]func(paho.PublishReceived) (bool, error){
			func(pr paho.PublishReceived) (bool, error) {
				beforeAlias = append(beforeAlias, pr.Packet.Topic)
				return false, nil
			},
			aliasHandler,
			func(pr paho.PublishReceived) (bool, error) {
				afterAlias = append(afterAlias, pr.Packet.Topic)
				processed <- struct{}{}
				return false, nil
			},
		},
	)
	require.NoError(t, ts.SendPacket(&packets.Publish{
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: paho.Uint16(1)},
	}))
	require.NoError(t, ts.SendPacket(&packets.Publish{
		Properties: &packets.Properties{TopicAlias: paho.Uint16(1)},
	}))
	for range 2 {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ordered handlers")
		}
	}

	assert.Equal(t, []string{"test/topic", ""}, beforeAlias)
	assert.Equal(t, []string{"test/topic", "test/topic"}, afterAlias)
}

func TestTopicAliasHandlerResetsForNewClient(t *testing.T) {
	handler := NewTopicAliasHandler()
	c1, _ := newConnectedClientForTest(t, 1, nil)
	c2, _ := newConnectedClientForTest(t, 1, nil)

	_, err := handler(paho.PublishReceived{
		Packet: &paho.Publish{Topic: "old/topic", Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(1)}},
		Client: c1,
	})
	require.NoError(t, err)

	_, err = handler(paho.PublishReceived{
		Packet: &paho.Publish{Properties: &paho.PublishProperties{TopicAlias: paho.Uint16(1)}},
		Client: c2,
	})
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, err, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectProtocolError), disconnectErr.Packet.ReasonCode)
}

func TestTopicAliasInvalidDisconnectsBeforeHandlers(t *testing.T) {
	handlerCalled := false
	errorReceived := make(chan error, 1)
	aliasHandler := NewTopicAliasHandler()
	c, ts := newConnectedClientWithConfigForTest(t, 1, paho.ClientConfig{
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){
			aliasHandler,
			func(paho.PublishReceived) (bool, error) {
				handlerCalled = true
				return false, nil
			},
		},
		OnClientError: func(err error) {
			var disconnectErr *handlerDisconnectError
			if errors.As(err, &disconnectErr) {
				errorReceived <- err
			}
		},
	})

	require.NoError(t, ts.SendPacket(&packets.Publish{
		Topic:      "test/topic",
		Properties: &packets.Properties{TopicAlias: paho.Uint16(2)},
	}))

	var handlerErr error
	select {
	case handlerErr = <-errorReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler error")
	}
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client disconnection")
	}

	assert.False(t, handlerCalled)
	assert.ErrorContains(t, handlerErr, "topic alias 2 exceeds maximum 1")
	var disconnectErr *handlerDisconnectError
	require.ErrorAs(t, handlerErr, &disconnectErr)
	assert.Equal(t, byte(packets.DisconnectTopicAliasInvalid), disconnectErr.Packet.ReasonCode)
}
