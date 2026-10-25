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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eclipse/paho.golang/autopaho/queue"
)

func TestDurableQueueFilesystem(t *testing.T) {
	dir := t.TempDir()
	opts := []Option{WithDurable()}
	// Directory sync is not supported on every platform/filesystem. Check the OS
	// directly, then require the public constructor to report that limitation.
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	syncErr := f.Sync()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if syncErr != nil {
		if q, err := New(dir, "q-", ".que", opts...); err == nil || q != nil {
			t.Fatalf("accepted unsupported directory sync: queue=%v, error=%v", q, err)
		}
		return
	}

	dir = filepath.Join(dir, "new", "queue")
	q, err := New(dir, "q-", "que", opts...)
	if err != nil {
		t.Fatal(err)
	}
	if q.syncFile == nil {
		t.Fatal("WithDurable did not enable syncing")
	}
	for _, payload := range []string{"first", "second"} {
		if err := q.Enqueue(strings.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
	}
	// The old instance is no longer used after reopening the same namespace.
	q, err = New(dir, "q-", ".que", opts...)
	if err != nil {
		t.Fatal(err)
	}
	e, err := q.Peek()
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Reader()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "first" {
		t.Fatalf("first entry: %q, %v", data, err)
	}
	if err := e.Leave(); err != nil {
		t.Fatal(err)
	}
	e, err = q.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Quarantine(); err != nil {
		t.Fatal(err)
	}
	take(t, q, "second")
	// Durable mode changes no filenames; the default mode can reopen its data.
	q, err = New(dir, "q-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	if q.syncFile != nil {
		t.Fatal("durable option affected a later default constructor")
	}
	if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("removed entries reappeared: %v", err)
	}
}

func TestQueueDefaultDoesNotSync(t *testing.T) {
	for _, opts := range [][]Option{nil, {}} {
		q, err := New(t.TempDir(), "", "", opts...)
		if err != nil {
			t.Fatal(err)
		}
		if q.syncFile != nil {
			t.Fatal("default queue enabled syncing")
		}
		if err := q.Enqueue(strings.NewReader("default")); err != nil {
			t.Fatal(err)
		}
		take(t, q, "default")
	}
}

func TestDurableEnqueueSyncOrder(t *testing.T) {
	q, err := New(t.TempDir(), "q-", ".que")
	if err != nil {
		t.Fatal(err)
	}
	wait := q.Wait()
	var calls []string
	var handles []*os.File
	q.syncFile = func(f *os.File) error {
		handles = append(handles, f)
		// Ensure an assertion failure in this callback cannot leak the handle.
		t.Cleanup(func() { _ = f.Close() })
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		entries, _, err := readEntries(q.path, q.prefix, q.extension)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			calls = append(calls, "directory")
			if len(entries) != 1 {
				t.Fatalf("directory sync before publication: %v", entries)
			}
			// Read checks closure portably; Stat may return a native invalid-handle error on Windows.
			if _, err := handles[0].Read(nil); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("entry file not closed before directory sync: %v", err)
			}
		} else {
			calls = append(calls, "file")
			if len(entries) != 0 {
				t.Fatalf("entry published before file sync: %v", entries)
			}
			data, err := os.ReadFile(f.Name())
			if err != nil || string(data) != "complete" {
				t.Fatalf("sync before full write: %q, %v", data, err)
			}
		}
		select {
		case <-wait:
			t.Fatal("consumer notified before syncing finished")
		default:
		}
		return nil
	}
	if err := q.Enqueue(strings.NewReader("complete")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"file", "directory"}) {
		t.Fatalf("sync order: %v", calls)
	}
	select {
	case <-wait:
	default:
		t.Fatal("consumer not notified after successful enqueue")
	}
	for _, f := range handles {
		if _, err := f.Read(nil); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("sync handle left open: %v", err)
		}
	}
}

func TestDurableEnqueueSyncFailure(t *testing.T) {
	for _, failDirectory := range []bool{false, true} {
		name := "file"
		if failDirectory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			q, err := New(t.TempDir(), "", "")
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("sync failed")
			var handles []*os.File
			q.syncFile = func(f *os.File) error {
				handles = append(handles, f)
				t.Cleanup(func() { _ = f.Close() })
				info, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if info.IsDir() == failDirectory {
					return failure
				}
				return nil
			}
			wait := q.Wait()
			err = q.Enqueue(strings.NewReader("message"))
			if !errors.Is(err, failure) || errors.Is(err, ErrDurabilityUncertain) != failDirectory {
				t.Fatalf("wrong sync error: %v", err)
			}
			for _, f := range handles {
				if _, err := f.Read(nil); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("failed sync leaked handle: %v", err)
				}
			}
			files, err := os.ReadDir(q.path)
			if err != nil {
				t.Fatal(err)
			}
			q.syncFile = func(*os.File) error { return nil }
			if failDirectory {
				if len(files) != 1 {
					t.Fatalf("published file lost or temp left behind: %v", files)
				}
				select {
				case <-wait:
				default:
					t.Fatal("published entry did not wake waiting consumer")
				}
				take(t, q, "message")
			} else {
				if len(files) != 0 {
					t.Fatalf("failed file sync left files: %v", files)
				}
				select {
				case <-wait:
					t.Fatal("failed file sync woke consumer")
				default:
				}
			}
			if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
				t.Fatalf("expected empty queue: %v", err)
			}
			if err := q.Enqueue(strings.NewReader("next")); err != nil {
				t.Fatal(err)
			}
			take(t, q, "next")
		})
	}
}

func TestDurableEntryChangesSyncDirectory(t *testing.T) {
	for _, operation := range []string{"remove", "quarantine", "quarantine fallback"} {
		for _, failSync := range []bool{false, true} {
			t.Run(operation+"/"+map[bool]string{false: "success", true: "sync failure"}[failSync], func(t *testing.T) {
				q, err := New(t.TempDir(), "q-", ".que")
				if err != nil {
					t.Fatal(err)
				}
				if err := q.Enqueue(strings.NewReader("message")); err != nil {
					t.Fatal(err)
				}
				e, err := q.Peek()
				if err != nil {
					t.Fatal(err)
				}
				if operation == "quarantine fallback" {
					// A directory at the quarantine destination makes rename fail.
					if err := os.Mkdir(e.(entry).f.Name()+corruptExtension, 0700); err != nil {
						t.Fatal(err)
					}
				}
				failure := errors.New("directory sync failed")
				calls := 0
				q.syncFile = func(f *os.File) error {
					calls++
					info, err := f.Stat()
					if err != nil || !info.IsDir() {
						t.Fatalf("expected directory sync: %v", err)
					}
					entries, _, err := readEntries(q.path, q.prefix, q.extension)
					if err != nil || len(entries) != 0 {
						t.Fatalf("entry still live at sync: %v, %v", entries, err)
					}
					if failSync {
						return failure
					}
					return nil
				}
				if operation == "remove" {
					err = e.Remove()
				} else {
					err = e.Quarantine()
				}
				if calls != 1 || errors.Is(err, failure) != failSync || errors.Is(err, ErrDurabilityUncertain) != failSync {
					t.Fatalf("sync calls=%d, error=%v", calls, err)
				}
				if !failSync && (err != nil) != (operation == "quarantine fallback") {
					t.Fatalf("unexpected operation error: %v", err)
				}
				if _, err := q.Peek(); !errors.Is(err, queue.ErrEmpty) {
					t.Fatalf("entry still in queue: %v", err)
				}
			})
		}
	}
}

func TestDurableDirectoryCreation(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "new", "queue")
	var calls []string
	var handles []*os.File
	q, err := newQueue(dir, "q-", ".que", func(f *os.File) error {
		calls = append(calls, f.Name())
		handles = append(handles, f)
		t.Cleanup(func() { _ = f.Close() })
		return nil
	})
	if err != nil || q == nil {
		t.Fatalf("creating durable queue: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for current := resolved; ; current = filepath.Dir(current) {
		want = append(want, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("synced directories %v, want %v", calls, want)
	}
	for _, f := range handles {
		if _, err := f.Read(nil); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("directory creation leaked handle: %v", err)
		}
	}

	failure := errors.New("directory sync unsupported")
	q, err = newQueue(dir, "q-", ".que", func(*os.File) error { return failure })
	if !errors.Is(err, failure) || q != nil {
		t.Fatalf("constructor silently ignored sync failure: queue=%v, error=%v", q, err)
	}

	// Retrying a failed initialization must still persist the parent entries,
	// even though all the directories now exist.
	calls = nil
	if _, err := newQueue(dir, "q-", ".que", func(f *os.File) error {
		calls = append(calls, f.Name())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("retry skipped parent directory sync: %v, want %v", calls, want)
	}
}

func TestDurableDirectoryThroughSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	var calls []string
	_, err := newQueue(filepath.Join(link, "queue"), "q-", ".que", func(f *os.File) error {
		calls = append(calls, f.Name())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 || calls[0] != filepath.Join(resolved, "queue") || calls[1] != resolved {
		t.Fatalf("did not sync directory in its actual parent: %v", calls)
	}
}
