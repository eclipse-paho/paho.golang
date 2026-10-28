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

// These helpers are intentionally duplicated in the file queue and store so
// each implementation can be copied and customized independently.
package file

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// syncParentDirectories is false because we rely on NTFS's directory-structure flush.
// Opening ancestors for flushing would require write access outside the
// application's storage directory, including protected system directories.
// See MS-FSA 2.1.5.7 and product behavior note 80:
// https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa/0de7dc40-9627-437e-a4df-c4696cdc3d02
const syncParentDirectories = false

var getVolumeInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumeInformationByHandleW")

// openDirectoryForSync returns a writable NTFS directory handle for Sync and Close. Windows
// FlushFileBuffers requires write access, and opening a directory for writing
// requires FILE_FLAG_BACKUP_SEMANTICS. os.OpenFile also handles long paths.
func openDirectoryForSync(path string) (*os.File, error) {
	return openNTFSDirectory(path, directoryFilesystem)
}

func openNTFSDirectory(path string, filesystem func(*os.File) (string, error)) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	name, err := filesystem(f)
	if err == nil && !strings.EqualFold(name, "NTFS") {
		// MS-FSA product behavior note 80: other filesystems may report a
		// successful flush without persisting the directory structure.
		err = fmt.Errorf("durable directory sync requires NTFS, got %q: %w", name, errors.ErrUnsupported)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open directory for sync", Path: path, Err: errors.Join(err, f.Close())}
	}
	return f, nil
}

// Query the filesystem through the opened handle so junctions and volume mount
// points cannot accidentally select a different volume by name. Query failures,
// including unsupported remote volumes, are returned to the caller.
func directoryFilesystem(f *os.File) (string, error) {
	if err := getVolumeInformationByHandle.Find(); err != nil {
		return "", err
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var name [syscall.MAX_PATH + 1]uint16
	var queryErr error
	err = conn.Control(func(handle uintptr) {
		r, _, callErr := getVolumeInformationByHandle.Call(handle, 0, 0, 0, 0, 0,
			uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		if r == 0 {
			queryErr = os.NewSyscallError("GetVolumeInformationByHandleW", callErr)
		}
	})
	if err != nil {
		return "", err
	}
	if queryErr != nil {
		return "", queryErr
	}
	return syscall.UTF16ToString(name[:]), nil
}
