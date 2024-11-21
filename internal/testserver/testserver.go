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
 *  SPDX-License-Identifier: EPL-2.0 OR BSD-3-Clause
 */

package testserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/packets"
)

// Implements a very limited (not standards compliant) MQTT server for use in tests.
//
// Testing an MQTT client without a server is difficult, using a real server complicates the setup (e.g. server must
// be available, how do we simulate disconnection, managing timing of ACKs, etc). A workaround is to implement limited
// MQTT server functions specifically for testing.
//
// `paho` goes some way towards this in `server_test.go` but that implementation only returns predetermined packets which
// makes testing persistence of the session state difficult. This implementation goes further and permits the client
// to subscribe to messages that it, itself, publishes.
//
// To simplify testing of the session state, the test server will automatically disconnect if it receives messages that
// are one of the following strings:
//
// * `CloseOnPublishReceived` - Disconnects when the `PUBLISH` message has been received (before any response) - Warning this will result in a loop if the client resends the message.
// * `CloseOnPubAckReceived` - Disconnects when the `PUBACK` message has been received (before any response)
// * `CloseOnPubRecReceived` - Disconnects when the `PUBREC` message has been received (before any response)
// * `CloseOnPubRelReceived` - Disconnects when the `PUBREL` message has been received (before any response)
//
// Limitations include (but are not limited to):
// * Only supports a single connection
// * ignores most options
// * does not adhere to the spec
// * relies on the paho `packets` package (so encoding/decoding of packets is not really tested when using this server).
//
// This is a work in progress that will evolve as persistent session state support is introduced in the client.

// Message bodies that trigger an action (note that after action completed `Done` will be appended)
const (
	CloseOnPublishReceived            = `CloseOnPublishReceived` // Disconnects when the `PUBLISH` message has been received (before any response)
	CloseOnPubAckReceived             = `CloseOnPubAckReceived`  // Disconnects when the `PUBACK` message has been received (before any response)
	CloseOnPubRecReceived             = `CloseOnPubRecReceived`  // Disconnects when the `PUBREC` message has been received (before any response)
	CloseOnPubRelReceived             = `CloseOnPubRelReceived`  // Disconnects when the `PUBREL` message has been received (before any response)
	CloseOnPubCompReceived            = `CloseOnPubCompReceived` // Disconnects when the `PUBREL` message has been received (before any response)
	AppendAfterActionProcessed        = `Done`                   // Appended to message body after action carried out (does not apply to Publish)
	midInitial                 uint16 = 200                      // Server side MIDs will start here (having different start points makes the logs easier to follow)
	midMax                     uint16 = 65535
	outgoingChanSize                  = 100 // Size of chan for outgoing packets (enables multiple packets to be in flight)
	delayBetweenOutdoing              = time.Millisecond
)

// Logger mirrors paho.Logger
type Logger interface {
	Println(v ...interface{})
	Printf(format string, v ...interface{})
}

// ErrorMIDsExhausted is returned from Request() when there are no
// free message ids to be used.
var ErrorMIDsExhausted = errors.New("all message ids in use")

type subscription struct {
	qos byte
}

// StateInfo holds state information relating to a message ID
type StateInfo struct {
	Sent *packets.ControlPacket // The most recent packet we sent with the relevant ID (will be resent on reconnection)
	// For publish related packets (included to simplify later operations)
	QOS     byte   // The QOS in the `PUBLISH`
	Topic   string // The topic from the original `PUBLISH` (if applicable)
	Payload []byte // The payload from `PUBLISH` (or nil for other messages)
}

// NewStateInfo creates a StateInfo
func NewStateInfo(sent *packets.ControlPacket, qos byte, topic string, payload []byte) *StateInfo {
	return &StateInfo{
		Sent:    sent,
		QOS:     qos,
		Topic:   topic,
		Payload: payload,
	}
}

// Instance an instance of the test server
// Note that many variables are not mutex protected (because they are private and only accessed from one goroutine)
type Instance struct {
	logger          Logger // Used to output status info to assist with debugging
	connected       atomic.Bool
	packetReceived  func(publish *packets.ControlPacket) error      // Will be called when a packet is received (return error to drop connection)
	overrideConnAck func(cp *packets.Connect, cap *packets.Connack) // Will be called before CONNECT response transmitted, cap can be modified

	// Below are not thread-safe (should only be accessed after checking connected)
	connPktDone           bool                    // true if we have processed a CONNECT packet (ever!)
	sessionPresent        bool                    // true if a session exists
	sessionExpiryInterval uint32                  // as set on the most recent `connect` (we treat anything >0 as infinite)
	subscriptions         map[string]subscription // Map from topic to subscription info

	// The serverSessionState will only ever contain PUBLISH messages because that is the only thing we initiate.
	serverSessionState map[uint16]*StateInfo // message id to the last thing we sent for messages originating here

	// When we, as the server, send a packet, we need to generate a unique ID
	serverMIDs *MIDs

	// The clientSessionState will only ever contain PUBLISH messages because all other messages are responded to immediately
	// (and there is no need to retain them)
	clientSessionState map[uint16]*StateInfo // message id to the last thing we sent for messages originating on the client
}

// New creates a new instance of the simulator
func New(logger Logger) *Instance {
	if logger == nil {
		l := log.New(io.Discard, "", 0) // Default to nil logger
		logger = l
	}
	return &Instance{
		logger:                logger,
		connected:             atomic.Bool{},
		connPktDone:           false,
		sessionPresent:        false,
		sessionExpiryInterval: 0,
		subscriptions:         make(map[string]subscription),
		serverSessionState:    make(map[uint16]*StateInfo),
		serverMIDs:            &MIDs{lastMid: midInitial, index: make([]bool, int(midMax))},
		clientSessionState:    make(map[uint16]*StateInfo),
	}
}

// Connected returns true if the test server has an active connection with the client
func (i *Instance) Connected() bool {
	return i.connected.Load()
}

// Connect establishes a connection to the test server
// Note that this can fail!
// Returns a net.Conn (to pass to paho), a channel that will be closed when connection has shutdown and
// an error (if any).
func (i *Instance) Connect(ctx context.Context) (net.Conn, chan struct{}, error) {
	if !i.connected.CompareAndSwap(false, true) {
		return nil, nil, errors.New("already connected") // We only support a single connection
	}
	i.connPktDone = false // Connection packet should be the first thing we receive after each connection

	userCon, ourCon, err := netPipe(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Ensure both connections support thread safe writes
	userCon = packets.NewThreadSafeConn(userCon)
	ourCon = packets.NewThreadSafeConn(ourCon) // Should not be necessary but may avoid hard to spot bugs

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return // shutdown via another mechanism
		case <-ctx.Done(): // Ensure that we exit cleanly if context closed
		}

		if err := ourCon.Close(); err != nil {
			i.logger.Printf("error closing ourConn: %s", err)
		}
	}()

	outGoingPackets := make(chan *packets.ControlPacket, outgoingChanSize)
	go func() {
		if err := i.handleIncoming(ourCon, outGoingPackets); err != nil {
			i.logger.Println("handleIncoming closed with error", err)
		} else {
			i.logger.Println("handleIncoming closed cleanly")
		}
		close(outGoingPackets)
	}()

	go func() {
		i.handleOutgoing(outGoingPackets, ourCon) // will return after outGoingPackets closed
		if err := ourCon.Close(); err != nil {    // Ensure the other end receives notification of the closure
			i.logger.Printf("error closing ourConn: %s", err)
		}
		i.connected.Store(false) // Must be true to reach this point...
		i.logger.Println("disconnected")
		close(done)
	}()

	i.logger.Println("connection up")
	return userCon, done, nil
}

// handleIncoming runs as a goroutine processing inbound data received on net.Conn until an error occurs (i.e. Conn Closed)
func (i *Instance) handleIncoming(conn io.Reader, out chan<- *packets.ControlPacket) error {
	for {
		p, err := packets.ReadPacket(conn, 5)
		if err != nil {
			r := bufio.NewReader(conn)
			time.Sleep(time.Millisecond) // Wait a millisecond for other data to come in (helps with debugging)
			buffSize := r.Buffered()
			remaining := make([]byte, buffSize)
			_, _ = r.Read(remaining)
			return fmt.Errorf("handleIncoming:ReadPacket: %w Remaining data: %v", err, remaining)
		}
		if err = i.processIncoming(p, out); err != nil {
			return fmt.Errorf("handleIncoming:processIncoming: %w", err)
		}
	}
}

// handleOutgoing runs as a goroutine and transmits data via the connection. Doing things this way should simplify
// future enhancements
func (i *Instance) handleOutgoing(in <-chan *packets.ControlPacket, out io.Writer) {
	for p := range in {
		i.logger.Println("Sending packet to client ", p)
		_, _ = p.WriteTo(out)            // We can just ignore any errors (Close will be picked up in handleIncoming, and the chan closed)
		time.Sleep(delayBetweenOutdoing) // Slow down responses to enable multiple inflight messages
	}
}

// processIncoming checks the buffer for valid MQTT
// If this returns an error then the connection should be dropped
// func (i *Instance) processIncoming(in io.Reader, out chan<- *packets.ControlPacket) error {
func (i *Instance) processIncoming(cp *packets.ControlPacket, out chan<- *packets.ControlPacket) error {
	disconnectInvalidPacket := func() {
		response := packets.NewControlPacket(packets.DISCONNECT)
		response.Content.(*packets.Disconnect).ReasonCode = 0x82 // protocol error
		out <- response
	}

	i.logger.Println("packet received", cp)

	if i.packetReceived != nil {
		if err := i.packetReceived(cp); err != nil {
			return err
		}
	}

	// the first packet sent from the Client to the Server MUST be a CONNECT packet [MQTT-3.1.0-1].
	if !i.connPktDone && cp.Type != packets.CONNECT {
		disconnectInvalidPacket()
		return errors.New(fmt.Sprintf("received %s before CONNECT", cp.PacketType()))
	}

	switch cp.Type {
	case packets.CONNECT: // Client is connecting
		if i.connPktDone {
			// The Server MUST process a second CONNECT packet sent from a Client as a Protocol Error and close the Network Connection [MQTT-3.1.0-2]
			disconnectInvalidPacket()
			return errors.New("received additional CONNECT packet")
		}
		i.connPktDone = true
		p := cp.Content.(*packets.Connect)
		response := packets.NewControlPacket(packets.CONNACK)

		i.sessionExpiryInterval = 0
		if p.Properties.SessionExpiryInterval != nil {
			i.sessionExpiryInterval = *p.Properties.SessionExpiryInterval
		}

		// Session is only retained if there was one (i.sessionExpiryInterval) and connect does not clean it
		if i.sessionExpiryInterval == 0 || p.CleanStart == true {
			i.subscriptions = make(map[string]subscription)
			i.serverSessionState = make(map[uint16]*StateInfo)
			i.serverMIDs.Clear()
			i.clientSessionState = make(map[uint16]*StateInfo)
		} else {
			response.Content.(*packets.Connack).SessionPresent = true
			// Only publish packets (QOS 1+) remain in the session state, so we need to ensure serverMIDs match (not
			// really needed at the time of writing, but we start sending other request types).
			i.serverMIDs.Clear()
			for mid := range i.serverSessionState {
				i.serverMIDs.Allocate(mid)
			}
		}
		// We return whatever session expiry interval was requested
		response.Content.(*packets.Connack).Properties = &packets.Properties{
			SessionExpiryInterval: &i.sessionExpiryInterval,
		}
		if i.overrideConnAck != nil {
			i.overrideConnAck(p, response.Content.(*packets.Connack))
		}
		out <- response
		return nil
	case packets.PUBLISH: // client is publishing something (warning - if client resends the same message then we will close connection again)
		p := cp.Content.(*packets.Publish)
		if bytes.Compare(p.Payload, []byte(CloseOnPublishReceived)) == 0 {
			out <- nil
			return nil // act as if this was not received
		}
		idInUse := false
		if i.clientSessionState[p.PacketID] != nil {
			idInUse = true
		}
		switch p.QoS {
		case 0:
			if p.PacketID != 0 {
				return fmt.Errorf("received QOS 0 PUBLISH with Packer Identifier other than 0")
			}
			i.sendMessageToSubscriber(p, out)
			return nil // No response to QOS0 publish
		case 1:
			if p.PacketID == 0 { // Check for spec compliance (common issue is to forger to set packer identifier)
				return fmt.Errorf("received QOS 1 PUBLISH with Packer IDentifier of 0")
			}
			if idInUse && !p.Duplicate {
				// We have a packet with this ID, so let's just ignore this one (could add a check that the existing packet is a `PUBLISH`)
				i.logger.Printf("Received duplicate PUBLISH that is not flagged as DUP")
				return nil
			}
			response := packets.NewControlPacket(packets.PUBACK)
			r := response.Content.(*packets.Puback)
			r.PacketID = p.PacketID
			if idInUse {
				r.ReasonCode = 0x91 // Packet Identifier in use
			} else {
				i.clientSessionState[p.PacketID] = NewStateInfo(response, 1, p.Topic, p.Payload)
			}
			out <- response
			i.sendMessageToSubscriber(p, out)
			return nil
		case 2:
			if p.PacketID == 0 { // Check for spec compliance (common issue is to forger to set packer identifier)
				return fmt.Errorf("received QOS 2 PUBLISH with Packer IDentifier of 0")
			}
			if idInUse && !p.Duplicate {
				// We have a packet with this ID, so let's just ignore this one (could add a check that the existing packet is a `PUBLISH`)
				return errors.New("received duplicate PUBLISH that is not flagged as DUP")
			}
			response := packets.NewControlPacket(packets.PUBREC)
			r := response.Content.(*packets.Pubrec)
			r.PacketID = p.PacketID
			if idInUse {
				r.ReasonCode = 0x91 // Packet Identifier in use
			} else {
				i.clientSessionState[p.PacketID] = NewStateInfo(response, 2, p.Topic, p.Payload)
				i.sendMessageToSubscriber(p, out) // safe as will not be done on duplicates
			}
			out <- response
			return nil
		default:
			disconnectInvalidPacket()
			return fmt.Errorf("received PUBLISH with invalid QOS (%d)", p.QoS)
		}
	case packets.PUBACK: // client is acknowledging QOS1 PUBLISH that we sent
		p := cp.Content.(*packets.Puback)
		state := i.serverSessionState[p.PacketID]
		if state == nil {
			return nil // Probably a retransmitted PUBACK (may want to add a check for this)
		}
		if state.QOS != 1 {
			disconnectInvalidPacket()
			return fmt.Errorf("received PUBACK but state for packet id is not a QOS 1 PUBLISH (QOS: %d)", state.QOS)
		}
		if bytes.Compare(state.Payload, []byte(CloseOnPubAckReceived)) == 0 {
			state.Payload = append(state.Payload, []byte(AppendAfterActionProcessed)...)
			out <- nil
			return nil
		}
		// Ignore ReasonCode as spec says PUBLISH is treated as acknowledged regardless
		delete(i.serverSessionState, p.PacketID) // Packet ID now available for reuse
		return nil                               // Nothing further to do
	case packets.PUBREC: // client is acknowledging QOS2 PUBLISH that we sent (phase1)
		p := cp.Content.(*packets.Pubrec)
		state := i.serverSessionState[p.PacketID]
		if state == nil {
			disconnectInvalidPacket()
			return errors.New("received PUBREC but state is empty")
		}
		if state.QOS != 2 {
			disconnectInvalidPacket()
			return fmt.Errorf("received PUBACK but state for packet id is not a QOS 2 PUBLISH (QOS: %d)", state.QOS)
		}
		_, ok := state.Sent.Content.(*packets.Pubrel)
		if ok {
			return nil // Assume this is a duplicate PUBREC (we have already responded, no need to do so again)
		}
		_, ok = state.Sent.Content.(*packets.Publish)
		if !ok {
			disconnectInvalidPacket()
			return errors.New("received PUBREC but last packet sent with this ID is not a PUBLISH or PUBREL")
		}
		if bytes.Compare(state.Payload, []byte(CloseOnPubRecReceived)) == 0 {
			state.Payload = append(state.Payload, []byte(AppendAfterActionProcessed)...)
			out <- nil
			return nil
		}
		if p.ReasonCode >= 127 {
			// If PUBACK or PUBREC is received containing a Reason Code of 0x80 or greater, then no further ACKs are expected
			delete(i.serverSessionState, cp.PacketID())
			return nil // Nothing further to do (have processed error)
		}
		// Respond with PUBREL
		response := packets.NewControlPacket(packets.PUBREL)
		r := response.Content.(*packets.Pubrel)
		r.PacketID = p.PacketID
		state.Sent = response
		out <- response
		return nil
	case packets.PUBREL: // Client releasing a message it sent
		p := cp.Content.(*packets.Pubrel)
		state := i.clientSessionState[p.PacketID]
		idInUse := true // Assume ID is in state
		if state == nil {
			idInUse = false
		} else {
			if state.QOS != 2 {
				disconnectInvalidPacket()
				return fmt.Errorf("received PUBREL but state for packet id is not a QOS 2 PUBLISH (QOS: %d)", state.QOS)
			}
			if bytes.Compare(state.Payload, []byte(CloseOnPubRelReceived)) == 0 {
				state.Payload = append(state.Payload, []byte(AppendAfterActionProcessed)...)
				out <- nil
				return nil
			}
			_, ok := state.Sent.Content.(*packets.Pubrec)
			if !ok {
				disconnectInvalidPacket()
				return errors.New("received PUBREC but last packet sent with this ID is not a PUBREC")
			}
			delete(i.clientSessionState, cp.PacketID()) // Sending PUBCOMP so transaction is complete
		}
		response := packets.NewControlPacket(packets.PUBCOMP)
		r := response.Content.(*packets.Pubcomp)
		r.PacketID = cp.PacketID()
		if !idInUse {
			r.ReasonCode = 0x92 // Packet Identifier was not found
		}
		out <- response
		return nil
	case packets.PUBCOMP: // client is acknowledging QOS2 PUBLISH that we sent (phase2)
		p := cp.Content.(*packets.Pubcomp)
		state := i.serverSessionState[p.PacketID]
		if state == nil {
			return nil // Probably a retransmitted `PUBCOMP` (may want to add a check for this)
		}
		if state.QOS != 2 {
			disconnectInvalidPacket()
			return fmt.Errorf("received PUBCOMP but state for packet id is not a QOS 2 PUBLISH (QOS: %d)", state.QOS)
		}
		_, ok := state.Sent.Content.(*packets.Pubrel)
		if !ok {
			disconnectInvalidPacket()
			return errors.New("received PUBCOMP but last packet sent with this ID is not a PUBREL")
		}
		if bytes.Compare(state.Payload, []byte(CloseOnPubCompReceived)) == 0 {
			state.Payload = append(state.Payload, []byte(AppendAfterActionProcessed)...)
			out <- nil
			return nil
		}
		delete(i.serverSessionState, p.PacketID) // Packet ID now available for reuse
		return nil                               // Nothing further to do
	case packets.SUBSCRIBE:
		sp := cp.Content.(*packets.Subscribe)
		response := packets.NewControlPacket(packets.SUBACK)
		r := response.Content.(*packets.Suback)
		r.PacketID = sp.PacketID
		for _, sub := range sp.Subscriptions {
			i.subscriptions[sub.Topic] = subscription{qos: sub.QoS}
			r.Reasons = append(r.Reasons, sub.QoS) // accept all subs
		}
		out <- response
		return nil
	case packets.UNSUBSCRIBE:
		sp := cp.Content.(*packets.Unsubscribe)
		response := packets.NewControlPacket(packets.UNSUBACK)
		r := response.Content.(*packets.Unsuback)
		r.PacketID = sp.PacketID
		for _, topic := range sp.Topics {
			if _, ok := i.subscriptions[topic]; ok {
				delete(i.subscriptions, topic)
				r.Reasons = append(r.Reasons, 0x0) // Success
			} else {
				r.Reasons = append(r.Reasons, 0x11) // No subscription existed
			}
		}
		out <- response
		return nil
	case packets.PINGREQ:
		out <- packets.NewControlPacket(packets.PINGRESP)
		return nil
	case packets.DISCONNECT:
		p := cp.Content.(*packets.Disconnect)
		return fmt.Errorf("disconnect received with resaon %d", p.ReasonCode)
	case packets.AUTH:
		authProp := cp.Content.(*packets.Auth).Properties
		switch authProp.AuthMethod {
		case "TEST":
			if !bytes.Equal(authProp.AuthData, []byte("secret data")) {
				return fmt.Errorf("invalid authentication data received: %s", authProp.AuthData)
			}
		default:
			return fmt.Errorf("invalid authentication method received: %s", authProp.AuthMethod)
		}
		response := packets.NewControlPacket(packets.AUTH)
		r := response.Content.(*packets.Auth)
		r.ReasonCode = packets.AuthSuccess
		out <- response
		return nil
	default:
		return fmt.Errorf("unsupported packet type %d received", cp.Type)
	}
}

// SetPacketReceivedCallback sets callback that will be called whenever a packet is received
// If the callback returns an error the connection will be dropped
// Note: this is not thread safe - call before sending any messages
func (i *Instance) SetPacketReceivedCallback(callback func(publish *packets.ControlPacket) error) {
	i.packetReceived = callback
}

// SetConnectCallback sets callback that will be called before the response to a CONNECT packet is sent.
// The CONNACK packet mak be altered if required.
// Note: this is not thread safe - call before sending any messages
func (i *Instance) SetConnectCallback(callback func(cp *packets.Connect, cap *packets.Connack)) {
	i.overrideConnAck = callback
}

// sendMessageToSubscriber starts the process of sending a message to subscriber (as the topic must be an exact match,
// there can only be one subscriber)
func (i *Instance) sendMessageToSubscriber(inboundPub *packets.Publish, out chan<- *packets.ControlPacket) {
	if sub, ok := i.subscriptions[inboundPub.Topic]; ok {
		pub := packets.NewControlPacket(packets.PUBLISH)
		p := pub.Content.(*packets.Publish)
		p.Topic = inboundPub.Topic
		p.Payload = inboundPub.Payload

		p.QoS = inboundPub.QoS // Qos is lower of publish or sub
		if p.QoS > sub.qos {
			p.QoS = sub.qos
		}

		if p.QoS > 0 { // QOS 0 messages do not have an Identifier
			var err error
			p.PacketID, err = i.serverMIDs.Request()
			if err != nil {
				panic(err) // Cannot really do anything else; this should never happen
			}
			i.serverSessionState[p.PacketID] = NewStateInfo(pub, p.QoS, p.Topic, p.Payload) // New message to add to state
		}
		i.logger.Println("sending message to subscriber ", pub)
		out <- pub
	}
}

// MIDs is a simple service to provide message IDs (mostly copied from paho)
type MIDs struct {
	sync.Mutex
	lastMid uint16
	index   []bool
}

// Request is the library provided MIDService's implementation of
// the required interface function()
func (m *MIDs) Request() (uint16, error) {
	m.Lock()
	defer m.Unlock()
	for i := m.lastMid + 1; i != 0; i++ { // scan from lastMid + 1 to the end of range
		if m.index[i-1] {
			continue
		}
		m.index[i-1] = true
		m.lastMid = i
		return i, nil
	}
	for i := uint16(0); i < m.lastMid; i++ { // scan from start of range to lastMid (inclusive)
		if m.index[i] {
			continue
		}
		m.index[i] = true
		m.lastMid = i + 1
		return i + 1, nil
	}
	return 0, ErrorMIDsExhausted
}

// Allocate sets a specific MID (for testing only; MID must be free!)
func (m *MIDs) Allocate(i uint16) {
	m.Lock()
	if m.index[i-1] {
		panic("Allocate called with taken MID")
	}
	m.index[i-1] = true
	m.Unlock()
}

// Free releases a MID for reuse
func (m *MIDs) Free(i uint16) {
	m.Lock()
	m.index[i-1] = false
	m.Unlock()
}

// Clear resets all MIDs to unused
// Note: Does not reset lastMid because retaining a sequence can make debugging easier
func (m *MIDs) Clear() {
	m.index = make([]bool, int(midMax))
}

// netPipe simulates a network link using a real connection.
// This is used over net.Pipe because net.Pipe is synchronous and this can create confusing results
// because it does not work like a real network connection (a call to `Write` will block until the other
// end calls `Read` whereas with a real connection there are buffers etc).
// There are many ways to do this, but using a real connection is simple and effective!
func netPipe(ctx context.Context) (net.Conn, net.Conn, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0") // Port 0 is wildcard port; OS will choose port for us
	if err != nil {
		return nil, nil, err
	}
	defer l.Close()
	var d net.Dialer
	userCon, err := d.DialContext(ctx, "tcp", l.Addr().String()) // Dial the port we just listened on
	if err != nil {
		return nil, nil, err
	}
	ourCon, err := l.Accept() // Should return immediately
	if err != nil {
		userCon.Close()
		return nil, nil, err
	}
	return userCon, ourCon, nil
}
