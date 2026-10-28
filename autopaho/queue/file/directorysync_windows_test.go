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
	"testing"
)

// TestUnsupportedFilesystemsCloseHandle checks that rejecting a non-NTFS volume
// or failing the filesystem query returns an error and closes the opened handle.
// Intentionally duplicated in paho/store/file/directorysync_windows_test.go; keep both copies in sync.
func TestUnsupportedFilesystemsCloseHandle(t *testing.T) {
	failure := errors.New("filesystem query failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"FAT", nil}, {"FAT32", nil}, {"exFAT", nil}, {"ReFS", nil}, {"", failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened *os.File
			f, err := openNTFSDirectory(t.TempDir(), func(f *os.File) (string, error) {
				opened = f
				t.Cleanup(func() { _ = f.Close() })
				return tc.name, tc.err
			})
			want := tc.err
			if want == nil {
				want = errors.ErrUnsupported
			}
			if f != nil || !errors.Is(err, want) {
				t.Fatalf("open returned %v, %v; want nil, %v", f, err, want)
			}
			if opened == nil {
				t.Fatal("failed before checking the filesystem")
			}
			if _, err := opened.Read(nil); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("rejected filesystem leaked handle: %v", err)
			}
		})
	}
}

// TestFilesystemQueryOnClosedHandle checks that querying a closed directory
// handle returns an error without reporting a filesystem name.
// Intentionally duplicated in paho/store/file/directorysync_windows_test.go; keep both copies in sync.
func TestFilesystemQueryOnClosedHandle(t *testing.T) {
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// RawConn.Control does not translate its closed-file error to os.ErrClosed.
	// Require the query to fail without returning a filesystem name.
	if name, err := directoryFilesystem(f); err == nil || name != "" {
		t.Fatalf("filesystem query on closed handle returned %q, %v; want empty name and error", name, err)
	}
}
