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
 * AI Disclosure: The durable queue support was generated with OpenAI GPT-6 via Codex.
 * AI-generated portions are made available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

// Package file provides a sequence-ordered disk queue. The basicfile sibling
// package retains <- v0.23 timestamp-ordered implementation as an opt-out.
package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/eclipse/paho.golang/autopaho/queue"
)

// A queue implementation that stores all data on disk
// This will be slow when there are a lot of messages queued. That is because the directory is read with every call to
// Peek/DeQueue (could cache some of this in RAM, but there is a reasonable chance that the OS does this for us).

const (
	folderPermissions = os.FileMode(0770)
	corruptExtension  = ".CORRUPT" // quarantined files will be given this extension
)

// Option configures a file queue created by New.
type Option func(*options)

type options struct {
	durable bool
}

// Queue is a sequence-ordered file-based queue.
type Queue struct {
	mu              sync.Mutex
	path            string
	prefix          string
	extension       string
	queueEmpty      bool                 // true is the queue is currently empty
	waiting         []chan<- struct{}    // closed when something arrives in the queue
	waitingForEmpty []chan<- struct{}    // closed when queue is empty
	lastSequence    uint64               // The last sequence written to disk
	syncFile        func(*os.File) error // nil preserves the default unsynced writes
}

// New creates a new file-based queue.
// Note that a file is written, read and deleted as part of this process to check that the path is usable.
// New entries are ordered by a sequence in their filenames, independently of the clock and filesystem timestamp
// resolution. Legacy files are read first, ordered by ModTime and then filename. Only one Queue may use a
// path/prefix/extension namespace at a time, and its files must not be modified externally.
// By default New does not sync writes. Pass WithDurable() to enable durable mode.
func New(path string, prefix string, extension string, opts ...Option) (*Queue, error) {
	var config options
	for _, opt := range opts {
		opt(&config)
	}
	var syncFile func(*os.File) error
	if config.durable {
		syncFile = (*os.File).Sync
	}
	return newQueue(path, prefix, extension, syncFile)
}

func newQueue(path, prefix, extension string, syncFile func(*os.File) error) (*Queue, error) {
	if len(extension) > 0 && extension[0] != '.' {
		extension = "." + extension
	}

	if err := makeQueueDirectory(path, syncFile); err != nil {
		return nil, fmt.Errorf("failed to prepare queue folder: %w", err)
	}

	if err := probeDirectory(path, prefix, extension, ""); err != nil {
		return nil, err
	}

	q := &Queue{
		path:      path,
		prefix:    prefix,
		extension: extension,
		syncFile:  syncFile,
	}

	entries, last, err := readEntries(path, prefix, extension)
	if err != nil {
		return nil, fmt.Errorf("failed reading queue: %w", err)
	}
	for _, e := range entries {
		if e.Sequence != 0 && e.Key != "" {
			return nil, fmt.Errorf("invalid queue filename %q", e.Name)
		}
	}
	q.lastSequence = last
	q.queueEmpty = len(entries) == 0

	return q, nil

}

// Wait returns a channel that will be closed when there is something in the queue
func (q *Queue) Wait() chan struct{} {
	c := make(chan struct{})
	q.mu.Lock()
	if !q.queueEmpty {
		q.mu.Unlock()
		close(c)
		return c
	}
	q.waiting = append(q.waiting, c)
	q.mu.Unlock()
	return c
}

// WaitForEmpty returns a channel that is closed when the queue is empty
func (q *Queue) WaitForEmpty() chan struct{} {
	c := make(chan struct{})
	q.mu.Lock()
	if q.queueEmpty {
		q.mu.Unlock()
		close(c)
		return c
	}
	q.waitingForEmpty = append(q.waitingForEmpty, c)
	q.mu.Unlock()
	return c
}

// Enqueue adds an item to the queue. By default it writes directly to a new file;
// a crash can leave a partial entry. Peek waits for writes to finish, and failed
// writes attempt to remove the entry. In durable mode, it syncs a temporary file
// before renaming it into place and syncing the directory. ErrDurabilityUncertain
// means the entry was published but directory sync failed; retrying can enqueue a duplicate.
func (q *Queue) Enqueue(p io.Reader) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	err := q.put(p)
	if err != nil {
		return err
	}
	err = q.syncDirectory()
	// A completed entry remains visible even if the subsequent directory sync failed.
	if q.queueEmpty {
		q.queueEmpty = false
		for _, c := range q.waiting {
			close(c)
		}
		q.waiting = q.waiting[:0]
	}
	return err
}

// Peek retrieves the oldest item from the queue (without removing it)
func (q *Queue) Peek() (queue.Entry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queueEmpty {
		return nil, queue.ErrEmpty
	}
	e, err := q.get()
	if err == io.EOF {
		q.queueEmpty = true
		for _, c := range q.waitingForEmpty {
			close(c)
		}
		q.waitingForEmpty = q.waitingForEmpty[:0]
		return nil, queue.ErrEmpty
	}
	return e, err
}

// put writes out an item to disk
func (q *Queue) put(p io.Reader) error {
	sequence, err := nextSequence(&q.lastSequence)
	if err != nil {
		return err
	}
	name := sequenceFilename(q.prefix, q.extension, sequence, "")
	return writeEntry(q.path, name, func(w io.Writer) error {
		_, err := io.Copy(w, p)
		return err
	}, q.syncFile)
}

// get() returns a ReadCloser that accesses the oldest file available
// caller must hold lock on mu
func (q *Queue) get() (entry, error) {
	fn, err := q.oldestEntry()
	if err != nil {
		return entry{}, err
	}
	f, err := os.Open(fn)
	if err != nil {
		return entry{}, err
	}
	return entry{f: f, syncDirectory: q.syncDirectory}, nil
}

// oldestEntry returns the filename of the oldest entry in the queue (if any - io.EOF means none)
func (q *Queue) oldestEntry() (string, error) {
	entries, _, err := readEntries(q.path, q.prefix, q.extension)
	if err != nil {
		return "", fmt.Errorf("failed to read dir: %w", err)
	}

	if len(entries) == 0 {
		return "", io.EOF
	}
	return filepath.Join(q.path, entries[0].Name), nil
}

// entry is used to return a queue entry from Peek
type entry struct {
	f             *os.File
	syncDirectory func() error
}

// Reader provides access to the file contents
func (e entry) Reader() (io.Reader, error) {
	return e.f, nil
}

// Leave closes the entry leaving it in the queue (will be returned on subsequent calls to Peek)
func (e entry) Leave() error {
	return e.f.Close()
}

// Remove this entry from the queue
func (e entry) Remove() error {
	cErr := e.f.Close() // Want to attempt to remove the file regardless of any errors here
	if err := os.Remove(e.f.Name()); err != nil {
		return err
	}
	return e.syncAfterChange(cErr)
}

// Quarantine flag that this entry has an error (remove from queue, potentially retaining data with error flagged)
func (e entry) Quarantine() error {
	cErr := e.f.Close() // Want to attempt to move the file regardless of any errors here

	// Attempt to add an extension so Peek no longer finds the file.
	if err := os.Rename(e.f.Name(), e.f.Name()+corruptExtension); err != nil {
		// Attempt to remove the file (important that we don't end up in an infinite loop retrieving the same file!)
		if rErr := os.Remove(e.f.Name()); rErr != nil {
			return err // Error from rename is best thing to return
		}
		return e.syncAfterChange(fmt.Errorf("rename failed so file deleted: %w", err))
	}
	return e.syncAfterChange(cErr)
}

// Preserve the original operation error when directory sync succeeds or is disabled.
func (e entry) syncAfterChange(err error) error {
	if syncErr := e.syncDirectory(); syncErr != nil {
		return errors.Join(err, syncErr)
	}
	return err
}
