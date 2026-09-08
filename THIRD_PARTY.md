# Third-party notices

PikPak Vault is an independent client, not an official PikPak product. PikPak is a trademark of its respective owner. The application has its own identity and assets.

The PikPak protocol constants and captcha-sign salt values in `internal/pikpak/client.go` were obtained from the MIT-licensed rclone PikPak backend. The adapter is independently implemented against observed request/response shapes. References:

- https://github.com/rclone/rclone/tree/master/backend/pikpak
- https://github.com/Quan666/PikPakAPI (request/response reference only)
- TelDrive API schema and authentication/stream behavior: https://github.com/tgdrive/teldrive and https://github.com/tgdrive/teldrive-docs (protocol reference only; no TelDrive code bundled).

GCID calculation and resumable upload protocol in `internal/pikpak/upload.go` also follow the MIT-licensed rclone PikPak backend. S3 signing and transport use AWS SDK for Go v2 (Apache-2.0).

## rclone MIT license notice

Copyright (C) 2012 by Nick Craig-Wood http://www.craig-wood.com/nick/

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

## Dependencies

The exact versions are pinned in `go.sum` and `web/package-lock.json`. Go dependencies include modernc SQLite (BSD-3-Clause), golang.org/x/crypto (BSD-3-Clause), and gofrs/flock (BSD-3-Clause). Frontend dependencies include React, Vite, Tailwind CSS, Radix UI, Motion, TanStack Query/Virtual, hls.js, Sonner, and Lucide under their respective open-source licenses. Vidstack is MIT-licensed. Outfit fonts are distributed under SIL Open Font License 1.1 by the upstream `@fontsource/outfit` package. No remote font service is used at runtime.
