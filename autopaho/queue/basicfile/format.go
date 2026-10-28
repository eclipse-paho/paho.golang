/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 *
 * AI Disclosure: This file was largely generated with OpenAI GPT-5 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-5
 */

package basicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// checkLegacy refuses to open sequence-format data with a basicfile store or queue. Call it before writing a probe or
// modifying any data. extension must already be normalized. Callers must exclusively own the namespace afterwards.
func checkLegacy(path, prefix, extension string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to check basicfile namespace: %w", err)
	}
	patterns := []string{
		prefix + "s*" + extension,
		prefix + "s*" + extension + corruptExtension,
		"." + prefix + "s*" + extension + "-*.tmp*", // Probe may pad the temporary suffix.
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Mirror basicfile's glob-based namespace matching, including quarantine.
		// Also reject unfinished sequence writes: an empty prefix/extension lets
		// the original queue consume even dot-prefixed temporary files.
		// Legacy names are numeric, so reject the reserved s marker even if the
		// rest of the sequence name is malformed; do not reinterpret damaged data.
		for _, pattern := range patterns {
			match, err := filepath.Match(pattern, entry.Name())
			if err != nil {
				return fmt.Errorf("failed to check basicfile namespace: %w", err)
			}
			if match {
				return fmt.Errorf("basicfile cannot open sequence-format file %q; use the corresponding file package to drain or migrate the data before switching to basicfile", filepath.Join(path, entry.Name()))
			}
		}
	}
	return nil
}
