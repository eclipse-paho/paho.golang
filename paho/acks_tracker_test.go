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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/canonical/mqtt.golang/packets"
)

func TestAcksTracker(t *testing.T) {
	var (
		at acksTracker
		p1 = &packets.Publish{PacketID: 1}
		p2 = &packets.Publish{PacketID: 2}
		p3 = &packets.Publish{PacketID: 3}
		p4 = &packets.Publish{PacketID: 4} // to test not found
	)

	t.Run("flush-empty", func(t *testing.T) {
		at.flush(func(_ []*packets.Publish) {
			t.Fatal("flush should not call 'do' since no packets have been added nor acknowledged")
		})
	})

	t.Run("flush-without-acking", func(t *testing.T) {
		at.add(p1)
		at.add(p2)
		at.add(p3)
		require.Equal(t, ErrPacketNotFound, at.markAsAcked(p4))
		at.flush(func(_ []*packets.Publish) {
			t.Fatal("flush should not call 'do' since no packets have been acknowledged so far")
		})
	})

	t.Run("ack-in-the-middle", func(t *testing.T) {
		require.NoError(t, at.markAsAcked(p3))
		at.flush(func(_ []*packets.Publish) {
			t.Fatal("flush should not call 'do' since p1 and p2 have not been acknowledged yet")
		})
	})

	t.Run("idempotent-acking", func(t *testing.T) {
		require.NoError(t, at.markAsAcked(p3))
		require.NoError(t, at.markAsAcked(p3))
		require.NoError(t, at.markAsAcked(p3))
	})

	t.Run("ack-first", func(t *testing.T) {
		var flushCalled bool
		require.NoError(t, at.markAsAcked(p1))
		at.flush(func(pbs []*packets.Publish) {
			require.Equal(t, []*packets.Publish{p1}, pbs, "Only p1 expected even though p3 was acked, p2 is still missing")
			flushCalled = true
		})
		require.True(t, flushCalled)
	})

	t.Run("ack-after-flush", func(t *testing.T) {
		var flushCalled bool
		require.NoError(t, at.markAsAcked(p2))
		at.add(p4) // this should just be appended and not flushed (yet)
		at.flush(func(pbs []*packets.Publish) {
			require.Equal(t, []*packets.Publish{p2, p3}, pbs, "Only p2 and p3 expected, p1 was flushed in the previous call")
			flushCalled = true
		})
		require.True(t, flushCalled)
	})

	t.Run("ack-last", func(t *testing.T) {
		var flushCalled bool
		require.NoError(t, at.markAsAcked(p4))
		at.flush(func(pbs []*packets.Publish) {
			require.Equal(t, []*packets.Publish{p4}, pbs, "Only p4 expected, the rest was flushed in previous calls")
			flushCalled = true
		})
		require.True(t, flushCalled)
	})

	t.Run("flush-after-acking-everything", func(t *testing.T) {
		at.flush(func(_ []*packets.Publish) {
			t.Fatal("no call to 'do' expected, we flushed all packets already")
		})
	})
}
