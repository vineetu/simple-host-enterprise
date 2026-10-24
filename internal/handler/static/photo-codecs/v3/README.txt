Photo decoders for Add photos

Pinned upstream files from npm, except for the local UTIF patches below.
HEIC/HEIF and TIFF always decode in a dedicated Web Worker, only for files the
person selected or dropped themselves. No URL input. All runtime requests stay
on this server. decoder.js is the application's handwritten worker adapter.
The page terminates each worker after 20 seconds (also on success/error), shows
"Could not read this photo", continues the batch, and re-enables controls.
The picker probes its actual blob worker and Wasm permissions before accepting
HEIC/TIFF. Unsupported frames direct people to the photos link. MCP Apps CSP
metadata has no worker-src or Wasm permission field (2026-01-26 spec):
https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx
Inputs are capped at 80,000,000 bytes before reading them into memory; header
sizes are checked against 50,000,000 pixels before HEIC display / TIFF decode.
UTIF bounds each decoded image and tile buffer to 200,000,000 bytes. Deflate
output is copied incrementally into the strip/tile destination and aborts at
its byte limit, with at most one extra 16 KiB output chunk and pako state.
These are per-buffer limits, not a limit on total browser or Wasm memory.

Accepted risk (SIM-185, checked 2026-10-23): libheif-js 1.23.5 bundles libheif
1.23.5 and libde265 1.0.15, affected by CVE-2026-33164 and CVE-2026-33165.
Both require libde265 >=1.0.17. npm's latest libheif-js is still 1.23.5;
heic-decode 2.1.0 depends on libheif-js ^1.19.8; heic-to 1.6.6's build recipe
pins libde265 1.0.16. No verified patched prebuilt replacement was found.
Per the owner, retain the worker/timeout/selected-file mitigation, not a custom
Emscripten build. This bounds exposure; it does not patch the decoder bugs.
Sources:
https://github.com/strukturag/libde265/security/advisories/GHSA-wqrf-6rf5-v78r
https://github.com/strukturag/libde265/security/advisories/GHSA-653q-9f73-8hvg
https://registry.npmjs.org/libheif-js/latest
https://registry.npmjs.org/heic-decode/latest
https://registry.npmjs.org/heic-to/latest
https://github.com/strukturag/libheif/blob/v1.23.5/build-emscripten.sh
https://github.com/hoppergee/heic-to/blob/main/README.md

libheif-js 1.23.5: libheif.js (91,255 bytes), libheif.wasm (1,463,506 bytes).
LGPL-3.0. LICENSE-libheif-js and LICENSE-libheif contain the license texts,
including GNU GPL v3 and the upstream wrapper's MIT notice.
Package: https://registry.npmjs.org/libheif-js/-/libheif-js-1.23.5.tgz
Source and package build scripts:
https://github.com/catdad-experiments/libheif-js/tree/1.23.5
Corresponding Wasm source and build scripts:
https://github.com/catdad-experiments/libheif-emscripten/tree/v1.23.5
The source build pins libheif and libde265; follow its README to rebuild the
Wasm files. Replace libheif.js and libheif.wasm together to use a modified,
interface-compatible decoder. The application loads them as separate files.

UTIF 3.1.0 with local safety patches: UTIF.js. MIT, see LICENSE-UTIF.
Changes: visited-offset set per buffer and 128-IFD maximum across top-level,
SubIFD, Exif and maker-note traversal; IFD/value bounds before allocation;
pixel/tile allocation caps; streaming, bounded Deflate output. Original upstream file was 58,271 bytes.
License files are unchanged because the dependency versions did not change.
Package and source:
https://registry.npmjs.org/utif/-/utif-3.1.0.tgz
https://github.com/photopea/UTIF.js

pako 1.0.11: pako.min.js (45,988 bytes). MIT and zlib, see LICENSE-pako and LICENSE-pako-zlib.
Used by UTIF for compressed TIFFs.
Package and source:
https://registry.npmjs.org/pako/-/pako-1.0.11.tgz
https://github.com/nodeca/pako/tree/1.0.11

Bump the /v3/ asset directory if any decoder or adapter changes after release;
these URLs are served with a one-year immutable cache header.
