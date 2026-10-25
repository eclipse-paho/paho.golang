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

package file

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Exercise the real OS calls. In particular, an os.Open handle on Windows NTFS
// would fail Sync with ERROR_ACCESS_DENIED instead of flushing the directory.
func TestDirectorySync(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "directory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := openDirectoryForSync(dir)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "entry")
	if err := os.WriteFile(name, []byte("packet"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	renamed := name + ".renamed"
	if err := os.Rename(name, renamed); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("directory handle left open: %v", err)
	}
}
