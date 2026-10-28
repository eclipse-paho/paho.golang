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
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSequenceFilename checks fixed-width sequence formatting, optional packet
// keys, and prefix/extension handling, including the largest uint64 sequence.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestSequenceFilename(t *testing.T) {
	for _, tc := range []struct {
		prefix, extension string
		sequence          uint64
		key, want         string
	}{
		{"", "", 0, "", "s00000000000000000000"},
		{"p", ".pkt", 1, "00001", "ps00000000000000000001-00001.pkt"},
		{"p", ".pkt", 10, "65535", "ps00000000000000000010-65535.pkt"},
		{"q", ".que", 1 << 63, "", "qs09223372036854775808.que"},
		{"prefix-", ".pkt", math.MaxUint64, "65535", "prefix-s18446744073709551615-65535.pkt"},
	} {
		if got := sequenceFilename(tc.prefix, tc.extension, tc.sequence, tc.key); got != tc.want {
			t.Fatalf("sequence %d, key %q: got %q, want %q", tc.sequence, tc.key, got, tc.want)
		}
	}
}

// TestReadMixedFormats checks that legacy entries precede sequenced entries in
// their respective orders, while ignored files do not become live entries and
// quarantined sequences still advance the recovered counter.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestReadMixedFormats(t *testing.T) {
	for _, ext := range []string{".pkt", ""} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			// Legacy files sort by ModTime, then name; all precede sequences.
			names := []string{"p9" + ext, "p10" + ext, "p2" + ext,
				sequenceFilename("p", ext, 10, ""), sequenceFilename("p", ext, 2, ""),
				sequenceFilename("p", ext, 40, "") + corruptExtension, ".p-incomplete.tmp", "other" + ext,
				"p1" + ext + ".tmp", "p1-123" + ext + corruptExtension}
			for i, name := range names {
				fn := filepath.Join(dir, name)
				if err := os.WriteFile(fn, []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
				ts := time.Unix(1_600_000_100, 0)
				if i == 0 {
					ts = time.Unix(1_600_000_050, 0)
				}
				if err := os.Chtimes(fn, ts, ts); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(dir, sequenceFilename("p", ext, 41, "")), 0700); err != nil {
				t.Fatal(err)
			}
			entries, last, err := readEntries(dir, "p", ext)
			if err != nil || last != 40 {
				t.Fatalf("Read: last=%d, err=%v", last, err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.Name)
			}
			want := []string{names[0], names[1], names[2], names[4], names[3]}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// TestRejectInvalidSequences checks that malformed filenames and duplicate live
// sequences fail recovery rather than silently producing an ambiguous order.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestRejectInvalidSequences(t *testing.T) {
	for _, body := range []string{"s1", "s00000000000000000000", "s18446744073709551616", "s00000000000000000001-", "s00000000000000000001-abc"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, body+".pkt"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readEntries(dir, "", ".pkt"); err == nil {
				t.Fatal("accepted malformed sequence")
			}
		})
	}
	dir := t.TempDir()
	for _, key := range []string{"00001", "00002"} {
		if err := os.WriteFile(filepath.Join(dir, sequenceFilename("", ".pkt", 1, key)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := readEntries(dir, "", ".pkt"); err == nil {
		t.Fatal("accepted duplicate sequence")
	}
}

// TestReadEntriesIgnoresQuarantinedDuplicates checks that quarantined files do
// not conflict with live sequences but still reserve their sequence numbers.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestReadEntriesIgnoresQuarantinedDuplicates(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"p1.pkt", "p2.pkt", // Legacy entries may share sequence zero.
		sequenceFilename("p", ".pkt", 5, "00001"),
		sequenceFilename("p", ".pkt", 5, "00002") + corruptExtension,
		sequenceFilename("p", ".pkt", 99, "00003") + corruptExtension,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, last, err := readEntries(dir, "p", ".pkt")
	if err != nil || len(entries) != 3 || last != 99 {
		t.Fatalf("readEntries returned %d entries, sequence %d: %v", len(entries), last, err)
	}
}

// TestWritePublicationAndFailure checks that incomplete writes stay hidden,
// failed writes are cleaned up, and an existing sequence cannot be overwritten.
// Also exercised in autopaho/queue/file/files_test.go, with different visibility
// expectations: only the default queue writes directly to its final filename.
func TestWritePublicationAndFailure(t *testing.T) {
	for _, sync := range []func(*os.File) error{nil, (*os.File).Sync} {
		dir := t.TempDir()
		name := sequenceFilename("", "", 1, "")
		failure := errors.New("write failed")
		err := writeEntry(dir, name, func(w io.Writer) error {
			if _, err := w.Write([]byte("partial")); err != nil {
				t.Fatal(err)
			}
			entries, _, err := readEntries(dir, "", "")
			if err != nil || len(entries) != 0 {
				t.Fatalf("incomplete file visible: %v, %v", entries, err)
			}
			return failure
		}, sync)
		if !errors.Is(err, failure) {
			t.Fatalf("want write error, got %v", err)
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 0 {
			t.Fatalf("failed write left files: %v, %v", files, err)
		}
		write := func(w io.Writer) error { _, err := io.WriteString(w, "complete"); return err }
		if err := writeEntry(dir, name, write, sync); err != nil {
			t.Fatal(err)
		}
		if err := writeEntry(dir, name, write, sync); !errors.Is(err, os.ErrExist) {
			t.Fatalf("overwriting sequence was allowed: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != "complete" {
			t.Fatalf("committed data=%q, err=%v", got, err)
		}
	}
}

// TestFailedRenameCleansTemporaryFile checks that a failed publication closes
// and removes the temporary file without altering the blocking destination.
// Also exercised by TestDurableFailedRenameCleansTemporaryFile in
// autopaho/queue/file/files_test.go; that variant enables syncing to use rename.
func TestFailedRenameCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	name := sequenceFilename("p", ".pkt", 1, "")
	var opened *os.File
	err := writeEntry(dir, name, func(w io.Writer) error {
		opened = w.(*os.File)
		t.Cleanup(func() { _ = opened.Close() })
		if _, err := io.WriteString(w, "complete"); err != nil {
			return err
		}
		// Force rename to fail after the existence check and completed write.
		return os.Mkdir(filepath.Join(dir, name), 0700)
	}, nil)
	if err == nil {
		t.Fatal("expected rename failure")
	}
	if opened == nil {
		t.Fatalf("failed before writing: %v", err)
	}
	// Read checks closure portably; Stat may return a native invalid-handle error on Windows.
	if _, err := opened.Read(nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("temporary file was not closed: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != name || !files[0].IsDir() {
		t.Fatalf("rename failure left temporary files or changed the destination: %v, %v", files, err)
	}
}

// TestSequenceExhaustion checks that the final uint64 sequence is usable and
// subsequent allocation fails without wrapping or changing the counter.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestSequenceExhaustion(t *testing.T) {
	last := uint64(math.MaxUint64 - 1)
	if next, err := nextSequence(&last); err != nil || next != math.MaxUint64 {
		t.Fatalf("last sequence: %d, %v", next, err)
	}
	if _, err := nextSequence(&last); err == nil || last != math.MaxUint64 {
		t.Fatalf("counter wrapped: %d, %v", last, err)
	}
}

// TestProbePreservesFilesAndChecksLength checks that the constructor's filename
// probe preserves existing data, rejects overlong names, and leaves no probe files.
// Intentionally duplicated in autopaho/queue/file/files_test.go; keep both copies in sync.
func TestProbePreservesFilesAndChecksLength(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "pTEST.pkt")
	if err := os.WriteFile(name, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := probeDirectory(dir, "p", ".pkt", "65535"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil || string(got) != "keep" {
		t.Fatalf("probe modified existing file: %q, %v", got, err)
	}
	// This exceeds the component limit on the common filesystems we support.
	if err := probeDirectory(dir, strings.Repeat("p", 256), ".pkt", "65535"); err == nil {
		t.Fatal("accepted an overlong filename")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "pTEST.pkt" {
		t.Fatalf("probe left files: %v, %v", entries, err)
	}
}
