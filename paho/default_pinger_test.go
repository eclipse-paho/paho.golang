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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eclipse/paho.golang/internal/testserver"
	"github.com/eclipse/paho.golang/packets"
	paholog "github.com/eclipse/paho.golang/paho/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestDefaultPingerTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Use buffered connection (so no need to resd/throw away data)
		fakeServerConn, fakeClientConn := testserver.NewConnPair()
		defer fakeServerConn.Close()

		pinger := NewDefaultPinger()
		pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

		// Allow pinger to run for 10 seconds (should exit after 1 because first ping sent immediately)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		startTime := time.Now()
		pingErr := pinger.Run(ctx, fakeClientConn, 1)
		duration := time.Since(startTime)

		require.GreaterOrEqual(t, duration.Seconds(), 1.0, "Expected pinger to run for at least 1 second")
		assert.EqualError(t, pingErr, "PINGRESP timed out")
	})
}

func TestDefaultPingerSuccess(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Everything will shut down after 10 seconds, after which results will be checked
		var wg sync.WaitGroup
		startTime := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		fakeClientConn, fakeServerConn := testserver.NewConnPair()
		context.AfterFunc(ctx, func() {
			fakeServerConn.Close()
		})

		pinger := NewDefaultPinger()
		pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

		var pingErr error
		var duration time.Duration
		wg.Go(func() {
			pingErr = pinger.Run(ctx, fakeClientConn, 3)
			duration = time.Since(startTime)
			fakeServerConn.Close() // Allow goroutine handling comms to close
		})

		wg.Go(func() {
			// keep reading from fakeServerConn and call PingResp() when a PINGREQ is received
			for {
				recv, err := packets.ReadPacket(fakeServerConn)
				if err != nil {
					return
				}
				if recv.Type == packets.PINGREQ {
					pinger.PingResp()
				}
			}
		})

		wg.Wait()
		require.GreaterOrEqual(t, duration.Seconds(), 10.0, "Expected pinger to run for at least 10 seconds")
		require.NoError(t, pingErr)
	})
}

func TestDefaultPingerPacketSentReceived(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Everything will shut down after 10 seconds, after which results will be checked
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		fakeClientConn, fakeServerConn := testserver.NewConnPair()
		context.AfterFunc(ctx, func() {
			fakeServerConn.Close()
		})

		pinger := NewDefaultPinger()
		pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

		var pingErr error
		wg.Go(func() {
			pingErr = pinger.Run(ctx, fakeClientConn, 3) // 3-second keepalive
		})

		// keep calling PacketSent() (and PacketReceived) in a goroutine to check that the Pinger avoids sending PINGREQs when not needed
		wg.Go(func() {
			for ctx.Err() == nil {
				time.Sleep(time.Second) // Ensure initial ping is sent (and avoid tight loop)
				// Notify pinger that packets have been sent/received (so it should not ping)
				pinger.PacketSent()
				pinger.PacketReceived()
			}
		})

		// read from fakeServerConn and call PingResp() when a PINGREQ is received
		pingCount := 0
		var readPacketErr error
		wg.Go(func() {
			for {
				recv, err := packets.ReadPacket(fakeServerConn)
				if err != nil {
					readPacketErr = err
					return
				}
				if recv.Type == packets.PINGREQ {
					pingCount++
					pinger.PingResp()
				}
			}
		})

		// Wait for everything to finnish (10 seconds) and then check result
		wg.Wait()
		require.Equal(t, 1, pingCount, "Expected 1 ping") // Initial ping is sent immediately
		require.NoError(t, pingErr)
		require.ErrorIs(t, readPacketErr, net.ErrClosed)
	})
}

// TestDefaultPingerPacketSentOnly - If packets are being sent, but nothing received, then a pingreq should be sent
func TestDefaultPingerPacketSentOnly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // Test runs for 10 seconds
		defer cancel()

		fakeClientConn, fakeServerConn := testserver.NewConnPair()
		context.AfterFunc(ctx, func() {
			fakeServerConn.Close()
		})

		pinger := NewDefaultPinger()
		pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

		var pingErr error
		wg.Go(func() {
			pingErr = pinger.Run(ctx, fakeClientConn, 3)
		})

		// keep calling PacketSent() in a goroutine to check that the Pinger avoids sending PINGREQs when not needed
		wg.Go(func() {
			for ctx.Err() == nil {
				pinger.PacketSent()
				time.Sleep(time.Millisecond)
			}
		})

		// keep reading from fakeServerConn and call PingResp() when a PINGREQ is received
		// if more than one PINGREQ is received, the test will fail
		pingCount := 0
		var readPacketErr error
		wg.Go(func() {
			for {
				recv, err := packets.ReadPacket(fakeServerConn)
				if err != nil {
					readPacketErr = err
					return
				}
				if recv.Type == packets.PINGREQ {
					pingCount++
					pinger.PingResp()
				}
			}
		})

		wg.Wait()                                          // wait for test to run
		require.Equal(t, 4, pingCount, "Expected 4 pings") // 0,3,6,9 seconds
		require.NoError(t, pingErr)
		require.ErrorIs(t, readPacketErr, net.ErrClosed)
	})
}

// TestDefaultPingerPacketReceiveOnly - If packets are being received, but nothing sent, then a pingreq should be sent
func TestDefaultPingerPacketReceiveOnly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // Test runs for 10 seconds
		defer cancel()

		fakeClientConn, fakeServerConn := testserver.NewConnPair()
		context.AfterFunc(ctx, func() {
			fakeServerConn.Close()
		})

		pinger := NewDefaultPinger()
		pinger.SetDebug(paholog.NewTestLogger(t, "DefaultPinger:"))

		var pingErr error
		wg.Go(func() {
			pingErr = pinger.Run(ctx, fakeClientConn, 3)
		})

		// keep calling PacketSent() in a goroutine to check that the Pinger avoids sending PINGREQs when not needed
		wg.Go(func() {
			for ctx.Err() == nil {
				pinger.PacketReceived()
				time.Sleep(time.Millisecond)
			}
		})

		// keep reading from fakeServerConn and call PingResp() when a PINGREQ is received
		// if more than one PINGREQ is received, the test will fail
		pingCount := 0
		var readPacketErr error
		wg.Go(func() {
			for {
				recv, err := packets.ReadPacket(fakeServerConn)
				if err != nil {
					readPacketErr = err
					return
				}
				if recv.Type == packets.PINGREQ {
					pingCount++
					pinger.PingResp()
				}
			}
		})

		wg.Wait()                                          // wait for test to run
		require.Equal(t, 4, pingCount, "Expected 4 pings") // 0,3,6,9 seconds
		require.NoError(t, pingErr)
		require.ErrorIs(t, readPacketErr, net.ErrClosed)
	})
}

func TestDefaultPingerStartStop(t *testing.T) {
	t.Parallel()
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
