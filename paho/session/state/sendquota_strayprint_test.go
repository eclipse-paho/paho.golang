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

package state

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
)

// TestSendQuotaAcquireSilentOnCancelReadyRace is a regression guard ensuring that
// sendQuota.acquire never writes to stdout.
//
// There used to be a stray debug `fmt.Println` in the inner select of acquire:
// when ctx.Done() wins the OUTER select, acquire re-takes s.mu and re-checks
// whether the waiter's `ready` channel was already closed by a concurrent
// Release(). If Release() closed `ready` before the cancelled waiter re-acquired
// the mutex, the INNER `case <-ready:` arm runs (treating the acquire as having
// succeeded, err=nil). That arm carried the stray print, so it fired on the
// cancel/ready race window and leaked debug noise to stdout.
//
// This test races cancel() against Release() on the sole head waiter, captures
// stdout, and asserts the library writes ZERO bytes. It does not use t.Parallel()
// because stdout capture is process-global.
func TestSendQuotaAcquireSilentOnCancelReadyRace(t *testing.T) {
	// Capture os.Stdout for the duration of the test and drain it concurrently.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	var captured []byte
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		captured, _ = io.ReadAll(r)
	}()

	// Restore stdout and assert on captured bytes, no matter how the test exits.
	defer func() {
		os.Stdout = origStdout
		_ = w.Close()
		drainWG.Wait()
		_ = r.Close()
		if len(captured) != 0 {
			t.Fatalf("sendQuota.acquire wrote %d byte(s) to stdout (expected 0): %q", len(captured), captured)
		}
	}()

	// A few thousand iterations reliably hits the cancel/ready race window while
	// keeping runtime well under a second. The assertion (zero bytes) holds
	// regardless of whether any single iteration lands on the branch.
	const iterations = 4000
	for i := 0; i < iterations; i++ {
		s := newSendQuota(1)

		// Exhaust the single slot so the next acquire must queue.
		if err := s.Acquire(context.Background()); err != nil {
			t.Fatalf("iter %d: initial Acquire failed: %v", i, err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		var waiterWG sync.WaitGroup
		waiterWG.Add(1)
		go func() {
			defer waiterWG.Done()
			// Becomes the sole waiter; result intentionally ignored (either ctx
			// cancellation or a successful slot hand-off via Release is fine).
			_ = s.Acquire(ctx)
		}()

		// Spin until the waiter is reliably enqueued as the head waiter.
		for {
			s.mu.Lock()
			enqueued := len(s.waiters) == 1
			s.mu.Unlock()
			if enqueued {
				break
			}
		}

		// Fire cancel() and Release() simultaneously to race ctx.Done() against
		// the close(ready) in Release() inside acquire's selects.
		var raceWG sync.WaitGroup
		raceWG.Add(2)
		go func() {
			defer raceWG.Done()
			cancel()
		}()
		go func() {
			defer raceWG.Done()
			_ = s.Release()
		}()
		raceWG.Wait()

		waiterWG.Wait()
		cancel() // no-op if already cancelled; releases ctx resources
	}
}
