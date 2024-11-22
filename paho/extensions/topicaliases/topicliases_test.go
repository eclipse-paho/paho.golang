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

package topicaliases

import (
	"testing"

	"github.com/canonical/mqtt.golang/paho"
	"github.com/stretchr/testify/assert"
)

func TestTAHandler_PublishHook(t *testing.T) {
	tests := []struct {
		name            string
		aliasMax        uint16
		aliases         []string
		p               *paho.Publish
		expected        *paho.Publish
		expectedAliases []string
	}{
		{
			name:     "has alias",
			aliasMax: 4,
			aliases:  []string{"", "", "", "test", ""},
			p: &paho.Publish{
				Topic: "test",
			},
			expected: &paho.Publish{
				Properties: &paho.PublishProperties{
					TopicAlias: paho.Uint16(3),
				},
			},
			expectedAliases: []string{"", "", "", "test", ""},
		},
		{
			name:     "reset alias",
			aliasMax: 4,
			aliases:  []string{"", "", "", "test", ""},
			p: &paho.Publish{
				Topic: "test2",
				Properties: &paho.PublishProperties{
					TopicAlias: paho.Uint16(3),
				},
			},
			expected: &paho.Publish{
				Topic: "test2",
				Properties: &paho.PublishProperties{
					TopicAlias: paho.Uint16(3),
				},
			},
			expectedAliases: []string{"", "", "", "test2", ""},
		},
		{
			name:     "no alias",
			aliasMax: 4,
			aliases:  []string{"", "", "", "", ""},
			p: &paho.Publish{
				Topic: "test",
			},
			expected: &paho.Publish{
				Topic: "test",
				Properties: &paho.PublishProperties{
					TopicAlias: paho.Uint16(1),
				},
			},
			expectedAliases: []string{"", "test", "", "", ""},
		},
		{
			name:     "properties no alias",
			aliasMax: 4,
			aliases:  []string{"", "", "", "", ""},
			p: &paho.Publish{
				Topic:      "test",
				Properties: &paho.PublishProperties{},
			},
			expected: &paho.Publish{
				Topic: "test",
				Properties: &paho.PublishProperties{
					TopicAlias: paho.Uint16(1),
				},
			},
			expectedAliases: []string{"", "test", "", "", ""},
		},
		{
			name:     "no alias free",
			aliasMax: 1,
			aliases:  []string{"", "full"},
			p: &paho.Publish{
				Topic: "test",
			},
			expected: &paho.Publish{
				Topic: "test",
			},
			expectedAliases: []string{"", "full"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ta := &TAHandler{
				aliasMax: tt.aliasMax,
				aliases:  tt.aliases,
			}
			ta.PublishHook(tt.p)
			assert.Equal(t, tt.expected, tt.p)
			assert.Equal(t, tt.expectedAliases, ta.aliases)
		})
	}
}
