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

// Command delivery queues QoS 2 publications and waits for their final acknowledgements.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/autopaho/queue/memory"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/delivery"
)

// main publishes one batch to a local broker by default, then exits after confirmation or failure.
func main() {
	broker := flag.String("broker", "mqtt://localhost:1883", "MQTT v5 broker URL")
	topic := flag.String("topic", "paho/examples/delivery", "publication topic")
	count := flag.Int("count", 1000, "number of QoS 2 messages to queue")
	timeout := flag.Duration("timeout", 2*time.Minute, "maximum time to queue and confirm the batch")
	flag.Parse()

	u, err := url.Parse(*broker)
	if err != nil || u.Host == "" {
		log.Fatal("invalid broker URL: ", *broker)
	}
	if *count <= 0 || *timeout <= 0 || *topic == "" {
		log.Fatal("count and timeout must be positive, and topic must not be empty")
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signals, *timeout)
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		KeepAlive:                     20,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         600,
		Queue:                         memory.New(),
		OnConnectionUp: func(_ *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Print("connected")
		},
		OnConnectError: func(err error) { log.Printf("connection attempt failed: %v", err) },
		ClientConfig: paho.ClientConfig{
			ClientID:      "paho-delivery-" + rand.Text(),
			OnClientError: func(err error) { log.Printf("connection interrupted: %v", err) },
		},
	}
	err = publishAndWait(ctx, cfg, *topic, *count)
	cancel()
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
	log.Printf("delivery confirmed for all %d messages", *count)
}

// publishAndWait connects to the broker, transmits messages, and returns when all messages have been delivered (or an
// error occurs)
func publishAndWait(ctx context.Context, cfg autopaho.ClientConfig, topic string, count int) error {
	// Open a connection to the broker (and ensure it shuts down before we return)
	tracker := newBatchTracker()
	cfg.PublishEvents = tracker.delivery.Handler()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cm, err := autopaho.NewConnection(runCtx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		<-cm.Done() // Shut down the connection and queue workers before closing the session.
	}()

	// No AwaitConnection is needed: PublishViaQueue also works while disconnected.
	for i := range count {
		select {
		case err := <-tracker.result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		p := &paho.Publish{
			QoS: 2, Topic: topic, Payload: []byte(fmt.Sprintf("message %d", i+1)),
		}
		// Allocate and register this message's ID before the queue can transmit it.
		id, err := tracker.add(p)
		if err != nil {
			return err
		}

		// We publish via the queue, meaning the message will be sent when possible (call will return before this happens)
		if err := cm.PublishViaQueue(ctx, &autopaho.QueuePublish{Publish: p}); err != nil {
			tracker.delivery.ReportNotSent(p, err)
			return fmt.Errorf("queueing %s: %w", id, err)
		}
	}

	// At this point all messages are queued (some may have been delivered). We wait for the delivery extension to
	// report completion.
	tracker.doneAdding()
	log.Printf("queued %d messages; waiting for delivery confirmations", count)
	// An empty queue only means packets reached the client/session, not that the broker acknowledged them.
	select {
	case err := <-tracker.result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("waiting for delivery confirmations: %w", ctx.Err())
	}
}

// batchTracker waits for one batch of QoS 2 results from the delivery extension.
// Its buffered result lets callbacks report completion without waiting for the publishing goroutine.
type batchTracker struct {
	delivery *delivery.Tracker   // instance of the delivery extension
	mu       sync.Mutex          // protects the below
	pending  map[string]struct{} // records messages we are tracking (removed when PUBCOMP received)
	result   chan error          // A single message will be delivered to this channel when the tracking is done
	sealed   bool                // set to true when the tracker is no longer accepting messages
	finished bool                // set to true when the tracker has finished processing all messages
}

// newBatchTracker accepts messages as they are produced, without knowing their count or IDs.
func newBatchTracker() *batchTracker {
	t := &batchTracker{pending: make(map[string]struct{}), result: make(chan error, 1)}
	t.delivery = delivery.New(delivery.Config{OnResult: t.onResult, OnError: t.onError})
	return t
}

// add asks the extension to assign an ID and records it before the caller queues p.
// A queueing failure must abort the batch, because this ID would otherwise remain pending.
func (t *batchTracker) add(p *paho.Publish) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed || t.finished {
		return "", fmt.Errorf("delivery tracker is no longer accepting messages")
	}
	id, err := t.delivery.Track(p)
	if err != nil {
		return "", err
	}
	t.pending[id] = struct{}{}
	return id, nil
}

// doneAdding closes registration. Success requires both this call and confirmation
// of every registered message, even if acknowledgements arrive between additions.
func (t *batchTracker) doneAdding() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sealed = true
	if !t.finished && len(t.pending) == 0 {
		t.finish(nil)
	}
}

// onResult will be called by the delivery extension with confirmed QoS 2 deliveries
func (t *batchTracker) onResult(r delivery.Result) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if _, waiting := t.pending[r.ID]; !waiting {
		return // Ignore repeated confirmations and results outside this batch.
	}
	if r.Outcome != delivery.Acknowledged {
		t.finish(fmt.Errorf("publication %s has outcome %d: %v", r.ID, r.Outcome, r.Err))
		return
	}
	delete(t.pending, r.ID)
	if t.sealed && len(t.pending) == 0 {
		t.finish(nil)
	}
}

// onError will be called by the delivery extension when something goes wrong
func (t *batchTracker) onError(p delivery.Problem) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		t.finish(fmt.Errorf("tracking publication %s (packet %d): %w", p.ID, p.PacketID, p.Err))
	}
}

// finish publishes exactly one outcome; the caller holds t.mu.
func (t *batchTracker) finish(err error) {
	t.finished = true
	t.result <- err
}
