/*
 * Copyright (c) 2026 Contributors to the Eclipse Foundation
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v2.0
 * and Eclipse Distribution License v1.0 which accompany this distribution.
 * The Eclipse Public License is available at https://www.eclipse.org/legal/epl-2.0/
 * and the Eclipse Distribution License at http://www.eclipse.org/org/documents/edl-v10.php.
 *
 * AI Disclosure: This file was largely generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

package file

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// These helpers stay in this package so it can be copied and customized without importing another persistence package.
// If you make changes, please repeat them in `autopaho/queue/file/files.go`.

// diskEntry describes a committed file. Sequence zero denotes the legacy format.
type diskEntry struct {
	Name     string
	Key      string
	Sequence uint64
	Modified time.Time // Only used for legacy files.
}

// sequenceFilename uses a marker to distinguish sequences from legacy numeric filenames.
func sequenceFilename(prefix, extension string, sequence uint64, key string) string {
	// Format on the stack and concatenate once, avoiding intermediate strings
	// and interface allocations from fmt.Sprintf on every write.
	var digits [20]byte
	for i := len(digits) - 1; i >= 0; i-- {
		digits[i] = '0' + byte(sequence%10)
		sequence /= 10
	}
	if key != "" {
		return prefix + "s" + string(digits[:]) + "-" + key + extension
	}
	return prefix + "s" + string(digits[:]) + extension
}

// nextSequence reserves a sequence even if the subsequent write fails. Gaps are harmless.
func nextSequence(last *uint64) (uint64, error) {
	if *last == math.MaxUint64 {
		return 0, fmt.Errorf("file sequence exhausted")
	}
	*last += 1
	return *last, nil
}

// compareEntries places legacy entries first, then orders sequenced entries numerically.
// Equal legacy timestamps cannot be recovered; filenames break ties deterministically.
func compareEntries(a, b *diskEntry) int {
	if order := cmp.Compare(a.Sequence, b.Sequence); order != 0 {
		return order
	}
	if a.Sequence == 0 {
		if order := a.Modified.Compare(b.Modified); order != 0 {
			return order
		}
	}
	return strings.Compare(a.Name, b.Name)
}

// readEntries returns committed entries in order and the highest sequence, including
// quarantined files so their names cannot be reused after restarting.
func readEntries(path, prefix, extension string) ([]diskEntry, uint64, error) {
	files, err := os.ReadDir(path)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]diskEntry, 0, len(files))
	var last uint64
	quarantineSuffix := extension + corruptExtension
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		quarantined := strings.HasSuffix(name, quarantineSuffix)
		if quarantined {
			name = strings.TrimSuffix(name, corruptExtension)
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) || len(name) < len(prefix)+len(extension) {
			continue
		}
		body := name[len(prefix) : len(name)-len(extension)]
		entry := diskEntry{Name: file.Name()}
		if strings.HasPrefix(body, "s") {
			seq, key, _ := strings.Cut(body[1:], "-")
			if len(seq) != 20 || !digits(seq) || (strings.Contains(body, "-") && !digits(key)) {
				return nil, 0, fmt.Errorf("invalid sequence filename %q", file.Name())
			}
			entry.Sequence, err = strconv.ParseUint(seq, 10, 64)
			if err != nil || entry.Sequence == 0 {
				return nil, 0, fmt.Errorf("invalid sequence filename %q", file.Name())
			}
			entry.Key = key
			if entry.Sequence > last {
				last = entry.Sequence
			}
		} else if digits(body) {
			entry.Key = body
			if !quarantined {
				info, err := file.Info()
				if err != nil {
					return nil, 0, err
				}
				entry.Modified = info.ModTime()
			}
		} else {
			continue // Temporary files and files outside this namespace.
		}
		if !quarantined {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b diskEntry) int { return compareEntries(&a, &b) })
	// Sorting makes duplicate live sequences adjacent, so no separate map is
	// needed. Legacy entries all have sequence zero; quarantined entries were
	// excluded above.
	for i := 1; i < len(entries); i++ {
		if entries[i].Sequence != 0 && entries[i].Sequence == entries[i-1].Sequence {
			return nil, 0, fmt.Errorf("duplicate sequence in %q", entries[i].Name)
		}
	}
	return entries, last, nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func temp(path, name string) (*os.File, error) {
	// The extra leading dot keeps incomplete files out of readEntries.
	return os.CreateTemp(path, "."+name+"-*.tmp")
}

// probeDirectory checks the longest generated temporary filename, not just a short TEST
// name, and verifies read/write access without overwriting an existing file.
func probeDirectory(path, prefix, extension, key string) error {
	f, err := temp(path, sequenceFilename(prefix, extension, math.MaxUint64, key))
	if err != nil {
		return fmt.Errorf("failed to create test file (check filename length): %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.WriteString("test"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp's random decimal suffix can be shorter than its ten-digit
	// maximum. Pad the probe to exercise the actual maximum component length.
	length := len(sequenceFilename(prefix, extension, math.MaxUint64, key)) + len(".-1234567890.tmp")
	if padding := length - len(filepath.Base(name)); padding > 0 {
		longName := name + strings.Repeat("0", padding)
		if err := os.Rename(name, longName); err != nil {
			return fmt.Errorf("failed to create maximum-length filename: %w", err)
		}
		name = longName
	}
	if _, err := os.ReadFile(name); err != nil {
		return err
	}
	return os.Remove(name)
}

// writeEntry publishes a completed file with a rename. If syncFile is non-nil,
// it syncs the contents before publication. The caller handles directory sync.
func writeEntry(path, name string, write func(io.Writer) error, syncFile func(*os.File) error) error {
	final := filepath.Join(path, name)
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("sequence filename already exists: %w", os.ErrExist)
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := temp(path, name)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(f.Name())
		}
	}()
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if syncFile != nil {
		if err := syncFile(f); err != nil {
			f.Close()
			return fmt.Errorf("failed to sync store entry: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), final); err != nil {
		return err
	}
	published = true
	return nil
}
