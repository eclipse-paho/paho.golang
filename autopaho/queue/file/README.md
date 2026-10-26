<!--
Copyright (c) 2026 Contributors to the Eclipse Foundation

This program and the accompanying materials are made available under the 
Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and 
Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).

SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0

AI Disclosure: This file was largely generated with OpenAI GPT-5 via Codex.
AI-generated portions are made available under CC0-1.0.
Assisted-by: OpenAI GPT-5
AI Disclosure: The durable mode and benchmark documentation was generated with OpenAI GPT-6 via Codex.
Assisted-by: OpenAI GPT-6
-->

File queue
==========

This is a file-backed queue. Each queue entry is written to disk in a file named 
`<prefix>s<20-digit-sequence><extension>` (for example `queue-s00000000000000000042.que`). The sequence determines FIFO 
order, independently of filesystem timestamps and wall-clock changes. The file contains the raw packet.

At startup, the folder is scanned to determine the next sequence number is. Gaps are harmless. 
The counter is not reset while the instance is running; reopening an empty namespace may start again at 1. Exhausting the
uint64 counter returns an error instead of wrapping.

By default, an enqueue writes directly to its final filename, using exclusive creation so an existing entry cannot be
overwritten. This avoids a separate existence check and rename on each enqueue. The queue holds its mutex until the
write finishes, so `Peek` cannot expose an entry while it is being written. If reading the input, writing, or closing
fails, enqueue attempts to remove the new file; a process crash or failed cleanup can leave a partial entry.

AutoPaho decodes each queued MQTT packet before sending it. A truncated packet produces a decode error and is quarantined
by appending `.CORRUPT`; quarantined files are excluded from subsequent queue reads, including after restart. If the
quarantine rename fails, deletion is attempted instead. If both operations fail, the entry may be processed repeatedly.
This detects ordinary truncation, not all corruption: a full-length packet with damaged payload bytes may still decode
successfully. The queue itself is byte-oriented; applications consuming it directly must handle invalid entries.

With durable mode enabled, an enqueue writes and syncs a temporary file before closing and renaming it into place.
Incomplete temporary files left by a process crash are ignored and left on disk. Both modes use the same final filenames
and remain compatible when reopening an existing queue.

## Durability

By default, writes are not synced, so a successful enqueue does not guarantee that the message
will survive a power failure or OS crash. 

To enable durable mode use:

```go
import "github.com/eclipse/paho.golang/autopaho/queue/file"

q, err := file.New("./queue", "message-", ".que", file.WithDurable())
if err != nil {
    return err
}
// Use q as autopaho.ClientConfig.Queue.
```

Durable mode syncs each temporary file before closing and renaming it, then syncs the queue directory before `Enqueue` 
returns successfully. It also syncs directory changes made by `Remove` and `Quarantine`, including the deletion fallback 
when a quarantine rename fails. Initialization syncs the queue directory, including on retries after failed
initialization. On Windows, durable mode requires a local NTFS volume and permission to open the queue directory for
writing; it relies on the NTFS directory-structure flush. Other platforms also sync the ancestors up to the filesystem
root. Errors are returned instead of silently disabling durability.

If syncing a file fails, cleanup attempts to remove the temporary file and no entry is published. If the rename succeeds
but directory sync fails, `Enqueue` returns an error matching `file.ErrDurabilityUncertain` (check with `errors.Is`). The
entry remains available to consumers, but its survival across a power failure is uncertain. Retrying that enqueue can 
create a duplicate. The same error signals a completed removal or quarantine whose directory sync failed. The underlying 
error is also wrapped.

This mode can significantly reduce throughput. It requires an OS/filesystem that supports directory sync and storage 
that honors sync requests; it cannot protect against filesystem corruption or hardware that falsely reports completed 
writes. See the [Linux fsync documentation](https://man7.org/linux/man-pages/man2/fsync.2.html) and Microsoft's
[NTFS flush behavior](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa/0de7dc40-9627-437e-a4df-c4696cdc3d02).
Existing entries written without durable mode are not retroactively synced. The on-disk format is unchanged, so either 
mode can reopen the same queue. Only one queue instance may own a directory/prefix/extension namespace at a time.

## Benchmarks

[benchmark_test.go](benchmark_test.go) compares `basicfile` (the retained timestamp implementation), `file` (default 
options), and `file_durable` (`WithDurable()`) through their public APIs. All three use the same workloads:

| Benchmark | One measured operation |
| --- | --- |
| `BenchmarkQueueEnqueue` | Enqueue one entry; drain outside the timer after each batch of 128. |
| `BenchmarkQueueRoundTrip` | Enqueue, peek, read the entire entry, and remove it, retaining a backlog of 0 or 1,024 entries. |
| `BenchmarkQueueReopen` | Reopen a directory containing 0 or 1,024 entries, including constructor probes and syncs. |

Write and round-trip cases use 256-byte and 4-KiB entries. Reopen cases use 1-KiB entries. Setup, population, and final 
cleanup are excluded from timing and allocation counts; file counts stay bounded as the iteration count grows. `MB/s` 
counts each entry once, even when it is both written and read. Benchmarks check errors and read lengths; they do not 
test FIFO correctness or simulate crashes.

Run from the repository root, setting `TMPDIR` to an existing directory on the filesystem you want to measure 
(`b.TempDir` uses it):

```sh
TMPDIR=/path/on/test/filesystem go test -p 1 -run '^$' -bench '^BenchmarkQueue' -benchmem -benchtime=1s -count=5 ./autopaho/queue/file
```

Use a disk-backed directory when comparing durable writes: a RAM-backed `/tmp` does not measure storage flush latency. 
Run without `-race` for performance results and avoid concurrent disk workloads. These are serial benchmarks with warm 
filesystem caches, and results depend on the filesystem and storage. Unsupported directory sync causes the durable cases 
to fail rather than silently using another mode. The [session store benchmarks](../../../paho/store/readme.md#benchmarks) 
use the same implementation labels. Both `basicfile` and default `file` stores already sync file contents; the queue's 
corresponding modes do not.

## Compatibility

Legacy filenames(see "Changes from Release 0.23" below) containing the random numeric suffix remain readable and are 
consumed before sequenced entries. Legacy order uses modification time, with filename as a tiebreaker. New writes always 
use sequence filenames.

# Changes from Release 0.23

In release 0.23 `autopaho/queue/file` used file timestamps to maintain order. Unfortunately, on Linux, this may
not be reliable when files are written rapidly (see [issue 342](https://github.com/eclipse/paho.golang/issues/342).   

The original timestamp-based implementation is available as [`autopaho/queue/basicfile`](../basicfile/README.md) for 
users who accept its ordering limitations and wish to use it.
