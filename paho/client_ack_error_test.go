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

package paho

import (
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/internal/basictestserver"
	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/session/state"
)

type ackErrorSession struct {
	session.SessionManager
	err      error
	ackCalls int
}

func (s *ackErrorSession) Ack(*packets.Publish) error {
	s.ackCalls++
	return s.err
}

// TestSessionAckErrorClosesConnection confirms that an error writing an ACK to the store causes the connection to
// drop
func TestSessionAckErrorClosesConnection(t *testing.T) {
	ts := basictestserver.New(paholog.NewTestLogger(t, "TestServer:"))
	ts.SetResponse(packets.CONNACK, &packets.Connack{Properties: &packets.Properties{}})
	go ts.Run()
	defer ts.Stop()
	defer ts.ClientConn().Close()
	failure := errors.New("persisting acknowledgement failed")
	s := &ackErrorSession{SessionManager: state.NewInMemory(), err: failure}
	defer s.Close()
	reported := make(chan error, 8)
	c := NewClient(ClientConfig{
		Conn: ts.ClientConn(), Session: s,
		OnClientError: func(err error) {
			select {
			case reported <- err:
			default:
			}
		},
	})
	if _, err := c.Connect(t.Context(), &Connect{ClientID: "ack-error"}); err != nil {
		t.Fatal(err)
	}
	defer c.close()
	c.ack(&packets.Publish{QoS: 0})
	if s.ackCalls != 0 {
		t.Fatal("QoS 0 publish was passed to session acknowledgement handling")
	}
	c.ack(&packets.Publish{PacketID: 7, QoS: 2})
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case err := <-reported:
			if !errors.Is(err, failure) {
				continue
			}
		case <-timeout.C:
			t.Fatal("acknowledgement error not reported")
		}
		break
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("reported error without shutting down client")
	}
}
