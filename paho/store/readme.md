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

store
=====

Contains implementations of `session.storer`. These are used to store session states; that is store packets indexed by a
uint16, with the ability to retrieve in the order written.

The MQTT client will open two stores; one for PUBLISH packets (and related acknowledgements) originating from the server
(i.e. the server assigned the packet identifier) and the other for messages originating on the client (these may well 
share the same basic backend).

as per section 4.1 in the spec; The Session State in the Client consists of:
> · QoS 1 and QoS 2 messages which have been sent to the Server, but have not been completely acknowledged.
> · QoS 2 messages which have been received from the Server, but have not been completely acknowledged.

This means that we only need to store PUBLISH (QOS1+) related outbound packets (if we have not responded, the packet
will be resent). So the only things needed are:
 `PUBLISH` - This is the only situation where the message body is needed (so we can resend it).
 `PUBREC` - response to inbound PUBLISH. The only packet originating on the server that will be stored.
 `PUBREL` - relating to a PUBLISH we initiated
We do need to store the entire packet because, when resending it, this should include any properties.

The vast majority of operations will be writes. The only time data should be read is:
   * At startup an inventory of packets held will be needed (need to know which identifiers are allocated)
   * When connection is established, we load the packets, so they can be retransmitted

Design goals:
  - Simplicity
  - Store agnostic (allow for memory, disk files, database, REDIS etc)
  - Minimise data transfers (only read when data is needed)
  - Allow for properties (we use `packets.ControlPacket` so that a whole packet is stored; when resending we
    need to include properties etc).

File store ordering and compatibility
-------------------------------------

`paho/store/file` is the recommended file-backed store. In release 0.23 (and earlier) `paho/store/file` used file 
timestamps to maintain order. Unfortunately, on Linux, this may not be reliable when files are written rapidly (see 
[issue 342](https://github.com/eclipse/paho.golang/issues/342). As such `paho/store/file` has been updated to use a
sequence in the filename. The original timestamp-based store is available as [`paho/store/basicfile`](basicfile/README.md) for users who 
explicitly accept its ordering limitations.

Only one Store instance may own a directory/prefix/extension namespace at a time. External modification of its files 
while it is running is unsupported. Client and server stores can use different, non-overlapping prefixes in the same 
directory. Use short prefixes and extensions; initialization checks that the filesystem can create the longest generated 
temporary filename.

See the readmes within the filestore implementations for further information.

Benchmarks
----------

[file/benchmark_test.go](file/benchmark_test.go) compares `basicfile` (filesystem timestamp), `file` (default options), 
and `file_durable` (`WithDurable()`) through their public APIs.

| Benchmark | One measured operation |
| --- | --- |
| `BenchmarkStorePut` | Insert one PUBLISH with a fresh packet ID; reset outside the timer after each batch of 128. |
| `BenchmarkStoreRoundTrip` | Put a PUBLISH, get and read its entire encoded packet, close the reader, and delete it. |
| `BenchmarkStoreQoS2` | Put a PUBLISH, replace it with PUBREL, and delete it. |
| `BenchmarkStoreList` | List a fixed inventory of 1 or 1,024 packets. |
| `BenchmarkStoreReopen` | Reopen a directory containing 0 or 1,024 packets, including constructor probes, recovery scans, and syncs. |

Write and lifecycle cases use 256-byte and 4-KiB PUBLISH payloads and include packet serialization. Round-trip and QoS 2 
cases start and finish each iteration with an empty store, reusing a packet ID only after deletion. List and reopen 
cases use 1-KiB payloads. Setup, population, and final cleanup are excluded from timing and allocation counts. Disk 
usage is bounded independently of the iteration count. `MB/s` counts application payload bytes once per operation, 
excluding packet headers and PUBREL bytes. The QoS 2 benchmark measures storage operations, not a network exchange or 
session-manager work.

Run from the repository root, setting `TMPDIR` to an existing directory on the filesystem you want to measure 
(`b.TempDir` uses it):

```sh
TMPDIR=/path/on/test/filesystem go test -p 1 -run '^$' -bench '^BenchmarkStore' -benchmem -benchtime=1s -count=5 ./paho/store/file
```

Use a disk-backed directory to measure sync latency; a RAM-backed `/tmp` can hide the cost. Run without `-race` for 
performance results and avoid concurrent disk workloads. These serial benchmarks use warm filesystem caches; timings 
depend on the filesystem and storage. Unsupported directory sync causes the durable cases to fail rather than silently 
using another mode. To run the [queue benchmarks](../../autopaho/queue/file/README.md#benchmarks) alongside these,
use `-bench '^Benchmark(Queue|Store)'` and pass both package paths, retaining `-p 1` to run the packages sequentially.
