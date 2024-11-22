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

package autopaho

import (
	"fmt"
	"sync"

	"github.com/canonical/mqtt.golang/paho"
	"github.com/canonical/mqtt.golang/paho/log"
)

// errorHandler provides the onClientError callback function that will be called by the Paho library. The sole aim
// of this is to pass a single error onto the error channel (the library may send multiple errors; only the first
// will be processed).
// The callback userOnClientError will be called a maximum of one time. If userOnServerDisconnect is called, then
// userOnClientError will not be called (but there is a small chance that userOnClientError will be called followed
// by userOnServerDisconnect (if we encounter an error sending but there is a DISCONNECT in the queue).
type errorHandler struct {
	debug log.Logger

	mu      sync.Mutex
	errChan chan error // receives connection errors

	userOnClientError      func(error)            // User provided onClientError function
	userOnServerDisconnect func(*paho.Disconnect) // User provided OnServerDisconnect function
}

// shutdown prevents any further calls from emitting a message
func (e *errorHandler) shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errChan = nil
}

// onClientError called by the paho library when an error occurs. We assume that the error is always fatal
func (e *errorHandler) onClientError(err error) {
	if e.handleError(err) && e.userOnClientError != nil {
		go e.userOnClientError(err)
	}
}

// onClientError called by the paho library when the server requests a disconnection (for example, as part of a
// clean server shutdown). We want to begin attempting to reconnect when this occurs (and pass a detectable error
// to the user)
func (e *errorHandler) onServerDisconnect(d *paho.Disconnect) {
	e.handleError(&DisconnectError{err: fmt.Sprintf("server requested disconnect (reason: %d)", d.ReasonCode)})
	if e.userOnServerDisconnect != nil {
		go e.userOnServerDisconnect(d)
	}
}

// handleError ensures that only a single error is sent to the channel (all errors go to the users OnClientError function)
// Returns true if the error was sent to the channel (i.e. this is the first error we have seen)
func (e *errorHandler) handleError(err error) bool {
	e.mu.Lock()
	errChan := e.errChan // prevent any chance of deadlock with concurrent call to e.shutdown
	e.errChan = nil
	e.mu.Unlock()
	if errChan != nil {
		e.debug.Printf("handleError received error: %s", err)
		errChan <- err
		e.debug.Printf("handleError passed error on: %s", err)
		return true
	}
	e.debug.Printf("handleError received extra error: %s", err)
	return false
}

// DisconnectError will be passed when the server requests disconnection (allows this error type to be detected)
type DisconnectError struct{ err string }

func (d *DisconnectError) Error() string {
	return d.err
}

// ConnackError will be passed when the server denies connection in CONNACK packet
type ConnackError struct {
	ReasonCode byte   // CONNACK reason code
	Reason     string // CONNACK Reason string from properties
	Err        error  // underlying error
}

// NewConnackError returns a new ConnackError
func NewConnackError(err error, connack *paho.Connack) *ConnackError {
	reason := ""
	if connack.Properties != nil {
		reason = connack.Properties.ReasonString
	}
	return &ConnackError{
		ReasonCode: connack.ReasonCode,
		Reason:     reason,
		Err:        err,
	}
}

func (c *ConnackError) Error() string {
	return fmt.Sprintf("server denied connect (reason: %d): %s", c.ReasonCode, c.Err)
}

func (c *ConnackError) Unwrap() error {
	return c.Err
}
