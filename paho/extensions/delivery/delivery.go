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

// Package delivery correlates outgoing publications with application IDs stored
// in MQTT User Properties. It works with Paho and AutoPaho, including queued
// publications and packets recovered from a persistent session after a restart.
package delivery

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/session"
)

// PropertyKey is reserved for the application's tracking ID. Handler removes it
// from wire packets while retaining it in queued and stored packets.
const PropertyKey = "paho-delivery-id"

// Outcome describes what is known when tracking a publication ends.
type Outcome uint8

const (
	// Unknown means delivery cannot be confirmed: the session was lost, PUBCOMP reported an unknown packet ID,
	// or a queued publication was discarded after local rejection. The publication may already have been delivered.
	Unknown Outcome = iota
	// SendAttempted means QoS 0 reached BeforeSend. This occurs before the packet is written so don't rely on it too
	// much (QOS0 offers no delivery guarantees).
	SendAttempted
	// Acknowledged means successful PUBACK or PUBCOMP was received.
	Acknowledged
	// Rejected means the broker rejected the publication in PUBACK or PUBREC.
	Rejected
	// NotSent means the PUBLISH is known not to have been sent to the server or accepted into session state,
	// for example when the caller reports a failed enqueue operation.
	NotSent
)

var (
	// ErrSessionLost identifies loss of a publication's MQTT session.
	ErrSessionLost = errors.New("session lost; delivery is unknown")
	// ErrUnconfirmed identifies a PUBCOMP or local queue discard that cannot establish successful delivery.
	ErrUnconfirmed = errors.New("delivery unconfirmed")
	// ErrRejected identifies a negative PUBACK or PUBREC.
	ErrRejected = errors.New("publication rejected")
)

// Result contains an application ID and its outcome. It contains no packet references and may be retained after the
// callback returns. IDs are not MQTT packet identifiers (which may be reused); they must identify individual
// publications.
type Result struct {
	ID           string
	QoS          byte
	Outcome      Outcome
	PacketID     uint16
	Trigger      byte // MQTT packet type; zero for a local event without a packet.
	ReasonCode   byte // Meaningful only when Trigger is nonzero.
	ReasonString string
	Err          error // Rejection, unknown delivery, or the cause of a NotSent outcome.
}

// Problem provides information about a tracking or store error separately from delivery outcomes.
// It does not terminate tracking: a retained publication may still complete.
// ID may be empty if the source could not be read or has no tracking property.
// A final acknowledgement can produce both a Result and a Problem (e.g. when deleting the stored packet fails).
// Fields may be retained by the application.
type Problem struct {
	ID       string
	PacketID uint16
	Event    session.PublishEventType
	Err      error
}

// Config contains user supplied callbacks. They run synchronously and may overlap. They must
// return promptly and must not wait for MQTT operations or initiate disconnect/reconnect.
// Install callbacks before connecting.
type Config struct {
	OnResult func(Result)
	OnError  func(Problem)
}

// Tracker enables outgoing PUBLISH transactions to be tracked. Use one tracker per client session, retaining it across
// reconnections.
type Tracker struct {
	config Config
}

// New creates a tracker. Nil callbacks are allowed, but omitting OnError hides errors that may prevent a tracked
// publication from producing a Result.
func New(config Config) *Tracker {
	return &Tracker{config: config}
}

// Track generates and attaches a fresh ID before publishing or enqueueing p.
// Record the returned ID before submitting p, since callbacks can arrive promptly.
func (t *Tracker) Track(p *paho.Publish) (string, error) {
	id := rand.Text()
	if err := t.TrackWithID(p, id); err != nil {
		return "", err
	}
	return id, nil
}

// TrackWithID attaches a caller-supplied, nonempty MQTT UTF-8 string ID. The caller must ensure uniqueness across
// publications, including recovered packets.
// Properties are copied before modification. Call before handing p to a client or queue, never concurrently with
// its use. An existing tracking property is rejected to avoid accidental reuse.
func (t *Tracker) TrackWithID(p *paho.Publish, id string) error {
	if p == nil || p.QoS > 2 {
		return fmt.Errorf("tracking requires a publication with QoS 0, 1 or 2")
	}
	if id == "" || len(id) > 65535 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("tracking ID must be a nonempty MQTT UTF-8 string")
	}
	props := paho.PublishProperties{}
	if p.Properties != nil {
		props = *p.Properties
	}
	for _, u := range props.User {
		if u.Key == PropertyKey {
			return fmt.Errorf("publication already has a tracking ID")
		}
	}
	props.User = append(slices.Clone(props.User), paho.UserProperty{Key: PropertyKey, Value: id})
	p.Properties = &props
	return nil
}

// Handler returns callbacks for ClientConfig.PublishEvents.
func (t *Tracker) Handler() session.PublishEventHandler {
	return session.PublishEventHandler{
		ReadStoredPacket: true,
		BeforeSend:       t.beforeSend,
		PreparePubrel:    t.preparePubrel,
		OnEvent:          t.onEvent,
	}
}

// ReportNotSent can be called when the caller observes a failure before delivery was accepted, for example when
// PublishViaQueue returns an error. It reports the failure through OnResult.
// Do not use this for timeouts or network errors after a publication has entered session state because the message can
// still be delivered. Untracked publications and nil errors are ignored.
func (t *Tracker) ReportNotSent(p *paho.Publish, err error) {
	if p == nil || p.Properties == nil || err == nil {
		return
	}
	if id := p.Properties.User.Get(PropertyKey); id != "" {
		t.result(Result{ID: id, QoS: p.QoS, Outcome: NotSent, Err: err})
	}
}

// beforeSend strips metadata on every attempt and reports each QoS 0 attempt.
func (t *Tracker) beforeSend(e session.PublishSend) {
	id, qos := identity(e.Source)
	switch p := e.Outgoing.Content.(type) {
	case *packets.Publish:
		p.Properties = withoutID(p.Properties)
	case *packets.Pubrel:
		p.Properties = withoutID(p.Properties)
	}
	if id == "" || qos != 0 {
		return
	}
	t.result(Result{ID: id, QoS: 0, Outcome: SendAttempted})
}

// preparePubrel preserves the ID when the session replaces PUBLISH with PUBREL. Doing this means the ID remains in the
// session state (which may be held on disk).
func (t *Tracker) preparePubrel(e session.PubrelPreparation) {
	if e.Source == nil || e.Source.Properties == nil {
		return
	}
	for _, u := range e.Source.Properties.User {
		if u.Key == PropertyKey {
			e.Outgoing.Properties.User = append(e.Outgoing.Properties.User, u)
		}
	}
}

// onEvent reports acknowledgements, session loss, and local rejections through OnResult, and tracking or storage errors
// through OnError.
// An event may trigger both callbacks: for example, a successful acknowledgement followed by a store cleanup failure.
func (t *Tracker) onEvent(e session.PublishEvent) {
	id, qos := identity(e.Source)
	if (e.Type == session.PublishEventNotSent || e.Type == session.PublishEventQueueDiscarded) && e.Source != nil {
		if id != "" {
			r := Result{ID: id, QoS: qos, Outcome: NotSent, Err: e.Err}
			if e.Type == session.PublishEventQueueDiscarded {
				r.Outcome, r.Err = Unknown, errors.Join(ErrUnconfirmed, e.Err)
			}
			t.result(r)
		}
		return
	}
	if e.Err != nil {
		t.problem(Problem{ID: id, PacketID: e.PacketID, Event: e.Type, Err: e.Err})
	}
	if e.Source == nil {
		if e.Err == nil {
			t.problem(Problem{PacketID: e.PacketID, Event: e.Type, Err: errors.New("missing stored publication")})
		}
		return // Never guess an ID from a reusable MQTT packet identifier.
	}
	if id == "" || qos == 0 || e.Type == session.PublishEventPubrec {
		return // Untracked publications and successful PUBREC need no result.
	}
	r := Result{ID: id, QoS: qos, PacketID: e.PacketID}
	if e.Trigger != nil {
		r.Trigger = e.Trigger.Type
		var props *packets.Properties
		switch p := e.Trigger.Content.(type) {
		case *packets.Puback:
			r.ReasonCode, props = p.ReasonCode, p.Properties
		case *packets.Pubrec:
			r.ReasonCode, props = p.ReasonCode, p.Properties
		case *packets.Pubcomp:
			r.ReasonCode, props = p.ReasonCode, p.Properties
		case *packets.Connack:
			r.ReasonCode, props = p.ReasonCode, p.Properties
		case *packets.Disconnect:
			r.ReasonCode, props = p.ReasonCode, p.Properties
		}
		if props != nil {
			r.ReasonString = props.ReasonString
		}
	}
	if e.Type == session.PublishEventSessionLost {
		r.Outcome, r.Err = Unknown, ErrSessionLost
	} else if e.Type == session.PublishEventComplete {
		switch {
		case qos == 1 && r.Trigger == packets.PUBACK,
			qos == 2 && r.Trigger == packets.PUBREC && r.ReasonCode >= 0x80:
			if r.ReasonCode >= 0x80 {
				r.Outcome, r.Err = Rejected, fmt.Errorf("%w: reason 0x%02x", ErrRejected, r.ReasonCode)
			} else {
				r.Outcome = Acknowledged
			}
		case qos == 2 && r.Trigger == packets.PUBCOMP:
			if r.ReasonCode == 0 {
				r.Outcome = Acknowledged
			} else {
				// MQTT 5 section 3.7.2.1: 0x92 can be normal recovery, not rejection.
				r.Outcome, r.Err = Unknown, ErrUnconfirmed
			}
		default:
			t.problem(Problem{ID: id, PacketID: e.PacketID, Event: e.Type, Err: errors.New("unexpected completion acknowledgement")})
			return
		}
	} else {
		return
	}
	t.result(r)
}

// identity retrieves the application ID from a message.
func identity(cp *packets.ControlPacket) (string, byte) {
	if cp == nil {
		return "", 0
	}
	var props *packets.Properties
	var qos byte
	switch p := cp.Content.(type) {
	case *packets.Publish:
		props, qos = p.Properties, p.QoS
	case *packets.Pubrel:
		props, qos = p.Properties, 2
	}
	if props != nil {
		for _, u := range props.User {
			if u.Key == PropertyKey {
				return u.Value, qos
			}
		}
	}
	return "", qos
}

// withoutID returns a copy of the properties with the tracking ID removed.
// It copies the User slice so the original properties remain unchanged.
func withoutID(source *packets.Properties) *packets.Properties {
	if source == nil {
		return nil
	}
	props := *source
	props.User = slices.DeleteFunc(slices.Clone(source.User), func(u packets.User) bool {
		return u.Key == PropertyKey
	})
	return &props
}

// result calls the application's OnResult callback if configured.
func (t *Tracker) result(r Result) {
	if t.config.OnResult != nil {
		t.config.OnResult(r)
	}
}

// problem calls the application's OnError callback if configured.
func (t *Tracker) problem(p Problem) {
	if t.config.OnError != nil {
		t.config.OnError(p)
	}
}
