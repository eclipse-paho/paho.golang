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

// PublishEventType identifies a stage of an outgoing publication.
type PublishEventType uint8

const (
	// PublishEventPubrec reports a successful PUBREC, including repeats.
	PublishEventPubrec PublishEventType = iota
	// PublishEventComplete reports PUBACK, rejection PUBREC, or PUBCOMP. Consult Trigger for the reason code;
	// completion does not necessarily mean success.
	PublishEventComplete
	// PublishEventSessionLost reports a publication discarded when the MQTT session is cleared. Delivery status is
	// unknown. Ordinary reconnects and closing a persistent session do not generate this event for publications
	// retained in the store.
	PublishEventSessionLost
)

// PublishEvent describes an outgoing publication's progress. Source and Trigger are read-only, and all packet
// references are only valid during the callback.
type PublishEvent struct {
	Type     PublishEventType
	PacketID uint16
	// Trigger is the received acknowledgement, CONNACK, or DISCONNECT responsible for the event. It is nil for session
	// loss without a packet.
	Trigger *packets.ControlPacket
	// Source is the stored PUBLISH or PUBREL, provided only when ReadStoredPacket is true. It is nil when callback
	// source reads are disabled or fail. At PUBCOMP it is a PUBREL, so any application metadata needed must be copied
	// by PreparePubrel.
	Source *packets.ControlPacket
	// Err reports a local store read, decode, or cleanup error, not a broker rejection (which is available in Trigger).
	// A failed read at Pubrec prevents replacement of the stored PUBLISH so application metadata is not lost.
	// Session-loss enumeration and bulk cleanup failures use a separate event with PacketID zero and no Source.
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

// PubrelPreparation provides the packets for the first successful PUBREC. Source and Trigger are read-only. Outgoing
// has fresh, non-nil Properties; the callback may set properties valid for PUBREL (User and ReasonString), but must
// not change PacketID or ReasonCode. Data copied by reference from Source or Trigger remains read-only: clone slices or
// pointed-to values before editing them. Replacement data must not be mutated after the callback returns, as the
// library still needs it for persistence and transmission. Packet references are valid only during the callback.
type PubrelPreparation struct {
	// Source is the stored PUBLISH, or nil when ReadStoredPacket is false.
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
	// PreparePubrel runs on the first successful PUBREC, before persisting PUBREL and before OnEvent. Repeated or
	// rejection PUBRECs do not invoke it.
	// If the source cannot be read from the session store, this callback is skipped and processing of the PUBREC
	// stops. The stored PUBLISH remains in the session store; OnEvent, if set, receives the error.
	// This hook works independently of OnEvent.
	PreparePubrel func(PubrelPreparation)
	// OnEvent observes acknowledgements and session loss. All packets are read-only. Notifications may be missed or
	// repeated across crashes; applications should use stable IDs and idempotent handling.
	OnEvent func(PublishEvent)
	// ReadStoredPacket enables source reads for PreparePubrel and OnEvent. This adds session-store reads, for example
	// loading the linked PUBLISH when PUBREC is received. Reads needed to retransmit stored packets, including PUBREL
	// responses to repeated PUBREC, still occur when this is false; OnEvent receives a nil Source in that case.
	ReadStoredPacket bool
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
