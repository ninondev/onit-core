# Third-party notices

This repository's own code is licensed under the PolyForm Strict License 1.0.0
(see LICENSE). The Go dependencies below keep their own licenses; their source
is not vendored here and is fetched by the Go toolchain from upstream modules.

Direct dependencies:

| Module | Version | License |
| --- | --- | --- |
| github.com/mattn/go-sqlite3 | v1.14.32 | MIT (full text: go-sqlite3.txt) |
| modernc.org/sqlite | v1.44.3 | BSD-3-Clause |
| golang.org/x/crypto | v0.58.0 | BSD-3-Clause |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause |

Indirect dependencies (pulled in by modernc.org/sqlite):

| Module | Version | License |
| --- | --- | --- |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/mattn/go-isatty | v0.0.20 | MIT |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748 | BSD-3-Clause |
| golang.org/x/exp | v0.0.0-20251023183803 | BSD-3-Clause |
| modernc.org/libc | v1.67.6 | BSD-3-Clause |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.11.0 | BSD-3-Clause |

License types were read from each module's LICENSE file in the local module
cache at the versions above.
