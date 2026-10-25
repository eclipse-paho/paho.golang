<!--
Copyright (c) 2026 Contributors to the Eclipse Foundation

This program and the accompanying materials are made available under the 
Eclipse Public License v2.0 (https://www.eclipse.org/legal/epl-2.0/) and 
Eclipse Distribution License v1.0 (http://www.eclipse.org/org/documents/edl-v10.php).

SPDX-License-Identifier: (EPL-2.0 OR BSD-3-Clause) AND CC0-1.0

AI Disclosure: This file was largely generated with OpenAI GPT-5 via Codex.
AI-generated portions are made available under CC0-1.0.
Assisted-by: OpenAI GPT-5
-->

Basic file session store
========================

`basicfile` was the default (only) file store included with version 0.23 of the library (it was called file in that 
release). It generally works OK but relies upon file timestamps to maintain ordering. Unfortunately, on Linux, this may 
not be reliable when files are written rapidly (see [issue 342](https://github.com/eclipse/paho.golang/issues/342).

The new file implementation uses a sequence number to maintain ordering. This is more reliable than relying upon file 
timestamps but may be slower. The old package has been retained (as `basicfile`) to allow those who experience issues 
with the new implementation to revert. 

The constructor and methods are unchanged, so existing code can retain its alias:

```go
import file "github.com/eclipse/paho.golang/paho/store/basicfile"
```

Files use `<prefix><packet-ID><extension>`, with the packet ID in unpadded decimal. `Put` writes to a temporary file, 
syncs and closes it, then renames it over the packet ID's file. `List` scans the directory and sorts by modification 
time. The shorter filenames are not a guarantee of better performance.

Compatibility and switching
---------------------------

Because the new code is in `store/file` applications will switch to it by default (if `file` was used, the default store
is `memory`). The new store supports conversion from the legacy format. However, once you have converted, you cannot
revert to `basicfile` without deleting the files.

`New` checks if any files written by the new `store/file` are present and returns an error. 

Before switching back from `store/file`, allow in-flight exchanges to complete using `file` so the live store is 
drained. Alternatively, you could rename the files.