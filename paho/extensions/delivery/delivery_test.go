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

package delivery_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/extensions/delivery"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/session/state"
	filestore "github.com/eclipse/paho.golang/paho/store/file"
	"github.com/eclipse/paho.golang/paho/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// publication attaches an ID and converts a publication into the session packet format.
func publication(t *testing.T, tracker *delivery.Tracker, qos byte, id string) *packets.ControlPacket {
	t.Helper()
	p := &paho.Publish{QoS: qos, Topic: "delivery/test", Payload: []byte("body")}
	require.NoError(t, tracker.TrackWithID(p, id))
	return p.Packet().ToControlPacket()
}

// acknowledgement builds a final or intermediate acknowledgement with diagnostic properties.
func acknowledgement(kind byte, id uint16, reason byte) *packets.ControlPacket {
	cp := packets.NewControlPacket(kind)
	props := &packets.Properties{ReasonString: "broker explanation"}
	switch p := cp.Content.(type) {
	case *packets.Puback:
		p.PacketID, p.ReasonCode, p.Properties = id, reason, props
	case *packets.Pubrec:
		p.PacketID, p.ReasonCode, p.Properties = id, reason, props
	case *packets.Pubcomp:
		p.PacketID, p.ReasonCode, p.Properties = id, reason, props
	}
	return cp
}

type cleanupErrorStore struct {
	*memory.Store
	deleteErr, resetErr error
	applyDeleteOnError  bool
}

func (s *cleanupErrorStore) Delete(id uint16) error {
	if s.deleteErr != nil && !s.applyDeleteOnError {
		return s.deleteErr
	}
	if err := s.Store.Delete(id); err != nil {
		return err
	}
	return s.deleteErr
}

func (s *cleanupErrorStore) Reset() error {
	if s.resetErr != nil {
		return s.resetErr
	}
	return s.Store.Reset()
}

// TestDeliveryAfterCleanupFailure combines acknowledgement tracking with durable cleanup.
// Recovery must neither repeat the result nor report an acknowledged publication as lost.
func TestDeliveryAfterCleanupFailure(t *testing.T) {
	for _, recovery := range []struct {
		name      string
		expiry    uint32
		present   bool
		failReset bool
	}{
		{name: "resume", expiry: 600, present: true},
		{name: "broker reset", expiry: 600},
		{name: "broker reset retry", expiry: 600, failReset: true},
		{name: "expired session"},
		{name: "expired session reset retry", failReset: true},
	} {
		for _, ack := range []struct {
			qos     byte
			kind    byte
			reason  byte
			outcome delivery.Outcome
		}{
			{1, packets.PUBACK, 0, delivery.Acknowledged},
			{1, packets.PUBACK, 0x87, delivery.Rejected},
			{2, packets.PUBREC, 0x87, delivery.Rejected},
			{2, packets.PUBCOMP, 0, delivery.Acknowledged},
		} {
			for _, applied := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/%x/applied=%t", recovery.name, ack.kind, ack.reason, applied), func(t *testing.T) {
					store := &cleanupErrorStore{Store: memory.New(), applyDeleteOnError: applied}
					s := state.New(store, memory.New())
					defer s.Close()
					var results []delivery.Result
					var problems []delivery.Problem
					tracker := delivery.New(delivery.Config{
						OnResult: func(r delivery.Result) { results = append(results, r) },
						OnError:  func(p delivery.Problem) { problems = append(problems, p) },
					})
					s.SetPublishEventHandler(tracker.Handler())
					connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &recovery.expiry}}
					require.NoError(t, s.ConAckReceived(io.Discard, connect, &packets.Connack{}))
					pub := publication(t, tracker, ack.qos, "acknowledged").Content.(*packets.Publish)
					response, err := s.AddToSession(t.Context(), pub)
					require.NoError(t, err)
					if ack.kind == packets.PUBCOMP {
						require.NoError(t, s.PacketReceived(acknowledgement(packets.PUBREC, pub.PacketID, 0), nil))
					}
					pending := publication(t, tracker, 1, "pending").Content.(*packets.Publish)
					_, err = s.AddToSession(t.Context(), pending)
					require.NoError(t, err)
					store.deleteErr = errors.New("delete failed")
					final := acknowledgement(ack.kind, pub.PacketID, ack.reason)
					require.ErrorIs(t, s.PacketReceived(final, nil), store.deleteErr)
					require.Len(t, results, 1)
					assert.Equal(t, "acknowledged", results[0].ID)
					assert.Equal(t, ack.outcome, results[0].Outcome)
					assert.Equal(t, ack.kind, results[0].Trigger)
					assert.Equal(t, ack.reason, results[0].ReasonCode)
					require.Len(t, problems, 1)
					assert.Equal(t, "acknowledged", problems[0].ID)
					assert.ErrorIs(t, problems[0].Err, store.deleteErr)
					select {
					case <-response:
						t.Fatal("response must remain pending until durable cleanup succeeds")
					default:
					}

					if recovery.failReset {
						store.resetErr = errors.New("reset failed")
					}
					var wire bytes.Buffer
					err = s.ConnectionLost(nil)
					if recovery.expiry != 0 {
						require.NoError(t, err)
						err = s.ConAckReceived(&wire, connect, &packets.Connack{SessionPresent: recovery.present})
					}
					if recovery.failReset {
						require.ErrorIs(t, err, store.resetErr)
						require.Len(t, problems, 2)
						assert.ErrorIs(t, problems[1].Err, store.resetErr)
					} else if recovery.present && !applied {
						require.ErrorIs(t, err, store.deleteErr)
					} else {
						require.NoError(t, err)
					}
					if recovery.failReset || recovery.present && !applied {
						select {
						case <-response:
							t.Fatal("failed recovery completed the pending response")
						default:
						}
						assert.Zero(t, wire.Len(), "failed recovery must not retransmit packets")
						store.deleteErr, store.resetErr = nil, nil
						require.NoError(t, s.ConAckReceived(&wire, connect, &packets.Connack{SessionPresent: recovery.present}))
					}
					select {
					case got, open := <-response:
						require.True(t, open)
						assert.Equal(t, *final, got)
					default:
						t.Fatal("cleanup did not deliver the original acknowledgement")
					}
					_, open := <-response
					assert.False(t, open)
					if recovery.present {
						assert.Len(t, results, 1, "recovery must not repeat a completion result")
						replay, err := packets.ReadPacket(&wire)
						require.NoError(t, err)
						assert.Equal(t, pending.PacketID, replay.PacketID())
					} else {
						require.Len(t, results, 2, "only the unacknowledged publication is lost")
						assert.Equal(t, "pending", results[1].ID)
						assert.Equal(t, delivery.Unknown, results[1].Outcome)
						assert.ErrorIs(t, results[1].Err, delivery.ErrSessionLost)
					}
					assert.Zero(t, wire.Len(), "the acknowledged publication must not be retransmitted")
				})
			}
		}
	}
}

// TestTrack checks generated and supplied IDs, property ownership, and invalid input.
func TestTrack(t *testing.T) {
	tracker := delivery.New(delivery.Config{})
	props := &paho.PublishProperties{ContentType: "text/plain", User: paho.UserProperties{{Key: "public", Value: "keep"}}}
	first := &paho.Publish{QoS: 1, Properties: props}
	id, err := tracker.Track(first)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	assert.Equal(t, id, first.Properties.User.Get(delivery.PropertyKey))
	assert.Empty(t, props.User.Get(delivery.PropertyKey))
	assert.Equal(t, "text/plain", first.Properties.ContentType)
	assert.Equal(t, "keep", first.Properties.User.Get("public"))
	second := &paho.Publish{QoS: 2}
	other, err := tracker.Track(second)
	require.NoError(t, err)
	assert.NotEqual(t, id, other)
	require.Error(t, tracker.TrackWithID(first, "replacement"))
	assert.Equal(t, id, first.Properties.User.Get(delivery.PropertyKey))
	for _, invalid := range []string{"", "a\x00b", "\xff", strings.Repeat("x", 65536)} {
		require.Error(t, tracker.TrackWithID(&paho.Publish{}, invalid))
	}
	_, err = tracker.Track(nil)
	require.Error(t, err)
	_, err = tracker.Track(&paho.Publish{QoS: 3})
	require.Error(t, err)
}

// TestQoS0Attempt verifies concurrent retries each notify with the same ID while every wire copy
// is stripped, and that the original packet's properties remain intact.
func TestQoS0Attempt(t *testing.T) {
	var count atomic.Int32
	tracker := delivery.New(delivery.Config{OnResult: func(r delivery.Result) {
		assert.Equal(t, delivery.SendAttempted, r.Outcome)
		assert.Equal(t, "zero", r.ID)
		assert.Zero(t, r.Trigger)
		count.Add(1)
	}})
	cp := publication(t, tracker, 0, "zero")
	source := cp.Content.(*packets.Publish)
	source.Properties.User = append(source.Properties.User, packets.User{Key: "public", Value: "keep"})
	h := tracker.Handler()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			wire := h.PrepareSend(cp).Content.(*packets.Publish)
			assert.Equal(t, []packets.User{{Key: "public", Value: "keep"}}, wire.Properties.User)
		})
	}
	wg.Wait()
	assert.Equal(t, int32(20), count.Load())
	assert.Len(t, source.Properties.User, 2)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventSessionLost, Source: cp})
	assert.Equal(t, int32(20), count.Load(), "QoS 0 is not session-tracked")
	h.PrepareSend(cp)
	assert.Equal(t, int32(21), count.Load(), "later attempts also notify without explicit cleanup")
}

// TestAcknowledgements covers success, rejection and ambiguous recovery responses
// without requiring registration in the tracker that receives the notification.
func TestAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		qos     byte
		kind    byte
		reason  byte
		outcome delivery.Outcome
		err     error
	}{
		{1, packets.PUBACK, 0, delivery.Acknowledged, nil},
		{1, packets.PUBACK, 0x10, delivery.Acknowledged, nil},
		{1, packets.PUBACK, 0x87, delivery.Rejected, delivery.ErrRejected},
		{2, packets.PUBREC, 0x97, delivery.Rejected, delivery.ErrRejected},
		{2, packets.PUBCOMP, 0, delivery.Acknowledged, nil},
		{2, packets.PUBCOMP, 0x92, delivery.Unknown, delivery.ErrUnconfirmed},
	} {
		t.Run(fmt.Sprintf("qos%d/%d/%x", tc.qos, tc.kind, tc.reason), func(t *testing.T) {
			original := delivery.New(delivery.Config{})
			source := publication(t, original, tc.qos, "restored")
			if tc.kind == packets.PUBCOMP {
				rel := packets.NewControlPacket(packets.PUBREL)
				original.Handler().PreparePubrel(session.PubrelPreparation{Source: source.Content.(*packets.Publish), Outgoing: rel.Content.(*packets.Pubrel)})
				wire := original.Handler().PrepareSend(rel).Content.(*packets.Pubrel)
				assert.Empty(t, wire.Properties.User)
				assert.Len(t, rel.Content.(*packets.Pubrel).Properties.User, 1)
				source = rel
			}
			var results []delivery.Result
			tracker := delivery.New(delivery.Config{OnResult: func(r delivery.Result) { results = append(results, r) }})
			ack := acknowledgement(tc.kind, 23, tc.reason)
			tracker.Handler().OnEvent(session.PublishEvent{Type: session.PublishEventComplete, Source: source, Trigger: ack, PacketID: 23})
			require.Len(t, results, 1)
			r := results[0]
			assert.Equal(t, "restored", r.ID)
			assert.Equal(t, tc.qos, r.QoS)
			assert.Equal(t, tc.outcome, r.Outcome)
			assert.ErrorIs(t, r.Err, tc.err)
			assert.Equal(t, uint16(23), r.PacketID)
			assert.Equal(t, tc.kind, r.Trigger)
			assert.Equal(t, tc.reason, r.ReasonCode)
			assert.Equal(t, "broker explanation", r.ReasonString)
		})
	}
}

// TestStoreProblemsDoNotDetermineDelivery ensures transient errors do not end
// tracking and a cleanup error does not hide a successful broker acknowledgement.
func TestStoreProblemsDoNotDetermineDelivery(t *testing.T) {
	var results []delivery.Result
	var problems []delivery.Problem
	tracker := delivery.New(delivery.Config{
		OnResult: func(r delivery.Result) { results = append(results, r) },
		OnError:  func(p delivery.Problem) { problems = append(problems, p) },
	})
	h := tracker.Handler()
	readErr := errors.New("read failed")
	h.OnEvent(session.PublishEvent{Type: session.PublishEventPubrec, PacketID: 7, Err: readErr})
	require.Len(t, problems, 1)
	assert.ErrorIs(t, problems[0].Err, readErr)
	assert.Empty(t, problems[0].ID)
	assert.Empty(t, results)
	cp := publication(t, tracker, 1, "retry")
	deleteErr := errors.New("delete failed")
	h.OnEvent(session.PublishEvent{Type: session.PublishEventComplete, PacketID: 7, Source: cp, Trigger: acknowledgement(packets.PUBACK, 7, 0), Err: deleteErr})
	require.Len(t, results, 1)
	assert.Equal(t, delivery.Acknowledged, results[0].Outcome)
	assert.NoError(t, results[0].Err)
	require.Len(t, problems, 2)
	assert.Equal(t, "retry", problems[1].ID)
	assert.ErrorIs(t, problems[1].Err, deleteErr)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventSessionLost, Err: readErr})
	require.Len(t, problems, 3)
	assert.Len(t, results, 1, "bulk failure cannot be assigned to an application ID")
}

// TestMissingSourceErrors ensures source failures are reported without guessing a delivery outcome.
func TestMissingSourceErrors(t *testing.T) {
	readErr := errors.New("source read failed")
	var problems []delivery.Problem
	handler := delivery.New(delivery.Config{
		OnResult: func(delivery.Result) { t.Error("unreadable source must not produce a delivery result") },
		OnError:  func(p delivery.Problem) { problems = append(problems, p) },
	}).Handler()
	for _, kind := range []session.PublishEventType{session.PublishEventComplete, session.PublishEventQueueDiscarded} {
		eventErr := errors.New("event error")
		handler.OnEvent(session.PublishEvent{Type: kind, PacketID: 7, Err: errors.Join(readErr, eventErr)})
		require.NotEmpty(t, problems)
		problem := problems[len(problems)-1]
		assert.ErrorIs(t, problem.Err, readErr)
		assert.ErrorIs(t, problem.Err, eventErr)
		assert.Empty(t, problem.ID)
		assert.Equal(t, uint16(7), problem.PacketID)
		assert.Equal(t, kind, problem.Event)
	}
	assert.Len(t, problems, 2)
}

// TestTrackingUsesHeaders verifies that tracking requests only headers and works without a stored payload,
// including when preserving the application ID across the PUBLISH-to-PUBREL transition.
func TestTrackingUsesHeaders(t *testing.T) {
	for _, qos := range []byte{0, 1, 2} {
		t.Run(fmt.Sprintf("qos%d", qos), func(t *testing.T) {
			var results []delivery.Result
			tracker := delivery.New(delivery.Config{
				OnResult: func(r delivery.Result) { results = append(results, r) },
				OnError:  func(p delivery.Problem) { t.Errorf("unexpected tracking error: %v", p.Err) },
			})
			handler := tracker.Handler()
			assert.Equal(t, session.ReadStoredHeader, handler.ReadStoredPacket)
			packet := publication(t, tracker, qos, "headers")
			publish := packet.Content.(*packets.Publish)
			if qos > 0 {
				publish.PacketID = 7
			}
			header := *publish
			header.Payload = nil
			source := header.ToControlPacket()
			outgoing := *publish
			handler.BeforeSend(session.PublishSend{Source: packet, Outgoing: outgoing.ToControlPacket()})
			assert.Empty(t, outgoing.Properties.User, "tracking ID must be removed from the wire copy")
			assert.Equal(t, "body", string(outgoing.Payload))
			assert.Equal(t, "headers", publish.Properties.User[0].Value, "source metadata must be preserved")
			if qos == 1 {
				handler.OnEvent(session.PublishEvent{Type: session.PublishEventComplete, PacketID: 7, Source: source,
					Trigger: acknowledgement(packets.PUBACK, 7, 0)})
			} else if qos == 2 {
				rel := packets.NewControlPacket(packets.PUBREL)
				rel.Content.(*packets.Pubrel).PacketID = 7
				handler.PreparePubrel(session.PubrelPreparation{Source: &header, Outgoing: rel.Content.(*packets.Pubrel)})
				handler.OnEvent(session.PublishEvent{Type: session.PublishEventComplete, PacketID: 7, Source: rel,
					Trigger: acknowledgement(packets.PUBCOMP, 7, 0)})
			}
			require.Len(t, results, 1)
			assert.Equal(t, "headers", results[0].ID)
			assert.Equal(t, qos, results[0].QoS)
			if qos == 0 {
				assert.Equal(t, delivery.SendAttempted, results[0].Outcome)
			} else {
				assert.Equal(t, delivery.Acknowledged, results[0].Outcome)
			}
		})
	}
}

// TestSubmissionAndUntrackedPackets covers errors before enqueue acceptance and
// confirms ordinary untracked publications and successful PUBREC are ignored.
func TestSubmissionAndUntrackedPackets(t *testing.T) {
	var results []delivery.Result
	var problems []delivery.Problem
	tracker := delivery.New(delivery.Config{
		OnResult: func(r delivery.Result) { results = append(results, r) },
		OnError:  func(p delivery.Problem) { problems = append(problems, p) },
	})
	h := tracker.Handler()
	cp := (&paho.Publish{QoS: 1}).Packet().ToControlPacket()
	h.PrepareSend(cp)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventComplete, Source: cp, Trigger: acknowledgement(packets.PUBACK, 1, 0)})
	tracked := publication(t, tracker, 2, "two")
	h.OnEvent(session.PublishEvent{Type: session.PublishEventPubrec, Source: tracked, Trigger: acknowledgement(packets.PUBREC, 1, 0)})
	assert.Empty(t, results)
	assert.Empty(t, problems)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventNotSent, Source: cp, Err: paho.ErrInvalidArguments})
	h.OnEvent(session.PublishEvent{Type: session.PublishEventQueueDiscarded, Source: cp, Err: paho.ErrInvalidArguments})
	assert.Empty(t, results, "local rejection of an untracked publication is ignored")
	assert.Empty(t, problems)
	p := &paho.Publish{QoS: 0}
	id, err := tracker.Track(p)
	require.NoError(t, err)
	enqueueErr := errors.New("queue full")
	tracker.ReportNotSent(p, enqueueErr)
	require.Len(t, results, 1)
	assert.Equal(t, id, results[0].ID)
	assert.Equal(t, delivery.NotSent, results[0].Outcome)
	assert.ErrorIs(t, results[0].Err, enqueueErr)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventComplete})
	require.Len(t, problems, 1)
	h.OnEvent(session.PublishEvent{Type: session.PublishEventComplete, Source: tracked})
	require.Len(t, problems, 2)
}

// TestRestoredSession uses real disk storage and new tracker/session instances at
// both PUBLISH and PUBREL stages, resuming or losing the broker session on restart.
func TestRestoredSession(t *testing.T) {
	for _, stage := range []string{"qos1", "qos2-publish", "qos2-pubrel"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%t", stage, present), func(t *testing.T) {
				dir := t.TempDir()
				store, err := filestore.New(dir, "client-", ".pkt")
				require.NoError(t, err)
				old := state.New(store, memory.New())
				defer old.Close()
				original := delivery.New(delivery.Config{})
				old.SetPublishEventHandler(original.Handler())
				expiry := uint32(600)
				connect := &packets.Connect{Properties: &packets.Properties{SessionExpiryInterval: &expiry}}
				require.NoError(t, old.ConAckReceived(io.Discard, connect, &packets.Connack{}))
				qos := byte(2)
				if stage == "qos1" {
					qos = 1
				}
				cp := publication(t, original, qos, "persisted")
				p := cp.Content.(*packets.Publish)
				_, err = old.AddToSession(t.Context(), p)
				require.NoError(t, err)
				if stage == "qos2-pubrel" {
					require.NoError(t, old.PacketReceived(acknowledgement(packets.PUBREC, p.PacketID, 0), nil))
				}
				require.NoError(t, old.Close())
				store, err = filestore.New(dir, "client-", ".pkt")
				require.NoError(t, err)
				restored := state.New(store, memory.New())
				defer restored.Close()
				var results []delivery.Result
				fresh := delivery.New(delivery.Config{
					OnResult: func(r delivery.Result) { results = append(results, r) },
					OnError:  func(p delivery.Problem) { t.Errorf("tracking error: %v", p.Err) },
				})
				restored.SetPublishEventHandler(fresh.Handler())
				var wire bytes.Buffer
				require.NoError(t, restored.ConAckReceived(&wire, connect, &packets.Connack{SessionPresent: present}))
				if present {
					require.Empty(t, results)
					retransmitted, err := packets.ReadPacket(&wire)
					require.NoError(t, err)
					switch p := retransmitted.Content.(type) {
					case *packets.Publish:
						assert.Empty(t, p.Properties.User)
					case *packets.Pubrel:
						assert.Empty(t, p.Properties.User)
					default:
						t.Fatalf("unexpected retransmission: %T", p)
					}
					if stage == "qos2-publish" {
						require.NoError(t, restored.PacketReceived(acknowledgement(packets.PUBREC, p.PacketID, 0), nil))
					}
					final := byte(packets.PUBCOMP)
					if qos == 1 {
						final = packets.PUBACK
					}
					require.NoError(t, restored.PacketReceived(acknowledgement(final, p.PacketID, 0), nil))
				}
				require.Len(t, results, 1)
				assert.Equal(t, "persisted", results[0].ID)
				assert.Equal(t, qos, results[0].QoS)
				if present {
					assert.Equal(t, delivery.Acknowledged, results[0].Outcome)
				} else {
					assert.Equal(t, delivery.Unknown, results[0].Outcome)
					assert.Equal(t, byte(packets.CONNACK), results[0].Trigger)
					assert.ErrorIs(t, results[0].Err, delivery.ErrSessionLost)
				}
			})
		}
	}
}
