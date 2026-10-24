Canvas regression samples for the Add photos JPEG/WebP header validator.
Generated 2026-10-23 by scripts/e2e-photo-safety.cjs using Playwright:
Chromium 156.0.8078.4, Firefox 157.0, WebKit 27.2 (Safari's engine).
All three supply both JPEG and WebP. These are unmodified canvas
outputs of the same 32x24 two-colour drawing. No third-party image content.
WebKit is an engine check, not a run of the Safari application on macOS/iOS.

Deflate overflow regression (2026-10-24): deflate-overflow.tiff declares a
1x1, 8-bit grayscale strip, but its zlib payload expands to 1 MiB of zeroes.
Generated locally from a nine-entry little-endian IFD and Python zlib.compress.
No third-party image content. The decoder must abort at its first output chunk.
