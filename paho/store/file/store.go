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
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	folderPermissions = os.FileMode(0o770)
	filePermissions   = os.FileMode(0o666)
	tmpExtension      = ".tmp"
	corruptExtension  = ".CORRUPT" // quarantined files will be given this extension
)

// New creates a file Store. Note that a file is written, read and deleted as part of this process to check that the
// path is usable.
// NOTE: Order is maintained using file ModTime, so there may be issues if the interval between messages is less than
// the file system ModTime resolution.
func New(path string, prefix string, extension string) (*Store, error) {
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

	return &Store{
		path:      path,
		prefix:    prefix,
		extension: extension,
	}, nil
}

// Store is an implementation of a Store that stores the data on disk
type Store struct {
	// server store - holds packets where the message ID was generated on the server
	sync.Mutex // Is this needed?
	path       string
	prefix     string
	extension  string
}

// Put stores the packet
// The store is performed via a temporary file to reduce the chance that a partially written file will
// cause issues.
func (s *Store) Put(packetID uint16, packetType byte, w io.WriterTo) error {
	s.Lock()
	defer s.Unlock()

	f, err := os.CreateTemp(s.path, s.fileNamePrefix(packetID)+"-*"+s.extension+tmpExtension)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpFn := f.Name()
	if _, err = w.WriteTo(f); err != nil {
		f.Close()
		_ = os.Remove(tmpFn)
		return fmt.Errorf("failed to write packet to temp file: %w", err)
	}
	f.Sync() // want timestamps to be as accurate as possible (close and rename do not imply sync)
	if err = f.Close(); err != nil {
		_ = os.Remove(tmpFn)
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err = os.Rename(tmpFn, s.filePathForId(packetID)); err != nil {
		_ = os.Remove(tmpFn)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}

// Get retrieves the requested packet
// Note that callers MUST close the returned ReadCloser
func (s *Store) Get(packetID uint16) (io.ReadCloser, error) {
	s.Lock()
	defer s.Unlock()
	f, err := os.OpenFile(s.filePathForId(packetID), os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open packet file: %w", err)
	}
	return f, nil
}

// Delete removes the message with the specified store ID
func (s *Store) Delete(id uint16) error {
	s.Lock()
	defer s.Unlock()
	return s.delete(id)
}

// Quarantine is called if a corrupt packet is detected.
// There is little we can do other than deleting the packet.
func (s *Store) Quarantine(id uint16) error {
	s.Lock()
	defer s.Unlock()
	f, err := os.CreateTemp(s.path, s.fileNamePrefix(id)+"-*"+s.extension+corruptExtension)
	if err != nil {
		s.delete(id) // delete the file (otherwise it may be sent on every reconnection)
		return fmt.Errorf("failed to create quarantine file: %w", err)
	}
	tmpFn := f.Name()
	if err := f.Close(); err != nil {
		s.delete(id) // delete the file (otherwise it may be sent on every reconnection)
		return fmt.Errorf("failed to close newly created quarantine file: %w", err)
	}
	if err := os.Rename(s.filePathForId(id), tmpFn); err != nil {
		s.delete(id) // delete the file (otherwise it may be sent on every reconnection)
		return fmt.Errorf("failed to move packet into quarantine: %w", err)
	}
	return nil
}

type idAndModTime struct {
	id      uint16
	modTime time.Time
}

// List returns packet IDs in the order they were Put
func (s *Store) List() ([]uint16, error) {
	s.Lock()
	defer s.Unlock()

	return s.list()
}

// Reset clears the store (deleting all messages)
func (s *Store) Reset() error {
	s.Lock()
	defer s.Unlock()
	ids, err := s.list()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if dErr := s.delete(id); dErr != nil {
			err = dErr // Attempt to delete all files and just return the last error
		}
	}
	return err
}

// String is for debugging purposes; it's too expensive to read in the data for this
func (s *Store) String() string {
	return fmt.Sprintf("store path: %s, prefix: %s, extension: %s", s.path, s.prefix, s.extension)
}

// list returns packet IDs in the order they were Put
// caller must lock mutex
func (s *Store) list() ([]uint16, error) {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return nil, fmt.Errorf("failed to read dir: %w", err)
	}

	ids := make([]idAndModTime, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fn := entry.Name()
		if match, err := filepath.Match(s.prefix+"*"+s.extension, fn); err != nil {
			return nil, fmt.Errorf("failed to read match %s: %w", fn, err)
		} else if !match {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve file info for %s: %w", fn, err)
		}
		idStr := fn[len(s.prefix) : len(fn)-len(s.extension)]
		id, err := strconv.ParseInt(idStr, 10, 0)
		if err != nil { // Could ignore, but raising an error seems safer
			return nil, fmt.Errorf("invalid id in filename %s", fn)
		}
		ids = append(ids, idAndModTime{uint16(id), info.ModTime()}) // Possible this will truncate id
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].modTime.Before(ids[j].modTime)
	})
	ret := make([]uint16, len(entries))
	for i := range ids {
		ret[i] = ids[i].id
	}
	return ret, nil
}

// delete removes the message with the specified store ID
// caller must gain any required locks
func (s *Store) delete(id uint16) error {
	if err := os.Remove(s.filePathForId(id)); err != nil {
		return fmt.Errorf("failed to remove packet file: %w", err)
	}
	return nil
}

// filePathForId returns the full path of the file used to store info for the passed in packet id
func (s *Store) filePathForId(packetID uint16) string {
	return filepath.Join(s.path, s.fileNamePrefix(packetID)+s.extension)
}

// fileNamePrefix returns the beginning of the filename for the specified packet ID
func (s *Store) fileNamePrefix(packetID uint16) string {
	return s.prefix + strconv.FormatInt(int64(packetID), 10)
}
