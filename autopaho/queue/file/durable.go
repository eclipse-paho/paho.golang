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

// By default, the store does not call Sync after altering the filesystem. This can lead to corruption during an
// unexpected shutdown (e.g. power loss). Enabling durable writes does have a significant impact on performance.

package file

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WithDurable syncs each new entry before renaming it into place, then syncs the directory.
// Entry removal and quarantine also sync the directory.
// WARNING: This can significantly reduce throughput and requires filesystem support for directory sync.
// Durability depends on the OS and storage honoring Sync.
func WithDurable() Option {
	return func(o *options) {
		o.durable = true
	}
}

// ErrDurabilityUncertain means an entry was added, removed, or quarantined, but syncing the directory failed.
// The change is visible, but its survival across power loss is uncertain. Retrying Enqueue can create a duplicate.
// The returned error also wraps the underlying filesystem error for use with errors.Is/As.
var ErrDurabilityUncertain = errors.New("queue change applied but durability uncertain")

// syncDirectory is called only after a queue entry's directory change succeeds.
func (q *Queue) syncDirectory() error {
	if err := syncDirectory(q.path, q.syncFile); err != nil {
		return fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	return nil
}

func syncDirectory(path string, syncFile func(*os.File) error) error {
	if syncFile == nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open queue directory for sync: %w", err)
	}
	err = syncFile(f)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("sync queue directory %q: %w", path, err)
	}
	return nil
}

func makeQueueDirectory(path string, syncFile func(*os.File) error) error {
	if err := os.MkdirAll(path, folderPermissions); err != nil {
		return err
	}
	if syncFile == nil {
		return nil
	}

	// Persist the path from the leaf up to the root. Include existing ancestors:
	// they may have been created by an earlier constructor whose sync failed.
	// Stopping at the first existing directory would leave retries unsafe.
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
