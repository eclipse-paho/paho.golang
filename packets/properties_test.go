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

package packets

import (
	"fmt"
	"testing"
)

func TestPropertiess(t *testing.T) {
	if !ValidateID(PUBLISH, PropPayloadFormat) {
		t.Fatalf("'payloadFormat' is valid for 'PUBLISH' packets")
	}

	if !ValidateID(PUBLISH, PropMessageExpiry) {
		t.Fatalf("'messageExpiry' is valid for 'PUBLISH' packets")
	}

	if !ValidateID(PUBLISH, PropResponseTopic) {
		t.Fatalf("'responseTopic' is valid for 'PUBLISH' packets")
	}

	if !ValidateID(PUBLISH, PropCorrelationData) {
		t.Fatalf("'correlationData' is valid for 'PUBLISH' packets")
	}

	if !ValidateID(CONNECT, PropSessionExpiryInterval) {
		t.Fatalf("'sessionExpiryInterval' is valid for 'CONNECT' packets")
	}

	if !ValidateID(DISCONNECT, PropSessionExpiryInterval) {
		t.Fatalf("'sessionExpiryInterval' is valid for 'DISCONNECT' packets")
	}

	if !ValidateID(CONNACK, PropAssignedClientID) {
		t.Fatalf("'assignedClientID' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNACK, PropServerKeepAlive) {
		t.Fatalf("'serverKeepAlive' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNECT, PropAuthMethod) {
		t.Fatalf("'authMethod' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNACK, PropAuthMethod) {
		t.Fatalf("'authMethod' is valid for 'CONNACK' packets")
	}

	if !ValidateID(AUTH, PropAuthMethod) {
		t.Fatalf("'authMethod' is valid for 'auth' packets")
	}

	if !ValidateID(CONNECT, PropAuthData) {
		t.Fatalf("'authData' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNACK, PropAuthData) {
		t.Fatalf("'authData' is valid for 'CONNACK' packets")
	}

	if !ValidateID(AUTH, PropAuthData) {
		t.Fatalf("'authData' is valid for 'auth' packets")
	}

	if !ValidateID(CONNECT, PropRequestProblemInfo) {
		t.Fatalf("'requestProblemInfo' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNECT, PropWillDelayInterval) {
		t.Fatalf("'willDelayInterval' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNECT, PropRequestResponseInfo) {
		t.Fatalf("'requestResponseInfo' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNACK, PropResponseInfo) {
		t.Fatalf("'ResponseInfo' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNACK, PropServerReference) {
		t.Fatalf("'serverReference' is valid for 'CONNACK' packets")
	}

	if !ValidateID(DISCONNECT, PropServerReference) {
		t.Fatalf("'serverReference' is valid for 'DISCONNECT' packets")
	}

	if !ValidateID(CONNACK, PropReasonString) {
		t.Fatalf("'reasonString' is valid for 'CONNACK' packets")
	}

	if !ValidateID(DISCONNECT, PropReasonString) {
		t.Fatalf("'reasonString' is valid for 'DISCONNECT' packets")
	}

	if !ValidateID(CONNECT, PropReceiveMaximum) {
		t.Fatalf("'receiveMaximum' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNACK, PropReceiveMaximum) {
		t.Fatalf("'receiveMaximum' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNECT, PropTopicAliasMaximum) {
		t.Fatalf("'topicAliasMaximum' is valid for 'CONNECT' packets")
	}

	if !ValidateID(CONNACK, PropTopicAliasMaximum) {
		t.Fatalf("'topicAliasMaximum' is valid for 'CONNACK' packets")
	}

	if !ValidateID(PUBLISH, PropTopicAlias) {
		t.Fatalf("'topicAlias' is valid for 'PUBLISH' packets")
	}

	if !ValidateID(CONNACK, PropMaximumQOS) {
		t.Fatalf("'maximumQOS' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNACK, PropRetainAvailable) {
		t.Fatalf("'retainAvailable' is valid for 'CONNACK' packets")
	}

	if !ValidateID(CONNECT, PropUser) {
		t.Fatalf("'user' is valid for 'CONNECT' packets")
	}

	if !ValidateID(PUBLISH, PropUser) {
		t.Fatalf("'user' is valid for 'PUBLISH' packets")
	}
}

func TestInvalidProperties(t *testing.T) {
	if ValidateID(PUBLISH, PropRequestResponseInfo) {
		t.Fatalf("'requestReplyInfo' is invalid for 'PUBLISH' packets")
	}
}

func BenchmarkPropertyCreationStruct(b *testing.B) {
	var p *Properties
	pf := byte(1)
	pe := uint32(32)
	for i := 0; i < b.N; i++ {
		p = &Properties{
			PayloadFormat:   &pf,
			MessageExpiry:   &pe,
			ContentType:     "mime/json",
			ResponseTopic:   "x/y",
			CorrelationData: []byte("corelid"),
		}
	}
	_ = fmt.Sprintln(p)
}
