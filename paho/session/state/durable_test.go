/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was largely generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package state

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session"
	storefile "github.com/eclipse/paho.golang/paho/store/file"
	"github.com/eclipse/paho.golang/paho/store/memory"
)

type persistenceErrorStore struct {
	*memory.Store
	putErr, deleteErr, getErr, quarantineErr, listErr, resetErr error
	applyMutationOnError                                        bool
	putCalls                                                    int
}

func (s *persistenceErrorStore) Put(id uint16, kind byte, w io.WriterTo) error {
	s.putCalls++
	if s.putErr != nil && !s.applyMutationOnError {
		return s.putErr
	}
	if err := s.Store.Put(id, kind, w); err != nil {
		return err
	}
	return s.putErr
}

func (s *persistenceErrorStore) Delete(id uint16) error {
	if s.deleteErr != nil && !s.applyMutationOnError {
		return s.deleteErr
	}
	if err := s.Store.Delete(id); err != nil {
		return err
	}
	return s.deleteErr
}

func (s *persistenceErrorStore) Get(id uint16) (io.ReadCloser, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(id)
}

func (s *persistenceErrorStore) Quarantine(id uint16) error {
	if s.quarantineErr != nil {
		return s.quarantineErr
	}
	return s.Store.Quarantine(id)
}

func (s *persistenceErrorStore) List() ([]uint16, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List()
}

func (s *persistenceErrorStore) Reset() error {
	if s.resetErr != nil && !s.applyMutationOnError {
		return s.resetErr
	}
	if err := s.Store.Reset(); err != nil {
		return err
	}
	return s.resetErr
}

type persistenceCheckingWriter func([]byte) (int, error)

func (w persistenceCheckingWriter) Write(b []byte) (int, error) { return w(b) }

// TestInboundQoS2PersistsBeforeAcknowledging checks that PUBREC waits for stored
// state and PUBCOMP waits for its deletion. Persistence failures must neither send
// the acknowledgement nor advance the in-memory transaction.
func TestInboundQoS2PersistsBeforeAcknowledging(t *testing.T) {
	store := &persistenceErrorStore{Store: memory.New()}
	s := New(memory.New(), store)
	t.Cleanup(func() { _ = s.Close() })
	var wire bytes.Buffer
	var wantStored bool
	writer := persistenceCheckingWriter(func(b []byte) (int, error) {
		ids, err := store.List()
		if err != nil || (len(ids) != 0) != wantStored {
			t.Fatalf("acknowledged before storing state: IDs=%v, error=%v", ids, err)
		}
		return wire.Write(b)
	})
	if err := s.ConAckReceived(writer, &packets.Connect{}, &packets.Connack{}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("persistence failed")
	store.putErr = failure
	pb := &packets.Publish{PacketID: 7, QoS: 2, Topic: "durable/inbound"}
	if err := s.Ack(pb); !errors.Is(err, failure) {
		t.Fatalf("ignored PUBREC persistence error: %v", err)
	}
	if wire.Len() != 0 || len(s.serverPackets) != 0 {
		t.Fatal("failed PUBREC persistence sent an acknowledgement or advanced state")
	}
	store.putErr = nil
	wantStored = true
	if err := s.Ack(pb); err != nil {
		t.Fatal(err)
	}
	if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBREC {
		t.Fatalf("expected PUBREC: %v, %v", cp, err)
	}
	store.deleteErr = failure
	pubrel := packets.NewControlPacket(packets.PUBREL)
	pubrel.Content.(*packets.Pubrel).PacketID = 7
	if err := s.PacketReceived(pubrel, nil); !errors.Is(err, failure) {
		t.Fatalf("ignored PUBCOMP persistence error: %v", err)
	}
	if wire.Len() != 0 || s.serverPackets[7] != packets.PUBREC {
		t.Fatal("failed deletion sent PUBCOMP or forgot the inbound transaction")
	}
	store.deleteErr = nil
	wantStored = false
	if err := s.PacketReceived(pubrel, nil); err != nil {
		t.Fatal(err)
	}
	if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBCOMP {
		t.Fatalf("expected PUBCOMP: %v, %v", cp, err)
	}
	// A repeated PUBREL after successful deletion still needs PUBCOMP.
	if err := s.PacketReceived(pubrel, nil); err != nil {
		t.Fatal(err)
	}
	if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBCOMP {
		t.Fatalf("unknown PUBREL was not acknowledged: %v, %v", cp, err)
	}
}

// TestInboundQoS2DuplicateDoesNotRewriteState checks that a persisted PUBREC can
// still be resent after reconnect or recovery when the store cannot accept new
// writes. The duplicate must not be delivered again, and PUBREL must be able to
// complete the exchange and release the stored state.
func TestInboundQoS2DuplicateDoesNotRewriteState(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			store := &persistenceErrorStore{Store: memory.New()}
			s := New(memory.New(), store)
			t.Cleanup(func() { _ = s.Close() })
			expiry := uint32(600)
			connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
			connack := &packets.Connack{SessionPresent: true}
			if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Fatal(err)
			}
			pub := &packets.Publish{PacketID: 7, QoS: 2, Topic: "durable/inbound"}
			if err := s.Ack(pub); err != nil {
				t.Fatal(err)
			}
			if err := s.ConnectionLost(nil); err != nil {
				t.Fatal(err)
			}
			if restart {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = New(memory.New(), store)
			}
			failure := errors.New("no space for new writes")
			store.putErr = failure
			var wire bytes.Buffer
			if err := s.ConAckReceived(&wire, connect, connack); err != nil {
				t.Fatal(err)
			}
			pub.Duplicate = true
			delivered := make(chan *packets.Publish, 1)
			if err := s.PacketReceived(pub.ToControlPacket(), delivered); err != nil {
				t.Fatalf("persisted PUBREC could not be resent: %v", err)
			}
			if len(delivered) != 0 {
				t.Fatal("redelivered an acknowledged QoS 2 message")
			}
			if store.putCalls != 1 {
				t.Fatalf("rewrote persisted PUBREC: Put called %d times", store.putCalls)
			}
			if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBREC || cp.PacketID() != pub.PacketID {
				t.Fatalf("expected repeated PUBREC: %v, %v", cp, err)
			}
			pubrel := packets.NewControlPacket(packets.PUBREL)
			pubrel.Content.(*packets.Pubrel).PacketID = pub.PacketID
			if err := s.PacketReceived(pubrel, nil); err != nil {
				t.Fatal(err)
			}
			if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBCOMP || cp.PacketID() != pub.PacketID {
				t.Fatalf("expected PUBCOMP: %v, %v", cp, err)
			}
			if ids, err := store.List(); err != nil || len(ids) != 0 {
				t.Fatalf("completed transaction retained stored state: %v, %v", ids, err)
			}

			// Once the exchange completes, even a PUBLISH with DUP set must be
			// treated as new and persisted before it can be acknowledged.
			if err := s.PacketReceived(pub.ToControlPacket(), delivered); err != nil {
				t.Fatal(err)
			}
			if len(delivered) != 1 {
				t.Fatal("suppressed a new message that reused a completed packet ID")
			}
			if err := s.Ack(<-delivered); !errors.Is(err, failure) {
				t.Fatalf("new transaction bypassed persistence: %v", err)
			}
			if wire.Len() != 0 {
				t.Fatal("acknowledged a new transaction before persisting its PUBREC")
			}
		})
	}
}

// TestInboundMutationErrorReloadsSession checks recovery with a store that can
// resume operations after an error. Since the mutation may already have happened,
// reconnect must recover the actual inventory rather than reuse cached state.
func TestInboundMutationErrorReloadsSession(t *testing.T) {
	for _, ack := range []byte{packets.PUBREC, packets.PUBCOMP} {
		t.Run(packets.NewControlPacket(ack).PacketType(), func(t *testing.T) {
			store := &persistenceErrorStore{Store: memory.New(), applyMutationOnError: true}
			s := New(memory.New(), store)
			t.Cleanup(func() { _ = s.Close() })
			expiry := uint32(600)
			connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
			connack := &packets.Connack{SessionPresent: true}
			if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Fatal(err)
			}
			pub := &packets.Publish{PacketID: 7, QoS: 2, Topic: "recovered/inbound"}
			pubrel := packets.NewControlPacket(packets.PUBREL)
			pubrel.Content.(*packets.Pubrel).PacketID = pub.PacketID
			failure := errors.New("mutation applied before failure")
			var err error
			if ack == packets.PUBREC {
				store.putErr = failure
				err = s.Ack(pub)
			} else {
				if err := s.Ack(pub); err != nil {
					t.Fatal(err)
				}
				store.deleteErr = failure
				err = s.PacketReceived(pubrel, nil)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("mutation error was lost: %v", err)
			}
			if err := s.ConnectionLost(nil); err != nil {
				t.Fatal(err)
			}
			store.putErr, store.deleteErr = nil, nil
			var wire bytes.Buffer
			if err := s.ConAckReceived(&wire, connect, connack); err != nil {
				t.Fatalf("recovered store could not reconnect: %v", err)
			}
			if ack == packets.PUBREC {
				pub.Duplicate = true
				delivered := make(chan *packets.Publish, 1)
				if err := s.PacketReceived(pub.ToControlPacket(), delivered); err != nil {
					t.Fatal(err)
				}
				if len(delivered) != 0 {
					t.Fatal("did not reload persisted PUBREC; redelivered the message")
				}
			} else if err := s.PacketReceived(pubrel, nil); err != nil {
				t.Fatalf("did not reload completed transaction: %v", err)
			}
			if got, err := packets.ReadPacket(&wire); err != nil || got.Type != ack || got.PacketID() != pub.PacketID {
				t.Fatalf("unexpected recovered acknowledgement: %v, %v", got, err)
			}
		})
	}
}

// TestFinalAcknowledgementWaitsForStoreDeletion checks that PUBACK, PUBCOMP, and
// negative PUBREC processing retain the packet ID and withhold completion until
// deleting the persisted packet succeeds.
func TestFinalAcknowledgementWaitsForStoreDeletion(t *testing.T) {
	for _, kind := range []byte{packets.PUBACK, packets.PUBCOMP, packets.PUBREC} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			store := &persistenceErrorStore{Store: memory.New()}
			s := New(store, memory.New())
			t.Cleanup(func() { _ = s.Close() })
			expiry := uint32(600)
			connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
			connack := &packets.Connack{SessionPresent: true}
			if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Fatal(err)
			}
			qos := byte(2)
			if kind == packets.PUBACK {
				qos = 1
			}
			pb := &packets.Publish{QoS: qos, Topic: "durable/outbound"}
			response, err := s.AddToSession(t.Context(), pb)
			if err != nil {
				t.Fatal(err)
			}
			if kind == packets.PUBCOMP {
				if err := s.PacketReceived((&packets.Pubrec{PacketID: pb.PacketID}).ToControlPacket(), nil); err != nil {
					t.Fatal(err)
				}
			}
			ack := packets.NewControlPacket(kind)
			switch p := ack.Content.(type) {
			case *packets.Puback:
				p.PacketID = pb.PacketID
			case *packets.Pubcomp:
				p.PacketID = pb.PacketID
			case *packets.Pubrec:
				p.PacketID, p.ReasonCode = pb.PacketID, 0x80
			}
			failure := errors.New("delete failed")
			store.deleteErr = failure
			if err := s.PacketReceived(ack, nil); !errors.Is(err, failure) {
				t.Fatalf("ignored deletion failure: %v", err)
			}
			select {
			case <-response:
				t.Fatal("reported completion before stored state was removed")
			default:
			}
			if _, ok := s.clientPackets[pb.PacketID]; !ok {
				t.Fatal("released packet identifier after deletion failure")
			}
			if err := s.ConnectionLost(nil); err != nil {
				t.Fatal(err)
			}
			var replay bytes.Buffer
			if err := s.ConAckReceived(&replay, connect, connack); !errors.Is(err, failure) {
				t.Fatalf("recovery ignored deletion failure: %v", err)
			}
			if replay.Len() != 0 {
				t.Fatal("retransmitted an acknowledged packet while deletion was pending")
			}
			select {
			case <-response:
				t.Fatal("recovery completed a transaction still in the store")
			default:
			}
			if _, ok := s.clientPackets[pb.PacketID]; !ok {
				t.Fatal("recovery released an identifier still in the store")
			}
			if _, err := s.AddToSession(t.Context(), &packets.Publish{QoS: 1}); !errors.Is(err, session.ErrNoConnection) {
				t.Fatalf("accepted a new publication while deletion was pending: %v", err)
			}
			store.deleteErr = nil
			if err := s.ConAckReceived(&replay, connect, connack); err != nil {
				t.Fatal(err)
			}
			if replay.Len() != 0 {
				t.Fatal("retransmitted an acknowledged packet after deletion succeeded")
			}
			select {
			case got, open := <-response:
				if !open || !reflect.DeepEqual(got, *ack) {
					t.Fatalf("final acknowledgement was not preserved: %#v, open=%t", got, open)
				}
			default:
				t.Fatal("successful deletion did not complete the transaction")
			}
			if _, open := <-response; open {
				t.Fatal("completed response channel was not closed")
			}
			if _, ok := s.clientPackets[pb.PacketID]; ok {
				t.Fatal("successful deletion retained packet identifier")
			}
			if ids, err := store.List(); err != nil || len(ids) != 0 {
				t.Fatalf("acknowledged packet remains in the store: %v, %v", ids, err)
			}
			if s.inflight.quota != s.inflight.initialQuota {
				t.Fatalf("acknowledged packet consumed quota after recovery: %d", s.inflight.quota)
			}
		})
	}
}

// TestFinalAcknowledgementSurvivesSessionReset covers both automatic cleanup on
// disconnect and a broker that has discarded a persistent session. A failed
// reset must keep the original acknowledgement until cleanup can be retried.
func TestFinalAcknowledgementSurvivesSessionReset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiry    uint32
		failReset bool
	}{
		{name: "session expires"},
		{name: "session expires with reset failure", failReset: true},
		{name: "broker lost session", expiry: 600},
		{name: "broker lost session with reset failure", expiry: 600, failReset: true},
	} {
		for _, kind := range []byte{packets.PUBACK, packets.PUBCOMP, packets.PUBREC} {
			t.Run(tc.name+"/"+packets.NewControlPacket(kind).PacketType(), func(t *testing.T) {
				store := &persistenceErrorStore{Store: memory.New()}
				s := New(store, memory.New())
				t.Cleanup(func() { _ = s.Close() })
				connect := &packets.Connect{}
				if tc.expiry != 0 {
					connect.Properties = &packets.Properties{SessionExpiryInterval: &tc.expiry}
				}
				if err := s.ConAckReceived(io.Discard, connect, &packets.Connack{}); err != nil {
					t.Fatal(err)
				}
				qos := byte(2)
				if kind == packets.PUBACK {
					qos = 1
				}
				publish := &packets.Publish{QoS: qos, Topic: "cleanup/acknowledged"}
				response, err := s.AddToSession(t.Context(), publish)
				if err != nil {
					t.Fatal(err)
				}
				if kind == packets.PUBCOMP {
					if err := s.PacketReceived((&packets.Pubrec{PacketID: publish.PacketID}).ToControlPacket(), nil); err != nil {
						t.Fatal(err)
					}
				}
				unacknowledged, err := s.AddToSession(t.Context(), &packets.Publish{QoS: 1, Topic: "cleanup/unacknowledged"})
				if err != nil {
					t.Fatal(err)
				}
				ack := packets.NewControlPacket(kind)
				switch packet := ack.Content.(type) {
				case *packets.Puback:
					packet.PacketID = publish.PacketID
				case *packets.Pubcomp:
					packet.PacketID = publish.PacketID
				case *packets.Pubrec:
					packet.PacketID, packet.ReasonCode = publish.PacketID, 0x87
				}
				store.deleteErr = errors.New("delete failed")
				if err := s.PacketReceived(ack, nil); !errors.Is(err, store.deleteErr) {
					t.Fatalf("ignored deletion failure: %v", err)
				}
				if tc.failReset {
					store.resetErr = errors.New("reset failed")
				}
				var replay bytes.Buffer
				err = s.ConnectionLost(nil)
				if tc.expiry != 0 {
					if err != nil {
						t.Fatal(err)
					}
					err = s.ConAckReceived(&replay, connect, &packets.Connack{SessionPresent: false})
				}
				if !errors.Is(err, store.resetErr) {
					t.Fatalf("cleanup returned %v, want %v", err, store.resetErr)
				}
				select {
				case got, open := <-unacknowledged:
					if !open || got.Type != 0 {
						t.Fatalf("unacknowledged publication was not discarded: %#v, open=%t", got, open)
					}
				default:
					t.Fatal("cleanup left an unacknowledged publication waiting")
				}
				if tc.failReset {
					select {
					case <-response:
						t.Fatal("reported completion before store cleanup succeeded")
					default:
					}
					if _, retained := s.clientPackets[publish.PacketID]; !retained {
						t.Fatal("released acknowledged packet identifier before store cleanup")
					}
					store.resetErr = nil
					if err := s.ConAckReceived(&replay, connect, &packets.Connack{SessionPresent: true}); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case got, open := <-response:
					if !open || !reflect.DeepEqual(got, *ack) {
						t.Fatalf("final acknowledgement was not preserved: %#v, open=%t", got, open)
					}
				default:
					t.Fatal("cleanup left the acknowledged publication waiting")
				}
				if _, open := <-response; open {
					t.Fatal("completed response channel was not closed")
				}
				if len(s.clientPackets) != 0 || replay.Len() != 0 {
					t.Fatal("retained or retransmitted packets from the discarded session")
				}
				if ids, err := store.List(); err != nil || len(ids) != 0 {
					t.Fatalf("cleanup did not clear the store: %v, %v", ids, err)
				}
			})
		}
	}
}

func TestFinalAcknowledgementRecoversAfterAppliedDeletionError(t *testing.T) {
	for _, kind := range []byte{packets.PUBACK, packets.PUBCOMP, packets.PUBREC} {
		t.Run(packets.NewControlPacket(kind).PacketType(), func(t *testing.T) {
			store := &persistenceErrorStore{Store: memory.New(), applyMutationOnError: true}
			sessionState := New(store, memory.New())
			t.Cleanup(func() { _ = sessionState.Close() })
			expiry := uint32(600)
			receiveMaximum := uint16(2)
			connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
			connack := &packets.Connack{SessionPresent: true, Properties: &packets.Properties{ReceiveMaximum: &receiveMaximum}}
			if err := sessionState.ConAckReceived(io.Discard, connect, connack); err != nil {
				t.Fatal(err)
			}
			qos := byte(2)
			if kind == packets.PUBACK {
				qos = 1
			}
			publish := &packets.Publish{QoS: qos, Topic: "recovered/outbound"}
			response, err := sessionState.AddToSession(t.Context(), publish)
			if err != nil {
				t.Fatal(err)
			}
			if kind == packets.PUBCOMP {
				if err := sessionState.PacketReceived((&packets.Pubrec{PacketID: publish.PacketID}).ToControlPacket(), nil); err != nil {
					t.Fatal(err)
				}
			}
			pending := &packets.Publish{QoS: 1, Topic: "recovered/pending"}
			pendingResponse, err := sessionState.AddToSession(t.Context(), pending)
			if err != nil {
				t.Fatal(err)
			}
			ack := packets.NewControlPacket(kind)
			switch packet := ack.Content.(type) {
			case *packets.Puback:
				packet.PacketID = publish.PacketID
			case *packets.Pubcomp:
				packet.PacketID = publish.PacketID
			case *packets.Pubrec:
				packet.PacketID, packet.ReasonCode = publish.PacketID, 0x80
			}
			failure := errors.New("delete applied before error")
			store.deleteErr = failure
			if err := sessionState.PacketReceived(ack, nil); !errors.Is(err, failure) {
				t.Fatalf("deletion error was lost: %v", err)
			}
			if err := sessionState.ConnectionLost(nil); err != nil {
				t.Fatal(err)
			}
			store.deleteErr = nil
			store.listErr = errors.New("inventory unavailable")
			if err := sessionState.ConAckReceived(io.Discard, connect, connack); !errors.Is(err, store.listErr) {
				t.Fatalf("recovery ignored inventory failure: %v", err)
			}
			select {
			case <-response:
				t.Fatal("reported completion without recovering the inventory")
			default:
			}
			if _, retained := sessionState.clientPackets[publish.PacketID]; !retained {
				t.Fatal("released packet identifier before recovery")
			}
			store.listErr = nil
			var replay bytes.Buffer
			if err := sessionState.ConAckReceived(&replay, connect, connack); err != nil {
				t.Fatal(err)
			}
			if packet, err := packets.ReadPacket(&replay); err != nil || packet.PacketID() != pending.PacketID || replay.Len() != 0 {
				t.Fatalf("expected only the pending publication to be retransmitted: %v, %v", packet, err)
			}
			select {
			case completed, open := <-response:
				if !open || !reflect.DeepEqual(completed, *ack) {
					t.Fatalf("final acknowledgement was not preserved: %#v, open=%t", completed, open)
				}
			default:
				t.Fatal("recovery left the completed publication waiting")
			}
			if _, open := <-response; open {
				t.Fatal("completed response channel was not closed")
			}
			if _, retained := sessionState.clientPackets[publish.PacketID]; retained {
				t.Fatal("recovery retained a completed packet identifier")
			}
			select {
			case <-pendingResponse:
				t.Fatal("recovery completed an unacknowledged publication")
			default:
			}
			if sessionState.inflight.quota != 1 {
				t.Fatalf("recovered quota=%d, want 1", sessionState.inflight.quota)
			}
			sessionState.lastMid = publish.PacketID - 1
			replacement := &packets.Publish{QoS: 1, Topic: "recovered/reused"}
			if _, err := sessionState.AddToSession(t.Context(), replacement); err != nil {
				t.Fatal(err)
			}
			if replacement.PacketID != publish.PacketID {
				t.Fatalf("allocated packet ID %d instead of recovered ID %d", replacement.PacketID, publish.PacketID)
			}
		})
	}
}

// TestSessionRecoveryStopsOnStoreFailure checks that client and server recovery
// propagate read or quarantine failures and leave the session disconnected so
// packet IDs cannot be allocated from an incompletely recovered inventory.
func TestSessionRecoveryStopsOnStoreFailure(t *testing.T) {
	for _, direction := range []string{"client", "server"} {
		for _, operation := range []string{"get", "quarantine unreadable", "quarantine wrong type"} {
			t.Run(direction+"/"+operation, func(t *testing.T) {
				store := &persistenceErrorStore{Store: memory.New()}
				failure := errors.New("recovery storage failure")
				var packet io.WriterTo = strings.NewReader("")
				if operation == "get" {
					store.getErr = failure
				} else {
					store.quarantineErr = failure
					if operation == "quarantine wrong type" {
						packet = packets.NewControlPacket(packets.PINGREQ)
					}
				}
				if err := store.Put(7, packets.PUBLISH, packet); err != nil {
					t.Fatal(err)
				}
				s := New(store, memory.New())
				if direction == "server" {
					s = New(memory.New(), store)
				}
				t.Cleanup(func() { _ = s.Close() })
				if err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{SessionPresent: true}); !errors.Is(err, failure) {
					t.Fatalf("connection accepted after failed recovery: %v", err)
				}
				if _, err := s.AddToSession(t.Context(), &packets.Subscribe{}); !errors.Is(err, session.ErrNoConnection) {
					t.Fatalf("allocated a packet ID after failed recovery: %v", err)
				}
			})
		}
	}
}

// TestDurableSessionStoresRestart checks that reopened file stores restore outbound
// PUBREL and inbound PUBREC state without redelivering an acknowledged QoS 2 publish,
// and that completed exchanges stay removed across a further reopen.
func TestDurableSessionStoresRestart(t *testing.T) {
	dir := t.TempDir()
	opts := []storefile.Option{storefile.WithDurable()}
	openStores := func() (*storefile.Store, *storefile.Store) {
		t.Helper()
		client, err := storefile.New(dir, "client-", ".pkt", opts...)
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("durable stores are unsupported on this filesystem: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		server, err := storefile.New(dir, "server-", ".pkt", opts...)
		if err != nil {
			t.Fatal(err)
		}
		return client, server
	}
	client, server := openStores()
	s := New(client, server)
	expiry := uint32(60)
	connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
	connack := &packets.Connack{SessionPresent: true}
	if err := s.ConAckReceived(io.Discard, connect, connack); err != nil {
		t.Fatal(err)
	}
	pub := &packets.Publish{QoS: 2, Topic: "durable/outbound"}
	if _, err := s.AddToSession(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	if err := s.PacketReceived((&packets.Pubrec{PacketID: pub.PacketID}).ToControlPacket(), nil); err != nil {
		t.Fatal(err)
	}
	inbound := &packets.Publish{PacketID: 42, QoS: 2, Topic: "durable/inbound"}
	if err := s.Ack(inbound); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	client, server = openStores()
	s = New(client, server)
	t.Cleanup(func() { _ = s.Close() })
	var wire bytes.Buffer
	if err := s.ConAckReceived(&wire, connect, connack); err != nil {
		t.Fatal(err)
	}
	if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBREL || cp.PacketID() != pub.PacketID {
		t.Fatalf("did not recover outbound PUBREL: %v, %v", cp, err)
	}
	inbound.Duplicate = true
	deliveries := make(chan *packets.Publish, 1)
	if err := s.PacketReceived(inbound.ToControlPacket(), deliveries); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatal("redelivered a QoS 2 message after recovering its PUBREC")
	}
	if cp, err := packets.ReadPacket(&wire); err != nil || cp.Type != packets.PUBREC {
		t.Fatalf("did not recover inbound acknowledgement: %v, %v", cp, err)
	}
	pubcomp := packets.NewControlPacket(packets.PUBCOMP)
	pubcomp.Content.(*packets.Pubcomp).PacketID = pub.PacketID
	if err := s.PacketReceived(pubcomp, nil); err != nil {
		t.Fatal(err)
	}
	pubrel := packets.NewControlPacket(packets.PUBREL)
	pubrel.Content.(*packets.Pubrel).PacketID = 42
	if err := s.PacketReceived(pubrel, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	client, server = openStores()
	for _, store := range []*storefile.Store{client, server} {
		if ids, err := store.List(); err != nil || len(ids) != 0 {
			t.Fatalf("completed session state reappeared: %v, %v", ids, err)
		}
	}
}
