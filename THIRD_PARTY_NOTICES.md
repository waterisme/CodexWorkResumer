# Third-Party Notices

CodexWorkResumer is an MIT-licensed fork of
[sybxxx/codex-auto-retry](https://github.com/sybxxx/codex-auto-retry). The
original copyright and permission notice are retained in `LICENSE`.
The entries below identify dependencies used by the Go binaries or the
embedded management panel. Versions are locked in `scripts/source/go.mod`
and `scripts/source/ui/package-lock.json`. The Go standard library/runtime
license and patent grant, plus full license texts for the 8 Go modules and
96 npm non-dev packages, are in `THIRD_PARTY_LICENSES.txt`.
Build-only dependencies are not installed on end-user machines. Re-check the
lockfiles, bundle, and any additional notice obligations before a public
binary release.

| Dependency | Version | License | Use |
| --- | --- | --- | --- |
| `github.com/gorilla/websocket` | 1.5.3 | BSD-2-Clause | Go runtime |
| `github.com/google/jsonschema-go` | 0.4.3 | MIT | Go runtime (indirect) |
| `github.com/modelcontextprotocol/go-sdk` | 1.6.1 | Apache-2.0 / MIT (per-file transition) | Go runtime |
| `github.com/segmentio/asm` | 1.1.3 | MIT | Go runtime (indirect) |
| `github.com/segmentio/encoding` | 0.5.4 | MIT | Go runtime (indirect) |
| `github.com/yosida95/uritemplate/v3` | 3.0.2 | BSD-3-Clause | Go runtime (indirect) |
| `golang.org/x/oauth2` | 0.35.0 | BSD-3-Clause | Go runtime (indirect) |
| `golang.org/x/sys` | 0.41.0 | BSD-3-Clause | Go runtime |
| `@modelcontextprotocol/ext-apps` | 1.7.5 | Apache-2.0 / MIT (per-file transition) | Embedded panel |
| `@modelcontextprotocol/sdk` | 1.32.1 | MIT | Embedded panel |
| `lucide` | 1.27.0 | ISC / MIT (icon-dependent) | Embedded panel |
| `zod` | 4.1.13 | MIT | Embedded panel |
| `vite`, `vite-plugin-singlefile`, `typescript` | locked versions | MIT | Build only |

The npm lockfile also includes transitive packages, including MPL-2.0
Lightning CSS build tooling. Those packages are marked dev-only and are not
included in the generated runtime license bundle or installed on end-user
machines. The locked dependency tree and bundle must be re-checked for each
release.

## Gorilla WebSocket

Copyright (c) 2013 The Gorilla WebSocket Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice,
   this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

## Model Context Protocol Go SDK 1.6.1 And MCP Apps SDK 1.7.5

Copyright (c) 2024-2026 Model Context Protocol, a Series of LF Projects, LLC,
and contributors.

The MCP project is transitioning from the MIT License to the Apache License,
Version 2.0. New contributions and contributions for which relicensing consent
has been obtained are licensed under Apache-2.0. Earlier contributions whose
authors have not granted relicensing consent remain under the MIT License.

Source and complete license text:

- https://github.com/modelcontextprotocol/go-sdk
- https://github.com/modelcontextprotocol/ext-apps

## Model Context Protocol TypeScript SDK

Copyright (c) 2024 Anthropic, PBC.

Licensed under the MIT License. Source and license text:
https://github.com/modelcontextprotocol/typescript-sdk

## Zod

Copyright (c) 2025 Colin McDonnell.

Licensed under the MIT License. Source and license text:
https://github.com/colinhacks/zod

## Lucide

Copyright (c) 2026 Lucide Icons and Contributors.

Licensed under the ISC License. Some icons are derived from Feather and remain
licensed under the MIT License, Copyright (c) 2013-present Cole Bemis. Source
and complete license text: https://github.com/lucide-icons/lucide

## Go System Calls

`golang.org/x/sys/windows` is used for the native Windows notification-area
controller and is licensed under the BSD 3-Clause License. Copyright (c)
2009 The Go Authors. Source and license text:
https://pkg.go.dev/golang.org/x/sys

## Panel Build Tooling

Vite and vite-plugin-singlefile are used only to build the embedded offline
panel and are licensed under the MIT License. They are not required on the
installed machine at runtime.
