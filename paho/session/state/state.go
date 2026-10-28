/*
 * Copyright (c) 2024 Contributors to the Eclipse Foundation
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
 * AI Disclosure: The durable state acknowledgement handling and its integration with delivery events
 * were generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
	"github.com/eclipse/paho.golang/paho/store/memory"
)

// The Session State, as per the MQTT spec, contains:
//
//	> QoS 1 and QoS 2 messages which have been sent to the Server, but have not been completely acknowledged.
//	> QoS 2 messages which have been received from the Server, but have not been completely acknowledged.
//
// and, importantly, is used when resending as follows:
//
// > When a Client reconnects with Clean Start set to 0 and a session is present, both the Client and Server MUST resend
// > any unacknowledged PUBLISH packets (where QoS > 0) and PUBREL packets using their original Packet Identifiers. This
// > is the only circumstance where a Client or Server is REQUIRED to resend messages. Clients and Servers MUST NOT
// > resend messages at any other time
//
// There are a few other areas where the State is important:
//    * When allocating a new packet identifier, we need to know what IDs are already in the session state.
//    * If a QOS2 Publish with `DUP=TRUE` is received, then we should not pass it to the client if we have previously
//    sent a `PUBREC` (indicating that the message has already been processed).
//    * Some subscribers may need a transaction (i.e. commit transaction when `PUBREL` received), this is not implemented
//       here, but the option is left open.
//
// This means that the following information may need to be retained after the connection is lost:
//  * The IDs of any transactions initiated by the client (so that we don't reuse IDs and can notify the requester (if
//    known) when a response is received or the request is removed from the state).
//  * For client initiated publish:
//     * Outgoing `PUBLISH` packets to allow resend.
//     * Outgoing `PUBREL` packets to allow resend.
//  * For server initiated publish:
//     * Outgoing `PUBREL` packets. The fact that this has been sent indicates that the user app has acknowledged the
//       message, and it should not be re-presented (doing so would breach the "exactly once" requirement).
//     * In memory only - the fact that a QOS2 PUBLISH has been received and sent to the handler, but not acknowledged.
//       This allows us to avoid presenting the message a second time (if the application is restarted, we have no option
//       but to re-present it because we have no way of knowing if the application completed handling it).
//
//  It is important to note that there are packets with identifiers that do not form part of the session state
//  (SUBSCRIBE, SUBACK, UNSUBSCRIBE, UNSUBACK). Whilst these will never be stored to disk, it is important to track
//  the packet IDs to ensure we don't reuse them.
//  For packets relating to client-initiated transactions sent during a `session.State` lifetime we also want to link
//  a channel to the message ID so that we can notify our user when the transaction is complete (allowing a call to, for
//  instance `Publish()` to block until the message is fully acknowledged even if we disconnect/reconnect in the interim.

const (
	midMin uint16 = 1
	midMax uint16 = 65535
)

type (
	// clientGenerated holds information on client-generated packets (e.g. an outgoing SUBSCRIBE request)
	clientGenerated struct {
		packetType byte // The type of the last packet sent (i.e. PUBLISH, SUBSCRIBE or UNSUBSCRIBE) - 0 means unknown until loaded from the store

		// When a message is fully acknowledged, we need to let the requester know by sending the final response to this
		// channel. One and only one message will be sent (and the channel closed).
		// We also guarantee to always send to the channel (assuming there is a clean shutdown) so that the end user knows
		// the status of the request.
		responseChan chan packets.ControlPacket
		// A final acknowledgement retained until the persisted packet is deleted or the store is reset.
		pendingResponse *packets.ControlPacket
	}
)

// State manages the session state. The client will send messages that may impact the state via
// us, and we will maintain the session state
type State struct {
	mu                    sync.Mutex // protects the whole struct
	connectionLostAt      time.Time  // Time that the connection was lost
	sessionExpiryInterval uint32     // The session expiry interval sent with the most recent CONNECT packet

	conn          io.Writer       // current connection or nil if we are not connected
	connCtx       context.Context // Context will be closed if the connection is lost (only valid when conn != nil)
	connCtxCancel func()          // Cancels the above context

	// client store - holds packets where the message ID was generated on the client (i.e. by paho.golang)
	clientPackets           map[uint16]clientGenerated // Store relating to messages sent TO the server
	clientStore             storer                     // Used to store session state that survives connection loss
	clientStoreResetPending bool                       // A failed reset must complete before stored packets can be reused
	lastMid                 uint16                     // The message ID most recently issued

	// server store - holds packets where the message ID was generated on the server
	serverPackets           map[uint16]byte // The last packet received from the server with this ID (cleared when the transaction is complete)
	serverStore             storer          // Used to store session state that survives connection loss
	serverStoreResetPending bool            // A failed reset must complete before stored packets can be reused
	serverStoreNeedsReload  bool            // A failed mutation may leave the cached packet IDs out of sync with the store

	// The number of messages in flight needs to be limited, as per receive maximum received from the server.
	inflight *sendQuota

	publishEvents   session.PublishEventHandler
	sessionLossDone chan struct{} // Non-nil while discarded packets are being reported and the stores reset

	debug  paholog.Logger
	errors paholog.Logger
}

// New creates a new state which will persist information using the passed in storer's.
func New(client storer, server storer) *State {
	return &State{
		clientStore: client,
		serverStore: server,
		debug:       paholog.NOOPLogger{},
		errors:      paholog.NOOPLogger{},
	}
}

// NewInMemory returns a default State that stores all information in memory
func NewInMemory() *State {
	return &State{
		clientStore: memory.New(),
		serverStore: memory.New(),
		debug:       paholog.NOOPLogger{},
		errors:      paholog.NOOPLogger{},
	}
}

// SetPublishEventHandler configures outgoing publish events. Clients call this before connecting.
// If using State directly, also apply handler.PrepareSend to the initial PUBLISH, after AddToSession and before
// writing to the connection.
func (s *State) SetPublishEventHandler(handler session.PublishEventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishEvents = handler
}

// SetDebugLogger takes an instance of the paho Logger interface
// and sets it to be used by the debug log endpoint
func (s *State) SetDebugLogger(l paholog.Logger) {
	s.debug = l
}

// SetErrorLogger takes an instance of the paho Logger interface
// and sets it to be used by the error log endpoint
func (s *State) SetErrorLogger(l paholog.Logger) {
	s.errors = l
}

// Close closes the session state
func (s *State) Close() error {
	s.mu.Lock()
	removed, err := s.connectionLost(nil) // Connection may be up in which case we need to cleanup.
	for packetID, cg := range s.clientPackets {
		removed = append(removed, cg) // Want to notify without holding mu
		delete(s.clientPackets, packetID)
	}
	s.mu.Unlock()
	notifyRemoved(removed)
	return err
}

// ConAckReceived will be called when the client receives a CONACK that indicates the connection has been successfully
// established. This indicates that a new connection is live and the passed in connection should be used going forward.
// It is also the trigger to resend any queued messages. Note that this function should not be called concurrently with
// others (we should not begin sending/receiving packets until after the CONACK has been processed).
// TODO: Add errors() function so we can notify the client of errors whilst transmitting the session stuff?
func (s *State) ConAckReceived(conn io.Writer, cp *packets.Connect, ca *packets.Connack) error {
	s.mu.Lock()
	removed, err := s.conAckReceived(conn, cp, ca)
	if err != nil {
		s.clearConnection()
	}
	s.mu.Unlock()
	notifyRemoved(removed)
	return err
}

// conAckReceived handles the transition of state caused by the receipt of CONNACK. Returns any packets removed
// from state.
// The caller must hold s.mu.
func (s *State) conAckReceived(conn io.Writer, cp *packets.Connect, ca *packets.Connack) ([]clientGenerated, error) {
	// We could use cp.Properties.SessionExpiryInterval /  ca.Properties.SessionExpiryInterval to clear the session
	// after the specified time period (if the Session Expiry Interval is absent the value in the CONNECT Packet used)
	// however, this is not something the generic client can really achieve (forks may wish to do this!).
	var removed []clientGenerated
	s.waitForSessionLossLocked()
	if err := s.resetPendingStores(); err != nil {
		return nil, fmt.Errorf("failed to complete pending session cleanup: %w", err)
	}
	if s.conn != nil {
		s.errors.Println("ConAckReceived called whilst connection active (you MUST call ConnectionLost before starting a new connection")
		var err error
		removed, err = s.connectionLost(nil) // assume the connection dropped
		if err != nil {
			return removed, fmt.Errorf("failed to clean up previous connection: %w", err)
		}
	}
	// If the Server accepts a connection with Clean Start set to 1, the Server MUST set Session Present to 0 in the
	// CONNACK packet in addition to setting a 0x00 (Success) Reason Code in the CONNACK packet [MQTT-3.2.2-2].
	// If the Server accepts a connection with Clean Start set to 0 and the Server has Session State for the ClientID,
	// it MUST set Session Present to 1 in the CONNACK packet, otherwise it MUST set Session Present to 0 in the CONNACK
	// packet. In both cases, it MUST set a 0x00 (Success) Reason Code in the CONNACK packet [MQTT-3.2.2-3].
	if !ca.SessionPresent {
		s.debug.Println("no session present - cleaning session state")
		cleaned, err := s.clean(&packets.ControlPacket{FixedHeader: packets.FixedHeader{Type: packets.CONNACK}, Content: ca})
		removed = append(removed, cleaned...)
		if err != nil {
			return removed, fmt.Errorf("failed to clean session state: %w", err)
		}
	}
	// Make the connection available only after s.clean has finished reading stored
	// packets and reporting session loss. Until then, AddToSession must reject new publications.
	s.conn = conn
	s.connCtx, s.connCtxCancel = context.WithCancel(context.Background())
	inFlight := uint16(len(s.clientPackets))
	s.debug.Printf("%d inflight transactions upon connection", inFlight)

	// "If the Session Expiry Interval is absent, the Session Expiry Interval in the CONNECT packet is used."
	// If the Session Expiry Interval is absent the value 0 is used. If it is set to 0, or is absent, the Session ends
	// when the Network Connection is closed (3.1.2.11.2).
	if ca.Properties != nil && ca.Properties.SessionExpiryInterval != nil {
		s.sessionExpiryInterval = *ca.Properties.SessionExpiryInterval
	} else if cp.Properties != nil && cp.Properties.SessionExpiryInterval != nil {
		s.sessionExpiryInterval = *cp.Properties.SessionExpiryInterval
	} else {
		s.sessionExpiryInterval = 0
	}

	// If clientPackets already exists, we re-use it so that the responseChan survives the reconnection
	// the map will be populated/repopulated when we retransmit the messages.
	if s.clientPackets == nil {
		s.clientPackets = make(map[uint16]clientGenerated) // This will be populated whilst packets are resent
	}

	if s.serverPackets == nil || s.serverStoreNeedsReload {
		// A storage error may have changed persistent state or made the store unusable.
		// Recover its inventory before accepting a connection that could deliver messages.
		if err := s.loadServerSession(ca); err != nil {
			return removed, fmt.Errorf("failed to load server session: %w", err)
		}
	}

	// As per section 4.9 "The send quota and Receive Maximum value are not preserved across Network Connections"
	recvMax := uint16(65535) // Default as per MQTT spec
	if ca.Properties != nil && ca.Properties.ReceiveMaximum != nil {
		recvMax = *ca.Properties.ReceiveMaximum
	}
	s.inflight = newSendQuota(recvMax)

	// Now we need to resend any packets in the store; this must happen in order, the simplest approach is to complete
	// the sending them before returning.
	toResend, err := s.clientStore.List()
	if err != nil {
		return removed, fmt.Errorf("failed to load stored message ids: %w", err)
	}
	// A final acknowledgement ends the exchange even if deleting its stored packet failed.
	// Retry cleanup before replaying anything; acknowledged packets must not be retransmitted
	// (including negative PUBACK/PUBREC responses, as required by MQTT-4.4.0-2).
	for packetID, transaction := range s.clientPackets {
		if transaction.pendingResponse == nil {
			continue
		}
		if i := slices.Index(toResend, packetID); i >= 0 {
			if err := s.clientStore.Delete(packetID); err != nil {
				return removed, fmt.Errorf("failed to remove acknowledged message %d from store: %w", packetID, err)
			}
			toResend = slices.Delete(toResend, i, i+1)
		}
		complete(transaction, *transaction.pendingResponse)
		delete(s.clientPackets, packetID)
	}
	s.debug.Printf("retransmitting %d messages", len(toResend))
	for _, id := range toResend {
		s.debug.Printf("resending message ID %d", id)
		r, err := s.clientStore.Get(id)
		if err != nil {
			return removed, fmt.Errorf("failed to load packet %d from client store: %w", id, err)
		}

		// DUP needs to be set when resending PUBLISH
		// Read/parse the full packet, so we can detect corruption (e.g. 0 byte file)
		p, err := packets.ReadPacket(r)
		if cErr := r.Close(); cErr != nil {
			s.errors.Printf("failed to close stored client packet %d: %s", id, cErr)
		}
		if err != nil { // If the packet cannot be read, we quarantine it; otherwise we may retry infinitely.
			if err := s.clientStore.Quarantine(id); err != nil {
				return removed, fmt.Errorf("failed to quarantine packet %d from client store: %w", id, err)
			}
			s.errors.Printf("failed to retrieve/parse packet %d from client store: %s", id, err)
			continue
		}

		switch p.Type {
		case packets.PUBLISH:
			pub := p.Content.(*packets.Publish)
			pub.Duplicate = true
		case packets.PUBREL:
		default:
			if err := s.clientStore.Quarantine(id); err != nil {
				return removed, fmt.Errorf("failed to quarantine packet %d from client store: %w", id, err)
			}
			s.errors.Printf("unexpected packet type %d (for packet identifier %d) in client store", p.Type, id)
			continue
		}

		// The messages being retransmitted form part of the "send quota"; however, as per the V5 spec,
		// the limit does not apply to messages being resent (the quota can go under 0)
		s.inflight.Retransmit() // This will never block (but is needed to block new messages)

		// Any failure from this point should result in loss of connection (so fatal)
		if s.publishEvents.BeforeSend != nil {
			handler, connCtx := s.publishEvents, s.connCtx
			s.mu.Unlock() // Holding mu whist running application code is likely to cause issues
			p = handler.PrepareSend(p)
			s.mu.Lock()
			if s.connCtx != connCtx { // indicates connection loss whist handling callbacks
				return removed, session.ErrNoConnection
			}
		}
		if _, err := p.WriteTo(conn); err != nil {
			s.debug.Printf("retransmitting of identifier %d failed: %s", id, err)
			return removed, fmt.Errorf("failed to retransmit message (%d): %w", id, err)
		}
		s.debug.Printf("retransmitted message with identifier %d", id)
		// On initial connection, the packet needs to be added to our record of client-generated packets.
		if _, ok := s.clientPackets[id]; !ok {
			response := make(chan packets.ControlPacket, 1)
			s.clientPackets[id] = clientGenerated{
				packetType:   p.Type,
				responseChan: response, // Nothing will wait on this
			}
		}
	}
	return removed, nil
}

// loadServerSession loads the server session state when first connecting or
// reconnecting after an inbound storage mutation failed.
// The caller must hold a lock on s.mu
func (s *State) loadServerSession(ca *packets.Connack) error {
	serverPackets := make(map[uint16]byte)
	ids, err := s.serverStore.List()
	if err != nil {
		return fmt.Errorf("failed to load stored server message ids: %w", err)
	}
	for _, id := range ids {
		r, err := s.serverStore.Get(id)
		if err != nil {
			return fmt.Errorf("failed to load packet %d from server store: %w", id, err)
		}
		// We only need to know the packet type, so there is no need to process the entire packet
		byte1 := make([]byte, 1)
		_, err = r.Read(byte1)
		_ = r.Close()
		if err != nil {
			if err := s.serverStore.Quarantine(id); err != nil {
				return fmt.Errorf("failed to quarantine packet %d from server store (failed to read): %w", id, err)
			}
			s.errors.Printf("packet %d from server store could not be read: %s", id, err)
			continue // don't want to fail, so quarantine and continue is the best we can do
		}
		packetType := byte1[0] >> 4
		switch packetType {
		case packets.PUBLISH:
			serverPackets[id] = packets.PUBLISH
		case packets.PUBREC:
			serverPackets[id] = packets.PUBREC
		default:
			if err := s.serverStore.Quarantine(id); err != nil {
				return fmt.Errorf("failed to quarantine packet %d from server store: %w", id, err)
			}
			s.errors.Printf("packet %d from server store had unexpected type %d", id, packetType)
			continue // don't want to fail, so quarantine and continue is the best we can do
		}
	}
	s.serverPackets = serverPackets
	s.serverStoreNeedsReload = false
	return nil
}

// ConnectionLost will be called when the connection is lost; either because we received a DISCONNECT packet or due
// to a network error (`nil` will be passed in)
func (s *State) ConnectionLost(dp *packets.Disconnect) error {
	s.mu.Lock()
	removed, err := s.connectionLost(dp)
	s.mu.Unlock()
	notifyRemoved(removed)
	return err
}

// connectionLost process loss of connection
// Caller MUST have locked c.Mu
func (s *State) connectionLost(dp *packets.Disconnect) ([]clientGenerated, error) {
	s.waitForSessionLossLocked()
	if s.conn == nil {
		// ConnectionLost may be called multiple times. A later call is also an opportunity to finish a store reset that
		// failed while the connection was being torn down.
		return nil, s.resetPendingStores()
	}
	s.clearConnection()

	if dp != nil && dp.Properties != nil && dp.Properties.SessionExpiryInterval != nil {
		s.sessionExpiryInterval = *dp.Properties.SessionExpiryInterval
	}
	// The Client and Server MUST store the Session State after the Network Connection is closed if the Session Expiry
	// Interval is greater than 0 [MQTT-3.1.2-23]
	if s.sessionExpiryInterval == 0 {
		s.debug.Println("sessionExpiryInterval is 0 and connection lost - cleaning session state")
		var trigger *packets.ControlPacket
		if dp != nil {
			trigger = &packets.ControlPacket{FixedHeader: packets.FixedHeader{Type: packets.DISCONNECT}, Content: dp}
		}
		return s.clean(trigger)
	}
	return s.tidy(), nil
}

// clearConnection marks the current network connection as unavailable. The old quota is retained while disconnected so
// acknowledgements already read from that connection can safely unwind transactions. A successful CONNACK replaces it.
// The caller must hold s.mu.
func (s *State) clearConnection() {
	if s.connCtxCancel != nil {
		s.connCtxCancel()
	}
	s.conn, s.connCtx, s.connCtxCancel = nil, nil, nil
	s.connectionLostAt = time.Now()
}

// AddToSession adds a packet to the session state (including allocation of a Message Identifier).
// If this function returns a nil error then:
//   - A slot has been allocated if the packet is a PUBLISH (function will block if RECEIVE MAXIMUM messages are inflight)
//   - A message Identifier has been added to the passed in packet
//   - Publish messages will have been written to the store (and will be automatically transmitted if a new connection
//     is established before the message is fully acknowledged - subject to state rules in the MQTTv5 spec)
//   - A state-owned response channel is returned. Exactly one value will be sent when either the message is fully
//     acknowledged or the packet is removed from the session (in which case the zero value will be sent), after which
//     the channel will be closed.
//
// On error the response channel is nil and runtime allocations are released.
// Persistent stores can report uncertain writes; follow their recovery requirements before reusing the session.
func (s *State) AddToSession(ctx context.Context, packet session.Packet) (<-chan packets.ControlPacket, error) {
	response := make(chan packets.ControlPacket, 1)
	if err := s.addToSession(ctx, packet, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *State) addToSession(ctx context.Context, packet session.Packet, response chan packets.ControlPacket) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock() // There may be a delay waiting for semaphore, so check for connection before and after
	if s.conn == nil {
		s.mu.Unlock()
		return session.ErrNoConnection
	}
	// If the connection is lost whilst we are waiting, then in Acquire should terminate.
	connCtx := s.connCtx
	// The send quota belongs to the connection identified by connCtx above, and is
	// replaced by ConAckReceived when a new connection is established. Take a
	// reference to it here, under the mutex, rather than reading s.inflight later:
	// Acquire may block, so it cannot be called while holding the mutex, and reading
	// the field after unlocking races a reconnection replacing it.
	inflight := s.inflight
	s.mu.Unlock()

	// If the connection drops while waiting, we should abort
	go func() {
		select {
		case <-connCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	pt := packet.Type()

	// Ensure only "RECEIVE MAXIMUM" PUBLISH transactions are in flight at any time
	if pt == packets.PUBLISH {
		if err := inflight.Acquire(ctx); err != nil {
			if connCtx.Err() != nil {
				return session.ErrNoConnection
			}
			return err // Allow user to confirm if it was their context that led to termination
		}
	}

	// We need the mutex again, so we recheck the connection as this may have been replaced.
	s.mu.Lock()
	if s.conn == nil || s.connCtx != connCtx {
		s.mu.Unlock()
		if pt == packets.PUBLISH {
			if qErr := inflight.Release(); qErr != nil {
				s.errors.Printf("quota release due to connection loss: %s", qErr)
			}
		}
		return session.ErrNoConnection
	}

	// We have a slot, so acquire a Message ID (error on failure, there may be a better option?)
	packetID, err := s.allocateNextPacketIdLocked(pt, response)
	if err != nil {
		s.mu.Unlock()
		if pt == packets.PUBLISH {
			if qErr := inflight.Release(); qErr != nil {
				s.errors.Printf("quota release due to packet id issue: %s", qErr)
			}
		}
		return err
	}
	packet.SetIdentifier(packetID)
	if pt == packets.PUBLISH {
		if err = s.clientStore.Put(packetID, pt, packet); err != nil {
			delete(s.clientPackets, packetID)
			s.mu.Unlock()
			if qErr := inflight.Release(); qErr != nil {
				s.errors.Printf("quota release due to store issue: %s", qErr)
			}
			packet.SetIdentifier(0) // ensure the Message identifier is not used
			return err
		}
	}
	s.mu.Unlock()
	return nil
}

// PacketNotSent releases an unsent subscription request. Only subscription
// requests can be removed; persisted publishes may already have been retransmitted
// independently by session recovery.
func (s *State) PacketNotSent(packetID uint16, response <-chan packets.ControlPacket) {
	s.mu.Lock()
	transaction, ok := s.clientPackets[packetID]
	if !ok || response == nil || transaction.responseChan != response ||
		(transaction.packetType != packets.SUBSCRIBE && transaction.packetType != packets.UNSUBSCRIBE) {
		s.mu.Unlock()
		return
	}
	delete(s.clientPackets, packetID)
	s.mu.Unlock()
	complete(transaction, packets.ControlPacket{})
}

// endClientGenerated should be called when a client-generated transaction has been fully acknowledged
// (or if, due to connection loss, it will never be acknowledged).
func (s *State) endClientGenerated(packetID uint16, recv *packets.ControlPacket) error {
	s.mu.Lock()
	handler := s.publishEvents
	var event *session.PublishEvent
	defer func() {
		s.mu.Unlock()
		if event != nil {
			handler.OnEvent(*event)
		}
	}()
	if cg, ok := s.clientPackets[packetID]; ok {
		if cg.pendingResponse != nil {
			// Cleanup retries must preserve the original acknowledgement and must
			// not report completion again, even if the stored packet is already gone.
			recv = cg.pendingResponse
		} else if handler.OnEvent != nil && (cg.packetType == packets.PUBLISH || cg.packetType == packets.PUBREL) {
			// Read application metadata before Delete can remove the source, including
			// when the deletion is applied but reports a persistence error.
			e := s.publishEventLocked(handler, session.PublishEventComplete, packetID, recv)
			event = &e
		}
		// Outgoing publish messages will be in the store (replaced with PUBREL that is sent)
		if cg.packetType == packets.PUBLISH || cg.packetType == packets.PUBREL {
			if err := s.clientStore.Delete(packetID); err != nil {
				// Notify the handler of the acknowledgement and cleanup error, but
				// retain the response and identifier until persistence cleanup succeeds.
				pendingResponse := *recv
				cg.pendingResponse = &pendingResponse
				s.clientPackets[packetID] = cg
				err = fmt.Errorf("failed to remove message %d from store: %w", packetID, err)
				if event != nil {
					event.Err = errors.Join(event.Err, err)
				}
				return err
			}
			if s.inflight != nil {
				if qErr := s.inflight.Release(); qErr != nil {
					s.errors.Printf("quota release due to %s: %s", recv.PacketType(), qErr)
				}
			}
		}
		complete(cg, *recv)
		delete(s.clientPackets, packetID)
	} else {
		s.debug.Println("received a response for a message ID we don't know:", recv.PacketID())
	}
	return nil
}

// Ack is called when the client message handlers have completed (or, if manual acknowledgements are enabled, when
// `client.ACK()` has been called - this may happen some time after the message was received and it is conceivable that
// the connection may have been dropped and reestablished in the interim).
// See issue 160 re issues when the State is called after the connection is dropped. We assume that the
// user will ensure that all ACK's are completed before the State is applied to a new connection (not doing
// this may have unpredictable results).
func (s *State) Ack(pb *packets.Publish) error {
	return s.ack(pb)
}

// ack sends an acknowledgment of the `PUBLISH` (which will have been received from the server)
// `s.mu` must NOT be locked when this is called.
// Note: Adding properties to the response is not currently supported. If this functionality is added, then it is
// important to note that QOS2 PUBREC's may be resent if a duplicate `PUBLISH` is received.
// Storage or communication errors must terminate the connection.
func (s *State) ack(pb *packets.Publish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	switch pb.QoS {
	case 1:
		pa := packets.Puback{
			Properties: &packets.Properties{},
			PacketID:   pb.PacketID,
		}
		if s.conn != nil {
			s.debug.Println("sending PUBACK")
			_, err = pa.WriteTo(s.conn)
			if err != nil {
				s.errors.Printf("failed to send PUBACK for %d: %s", pb.PacketID, err)
			}
		} else {
			s.debug.Println("PUBACK not send because connection down")
		}
		// We don't store outbound PUBACK. The server will retransmit the PUBLISH if the connection is reestablished
		// before it receives the ACK. Unfortunately, there is no definitive way to determine if
		// such messages are duplicates or not (so we are forced to treat them all as if they are new).
	case 2:
		pr := packets.Pubrec{
			Properties: &packets.Properties{},
			PacketID:   pb.PacketID,
		}
		// Persist duplicate suppression before acknowledging ownership. Otherwise a restart after PUBREC could deliver
		// the same QoS 2 message again, contrary to MQTT-4.3.3-10.
		// A duplicate can reuse a known persisted PUBREC without another write, which could fail even though the
		// required state already exists. Only trust the cached state if no storage mutation has failed.
		if !pb.Duplicate || s.serverPackets[pb.PacketID] != packets.PUBREC || s.serverStoreNeedsReload {
			cp := pr.ToControlPacket()
			if err := s.serverStore.Put(pb.PacketID, packets.PUBREC, cp); err != nil {
				s.serverStoreNeedsReload = true
				return fmt.Errorf("failed to store PUBREC for %d: %w", pb.PacketID, err)
			}
			s.serverPackets[pb.PacketID] = cp.Type
		}
		if s.conn != nil {
			s.debug.Printf("sending PUBREC")
			_, err = pr.WriteTo(s.conn)
			if err != nil {
				s.errors.Printf("failed to send PUBREC for %d: %s", pb.PacketID, err)
			}
		} else {
			s.debug.Println("PUBREC not send because connection down")
		}
	default:
		err = errors.New("ack called but publish not QOS 1 or 2")
	}
	return err
}

// PacketReceived should be called whenever one of the following is received:
// `PUBLISH` (QOS1+ only), `PUBACK`, `PUBREC`, `PUBREL`, `PUBCOMP`, `SUBACK`, `UNSUBACK`
// It will handle sending any neccessary response or passing the message to the client.
// pubChan will be sent a `PUBLISH` if applicable (and a receiver must be active whilst this function runs)
func (s *State) PacketReceived(recv *packets.ControlPacket, pubChan chan<- *packets.Publish) error {
	// Note: we do a type switch rather than using the packet type because it's safer and easier to understand
	switch rp := recv.Content.(type) {
	//
	// Packets in response to client-generated transactions
	//
	case *packets.Suback: // Not in store, just need to advise client and free Message Identifier
		s.debug.Println("received SUBACK packet with id ", rp.PacketID)
		return s.endClientGenerated(rp.PacketID, recv)
	case *packets.Unsuback: // Not in store, just need to advise client and free Message Identifier
		s.debug.Println("received UNSUBACK packet with id ", rp.PacketID)
		return s.endClientGenerated(rp.PacketID, recv)
	case *packets.Puback: // QOS 1 initial (and final) response
		s.debug.Println("received PUBACK packet with id ", rp.PacketID)
		return s.endClientGenerated(rp.PacketID, recv)
	case *packets.Pubrec: // Initial response to a QOS2 Publish
		return s.pubrecReceived(recv)
	case *packets.Pubcomp: // QOS 2 final response
		s.debug.Printf("received PUBCOMP packet with id %d", rp.PacketID)
		return s.endClientGenerated(rp.PacketID, recv)
		//
		// Packets relating to server generated PUBLISH
		//
	case *packets.Publish:
		s.debug.Printf("received QoS%d PUBLISH", rp.QoS)
		// There is no need to store the packet because it will be resent if not acknowledged before the connection is
		// reestablished.
		if rp.QoS > 0 {
			if rp.PacketID == 0 { // Invalid
				return fmt.Errorf("received QOS %d PUBLISH with 0 PacketID", rp.QoS)
			}
			if rp.QoS == 2 {
				s.mu.Lock()
				if lastSent, ok := s.serverPackets[rp.PacketID]; ok {
					// If we have sent a PUBREC, that means that the client has already seen this message, so we can
					// simply resend the acknowledgment.
					if lastSent == packets.PUBREC {
						// If the message is not flagged as a duplicate, then something is wrong; to avoid message loss,
						// we will treat this as a new message (because it appears there is a session mismatch, and we
						// have not actually seen this message).
						if rp.Duplicate {
							// The client has already seen this message meaning we do not want to resend it and, instead
							// immediately acknowledge it.
							s.mu.Unlock() // mu must be unlocked to call ack
							return s.ack(rp)
						}
						s.errors.Printf("received duplicate PUBLISH (%d) but dup flag not set (will assume this overwrites old publish)", rp.PacketID)
					} else {
						s.errors.Printf("received PUBLISH (%d) but lastSent type is %d (unexpected!)", lastSent)
					}
				}
				s.mu.Unlock()
			}
		}
		pubChan <- rp // the message will be passed to router (and thus the end user app)
		return nil
	case *packets.Pubrel:
		s.debug.Println("received PUBREL for", recv.PacketID())
		// Auto respond to pubrels unless failure code
		pr := recv.Content.(*packets.Pubrel)
		if pr.ReasonCode >= 0x80 {
			// Received a failure code meaning the server does not know about the message (so all we can do is to remove
			// it from our store).
			s.errors.Printf("received PUBREL with reason code %d ", pr.ReasonCode)
			return nil
		} else {
			pc := packets.Pubcomp{
				PacketID: pr.PacketID,
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.debug.Println("sending PUBCOMP for", pr.PacketID)
			var err error
			if s.conn != nil {
				// Persist completion before PUBCOMP makes this identifier reusable by the sender. An unknown PUBREL
				// must still receive PUBCOMP (MQTT-4.3.3-11).
				if _, known := s.serverPackets[pr.PacketID]; known {
					if err := s.serverStore.Delete(pr.PacketID); err != nil {
						s.serverStoreNeedsReload = true
						return fmt.Errorf("failed to remove message %d from server store: %w", pr.PacketID, err)
					}
					delete(s.serverPackets, pr.PacketID)
				}
				_, err = pc.WriteTo(s.conn)
				if err != nil {
					s.errors.Printf("failed to send PUBCOMP for %d: %s", pc.PacketID, err)
				}
			}
			// If the connection is down, retain state for the PUBREL resent upon reconnect.
			return err
		}
	default:
		s.errors.Printf("State.PacketReceived received unexpected packet: %#v ", rp)
		return nil
	}
}

// allocateNextPacketId assigns the next available packet ID
// Callers must NOT hold lock on s.mu
func (s *State) allocateNextPacketId(forPacketType byte) (uint16, <-chan packets.ControlPacket, error) {
	response := make(chan packets.ControlPacket, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	packetID, err := s.allocateNextPacketIdLocked(forPacketType, response)
	if err != nil {
		return 0, nil, err
	}
	return packetID, response, nil
}

// allocateNextPacketIdLocked assigns the next available packet ID.
// The caller must hold s.mu.
func (s *State) allocateNextPacketIdLocked(forPacketType byte, response chan packets.ControlPacket) (uint16, error) {
	cg := clientGenerated{
		packetType:   forPacketType,
		responseChan: response,
	}

	// Scan from lastMid to end of range.
	for i := s.lastMid + 1; i != 0; i++ {
		if _, ok := s.clientPackets[i]; ok {
			continue
		}
		s.clientPackets[i] = cg
		s.lastMid = i
		return i, nil
	}

	// Default struct will set s.lastMid=0 meaning we have already scanned all mids
	if s.lastMid == 0 {
		s.lastMid = 1
		return 0, session.ErrPacketIdentifiersExhausted
	}

	// Scan from start of range to lastMid (use +1 to avoid rolling over when s.lastMid = 65535)
	for i := uint16(0); i < s.lastMid; i++ {
		if _, ok := s.clientPackets[i+1]; ok {
			continue
		}
		s.clientPackets[i+1] = cg
		s.lastMid = i + 1
		return i + 1, nil
	}
	return 0, session.ErrPacketIdentifiersExhausted
}

// clean clears the session state and invokes any configured session-loss callbacks.
// Returned transactions must be passed to notifyRemoved to unblock their waiting callers.
// Does not touch inflight because this is not part of the session state (so is reset separately)
// caller is responsible for locking s.mu and making the connection unavailable.
// mu is temporarily released for callbacks; other connection transitions must wait for sessionLossDone before accessing
// the stores.
func (s *State) clean(trigger *packets.ControlPacket) ([]clientGenerated, error) {
	s.debug.Println("State.clean() called")

	// If there is an OnEvent handler we will need to notify it of transactions removed from the state
	handler := s.publishEvents
	var ids []uint16
	var listErr error
	if handler.OnEvent != nil {
		// The callback needs to be called with the
		ids, listErr = s.sessionLostPacketIDsLocked()
		s.sessionLossDone = make(chan struct{})
		defer func() {
			close(s.sessionLossDone)
			s.sessionLossDone = nil
		}()
	}
	removed := make([]clientGenerated, 0, len(s.clientPackets))
	for packetID, p := range s.clientPackets {
		if p.pendingResponse != nil {
			// Retain acknowledged transactions until Reset succeeds, including across retries.
			continue
		}
		removed = append(removed, p)
		delete(s.clientPackets, packetID)
	}
	s.serverPackets = make(map[uint16]byte)

	// Mark both stores for reset. Cleanup is attempted below; any failed reset must succeed before a subsequent
	// connection can reuse the stores. These flags are not persisted. If the process exits before cleanup succeeds,
	// a new State will have no record that the remaining stored packets must be discarded.
	s.serverStoreResetPending = true
	s.clientStoreResetPending = true

	// Notify the handler of removed transactions
	if handler.OnEvent != nil {
		// As with other completion events, release blocked callers before invoking
		// application callbacks. The cleanup gate protects the stores while mu is free.
		s.mu.Unlock()
		notifyRemoved(removed)
		s.mu.Lock()
		removed = nil
		if listErr != nil {
			s.notifyPublishEventLocked(handler, session.PublishEvent{
				Type: session.PublishEventSessionLost, Trigger: trigger,
				Err: fmt.Errorf("listing discarded publications: %w", listErr),
			})
		}
		for _, id := range ids {
			event := s.publishEventLocked(handler, session.PublishEventSessionLost, id, trigger)
			s.notifyPublishEventLocked(handler, event)
		}
	}

	// complete cleanup
	err := s.resetPendingStores()
	if err != nil && handler.OnEvent != nil {
		// Earlier events have already been delivered, so report a the reset failure separately instead of retaining
		// the source packets.
		s.notifyPublishEventLocked(handler, session.PublishEvent{
			Type: session.PublishEventSessionLost, Trigger: trigger, Err: err,
		})
	}
	return removed, err
}

// resetPendingStores completes any store cleanup required by a prior connection state change. A store remains
// pending until Reset succeeds, and ConAckReceived will not load or retransmit session state while either store is pending.
// The caller must hold s.mu.
func (s *State) resetPendingStores() error {
	var serverErr, clientErr error
	if s.serverStoreResetPending {
		if err := s.serverStore.Reset(); err != nil {
			serverErr = fmt.Errorf("failed to reset server store: %w", err)
		} else {
			s.serverStoreResetPending = false
			s.serverStoreNeedsReload = false
		}
	}
	if s.clientStoreResetPending {
		if err := s.clientStore.Reset(); err != nil {
			clientErr = fmt.Errorf("failed to reset client store: %w", err)
		} else {
			s.clientStoreResetPending = false
			// There may be transactions that have a pendingResponse due to an earlier failure - clear these
			for packetID, transaction := range s.clientPackets {
				if transaction.pendingResponse != nil {
					complete(transaction, *transaction.pendingResponse)
					delete(s.clientPackets, packetID)
				}
			}
		}
	}
	return errors.Join(serverErr, clientErr)
}

// tidy removes transactions that are scoped to one network connection while
// retaining the MQTT session state that should survive reconnection.
// returns the packets that were removed.
// as per section 4.1 in the spec; The Session State in the Client consists of:
// > · QoS 1 and QoS 2 messages which have been sent to the Server, but have not been completely acknowledged.
// > · QoS 2 messages which have been received from the Server, but have not been completely acknowledged.
// This means that we keep PUBLISH, PUBREC and PUBREL packets. PUBACK and PUBCOMP will not be stored (the MID
// will be free once they have been sent). PUBREC is retained so we can check newly received PUBLISH messages (and
// confirm if they have already been processed).
// We only resend PUBLISH (where QoS > 0) and PUBREL packets (as per spec section 4.4)
// caller is responsible for locking s.mu
func (s *State) tidy() []clientGenerated {
	s.debug.Println("State.tidy() called")
	for id, p := range s.serverPackets {
		switch p {
		case packets.PUBREC:
			// For inbound messages, we only retain `PUBREC` messages so that we can determine if a PUBLISH received has
			// already been processed (the `PUBREL` will be sent when the message has been processed by our user).
			// The broker will resend any `PUBLISH` and `PUBREL` messages, so there is no need to retain those.
		default:
			delete(s.serverPackets, id)
			if err := s.serverStore.Delete(id); err != nil {
				s.serverStoreNeedsReload = true
				s.errors.Printf("failed to remove server packet %d while tidying session: %s", id, err)
			}
		}
	}

	var removed []clientGenerated
	for id, p := range s.clientPackets {
		// We only need to remember PUBLISH and PUBREL messages (both originating from a PUBLISH).
		if p.packetType == packets.PUBLISH || p.packetType == packets.PUBREL {
			continue
		}
		delete(s.clientPackets, id)
		removed = append(removed, p)
	}
	return removed
}

// publishEventLocked creates a PublishEvent for packetID. When requested, it loads the stored packet into Source,
// recording any error in Err. Header mode currently uses the full decoder; callers must not rely on the payload.
// The caller must hold s.mu.
func (s *State) publishEventLocked(handler session.PublishEventHandler, kind session.PublishEventType, id uint16, trigger *packets.ControlPacket) session.PublishEvent {
	e := session.PublishEvent{Type: kind, PacketID: id, Trigger: trigger}
	if handler.ReadStoredPacket == session.ReadStoredNone {
		return e
	}
	r, err := s.clientStore.Get(id)
	if err != nil {
		e.Err = fmt.Errorf("loading publish event source %d: %w", id, err)
		return e
	}
	p, readErr := packets.ReadPacket(r)
	if err = errors.Join(readErr, r.Close()); err != nil {
		e.Err = fmt.Errorf("reading publish event source %d: %w", id, err)
		return e
	}
	if (p.Type != packets.PUBLISH && p.Type != packets.PUBREL) || p.PacketID() != id {
		e.Err = fmt.Errorf("unexpected stored packet for publish event %d: %s (id %d)", id, p.PacketType(), p.PacketID())
		return e
	}
	e.Source = p
	return e
}

// sessionLostPacketIDsLocked returns packet IDs for session-loss notifications.
// It combines IDs from clientStore and outgoing PUBLISH/PUBREL entries in clientPackets, without duplicates.
// Acknowledged transactions awaiting persistence cleanup have already reported their final outcome.
// Any store listing error is also returned. The caller must hold s.mu.
func (s *State) sessionLostPacketIDsLocked() ([]uint16, error) {
	ids, err := s.clientStore.List()
	ids = slices.DeleteFunc(ids, func(id uint16) bool {
		return s.clientPackets[id].pendingResponse != nil
	})
	seen := make(map[uint16]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for id, cg := range s.clientPackets {
		if cg.pendingResponse == nil && !seen[id] && (cg.packetType == packets.PUBLISH || cg.packetType == packets.PUBREL) {
			ids = append(ids, id)
		}
	}
	return ids, err
}

// waitForSessionLossLocked waits for any ongoing session-loss notifications and store cleanup to finish.
// It releases s.mu while waiting and reacquires it before returning.
// This must be called before a connection transition accesses or changes session state or its stores.
// The caller must hold s.mu.
func (s *State) waitForSessionLossLocked() {
	for s.sessionLossDone != nil {
		done := s.sessionLossDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
	}
}

// notifyPublishEventLocked temporarily unlocks s.mu and calls handler.OnEvent. The caller must hold s.mu and ensure
// the session state remains protected while the callback runs.
func (s *State) notifyPublishEventLocked(handler session.PublishEventHandler, event session.PublishEvent) {
	s.mu.Unlock()
	defer s.mu.Lock()
	handler.OnEvent(event)
}

// pubrecReceived handles a PUBREC for an outgoing QoS 2 publication.
// A rejection completes the transaction. On the first successful PUBREC, it
// invokes the configured callbacks and stores PUBREL before sending it.
// Repeated successful PUBRECs resend PUBREL without replacing the stored packet.
// An unknown packet ID receives PUBREL with reason code 0x92 (Packet Identifier not found).
func (s *State) pubrecReceived(recv *packets.ControlPacket) error {
	rp := recv.Content.(*packets.Pubrec)
	s.mu.Lock()
	cg, ok := s.clientPackets[rp.PacketID]
	conn, connCtx := s.conn, s.connCtx
	handler := s.publishEvents
	if !ok || (cg.packetType != packets.PUBLISH && cg.packetType != packets.PUBREL) {
		// either we don't know about this packetID or it's not PUBLISH related (note that we don't currently confirm
		// that it's a QOS2 transaction). Respond with "Packet Identifier not found"
		s.mu.Unlock()
		if conn == nil {
			return session.ErrNoConnection
		}
		outgoing := &packets.ControlPacket{
			FixedHeader: packets.FixedHeader{Type: packets.PUBREL, Flags: 2},
			Content:     &packets.Pubrel{PacketID: rp.PacketID, ReasonCode: 0x92},
		}
		_, err := handler.PrepareSend(outgoing).WriteTo(conn)
		return err
	}
	// PUBREC reason code >= 0x80 rejects the publication and completes the transaction; no PUBREL is sent.
	if rp.ReasonCode >= 0x80 {
		s.mu.Unlock()
		return s.endClientGenerated(rp.PacketID, recv)
	}

	pl := &packets.Pubrel{PacketID: rp.PacketID, Properties: &packets.Properties{}}
	outgoing := &packets.ControlPacket{FixedHeader: packets.FixedHeader{Type: packets.PUBREL, Flags: 2}, Content: pl}

	// We may need to call user function that can edit the outgoing packet
	prepare := cg.packetType == packets.PUBLISH && handler.PreparePubrel != nil
	if handler.OnEvent != nil || prepare || cg.packetType == packets.PUBREL {
		sourceHandler := handler
		if cg.packetType == packets.PUBREL {
			// Repeated PUBREC resends the persisted PUBREL, including its properties. This means the stored packet is
			// needed for the outgoing response below.
			sourceHandler.ReadStoredPacket = session.ReadStoredFull
		}
		event := s.publishEventLocked(sourceHandler, session.PublishEventPubrec, rp.PacketID, recv)
		if event.Source != nil {
			source := event.Source
			if source.Type != cg.packetType {
				event.Err = fmt.Errorf("stored packet type %d differs from transaction type %d", source.Type, cg.packetType)
				event.Source = nil
			} else if cg.packetType == packets.PUBREL {
				outgoing = source
			}
		}
		if handler.ReadStoredPacket == session.ReadStoredNone {
			event.Source = nil // Retransmission may require a store read even when callbacks opt out.
		}
		s.mu.Unlock()
		if prepare && event.Err == nil {
			var source *packets.Publish
			if event.Source != nil {
				source = event.Source.Content.(*packets.Publish)
			}
			handler.PreparePubrel(session.PubrelPreparation{Source: source, Trigger: rp, Outgoing: pl})
		}
		if handler.OnEvent != nil {
			handler.OnEvent(event)
		}
		s.mu.Lock()
		current, exists := s.clientPackets[rp.PacketID]
		if s.connCtx != connCtx || !exists || current.responseChan != cg.responseChan || current.packetType != cg.packetType {
			s.mu.Unlock()
			return session.ErrNoConnection
		}
		if event.Err != nil {
			s.mu.Unlock()
			return event.Err
		}
	}
	if cg.packetType == packets.PUBLISH {
		// MQTT-4.3.3-6 forbids sending PUBLISH again once PUBREL has been sent. Commit PUBREL, before transmitting.
		if err := s.clientStore.Put(rp.PacketID, packets.PUBREL, outgoing); err != nil {
			s.mu.Unlock()
			return err
		}
		cg.packetType = packets.PUBREL
		s.clientPackets[rp.PacketID] = cg
	}
	s.mu.Unlock()
	if conn == nil {
		return session.ErrNoConnection
	}
	_, err := handler.PrepareSend(outgoing).WriteTo(conn)
	return err
}

// notifyRemoved releases callers whose transactions were discarded during a connection or session transition.
// Notifications happen outside s.mu so a slow receiver cannot block all other state operations.
func notifyRemoved(removed []clientGenerated) {
	for _, p := range removed {
		complete(p, packets.ControlPacket{})
	}
}

// complete delivers the one response promised for a client-generated transaction. State-owned channels are buffered
// and closed after the send; caller-owned channels retain the original SessionManager behavior.
func complete(transaction clientGenerated, response packets.ControlPacket) {
	if transaction.responseChan == nil {
		return
	}
	transaction.responseChan <- response
	close(transaction.responseChan)
}
