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
	"fmt"
	"math"

	"github.com/canonical/mqtt.golang/autopaho"
)

func main() {
	// used to calculate the number of iterations
	const interationBase = 1

	backoff := autopaho.DefaultExponentialBackoff()

	minBackoff := int64(math.MaxInt64)
	maxBackoff := int64(0)
	iterationsTotal := 0

	zeroAttmeptBackoffTime := backoff(0).Milliseconds()
	fmt.Printf("Backoff for attempt '0'       : %d\n", zeroAttmeptBackoffTime)

	for i := 0; i < 22; i++ {
		iterations := interationBase << i
		for j := 0; j < iterations; j++ {
			iterationsTotal++
			backoffTime := backoff(iterationsTotal).Milliseconds()
			if backoffTime < minBackoff {
				minBackoff = backoffTime
			}
			if backoffTime > maxBackoff {
				maxBackoff = backoffTime
			}
		}

		fmt.Printf("After % 8d iterations, min: %d, max: % 7d\n", iterationsTotal, minBackoff, maxBackoff)
	}
}
