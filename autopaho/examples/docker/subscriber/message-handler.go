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

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/canonical/mqtt.golang/paho"
)

// handler is a simple struct that provides a function to be called when a message is received. The message is parsed
// and the count followed by the raw message is written to the file (this makes it easier to sort the file)
type handler struct {
	writeToStdOut bool

	f *os.File
}

// NewHandler creates a new output handler and opens the output file (if applicable)
func NewHandler(writeToDisk bool, fileName string, writeToStdOut bool) *handler {
	var f *os.File
	if writeToDisk {
		var err error
		f, err = os.Create(fileName)
		if err != nil {
			panic(err)
		}
	}
	return &handler{
		writeToStdOut: writeToStdOut,
		f:             f,
	}
}

// Close closes the file
func (o *handler) Close() {
	if o.f != nil {
		if err := o.f.Close(); err != nil {
			fmt.Printf("ERROR closing file: %s", err)
		}
		o.f = nil
	}
}

// Message is used for marshalling/unmarshalling the JSON message (just a count)
type Message struct {
	Count uint64
}

// handle is called when a message is received
func (o *handler) handle(msg *paho.Publish) {
	// We extract the count and write that out first to simplify checking for missing values
	var m Message
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		fmt.Printf("Message could not be parsed (%s): %s", msg.Payload, err)
	}
	if o.f != nil {
		// Write out the number (make it long enough that sorting works) and the payload
		if _, err := o.f.WriteString(fmt.Sprintf("%09d %s\n", m.Count, msg.Payload)); err != nil {
			fmt.Printf("ERROR writing to file: %s", err)
		}
	}

	if o.writeToStdOut {
		fmt.Printf("received message: %s\n", msg.Payload)
	}
}
