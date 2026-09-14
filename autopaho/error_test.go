/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
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
	"errors"
	"fmt"
	"testing"

	"github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConnackError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		properties *paho.ConnackProperties
	}{
		{name: "without properties"},
		{
			name: "redirect without reason string",
			properties: &paho.ConnackProperties{
				ServerReference: "other.example:1883",
			},
		},
		{
			name: "redirect with reason string and user properties",
			properties: &paho.ConnackProperties{
				ServerReference: "other.example:1883",
				ReasonString:    "use another server",
				User:            paho.UserProperties{{Key: "detail", Value: "redirect"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ca := &paho.Connack{ReasonCode: 0x9C, Properties: tc.properties}
			underlying := errors.New("connection refused")
			err := fmt.Errorf("connection attempt: %w", NewConnackError(underlying, ca))

			var connackErr *ConnackError
			require.ErrorAs(t, err, &connackErr)
			require.Same(t, ca, connackErr.Connack)
			assert.ErrorIs(t, err, underlying)
			assert.Equal(t, underlying, connackErr.Err)
			assert.Equal(t, ca.ReasonCode, connackErr.ReasonCode)
			if tc.properties == nil {
				assert.Empty(t, connackErr.Reason)
			} else {
				assert.Equal(t, tc.properties.ReasonString, connackErr.Reason)
				assert.Equal(t, "other.example:1883", connackErr.Connack.Properties.ServerReference)
				assert.Equal(t, tc.properties.User, connackErr.Connack.Properties.User)
			}
			assert.EqualError(t, connackErr, "server denied connect (reason: 156): connection refused")
		})
	}
}
