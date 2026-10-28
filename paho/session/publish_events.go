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

package session

import "github.com/eclipse/paho.golang/packets"

// StoredPacketReadMode selects the source data needed by callbacks for stored publications. Implementations may
// obtain it from the session store or a cache. It does not control reads required for protocol operations.
type StoredPacketReadMode uint8

const (
	// ReadStoredNone disables stored source data for callbacks. This is the default.
	ReadStoredNone StoredPacketReadMode = iota
	// ReadStoredHeader provides the fixed and variable headers, including properties. Source may also contain the
	// payload, but callers must not rely on it. An absent payload does not mean the original publication was empty.
	ReadStoredHeader
	// ReadStoredFull provides the complete packet, including any payload.
	ReadStoredFull
)

// PublishEventType identifies a stage of an outgoing publication.
type PublishEventType uint8

const (
	// PublishEventPubrec reports a successful PUBREC, including repeats.
	PublishEventPubrec PublishEventType = iota
	// PublishEventComplete reports PUBACK, rejection PUBREC, or PUBCOMP. Consult Trigger for the reason code;
	// completion does not necessarily mean success.
	// If deleting the stored packet fails, Err includes the cleanup error. The event still reports the broker's
	// acknowledgement, but the response channel and packet identifier remain pending until cleanup succeeds.
	PublishEventComplete
	// PublishEventSessionLost reports a publication discarded when the MQTT session is cleared. Delivery status is
	// unknown. Ordinary reconnects and closing a persistent session do not generate this event for publications
	// retained in the store.
	PublishEventSessionLost
	// PublishEventNotSent reports a PUBLISH known not to have been sent to the server or accepted into session state.
	// Do not use it if an earlier attempt may have transmitted the publication. Source is the PUBLISH, Err is the
	// failure, PacketID is zero, and Trigger is nil.
	PublishEventNotSent
	// PublishEventQueueDiscarded reports a queued PUBLISH removed after local validation rejected its latest attempt.
	// Delivery is unknown: an earlier attempt, including before an application restart, may have transmitted it.
	// Source is the decoded queued PUBLISH, Err is the rejection error, PacketID is zero, and Trigger is nil.
	PublishEventQueueDiscarded
)

// PublishEvent describes an outgoing publication's progress. Source, Trigger, and their referenced data are read-only
// and only valid during the callback. Copy any data needed afterwards.
type PublishEvent struct {
	Type     PublishEventType
	PacketID uint16
	// Trigger is the received acknowledgement, CONNACK, or DISCONNECT responsible for the event. It is nil for session
	// loss without a packet, PublishEventNotSent, and PublishEventQueueDiscarded.
	Trigger *packets.ControlPacket
	// Source provides the stored PUBLISH or PUBREL at the detail selected by ReadStoredPacket. It is nil with
	// ReadStoredNone or when loading fails. PublishEventNotSent and PublishEventQueueDiscarded always provide the
	// complete rejected PUBLISH, regardless of ReadStoredPacket. At PUBCOMP it is a PUBREL, so any application metadata
	// needed must be copied by PreparePubrel.
	Source *packets.ControlPacket
	// Err reports a local store read, decode, or cleanup error, not a broker rejection (which is available in Trigger).
	// A failed read at Pubrec prevents replacement of the stored PUBLISH so application metadata is not lost.
	// Session-loss enumeration and bulk cleanup failures use a separate event with PacketID zero and no Source.
	// For PublishEventNotSent and PublishEventQueueDiscarded, Err instead reports the local failure.
	Err error
}

// PublishSend provides a read-only Source and an editable copy of the outgoing PUBLISH or PUBREL to be sent. Packet
// references are valid only during the callback.
//
// Outgoing is a shallow copy of the packet. Referenced data, including Properties, its slices and pointer targets, and
// Payload, is shared with Source. Before modifying shared data, clone it and assign the copy to Outgoing. Properties
// may be nil; allocate if needed. Assigning Topic or replacing Payload or Properties does not modify Source.
// Replacement data must not be mutated after the callback returns.
//
// Do not change the control packet's FixedHeader or Content, or the concrete packet's PacketID, QoS, Duplicate, Retain,
// or ReasonCode fields where present. These fields are managed by the protocol/session. Changes must produce a valid
// MQTT packet and remain consistent with retransmission of the same publication; for example, a topic alias may change
// but must resolve to the same topic.
type PublishSend struct {
	Source   *packets.ControlPacket
	Outgoing *packets.ControlPacket
}

// PubrelPreparation provides the packets for the transition from PUBLISH to PUBREL. Source and Trigger are read-only.
// Outgoing has fresh, non-nil Properties; the callback may set properties valid for PUBREL (User and ReasonString), but
// must not change PacketID or ReasonCode. Data copied by reference from Source or Trigger remains read-only: clone slices or
// pointed-to values before editing them. Replacement data must not be mutated after the callback returns, as the
// library still needs it for persistence and transmission. Packet references are valid only during the callback.
type PubrelPreparation struct {
	// Source provides the stored PUBLISH at the detail selected by ReadStoredPacket, or nil with ReadStoredNone.
	Source  *packets.Publish
	Trigger *packets.Pubrec
	// Outgoing is the PUBREL to persist before transmission, with non-nil Properties.
	Outgoing *packets.Pubrel
}

// PublishEventHandler configures independent hooks and notifications for outgoing publications. Register it before
// connecting, including after a restart.
// Callbacks run synchronously, outside session/store locks. Calls from different publishers and connection workers may
// overlap. They must return promptly and must not wait for MQTT operations, disconnect, or reconnect: doing so may
// deadlock the protocol worker. Copy any data needed by asynchronous work.
type PublishEventHandler struct {
	// BeforeSend runs before each PUBLISH or PUBREL write, including retransmissions and QoS 0. Changes affect only the
	// wire copy.
	// See PublishSend for permitted changes and ownership of shared data.
	BeforeSend func(PublishSend)
	// PreparePubrel runs on a successful PUBREC while the stored packet is still a PUBLISH, before persisting PUBREL
	// and before OnEvent. Once PUBREL is stored, repeated PUBRECs do not invoke it. Rejection PUBRECs do not invoke it.
	// If the source cannot be read from the session store, this callback is skipped and processing of the PUBREC
	// stops. The stored PUBLISH remains in the session store; OnEvent, if set, receives the error.
	// This hook works independently of OnEvent.
	PreparePubrel func(PubrelPreparation)
	// OnEvent observes acknowledgements, session loss, and local rejection of queued publications. All packets are
	// read-only. Notifications may be missed or repeated across crashes; applications should use stable IDs and
	// idempotent handling.
	OnEvent func(PublishEvent)
	// ReadStoredPacket selects the source data provided to PreparePubrel and OnEvent for stored publications.
	// The default is ReadStoredNone. ReadStoredHeader and ReadStoredFull may add session-store reads, for example
	// loading the linked PUBLISH when PUBREC is received. Select ReadStoredFull if a callback needs the payload.
	// Reads needed to retransmit stored packets, including PUBREL responses to repeated PUBREC, still occur with
	// ReadStoredNone; callbacks receive a nil Source in that case. Already-decoded sources for BeforeSend,
	// PublishEventNotSent, and PublishEventQueueDiscarded are always complete, regardless of this setting.
	ReadStoredPacket StoredPacketReadMode
}

// PrepareSend applies BeforeSend to a shallow copy of a PUBLISH or PUBREL.
// Properties, slices, and pointer targets are not copied so callbacks must copy these before modifying their
// contents (see PublishSend).
// SessionManager implementations can use this for their retransmission paths.
func (h PublishEventHandler) PrepareSend(source *packets.ControlPacket) *packets.ControlPacket {
	if h.BeforeSend == nil {
		return source
	}
	outgoing := *source
	switch p := source.Content.(type) {
	case *packets.Publish:
		c := *p
		outgoing.Content = &c
	case *packets.Pubrel:
		c := *p
		outgoing.Content = &c
	default:
		return source
	}
	h.BeforeSend(PublishSend{Source: source, Outgoing: &outgoing})
	return &outgoing
}
