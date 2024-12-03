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
	"math/rand"
	"time"
)

// Backoff function to compute backoff duration for the Nth attempt
// attempt starts at "0" indicating the delay BEFORE the first attempt
type Backoff func(attempt int) time.Duration

////////////////////////////////////////////////////////////////////////////////
// implementation for constant backoff
////////////////////////////////////////////////////////////////////////////////

// Creates a new backoff with constant delay (for attempt > 0, otherwise the backoff is 0).
func NewConstantBackoff(delay time.Duration) Backoff {
	return func(attempt int) time.Duration {
		if attempt <= 0 {
			return 0
		}
		return delay
	}
}

////////////////////////////////////////////////////////////////////////////////
// implementation for an exponential backoff
////////////////////////////////////////////////////////////////////////////////

// NewExponentialBackoff provides a random duration within a range starting
// from a fixed min value up to a "moving" max value that increases
// exponentially for each attempt up to the specified max value.
//
// The "moving" max is computed by multiplying the initial max value with the
// factor for each attemt up the specified max value.
//
// Configuration parameters:
//   - minDelay        - lower bound for computed backoff
//   - maxDelay        - upper bound for computed backoff
//   - initialMaxDelay - initial max value which wiil incerease exponentially up to the max delay
//   - factor          - factor for the exponential increase of initial max delay
func NewExponentialBackoff(
	minDelay time.Duration, // lower bound for computed backoff
	maxDelay time.Duration, // upper bound for computed backoff
	initialMaxDelay time.Duration, // initial max value which wiil incerease exponentially up to the max delay
	factor float32, // factor for the exponential increase of initial max delay
) Backoff {
	if minDelay <= 0 {
		panic("min delay must NOT be less than or equal to: 0")
	}
	if maxDelay <= minDelay {
		panic("max delay must NOT be less than or equal to: min delay")
	}
	if initialMaxDelay < minDelay || maxDelay < initialMaxDelay {
		panic("initial max delay must be in range of: (min, max) delay")
	}
	if factor <= 1 {
		panic("factor must NOT be less than or equal to: 1")
	}

	// for simplicity using numbers instead of duration internally
	minDelayMillis := minDelay.Milliseconds()
	maxDelayMillis := maxDelay.Milliseconds()
	initialMaxDelayMillis := initialMaxDelay.Milliseconds()

	// computes the "moving" max value based on the given attempt by multiplying
	// it with the factor and ensures it does not exceed the specified max value
	computeMaxDelayForAttempt := func(attempt int) int64 {
		// only "moving part",
		// will be multiplied by "factor" up to the max value for each attempt
		movingMaxMillis := initialMaxDelayMillis

		// computaion is based on 1 as 0 is the backoff for the first attempt
		for i := 1; i < attempt; i++ {
			movingMaxMillis = int64(float32(movingMaxMillis) * factor)
			// ensure we stay in range
			// check for range overflow / numerical overflow
			if maxDelayMillis < movingMaxMillis || movingMaxMillis < minDelayMillis {
				movingMaxMillis = maxDelayMillis
				// stop as we reached max value already
				break
			}
		}

		return movingMaxMillis
	}

	return func(attempt int) time.Duration {
		if attempt <= 0 {
			return 0
		}

		maxDelayForAttemptMillis := computeMaxDelayForAttempt(attempt)
		randomMillisInRange := randRange(minDelayMillis, maxDelayForAttemptMillis)

		return time.Duration(randomMillisInRange) * time.Millisecond
	}
}

// DefaultExponentialBackoff returns an exponential backoff with default values.
//
// The default values are:
//   - min delay:          5 seconds
//   - max delay:         10 minutes
//   - initial max delay: 10 seconds
//   - factor:             1.5
func DefaultExponentialBackoff() Backoff {
	return NewExponentialBackoff(
		0o5*time.Second, // minDelay
		10*time.Minute,  // maxDelay
		10*time.Second,  // initialMaxDelay
		1.5,             // factor
	)
}

////////////////////////////////////////////////////////////////////////////////
// util functions
////////////////////////////////////////////////////////////////////////////////

// Returns a random number in the range of [start, end] (inclusive)
func randRange(start int64, end int64) int64 {
	normalizedRange := end - start + 1

	return rand.Int63n(normalizedRange) + start
}
