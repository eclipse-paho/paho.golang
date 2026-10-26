/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * This program and the accompanying materials are made available under the
 * Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and
 * Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).
 * AI Disclosure: This file was largely generated with OpenAI GPT-5 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-5
 */

package basicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the public constructor as well as filename recognition: the check
// must run before this basicfile implementation writes its TEST probe.
// Intentionally duplicated in paho/store/basicfile/format_test.go; keep both copies in sync.
func TestBasicfileRejectsSequences(t *testing.T) {
	for _, prefix := range []string{"p-", ""} {
		for _, extension := range []string{".pkt", "pkt", ""} {
			for _, body := range []string{
				"s00000000000000000001", "s00000000000000000001-00042",
				"s18446744073709551615-65535", "s1", // also refuse damaged sequence names
			} {
				for _, suffix := range []string{"", corruptExtension, "-123.tmp", "-123.tmp00"} {
					t.Run(prefix+"/"+extension+"/"+body+suffix, func(t *testing.T) {
						dir := t.TempDir()
						ext := extension
						if ext != "" && ext[0] != '.' {
							ext = "." + ext
						}
						name := prefix + body + ext + suffix
						if strings.HasPrefix(suffix, "-") {
							name = "." + name
						}
						// Include legacy data and a probe-name sentinel. Neither may be touched.
						names := []string{name, prefix + "7" + ext, prefix + "TEST" + ext}
						for _, fn := range names {
							if err := os.WriteFile(filepath.Join(dir, fn), []byte(fn), 0600); err != nil {
								t.Fatal(err)
							}
						}
						_, err := New(dir, prefix, extension)
						if err == nil || !strings.Contains(err.Error(), "basicfile cannot open sequence-format file") || !strings.Contains(err.Error(), name) {
							t.Fatalf("expected an actionable sequence-format error for %q, got %v", name, err)
						}
						entries, err := os.ReadDir(dir)
						if err != nil || len(entries) != len(names) {
							t.Fatalf("rejected open changed directory: %v, %v", entries, err)
						}
						for _, fn := range names {
							data, err := os.ReadFile(filepath.Join(dir, fn))
							if err != nil || string(data) != fn {
								t.Fatalf("rejected open changed %q: %q, %v", fn, data, err)
							}
						}
					})
				}
			}
		}
	}
}

// TestCheckLegacyNamespace checks that the downgrade safeguard respects namespace
// matching, ignores unrelated files and directories, and reports directory read errors.
// Intentionally duplicated in paho/store/basicfile/format_test.go; keep both copies in sync.
func TestCheckLegacyNamespace(t *testing.T) {
	for _, tt := range []struct {
		name, prefix, extension string
		directory, reject       bool
	}{
		{"p-1.pkt", "p-", ".pkt", false, false},
		{"p-1-123.pkt.CORRUPT", "p-", ".pkt", false, false},
		{"other-s00000000000000000001.pkt", "p-", ".pkt", false, false},
		{"p-s00000000000000000001.other", "p-", ".pkt", false, false},
		{"p-s00000000000000000001.other.CORRUPT", "p-", ".pkt", false, false},
		{".p-s00000000000000000001.pkt-123.tmp", "p-", ".pkt", false, true},
		{".other-s00000000000000000001.pkt-123.tmp", "p-", ".pkt", false, false},
		{".p-s00000000000000000001.other-123.tmp", "p-", ".pkt", false, false},
		{"p-s00000000000000000001.pkt", "p-", ".pkt", true, false},
		{"p-s00000000000000000001.pkt", "p-", "", false, true},
		// The original stores use filepath.Match; the guard must respect it too.
		{"p-s00000000000000000001.pkt", "p?", ".pkt", false, true},
	} {
		t.Run(tt.name+"/"+tt.prefix+"/"+tt.extension, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.name)
			if tt.directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkLegacy(dir, tt.prefix, tt.extension); (err != nil) != tt.reject {
				t.Fatalf("CheckLegacy = %v; want rejection %v", err, tt.reject)
			}
		})
	}
	if err := checkLegacy(filepath.Join(t.TempDir(), "missing"), "", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected directory read error, got %v", err)
	}
}
