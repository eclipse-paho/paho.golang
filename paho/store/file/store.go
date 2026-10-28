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
 * AI Disclosure: The sequence persistence and durable store support were generated with
 * OpenAI GPT-6 via Codex. AI-generated portions are available under CC0-1.0.
 * SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0
 * Assisted-by: OpenAI GPT-6
 */

// Package file provides a sequence-ordered disk store. The basicfile sibling
// package retains the original timestamp-ordered implementation as an opt-out.
package file

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
)

const (
	folderPermissions = os.FileMode(0770)
	corruptExtension  = ".CORRUPT" // quarantined files will be given this extension
)

// Option configures a file store created by New.
type Option func(*options)

type options struct {
	durable bool
}

// New creates a file Store. Note that a file is written, read and deleted as part of this process to check that the
// path is usable.
// Files are ordered by a sequence in their filenames; packet IDs provide lookup.
// Legacy files remain readable and sort before new entries, using ModTime and filename to break ties. A successful Put
// replaces the ID and moves it to the end. Only one Store may own a path/prefix/extension namespace at a time. Files must
// not be modified externally while it is open.
// New syncs file contents. Pass WithDurable() for durable directory changes too.
func New(path string, prefix string, extension string, opts ...Option) (*Store, error) {
	var config options
	for _, opt := range opts {
		opt(&config)
	}
	return newStore(path, prefix, extension, config, (*os.File).Sync)
}

func newStore(path, prefix, extension string, config options, syncFile func(*os.File) error) (*Store, error) {
	if len(extension) > 0 && extension[0] != '.' {
		extension = "." + extension
	}

	var syncDir func(*os.File) error
	if config.durable {
		syncDir = syncFile
	}
	if err := makeStoreDirectory(path, syncDir); err != nil {
		return nil, fmt.Errorf("failed to prepare store folder: %w", err)
	}

	if err := probeDirectory(path, prefix, extension, "65535"); err != nil {
		return nil, err
	}

	s := &Store{
		path:      path,
		prefix:    prefix,
		extension: extension,
		packets:   make(map[uint16]*storedPacket),
		durable:   config.durable,
		syncFile:  syncFile,
	}
	entries, last, err := readEntries(path, prefix, extension)
	if err != nil {
		return nil, fmt.Errorf("failed reading store: %w", err)
	}
	s.lastSequence = last
	for _, e := range entries {
		id, err := strconv.ParseUint(e.Key, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid packet ID in filename %q: %w", e.Name, err)
		}
		p := &storedPacket{diskEntry: e}
		if previous, ok := s.packets[uint16(id)]; ok {
			p.obsolete = append(previous.obsolete, previous.Name)
		}
		s.packets[uint16(id)] = p
	}
	// A crash between committing a replacement and removing its predecessor can
	// leave both files. Keep the newest and remove predecessors before serving it.
	for _, p := range s.packets {
		if err := s.removeObsolete(p); err != nil {
			return nil, fmt.Errorf("failed recovering replaced packet: %w", err)
		}
	}
	return s, nil
}

// Store is an implementation of a Store that stores the data on disk
type Store struct {
	sync.Mutex
	path         string
	prefix       string
	extension    string
	lastSequence uint64 // The last sequence written to disk
	packets      map[uint16]*storedPacket
	durable      bool
	syncFile     func(*os.File) error
	failure      error // Uncertain mutations require recovery with a new Store.
}

type storedPacket struct {
	diskEntry
	obsolete []string
}

// Put stores the packet
// The store is performed via a temporary file to reduce the chance that a partially written file will
// cause issues.
func (s *Store) Put(packetID uint16, packetType byte, w io.WriterTo) error {
	s.Lock()
	defer s.Unlock()
	if s.failure != nil {
		return s.failure
	}

	sequence, err := nextSequence(&s.lastSequence)
	if err != nil {
		return err
	}
	var key [5]byte
	for i, id := len(key)-1, packetID; i >= 0; i-- {
		key[i] = '0' + byte(id%10)
		id /= 10
	}
	name := sequenceFilename(s.prefix, s.extension, sequence, string(key[:]))
	if err := writeEntry(s.path, name, func(out io.Writer) error {
		_, err := w.WriteTo(out)
		return err
	}, s.syncFile); err != nil {
		return fmt.Errorf("failed to store packet: %w", err)
	}
	// The packet ID is already the map key; retaining a second string for it is
	// only useful while parsing filenames during startup.
	p := &storedPacket{diskEntry: diskEntry{Name: name, Sequence: sequence}}
	if previous, ok := s.packets[packetID]; ok {
		p.obsolete = append(previous.obsolete, previous.Name)
	}
	s.packets[packetID] = p
	// Persist the replacement before removing any older version. Otherwise a
	// power failure could lose both the new directory entry and its predecessor.
	if err := s.commitDirectory(); err != nil {
		return err
	}
	// The new file is committed. If cleanup fails, retain the names and retry
	// before deleting/quarantining this ID, or during restart recovery.
	_ = s.removeObsolete(p)
	return nil
}

// Get retrieves the requested packet
// Note that callers MUST close the returned ReadCloser
func (s *Store) Get(packetID uint16) (io.ReadCloser, error) {
	s.Lock()
	defer s.Unlock()
	if s.failure != nil {
		return nil, s.failure
	}
	p, ok := s.packets[packetID]
	if !ok {
		return nil, fmt.Errorf("packet %d: %w", packetID, os.ErrNotExist)
	}
	f, err := os.Open(filepath.Join(s.path, p.Name))
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
func (s *Store) Quarantine(id uint16) error {
	s.Lock()
	defer s.Unlock()
	if s.failure != nil {
		return s.failure
	}
	p, ok := s.packets[id]
	if !ok {
		return fmt.Errorf("packet %d: %w", id, os.ErrNotExist)
	}
	if err := s.removeObsolete(p); err != nil {
		return err
	}
	name := filepath.Join(s.path, p.Name)
	if err := os.Rename(name, name+corruptExtension); err != nil {
		return errors.Join(fmt.Errorf("failed to move packet into quarantine: %w", err), s.delete(id))
	}
	delete(s.packets, id)
	return s.commitDirectory()
}

// List returns packet IDs in the order they were Put
func (s *Store) List() ([]uint16, error) {
	s.Lock()
	defer s.Unlock()
	if s.failure != nil {
		return nil, s.failure
	}

	return s.list()
}

// Reset clears the store (deleting all messages)
func (s *Store) Reset() error {
	s.Lock()
	defer s.Unlock()
	if s.failure != nil {
		return s.failure
	}
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
	ids := make([]uint16, 0, len(s.packets))
	for id := range s.packets {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uint16) int {
		return compareEntries(&s.packets[a].diskEntry, &s.packets[b].diskEntry)
	})
	return ids, nil
}

// delete removes the message with the specified store ID
// caller must gain any required locks
func (s *Store) delete(id uint16) error {
	if s.failure != nil {
		return s.failure
	}
	p, ok := s.packets[id]
	if !ok {
		return fmt.Errorf("packet %d: %w", id, os.ErrNotExist)
	}
	// Never remove the newest version while an older one could be recovered.
	if err := s.removeObsolete(p); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.path, p.Name)); err != nil {
		return fmt.Errorf("failed to remove packet file: %w", err)
	}
	delete(s.packets, id)
	return s.commitDirectory()
}

func (s *Store) removeObsolete(p *storedPacket) error {
	if len(p.obsolete) == 0 {
		return nil
	}
	for _, name := range p.obsolete {
		if err := os.Remove(filepath.Join(s.path, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove replaced packet: %w", err)
		}
	}
	if s.durable {
		// Retain the names until the removals are durable, even if the files are
		// already absent. Delete/Quarantine must retry this barrier before removing
		// the newest packet, or an old PUBLISH could reappear after a PUBREL.
		if err := syncDirectory(s.path, s.syncFile); err != nil {
			return fmt.Errorf("failed to sync replaced packet cleanup: %w", err)
		}
	}
	p.obsolete = nil
	return nil
}
