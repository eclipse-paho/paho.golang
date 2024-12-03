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
 *  SPDX-License-Identifier: EPL-2.0 OR BSD-3-Clause
 */

package file

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/canonical/mqtt.golang/autopaho/queue"
)

// A queue implementation that stores all data on disk
// This will be slow when there are a lot of messages queued. That is because the directory is read with every call to
// Peek/DeQueue (could cache some of this in RAM, but there is a reasonable chance that the OS does this for us).

const (
	folderPermissions = os.FileMode(0o770)
	filePermissions   = os.FileMode(0o666)
	corruptExtension  = ".CORRUPT" // quarantined files will be given this extension
)

var maxTime = time.Unix(1<<63-62135596801, 999999999)

// Queue - basic file based queue
type Queue struct {
	mu              sync.Mutex
	path            string
	prefix          string
	extension       string
	queueEmpty      bool              // true is the queue is currently empty
	waiting         []chan<- struct{} // closed when something arrives in the queue
	waitingForEmpty []chan<- struct{} // closed when queue is empty
}

// New creates a new file-based queue. Note that a file is written, read and deleted as part of this process to check
// that the path is usable.
// NOTE: Order is maintained using file ModTime, so there may be issues if the interval between messages is less than
// the file system ModTime resolution.
func New(path string, prefix string, extension string) (*Queue, error) {
	if len(extension) > 0 && extension[0] != '.' {
		extension = "." + extension
	}

	folderExists := true
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			folderExists = false
		}
		return nil, fmt.Errorf("stat on folder failed: %w", err)
	}
	if !folderExists {
		if err := os.MkdirAll(path, folderPermissions); err != nil {
			return nil, fmt.Errorf("failed to create store folder: %w", err)
		}
	}

	// Better to fail fast; so we check that a file can be written/read with the passed in info
	fn := filepath.Join(path, prefix+"TEST"+extension)
	if err := os.WriteFile(fn, []byte("test"), filePermissions); err != nil {
		return nil, fmt.Errorf("failed to write test file to specified folder: %w", err)
	}
	if _, err := os.ReadFile(fn); err != nil {
		return nil, fmt.Errorf("failed to read test file from specified folder: %w", err)
	}
	if err := os.Remove(fn); err != nil {
		return nil, fmt.Errorf("failed to remove test file from specified folder: %w", err)
	}

	q := &Queue{
		path:      path,
		prefix:    prefix,
		extension: extension,
	}

	_, err := q.oldestEntry()
	if err == io.EOF {
		q.queueEmpty = true
	} else if err != nil {
		return nil, fmt.Errorf("failed checking for oldest entry: %w", err)
	}

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

// Enqueue add item to the queue.
func (q *Queue) Enqueue(p io.Reader) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	err := q.put(p)
	if err == nil && q.queueEmpty {
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
	// Use CreateTemp to generate a file with a unique name (it will be removed when packet has been transmitted)
	f, err := os.CreateTemp(q.path, q.prefix+"*"+q.extension)
	if err != nil {
		return err
	}

	if _, err = io.Copy(f, p); err != nil {
		f.Close()
		_ = os.Remove(f.Name()) // Attempt to remove the partial file (not much we can do if this fails)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(f.Name()) // Attempt to remove the partial file (not much we can do if this fails)
		return err
	}
	return nil
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
	return entry{f: f}, nil
}

// oldestEntry returns the filename of the oldest entry in the queue (if any - io.EOF means none)
func (q *Queue) oldestEntry() (string, error) {
	entries, err := os.ReadDir(q.path)
	if err != nil {
		return "", fmt.Errorf("failed to read dir: %w", err)
	}

	// Search for the oldest file
	var oldFn string
	oldTime := maxTime

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fn := entry.Name()
		if match, err := filepath.Match(q.prefix+"*"+q.extension, fn); err != nil {
			return "", fmt.Errorf("failed to read match %s: %w", fn, err)
		} else if !match {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("failed to retrieve file info for %s: %w", fn, err)
		}
		fileModTime := info.ModTime()
		if !fileModTime.Before(oldTime) {
			continue
		}
		oldFn = filepath.Join(q.path, fn)
		oldTime = fileModTime
	}
	if oldTime.Equal(maxTime) {
		return "", io.EOF
	}
	return oldFn, nil
}

// entry is used to return a queue entry from Peek
type entry struct {
	f *os.File
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
	if cErr != nil {
		return cErr
	}
	return nil
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
		return fmt.Errorf("rename failed so file deleted: %w", err)
	}
	if cErr != nil {
		return cErr
	}
	return nil
}
