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

package paho

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestDefaultPingerTimeout(t *testing.T) {
	fakeServerConn, fakeClientConn := net.Pipe()

	go func() {
		// keep reading from fakeServerConn and throw away the data
		buf := make([]byte, 1024)
		for {
			_, err := fakeServerConn.Read(buf)
			if err != nil {
				return
			}
		}
	}()
	defer fakeServerConn.Close()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 1)
	}()

	select {
	case err := <-pingResult:
		require.NotNil(t, err)
		assert.EqualError(t, err, "PINGRESP timed out")
	case <-time.After(10 * time.Second):
		t.Error("expected DefaultPinger to detect timeout and return error")
	}
}

func TestDefaultPingerSuccess(t *testing.T) {
	fakeClientConn, fakeServerConn := net.Pipe()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 3)
	}()

	go func() {
		// keep reading from fakeServerConn and call PingResp() when a PINGREQ is received
		for {
			recv, err := packets.ReadPacket(fakeServerConn, 5)
			if err != nil {
				return
			}
			if recv.Type == packets.PINGREQ {
				pinger.PingResp()
			}
		}
	}()
	defer fakeServerConn.Close()

	select {
	case err := <-pingResult:
		t.Errorf("expected DefaultPinger to not return error, got %v", err)
	case <-time.After(10 * time.Second):
		// PASS
	}
}

func TestDefaultPingerPacketSent(t *testing.T) {
	fakeClientConn, fakeServerConn := net.Pipe()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 3)
	}()

	// keep calling PacketSent() in a goroutine to check that the Pinger avoids sending PINGREQs when not needed
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// keep calling PacketSent()
			pinger.PacketSent()
		}
	}()
	defer close(stop)

	// keep reading from fakeServerConn and call PingResp() when a PINGREQ is received
	// if more than one PINGREQ is received, the test will fail
	count := 0
	tooManyPingreqs := make(chan struct{})
	go func() {
		for {
			recv, err := packets.ReadPacket(fakeServerConn, 5)
			if err != nil {
				return
			}
			if recv.Type == packets.PINGREQ {
				count++
				pinger.PingResp()
				if count > 1 { // we allow the count to be 1 because the first PINGREQ is sent immediately
					close(tooManyPingreqs)
				}
			}
		}
	}()
	defer fakeServerConn.Close()

	select {
	case <-tooManyPingreqs:
		t.Error("expected DefaultPinger to not send PINGREQs when not needed")
	case err := <-pingResult:
		t.Errorf("expected DefaultPinger to not return error, got %v", err)
	case <-time.After(10 * time.Second):
		// PASS
	}
}

func TestDefaultPingerStartStop(t *testing.T) {
	fakeServerConn, fakeClientConn := net.Pipe()

	go func() {
		// keep reading from fakeServerConn and throw away the data
		buf := make([]byte, 1024)
		for {
			_, err := fakeServerConn.Read(buf)
			if err != nil {
				return
			}
		}
	}()
	defer fakeServerConn.Close()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 1)
	}()

	time.Sleep(time.Millisecond) // Allow above go routine to start
	ping2Result := make(chan error, 1)
	go func() {
		ping2Result <- pinger.Run(ctx, fakeClientConn, 1)
	}()
	select {
	case <-time.After(time.Second):
		t.Fatal("Starting Run twice must fail immediately")
	case err := <-ping2Result:
		if err == nil {
			t.Fatal("Starting Run twice must return an error")
		}
	}

	select {
	case <-pingResult:
		t.Fatal("Ping should block until stopped or error")
	default:
	}
	cancel()
	select {
	case <-time.After(time.Second):
		t.Fatal("Cancelling context must stop pinger")
	case err := <-pingResult:
		if err != nil {
			t.Fatal("Cancelling context should result in nil error")
		}
	}

	// Confirm we can now call Run() again
	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 1)
	}()
	time.Sleep(time.Millisecond) // Allow above go routine to start
	cancel2()
	select {
	case <-time.After(time.Second):
		t.Fatal("Cancelling context must stop pinger")
	case err := <-pingResult:
		if err != nil {
			t.Fatal("Second call to Run should succeed (clean cancel should return nil error)")
		}
	}
}

// In case of slow and unstable network connection, the WriteTo operation may block for longer than KeepAlive interval
func TestDefaultPingerBlockingWriteTimeout(t *testing.T) {
	defer goleak.VerifyNone(t)
	fakeServerConn, fakeClientConn := net.Pipe()
	// intentionally do not read from fakeServerConn to simulate a blocking write operation
	defer fakeServerConn.Close()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 1)
	}()

	select {
	case err := <-pingResult:
		require.NotNil(t, err)
		assert.EqualError(t, err, "PINGRESP timed out")
	case <-time.After(10 * time.Second):
		t.Error("expected DefaultPinger to detect timeout and return error")
	}
}

func TestDefaultPingerContextCancelled(t *testing.T) {
	defer goleak.VerifyNone(t)
	fakeServerConn, fakeClientConn := net.Pipe()
	defer fakeServerConn.Close()

	pinger := NewDefaultPinger()
	pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pingResult := make(chan error, 1)
	go func() {
		pingResult <- pinger.Run(ctx, fakeClientConn, 60)
	}()

	select {
	case err := <-pingResult:
		require.Nil(t, err)
	case <-time.After(10 * time.Second):
		t.Error("expected DefaultPinger to exit when context is cancelled")
	}
}
