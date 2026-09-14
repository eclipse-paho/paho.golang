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

// NewExponentialBackoff returns a backoff which provides a random duration
// within a range starting from a fixed min value up to a "moving" max value
// that increases exponentially for each attempt up to the specified max value.
//
// The "moving" max is computed by multiplying the initial max value with the
// factor for each attempt up to the specified max value.
//
// Configuration parameters:
//   - minDelay        - absolute lower bound for computed backoff
//   - maxDelay        - absolute upper bound for computed backoff
//   - initialMaxDelay - initial upper bound before exponential growth
//   - factor          - multiplier applied to the upper bound on each attempt
func NewExponentialBackoff(
	minDelay time.Duration, // absolute lower bound for computed backoff
	maxDelay time.Duration, // absolute upper bound for computed backoff
	initialMaxDelay time.Duration, // initial upper bound before exponential growth
	factor float32, // multiplier applied to the upper bound on each attempt
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

	// use millisecond values internally to simplify the calculations
	minDelayMillis := minDelay.Milliseconds()
	maxDelayMillis := maxDelay.Milliseconds()
	initialMaxDelayMillis := initialMaxDelay.Milliseconds()

	// Calculates the "moving" attempt-specific upper bound.
	//
	// Upper bound starts at initialMaxDelay and is multiplied by factor
	// for each additional attempt, up to maxDelay.
	computeMaxDelayForAttempt := func(attempt int) int64 {

		// This is the exponentially growing upper bound, the only "moving part"
		// Will be multiplied by "factor" up to the absolute upper bound for each attempt
		movingMaxMillis := initialMaxDelayMillis

		// computation is based on 1 as 0 is the backoff for the first attempt
		for i := 1; i < attempt; i++ {
			movingMaxMillis = int64(float32(movingMaxMillis) * factor)
			// ensure we stay in range
			// check for range overflow / numerical overflow
			if maxDelayMillis < movingMaxMillis || movingMaxMillis < minDelayMillis {
				movingMaxMillis = maxDelayMillis
				// stop as we reached the absolute upper bound already
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
		05*time.Second, // minDelay
		10*time.Minute, // maxDelay
		10*time.Second, // initialMaxDelay
		1.5,            // factor
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
