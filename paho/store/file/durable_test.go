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
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/packets"
)

func durableTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := newStore(dir, "p", ".pkt", options{durable: true}, func(*os.File) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreDefaultStillSyncsFiles(t *testing.T) {
	for _, opts := range [][]Option{nil, {}} {
		s, err := New(t.TempDir(), "", "", opts...)
		if err != nil {
			t.Fatal(err)
		}
		if s.durable || s.syncFile == nil {
			t.Fatal("default must sync file contents without enabling directory sync")
		}
		calls := 0
		s.syncFile = func(f *os.File) error {
			info, err := f.Stat()
			if err != nil || info.IsDir() {
				t.Fatalf("default synced directory: %v", err)
			}
			calls++
			return nil
		}
		if err := s.Put(1, packets.PUBLISH, strings.NewReader("original")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(1); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("file sync calls=%d, want 1", calls)
		}
	}
}

func TestStoreWithDurableSyncsDirectories(t *testing.T) {
	dir := t.TempDir()
	opts := []Option{WithDurable()}
	f, err := openDirectoryForSync(dir)
	if errors.Is(err, errors.ErrUnsupported) {
		if s, err := New(dir, "p", ".pkt", opts...); s != nil || !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("accepted unsupported filesystem: store=%v, error=%v", s, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	syncErr := f.Sync()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if syncErr != nil {
		if s, err := New(dir, "p", ".pkt", opts...); err == nil || s != nil {
			t.Fatalf("accepted unsupported directory sync: store=%v, error=%v", s, err)
		}
		return
	}
	// Reusing the same option slice must configure each store independently.
	for _, prefix := range []string{"client-", "server-"} {
		s, err := New(dir, prefix, ".pkt", opts...)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		s.syncFile = func(f *os.File) error {
			info, err := f.Stat()
			if err != nil {
				return err
			}
			if info.IsDir() {
				calls = append(calls, "directory")
			} else {
				calls = append(calls, "file")
			}
			return nil
		}
		if err := s.Put(1, packets.PUBLISH, strings.NewReader("durable")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(1); err != nil {
			t.Fatal(err)
		}
		if want := []string{"file", "directory", "directory"}; !reflect.DeepEqual(calls, want) {
			t.Fatalf("sync calls=%v, want %v", calls, want)
		}
	}
}

func TestDurableReplacementSyncOrder(t *testing.T) {
	s := durableTestStore(t, t.TempDir())
	if err := s.Put(7, packets.PUBLISH, strings.NewReader("publish")); err != nil {
		t.Fatal(err)
	}
	oldName := filepath.Join(s.path, s.packets[7].Name)
	var calls []string
	var handles []*os.File
	s.syncFile = func(f *os.File) error {
		handles = append(handles, f)
		// Ensure an assertion failure in this callback cannot leak the handle.
		t.Cleanup(func() { _ = f.Close() })
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			calls = append(calls, "file")
			data, err := os.ReadFile(f.Name())
			if err != nil || string(data) != "pubrel" {
				t.Fatalf("file sync before complete write: %q, %v", data, err)
			}
		} else {
			calls = append(calls, "directory")
			// Read checks closure portably; Stat may return a native invalid-handle error on Windows.
			if _, err := handles[0].Read(nil); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("file not closed before publication: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(s.path, s.packets[7].Name))
			if err != nil || string(data) != "pubrel" {
				t.Fatalf("replacement missing at directory sync: %q, %v", data, err)
			}
		}
		_, err = os.Stat(oldName)
		if len(calls) < 3 && err != nil {
			t.Fatalf("predecessor removed before replacement was durable: %v", err)
		}
		if len(calls) == 3 && !os.IsNotExist(err) {
			t.Fatalf("cleanup sync before removing predecessor: %v", err)
		}
		return nil
	}
	if err := s.Put(7, packets.PUBREL, strings.NewReader("pubrel")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"file", "directory", "directory"}) {
		t.Fatalf("sync order=%v", calls)
	}
	for _, f := range handles {
		if _, err := f.Read(nil); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("sync leaked handle: %v", err)
		}
	}
}

func TestDurableFileSyncFailurePreservesPacket(t *testing.T) {
	s := durableTestStore(t, t.TempDir())
	if err := s.Put(7, packets.PUBLISH, strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("file sync failed")
	s.syncFile = func(*os.File) error { return failure }
	if err := s.Put(7, packets.PUBREL, strings.NewReader("replacement")); !errors.Is(err, failure) || errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("file sync failure: %v", err)
	}
	checkData(t, s, 7, "original")
	entries, err := os.ReadDir(s.path)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed write left files: %v, %v", entries, err)
	}
	s.syncFile = func(*os.File) error { return nil }
	if err := s.Put(7, packets.PUBREL, strings.NewReader("replacement")); err != nil {
		t.Fatal(err)
	}
	checkData(t, s, 7, "replacement")
}

func TestDurableMutationSyncFailureStopsStore(t *testing.T) {
	for _, operation := range []string{"put", "replace", "delete", "quarantine", "quarantine fallback", "reset"} {
		t.Run(operation, func(t *testing.T) {
			s := durableTestStore(t, t.TempDir())
			if operation != "put" {
				if err := s.Put(7, packets.PUBLISH, strings.NewReader("original")); err != nil {
					t.Fatal(err)
				}
			}
			if operation == "quarantine fallback" {
				if err := os.Mkdir(filepath.Join(s.path, s.packets[7].Name)+corruptExtension, 0700); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("directory sync failed")
			s.syncFile = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() {
					return failure
				}
				return nil
			}
			var err error
			switch operation {
			case "put", "replace":
				err = s.Put(7, packets.PUBREL, strings.NewReader("new"))
			case "delete":
				err = s.Delete(7)
			case "quarantine", "quarantine fallback":
				err = s.Quarantine(7)
			case "reset":
				err = s.Reset()
			}
			if !errors.Is(err, failure) || !errors.Is(err, ErrDurabilityUncertain) {
				t.Fatalf("uncertain mutation error: %v", err)
			}
			// The session manager can release IDs after store errors. Prevent all
			// subsequent access until a new store/session recovers what is on disk.
			for _, action := range []func() error{
				func() error { return s.Put(7, packets.PUBLISH, strings.NewReader("overwrite")) },
				func() error { _, err := s.Get(7); return err },
				func() error { _, err := s.List(); return err },
				func() error { return s.Delete(7) },
				func() error { return s.Quarantine(7) },
				s.Reset,
			} {
				if err := action(); !errors.Is(err, ErrDurabilityUncertain) {
					t.Fatalf("uncertain store allowed reuse: %v", err)
				}
			}
			s = durableTestStore(t, s.path)
			if operation == "put" || operation == "replace" {
				checkData(t, s, 7, "new")
				if err := s.Delete(7); err != nil {
					t.Fatal(err)
				}
			}
			checkIDs(t, s, []uint16{})
		})
	}
}

func TestDurableCleanupFailurePreservesNewest(t *testing.T) {
	for _, operation := range []string{"delete", "quarantine", "reset"} {
		t.Run(operation, func(t *testing.T) {
			s := durableTestStore(t, t.TempDir())
			if err := s.Put(7, packets.PUBLISH, strings.NewReader("publish")); err != nil {
				t.Fatal(err)
			}
			directorySyncs := 0
			failure := errors.New("cleanup sync failed")
			s.syncFile = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() {
					directorySyncs++
					if directorySyncs > 1 {
						return failure
					}
				}
				return nil
			}
			// Publication succeeded durably, so failed predecessor cleanup must not
			// report Put failure and cause AddToSession to release a stored packet ID.
			if err := s.Put(7, packets.PUBREL, strings.NewReader("pubrel")); err != nil {
				t.Fatal(err)
			}
			if len(s.packets[7].obsolete) != 1 {
				t.Fatal("forgot cleanup before it was durable")
			}
			var remove func() error
			switch operation {
			case "delete":
				remove = func() error { return s.Delete(7) }
			case "quarantine":
				remove = func() error { return s.Quarantine(7) }
			case "reset":
				remove = s.Reset
			}
			if err := remove(); !errors.Is(err, failure) {
				t.Fatalf("ignored cleanup barrier: %v", err)
			}
			checkData(t, s, 7, "pubrel")
			s.syncFile = func(*os.File) error { return nil }
			if err := remove(); err != nil {
				t.Fatal(err)
			}
			s = durableTestStore(t, s.path)
			checkIDs(t, s, []uint16{})
		})
	}
}

func TestDurableRecoverySyncFailure(t *testing.T) {
	dir := t.TempDir()
	oldName := filepath.Join(dir, "p7.pkt")
	newName := filepath.Join(dir, sequenceFilename("p", ".pkt", 1, "00007"))
	for name, data := range map[string]string{oldName: "publish", newName: "pubrel"} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("recovery sync failed")
	s, err := newStore(dir, "p", ".pkt", options{durable: true}, func(f *os.File) error {
		if f.Name() == dir {
			if _, err := os.Stat(oldName); os.IsNotExist(err) {
				return failure
			}
		}
		return nil
	})
	if s != nil || !errors.Is(err, failure) {
		t.Fatalf("recovery did not propagate cleanup sync error: %v, %v", s, err)
	}
	s = durableTestStore(t, dir)
	checkData(t, s, 7, "pubrel")
	if err := s.Delete(7); err != nil {
		t.Fatal(err)
	}
	s = durableTestStore(t, dir)
	checkIDs(t, s, []uint16{})
}

func TestDurableStoreDirectorySyncUnsupported(t *testing.T) {
	failure := errors.New("directory sync unsupported")
	s, err := newStore(filepath.Join(t.TempDir(), "new", "store"), "p", ".pkt", options{durable: true}, func(*os.File) error { return failure })
	if s != nil || !errors.Is(err, failure) {
		t.Fatalf("silently disabled durability: %v, %v", s, err)
	}
}
