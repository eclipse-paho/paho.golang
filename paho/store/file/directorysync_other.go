//go:build !windows

/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was largely generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

// These helpers are intentionally duplicated in the file queue and store so
// each implementation can be copied and customized independently.
package file

import "os"

// syncParentDirectories indicates whether initialization must also sync ancestor directories.
const syncParentDirectories = true

// openDirectoryForSync returns a directory handle on which the caller can call Sync and Close.
func openDirectoryForSync(path string) (*os.File, error) {
	return os.Open(path)
}
