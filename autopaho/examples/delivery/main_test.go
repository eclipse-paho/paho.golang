/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * All rights reserved. This program and the accompanying materials are made
 * available under the Eclipse Public License v2.0 and Eclipse Distribution
 * License v1.0 which accompany this distribution.
 * https://www.eclipse.org/legal/epl-2.0/
 * http://www.eclipse.org/org/documents/edl-v10.php
 *
 * AI Disclosure: This file was largely generated with OpenAI Codex (GPT-6).
 * The AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/delivery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishAndWait confirms all 1,000 queued QoS 2 messages and verifies that an
// empty queue and the last PUBREL are insufficient until the final PUBCOMP arrives.
func TestPublishAndWait(t *testing.T) {
	const count = 1000
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	server := testserver.New(nil)
	var publishes, releases atomic.Int32
	lastRelease, allowCompletion := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(allowCompletion) }) }
	defer unblock()
	server.SetPacketReceivedCallback(func(cp *packets.ControlPacket) error {
		var props *packets.Properties
		switch p := cp.Content.(type) {
		case *packets.Publish:
			assert.Equal(t, byte(2), p.QoS)
			assert.Equal(t, "example/test", p.Topic)
			publishes.Add(1)
			props = p.Properties
		case *packets.Pubrel:
			props = p.Properties
			if releases.Add(1) == count {
				close(lastRelease)
				select {
				case <-allowCompletion:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if props != nil {
			for _, u := range props.User {
				assert.NotEqual(t, delivery.PropertyKey, u.Key, "local tracking ID must not reach the broker")
			}
		}
		return nil
	})
	serverDone := make(chan chan struct{}, 1)
	q := memory.New()
	cfg := autopaho.ClientConfig{
		ServerUrls:            []*url.URL{{Scheme: "mqtt", Host: "test.invalid:1883"}},
		Queue:                 q,
		SessionExpiryInterval: 600,
		ClientConfig:          paho.ClientConfig{ClientID: "delivery-test"},
		AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, _ *url.URL) (net.Conn, error) {
			conn, done, err := server.Connect(ctx)
			if err == nil {
				select {
				case serverDone <- done:
				case <-ctx.Done():
					_ = conn.Close()
					return nil, ctx.Err()
				}
			}
			return conn, err
		},
	}
	result := make(chan error, 1)
	go func() { result <- publishAndWait(ctx, cfg, "example/test", count) }()
	t.Cleanup(func() {
		unblock()
		cancel()
		select {
		case done := <-serverDone:
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("test broker did not stop")
			}
		default:
		}
	})
	select {
	case <-lastRelease:
	case err := <-result:
		t.Fatalf("publisher stopped before the last PUBREL: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-q.WaitForEmpty():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-result:
		t.Fatalf("publisher exited before final PUBCOMP: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assert.Equal(t, int32(count), publishes.Load())
	assert.Equal(t, int32(count), releases.Load())
}

// completedResult constructs a successful outcome from the delivery extension.
func completedResult(id string) delivery.Result {
	return delivery.Result{ID: id, QoS: 2, Outcome: delivery.Acknowledged}
}

// TestTrackerWaitsForDistinctFinalAcknowledgements ensures duplicate
// notifications cannot make the example report delivery before every ID completes.
func TestTrackerWaitsForDistinctFinalAcknowledgements(t *testing.T) {
	tracker := newBatchTracker()
	one, err := tracker.add(&paho.Publish{QoS: 2})
	require.NoError(t, err)
	two, err := tracker.add(&paho.Publish{QoS: 2})
	require.NoError(t, err)
	require.NotEqual(t, one, two)
	tracker.doneAdding()
	tracker.onResult(completedResult(one))
	tracker.onResult(completedResult(one))
	select {
	case err := <-tracker.result:
		t.Fatalf("premature completion: %v", err)
	default:
	}
	tracker.onResult(completedResult(two))
	require.NoError(t, <-tracker.result)
	tracker.onResult(completedResult(two))
	assert.Empty(t, tracker.result, "only one result should be emitted")
}

// TestTrackerAcceptsMessagesAfterEarlyAcknowledgement verifies IDs are assigned as
// messages arrive and an empty pending set does not finish an open batch.
func TestTrackerAcceptsMessagesAfterEarlyAcknowledgement(t *testing.T) {
	tracker := newBatchTracker()
	props := &paho.PublishProperties{ContentType: "text/plain", User: paho.UserProperties{
		{Key: "public", Value: "keep"},
	}}
	first := &paho.Publish{QoS: 2, Properties: props}
	one, err := tracker.add(first)
	require.NoError(t, err)
	require.NotEmpty(t, one)
	assert.Equal(t, []string{one}, first.Properties.User.GetAll(delivery.PropertyKey))
	assert.Equal(t, "keep", first.Properties.User.Get("public"))
	assert.Equal(t, "text/plain", first.Properties.ContentType)
	assert.Empty(t, props.User.Get(delivery.PropertyKey), "original properties must remain unchanged")
	tracker.onResult(completedResult(one))
	assert.Empty(t, tracker.result, "producer may still add messages")

	second := &paho.Publish{QoS: 2}
	two, err := tracker.add(second)
	require.NoError(t, err)
	require.NotEqual(t, one, two)
	assert.Equal(t, two, second.Properties.User.Get(delivery.PropertyKey))
	tracker.onResult(completedResult(two))
	assert.Empty(t, tracker.result, "success still requires doneAdding")
	tracker.doneAdding()
	require.NoError(t, <-tracker.result)
	tracker.doneAdding()
	assert.Empty(t, tracker.result, "repeated doneAdding must not emit another result")
	_, err = tracker.add(&paho.Publish{QoS: 2})
	require.Error(t, err, "completed batches must reject further registration")
}

// TestTrackerFailures verifies that the example fails a batch on unknown or
// rejected delivery, local submission failure, or a tracking problem.
func TestTrackerFailures(t *testing.T) {
	for _, outcome := range []delivery.Outcome{delivery.Unknown, delivery.Rejected, delivery.NotSent} {
		tracker := newBatchTracker()
		id, err := tracker.add(&paho.Publish{QoS: 2})
		require.NoError(t, err)
		tracker.onResult(delivery.Result{ID: id, QoS: 2, Outcome: outcome, Err: errors.New("failed")})
		require.Error(t, <-tracker.result)
		tracker.onResult(completedResult(id))
		tracker.doneAdding()
		assert.Empty(t, tracker.result, "failure must not be followed by success")
	}
	tracker := newBatchTracker()
	tracker.onError(delivery.Problem{Err: errors.New("store read failed")})
	require.Error(t, <-tracker.result)
}

// TestPublishAndWaitCancellation verifies that a disconnected publisher exits when
// cancelled rather than waiting indefinitely for queued messages to complete.
func TestPublishAndWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempting := make(chan struct{})
	cfg := autopaho.ClientConfig{
		ServerUrls:   []*url.URL{{Scheme: "mqtt", Host: "test.invalid:1883"}},
		ClientConfig: paho.ClientConfig{ClientID: "cancel-test"},
		AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, _ *url.URL) (net.Conn, error) {
			close(attempting)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	done := make(chan error, 1)
	defer cancel()
	go func() { done <- publishAndWait(ctx, cfg, "example/test", 10) }()
	select {
	case <-attempting:
	case <-time.After(time.Second):
		t.Fatal("connection attempt did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("publisher did not stop after cancellation")
	}
}
