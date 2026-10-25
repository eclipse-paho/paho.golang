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
	"fmt"
	"os"
	"path/filepath"
)

// WithDurable syncs directory changes made by Put, Delete, Quarantine, Reset, and startup recovery. Replacements are
// durably published before older versions are removed. This can significantly reduce throughput and
// requires an OS/filesystem and storage that honor file and directory Sync.
func WithDurable() Option {
	return func(o *options) {
		o.durable = true
	}
}

// ErrDurabilityUncertain means a mutation was applied but its directory sync
// failed. Persistent state may have changed. The Store rejects further operations
// until it is reopened, so session code cannot reuse uncertain packet state.
// Stop the client and resolve the storage error before rebuilding its stores and
// session. The returned error also wraps the underlying filesystem error.
var ErrDurabilityUncertain = errors.New("store change applied but durability uncertain; reopen store before reuse")

// commitDirectory follows a mutation that changes the current packet state.
// Unlike queue entries, session writes cannot remain available after an uncertain
// result: AddToSession releases its packet ID when Put returns an error.
func (s *Store) commitDirectory() error {
	if !s.durable {
		return nil
	}
	if err := syncDirectory(s.path, s.syncFile); err != nil {
		s.failure = fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
		return s.failure
	}
	return nil
}

func syncDirectory(path string, syncFile func(*os.File) error) error {
	if syncFile == nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open store directory for sync: %w", err)
	}
	err = syncFile(f)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("sync store directory %q: %w", path, err)
	}
	return nil
}

func makeStoreDirectory(path string, syncFile func(*os.File) error) error {
	if err := os.MkdirAll(path, folderPermissions); err != nil {
		return err
	}
	if syncFile == nil {
		return nil
	}
	// Sync existing ancestors too: they may come from a failed initialization.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	current, err := filepath.Abs(resolved)
	if err != nil {
		return err
	}
	for {
		if err := syncDirectory(current, syncFile); err != nil {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}
