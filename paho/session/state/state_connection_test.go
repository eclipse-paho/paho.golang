/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
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

package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/store/memory"
)

// state_connection_test
// `state` manages MQTT sessions. These sessions may extend across many reconnections, so it's important that
// changes to the connection are handled correctly (somewhat difficult due to concurrency).
// The tests in this file replicate various situations where this could be handled badly.

// pausedPublish allows a reconnection to be completed after AddToSession has
// captured the current connection, but before it commits any session state.
type pausedPublish struct {
	packets.Publish
	typeCalled chan struct{}
	resume     chan struct{}
}

// Type is the first function called on the packet in AddToSession so represents a good place to pasue things
func (p *pausedPublish) Type() byte {
	close(p.typeCalled)
	<-p.resume
	return packets.PUBLISH
}

// TestAddToSessionDoesNotCrossConnections confirms correct handling of the sequence:
//  1. Connection up
//  2. Publish added to session
//  3. Concurrently, reconnection completes (no active session)
//  4. Publish processed
//
// In this situation ErrNoConnection should be returned because the initial connection was lost (this leaves it up to
// our user to re-send)
func TestAddToSessionDoesNotCrossConnections(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	connect := &packets.Connect{}
	connack := &packets.Connack{ReasonCode: 0, SessionPresent: false}
	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatalf("initial ConAckReceived: %v", err)
	}

	publish := &pausedPublish{
		Publish: packets.Publish{
			QoS:        1,
			Topic:      "connection/epoch",
			Properties: &packets.Properties{},
		},
		typeCalled: make(chan struct{}),
		resume:     make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.AddToSession(context.Background(), publish)
		result <- err
	}()

	<-publish.typeCalled
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}
	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatalf("second ConAckReceived: %v", err)
	}
	close(publish.resume)

	select {
	case err := <-result:
		if !errors.Is(err, session.ErrNoConnection) {
			t.Fatalf("AddToSession returned %v; want ErrNoConnection", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AddToSession did not return")
	}
	if publish.PacketID != 0 {
		t.Fatalf("AddToSession allocated packet ID %d on the replacement connection", publish.PacketID)
	}
	ids, err := s.clientStore.List()
	if err != nil {
		t.Fatalf("listing client store: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("AddToSession stored packet IDs %v on the replacement connection", ids)
	}
}

// TestPubrecResponseWhileConnectionChanges continually changes the connection used by State whilst simulating the
// receipt of packets. This is intended to be run with the race detector.
func TestPubrecResponseWhileConnectionChanges(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	connect := &packets.Connect{}
	connack := &packets.Connack{ReasonCode: 0, SessionPresent: false}
	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatalf("initial ConAckReceived: %v", err)
	}

	recv := packets.NewControlPacket(packets.PUBREC)
	recv.Content.(*packets.Pubrec).PacketID = 1 // Unknown ID uses the immediate PUBREL response path.

	const rounds = 200
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range rounds {
			if err := s.ConnectionLost(nil); err != nil {
				t.Errorf("ConnectionLost: %v", err)
				return
			}
			if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Errorf("ConAckReceived: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range rounds {
			_ = s.PacketReceived(recv, nil) //pubChan not needed as no PUBLISH sent
		}
	}()
	close(start)
	wg.Wait()
}

// TestConnectionLostRemovesNonSessionTransactions SUBSCRIBE packets are not part of the session state (completed
// subscriptions are, but that's for the broker to handle). This checks that these packets are removed from the state
// following a reconnection.
func TestConnectionLostRemovesNonSessionTransactions(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}

	subscribe := &packets.Subscribe{Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), subscribe)
	if err != nil {
		t.Fatalf("AddToSession: %v", err)
	}
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}

	s.mu.Lock()
	_, exists := s.clientPackets[subscribe.PacketID]
	s.mu.Unlock()
	if exists {
		t.Fatal("SUBSCRIBE packet ID retained after connection loss")
	}
	select {
	case <-response:
	default:
		t.Fatal("SUBSCRIBE waiter was not notified of connection loss")
	}
}

// TestConnectionLostNotifiesCleanedTransactions when a transaction is terminated due to a reconnection, some
// packets may be removed from the state. The user may be waiting on these, so we need to notify them.
func TestConnectionLostNotifiesCleanedTransactions(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}
	publish := &packets.Publish{QoS: 1, Topic: "clean/session", Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), publish)
	if err != nil {
		t.Fatalf("AddToSession: %v", err)
	}
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}

	select {
	case <-response:
	default:
		t.Fatal("PUBLISH waiter was not notified when its session state was deleted")
	}
}

// TestConnectionLostRetainsSessionTransactions confirms that a PUBREC is retained when the connection is lost.
func TestConnectionLostRetainsSessionTransactions(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}

	publish := &packets.Publish{QoS: 2, Topic: "persistent/session", Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), publish)
	if err != nil {
		t.Fatalf("AddToSession: %v", err)
	}
	pubrec := packets.NewControlPacket(packets.PUBREC)
	pubrec.Content.(*packets.Pubrec).PacketID = publish.PacketID
	if err := s.PacketReceived(pubrec, nil); err != nil {
		t.Fatalf("PacketReceived: %v", err)
	}
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}

	s.mu.Lock()
	transaction, exists := s.clientPackets[publish.PacketID]
	s.mu.Unlock()
	if !exists {
		t.Fatal("QoS 2 transaction was removed from a persistent session")
	}
	if transaction.packetType != packets.PUBREL {
		t.Fatalf("retained transaction type is %d; want PUBREL", transaction.packetType)
	}
	select {
	case <-response:
		t.Fatal("persistent QoS 2 waiter was notified before the transaction completed")
	default:
	}
}

type listFailStore struct{ *memory.Store }

func (s *listFailStore) List() ([]uint16, error) {
	return nil, errors.New("injected List failure")
}

type controlledListStore struct {
	*memory.Store
	fail bool
}

func (s *controlledListStore) List() ([]uint16, error) {
	if s.fail {
		return nil, errors.New("injected List failure")
	}
	return s.Store.List()
}

type putFailStore struct{ *memory.Store }

func (s *putFailStore) Put(uint16, byte, io.WriterTo) error {
	return errors.New("injected Put failure")
}

type resetFailStore struct {
	*memory.Store
	resetFailures int
	resetCalls    int
}

func (s *resetFailStore) Reset() error {
	s.resetCalls++
	if s.resetFailures > 0 {
		s.resetFailures--
		return errors.New("injected Reset failure")
	}
	return s.Store.Reset()
}

// TestPendingStoreResetBlocksStaleRetransmission confirms that transactions discarded with a broker session are not
// resurrected if the physical store reset initially fails. Only failed stores should be retried.
func TestPendingStoreResetBlocksStaleRetransmission(t *testing.T) {
	clientStore := &resetFailStore{Store: memory.New()}
	serverStore := &resetFailStore{Store: memory.New()}
	s := New(clientStore, serverStore)
	t.Cleanup(func() { _ = s.Close() })

	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{SessionPresent: true}); err != nil {
		t.Fatalf("initial ConAckReceived: %v", err)
	}
	publish := &packets.Publish{QoS: 1, Topic: "stale/session", Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), publish)
	if err != nil {
		t.Fatalf("AddToSession: %v", err)
	}
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}

	clientStore.resetFailures = 2
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{SessionPresent: false}); err == nil {
		t.Fatal("expected session cleanup to fail")
	}
	if removed, ok := <-response; !ok || removed.Type != 0 {
		t.Fatalf("removed response is type %d, open=%t; want zero value", removed.Type, ok)
	}
	if _, ok := <-response; ok {
		t.Fatal("response channel was not closed after the transaction was removed")
	}
	if clientStore.resetCalls != 1 || serverStore.resetCalls != 1 {
		t.Fatalf("initial reset calls: client=%d server=%d; want 1 each", clientStore.resetCalls, serverStore.resetCalls)
	}

	var retransmitted bytes.Buffer
	if err := s.ConAckReceived(&retransmitted, connect, &packets.Connack{SessionPresent: true}); err == nil {
		t.Fatal("expected connection to fail while client-store cleanup is still pending")
	}
	if retransmitted.Len() != 0 {
		t.Fatalf("wrote %d stale packet bytes while cleanup was pending", retransmitted.Len())
	}
	if clientStore.resetCalls != 2 || serverStore.resetCalls != 1 {
		t.Fatalf("pending reset calls: client=%d server=%d; want client=2 server=1", clientStore.resetCalls, serverStore.resetCalls)
	}

	if err := s.ConAckReceived(&retransmitted, connect, &packets.Connack{SessionPresent: true}); err != nil {
		t.Fatalf("ConAckReceived after cleanup succeeded: %v", err)
	}
	if retransmitted.Len() != 0 {
		t.Fatalf("retransmitted %d stale packet bytes after cleanup", retransmitted.Len())
	}
	if clientStore.resetCalls != 3 || serverStore.resetCalls != 1 {
		t.Fatalf("final reset calls: client=%d server=%d; want client=3 server=1", clientStore.resetCalls, serverStore.resetCalls)
	}
	ids, err := clientStore.List()
	if err != nil {
		t.Fatalf("listing client store: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("client store retained stale packet IDs %v", ids)
	}

	s.mu.Lock()
	clientPending := s.clientStoreResetPending
	serverPending := s.serverStoreResetPending
	s.mu.Unlock()
	if clientPending || serverPending {
		t.Fatalf("reset flags remain pending: client=%t server=%t", clientPending, serverPending)
	}
}

// TestAddToSessionReturnsNilChannelOnStoreFailure confirms that a response channel is only exposed after the packet has
// been fully accepted into the session.
func TestAddToSessionReturnsNilChannelOnStoreFailure(t *testing.T) {
	s := New(&putFailStore{Store: memory.New()}, memory.New())
	t.Cleanup(func() { _ = s.Close() })

	if err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}
	publish := &packets.Publish{QoS: 1, Topic: "store/failure", Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), publish)
	if err == nil {
		t.Fatal("AddToSession succeeded despite the store failure")
	}
	if response != nil {
		t.Fatal("AddToSession returned a response channel after the store failure")
	}
	if publish.PacketID != 0 {
		t.Fatalf("AddToSession left packet ID %d assigned after the store failure", publish.PacketID)
	}
}

// TestConAckFailureRollsBackConnectionState if we are unable to access our persisted store upon receipt of a CONNACK
// then what should we actually do???
func TestConAckFailureRollsBackConnectionState(t *testing.T) {
	s := New(&listFailStore{Store: memory.New()}, memory.New())
	t.Cleanup(func() { _ = s.Close() })

	err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{})
	if err == nil {
		t.Fatal("expected ConAckReceived to fail")
	}

	s.mu.Lock()
	connected := s.conn != nil || s.connCtx != nil || s.connCtxCancel != nil
	s.mu.Unlock()
	if connected {
		t.Fatal("failed ConAckReceived left the session marked connected")
	}

	publish := &packets.Publish{QoS: 1}
	response, err := s.AddToSession(context.Background(), publish)
	if !errors.Is(err, session.ErrNoConnection) {
		t.Fatalf("AddToSession after failed CONNACK returned %v; want ErrNoConnection", err)
	}
	if response != nil {
		t.Fatal("AddToSession returned a response channel after failing")
	}
}

// TestAckAfterFailedConAck confirms that a retained transaction can still be completed after CONNACK processing fails.
// The failed connection's quota is intentionally left available for acknowledgements already read from the connection.
func TestAckAfterFailedConAck(t *testing.T) {
	clientStore := &controlledListStore{Store: memory.New()}
	s := New(clientStore, memory.New())
	t.Cleanup(func() { _ = s.Close() })

	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{SessionPresent: true}); err != nil {
		t.Fatalf("initial ConAckReceived: %v", err)
	}
	publish := &packets.Publish{QoS: 1, Topic: "failed/connack", Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), publish)
	if err != nil {
		t.Fatalf("AddToSessionWithResponse: %v", err)
	}
	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}

	clientStore.fail = true
	if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{SessionPresent: true}); err == nil {
		t.Fatal("expected ConAckReceived to fail")
	}
	clientStore.fail = false

	ack := packets.NewControlPacket(packets.PUBACK)
	ack.Content.(*packets.Puback).PacketID = publish.PacketID
	if err := s.PacketReceived(ack, nil); err != nil {
		t.Fatalf("PacketReceived: %v", err)
	}
	if got, ok := <-response; !ok || got.Type != packets.PUBACK {
		t.Fatalf("response is type %d, open=%t; want PUBACK", got.Type, ok)
	}
	ids, err := clientStore.List()
	if err != nil {
		t.Fatalf("listing client store: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("completed packet remains in client store: %v", ids)
	}
}

// TestPubrecDoesNotAdvanceNonPublishTransaction confirms that a stray PUBREC cannot turn a SUBSCRIBE into a retained
// QoS 2 transaction.
func TestPubrecDoesNotAdvanceNonPublishTransaction(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	var written bytes.Buffer
	if err := s.ConAckReceived(&written, connect, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}
	subscribe := &packets.Subscribe{Properties: &packets.Properties{}}
	response, err := s.AddToSession(context.Background(), subscribe)
	if err != nil {
		t.Fatalf("AddToSessionWithResponse: %v", err)
	}

	pubrec := packets.NewControlPacket(packets.PUBREC)
	pubrec.Content.(*packets.Pubrec).PacketID = subscribe.PacketID
	if err := s.PacketReceived(pubrec, nil); err != nil {
		t.Fatalf("PacketReceived: %v", err)
	}

	s.mu.Lock()
	transaction, exists := s.clientPackets[subscribe.PacketID]
	s.mu.Unlock()
	if !exists || transaction.packetType != packets.SUBSCRIBE {
		t.Fatalf("transaction after PUBREC: exists=%t type=%d; want SUBSCRIBE", exists, transaction.packetType)
	}
	ids, err := s.clientStore.List()
	if err != nil {
		t.Fatalf("listing client store: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("stray PUBREC wrote client-store entries %v", ids)
	}
	writtenPacket, err := packets.ReadPacket(&written)
	if err != nil {
		t.Fatalf("reading PUBREL response: %v", err)
	}
	pubrel, ok := writtenPacket.Content.(*packets.Pubrel)
	if !ok || pubrel.PacketID != subscribe.PacketID || pubrel.ReasonCode != 0x92 {
		t.Fatalf("response is %#v; want PUBREL reason 0x92 for packet %d", writtenPacket.Content, subscribe.PacketID)
	}

	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}
	if got, ok := <-response; !ok || got.Type != 0 {
		t.Fatalf("removed response is type %d, open=%t; want zero value", got.Type, ok)
	}
}

// TestAddToSessionOwnsResponseChannel confirms that every accepted transaction gets a distinct, buffered, one-shot
// response channel owned by State.
func TestAddToSessionOwnsResponseChannel(t *testing.T) {
	s := NewInMemory()
	t.Cleanup(func() { _ = s.Close() })

	if err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}); err != nil {
		t.Fatalf("ConAckReceived: %v", err)
	}
	first := &packets.Subscribe{Properties: &packets.Properties{}}
	firstResponse, err := s.AddToSession(context.Background(), first)
	if err != nil {
		t.Fatalf("first AddToSession: %v", err)
	}
	second := &packets.Subscribe{Properties: &packets.Properties{}}
	secondResponse, err := s.AddToSession(context.Background(), second)
	if err != nil {
		t.Fatalf("second AddToSession: %v", err)
	}
	if firstResponse == secondResponse {
		t.Fatal("AddToSession reused a response channel")
	}
	if cap(firstResponse) != 1 || cap(secondResponse) != 1 {
		t.Fatalf("response channel capacities are %d and %d; want 1", cap(firstResponse), cap(secondResponse))
	}

	ack := packets.NewControlPacket(packets.SUBACK)
	ack.Content.(*packets.Suback).PacketID = first.PacketID
	if err := s.PacketReceived(ack, nil); err != nil {
		t.Fatalf("PacketReceived: %v", err)
	}
	if response, ok := <-firstResponse; !ok || response.Type != packets.SUBACK {
		t.Fatalf("first response is type %d, open=%t; want SUBACK on an open channel", response.Type, ok)
	}
	if _, ok := <-firstResponse; ok {
		t.Fatal("first response channel was not closed after its response")
	}

	if err := s.ConnectionLost(nil); err != nil {
		t.Fatalf("ConnectionLost: %v", err)
	}
	if response, ok := <-secondResponse; !ok || response.Type != 0 {
		t.Fatalf("removed response is type %d, open=%t; want zero value on an open channel", response.Type, ok)
	}
	if _, ok := <-secondResponse; ok {
		t.Fatal("second response channel was not closed after removal")
	}
}

// listFailOnceStore fails on the first attempt to call `List`, subsequent attempts succeed.
type listFailOnceStore struct {
	*memory.Store
	failed bool
}

func (s *listFailOnceStore) List() ([]uint16, error) {
	if !s.failed {
		s.failed = true
		return nil, errors.New("injected first List failure")
	}
	return s.Store.List()
}

// TestConAckRetriesServerStoreAfterFailure checks that intermittent failures of store.List() are handled
func TestConAckRetriesServerStoreAfterFailure(t *testing.T) {
	serverStore := &listFailOnceStore{Store: memory.New()}
	stored := packets.NewControlPacket(packets.PUBREC)
	stored.Content.(*packets.Pubrec).PacketID = 7
	if err := serverStore.Put(7, packets.PUBREC, stored); err != nil {
		t.Fatalf("seeding server store: %v", err)
	}
	s := New(memory.New(), serverStore)
	t.Cleanup(func() { _ = s.Close() })

	connect := &packets.Connect{}
	connack := &packets.Connack{SessionPresent: true}
	if err := s.ConAckReceived(io.Discard, connect, connack); err == nil {
		t.Fatal("expected first ConAckReceived to fail")
	}
	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatalf("second ConAckReceived: %v", err)
	}

	s.mu.Lock()
	packetType, exists := s.serverPackets[7]
	s.mu.Unlock()
	if !exists || packetType != packets.PUBREC {
		t.Fatalf("server session was not reloaded after retry: type=%d exists=%t", packetType, exists)
	}
}
