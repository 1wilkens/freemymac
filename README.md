# freemymac

A small terminal UI for finding out what's filling up your Mac's disk, with enough context to decide what to delete.

## Requirements

- macOS 12 (Monterey) or later, on Apple Silicon or Intel
- [Go](https://go.dev/dl/) 1.26 or newer, only to build it (`brew install go`). Older Go versions from 1.21 on download the right toolchain automatically.

No C compiler, Xcode or other dependencies are needed. Everything is pure Go.

## Build

```sh
git clone <repo-url> freemymac
cd freemymac
go build -o bin/freemymac .
```

Or install it onto your `PATH` (into `$(go env GOPATH)/bin`, usually `~/go/bin`):

```sh
go install .
```

To build binaries for both Mac architectures, e.g. to hand to someone without Go:

```sh
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/freemymac-darwin-arm64 .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/freemymac-darwin-amd64 .
```

Cross-compiling works from any OS. Binaries you build yourself run as-is. A binary someone sends you, or that you download through a browser, is quarantined by Gatekeeper: clear the flag with `xattr -d com.apple.quarantine freemymac-darwin-arm64`.

## Run

```sh
./bin/freemymac            # scan your home folder
./bin/freemymac ~/Projects # scan a specific folder
./bin/freemymac --help
go run . [folder]          # build and run in one step, handy during development
```

Scanning a home folder with ~1.6M files takes about 6 seconds on an M-series Mac. Press `?` in the app for all keys and the filter syntax, `q` to quit.

Before the first run, consider granting your terminal app **Full Disk Access** (System Settings › Privacy & Security › Full Disk Access). Otherwise folders like Mail, Messages and other apps' containers can't be read, and their size is missing from the totals.

It also builds and runs on Linux, which is handy for development. There, creation dates show as `-` and Finder features (Trash via Finder, reveal) aren't available; trashing moves items into `~/.Trash`.

## What you see

For every file and folder: **size on disk** (allocated blocks, so iCloud placeholders and sparse files count as what they actually use), file count, **created**, **modified** and **last accessed** dates. Modified dates over 1 year old are yellow, over 2 years orange. For folders, "modified" and "accessed" are the most recent of anything *inside*, so a folder shows as old only if nothing in it was touched. The detail pane shows the full path, logical vs. on-disk size, exact timestamps with relative age, and a 💡 hint for well-known locations (caches, DerivedData, node_modules, …).

## Keys

| key | action |
| --- | --- |
| `↑↓` `⏎`/`→` `←`/`⌫` | navigate the folder tree |
| `f` | flat view: every file beneath the current folder, biggest first |
| `/` | filter (see below); `esc`/`0` clears |
| `1`–`6` | presets: large files · old & big · installers/archives · dev junk folders · not opened in 6 months · videos/disk images |
| `c` | common space hogs (Caches, Xcode, simulators, Docker, iOS backups, package-manager caches…) |
| `s` / `S` | cycle sort (size, modified, created, accessed, name, files) / reverse |
| `.` | show/hide dotfiles |
| `space` `a` `u` | mark / mark all shown / unmark all |
| `d` | move marked (or selected) to Trash via Finder, so "Put Back" works |
| `X` | delete permanently |
| `o` | reveal in Finder |
| `r` | rescan current folder |

## Filters

Space-separated conditions that must all match; prefix `!` to negate.

```
size>500M            mod>1y   (not modified in a year)   mod<7d
created>2y           accessed>6m                         ext:dmg,zip,iso
name:*.log,cache     path:Library    type:dir|file|any   !path:Photos
```

Age units: `h d w m y`. Size units: `K M G T` (base 10, like Finder).

## Scan speed

On macOS the scanner uses `getattrlistbulk(2)`, which returns names, sizes and timestamps for hundreds of directory entries per system call instead of one `lstat` per file. It falls back to `fstatat` on filesystems that don't support it (SMB, some FUSE mounts). To check that both paths agree on your disk and time them across worker counts (wall vs. CPU time shows whether the scan is CPU-, kernel- or lock-bound):

```
./bin/freemymac --bench ~   # takes 2-3 minutes
```

## Notes

- Doesn't follow symlinks or cross onto other volumes. Hard links are counted once.
- Some folders (Mail, Messages, Safari, other apps' containers) are unreadable unless your terminal has **Full Disk Access** (System Settings › Privacy & Security). macOS may ask for permission the first time it scans Documents, Desktop or Downloads.
- Trashing asks Finder via AppleScript (macOS asks once for permission to control Finder). If that's denied, items are moved into `~/.Trash` directly. Space is only reclaimed once you empty the Trash.
- "Free" disk space excludes purgeable space, so it may read lower than Finder.
