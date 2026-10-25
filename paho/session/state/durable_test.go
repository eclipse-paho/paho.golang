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
	"os"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho/session"
	storefile "github.com/eclipse/paho.golang/paho/store/file"
	"github.com/eclipse/paho.golang/paho/store/memory"
)

type persistenceErrorStore struct {
	*memory.Store
	putErr, deleteErr, getErr, quarantineErr error
}

func (s *persistenceErrorStore) Put(id uint16, kind byte, w io.WriterTo) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.Store.Put(id, kind, w)
}

func (s *persistenceErrorStore) Delete(id uint16) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.Delete(id)
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

type persistenceCheckingWriter func([]byte) (int, error)

func (w persistenceCheckingWriter) Write(b []byte) (int, error) { return w(b) }

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

func TestFinalAcknowledgementWaitsForStoreDeletion(t *testing.T) {
	for _, kind := range []byte{packets.PUBACK, packets.PUBCOMP, packets.PUBREC} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			store := &persistenceErrorStore{Store: memory.New()}
			s := New(store, memory.New())
			t.Cleanup(func() { _ = s.Close() })
			if err := s.ConAckReceived(io.Discard, &packets.Connect{}, &packets.Connack{}); err != nil {
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
			store.deleteErr = nil
			if err := s.PacketReceived(ack, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-response:
				if got.Type != kind {
					t.Fatalf("completion type=%d, want %d", got.Type, kind)
				}
			default:
				t.Fatal("successful deletion did not complete the transaction")
			}
			if _, ok := s.clientPackets[pb.PacketID]; ok {
				t.Fatal("successful deletion retained packet identifier")
			}
		})
	}
}

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

func TestDurableSessionStoresRestart(t *testing.T) {
	dir := t.TempDir()
	opts := []storefile.Option{storefile.WithDurable()}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncErr := f.Sync()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if syncErr != nil {
		if s, err := storefile.New(dir, "client-", ".pkt", opts...); err == nil || s != nil {
			t.Fatalf("durable store accepted unsupported directory sync: %v, %v", s, err)
		}
		return
	}
	openStores := func() (*storefile.Store, *storefile.Store) {
		t.Helper()
		client, err := storefile.New(dir, "client-", ".pkt", opts...)
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
