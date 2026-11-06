# Raspak

[Русская версия](README.ru.md)

Archive tool for Windows: a small window, Explorer context menu, a single Go exe with no installation.

## Features

- **Formats.** zip, 7z, rar (including multi-volume `.part1.rar`), tar, tar.gz/.bz2/.xz/.zst/.lz4, single `.gz`, `.xz`, `.bz2`, `.zst`, `.lz4`, `.br`, `.lz`, split volumes `.001`, `.002`... (7-Zip and Total Commander).
- **Passwords.** zip (ZipCrypto and AES), 7z (including encrypted file lists), rar.
- **Cyrillic names** in zip files from old archivers (cp866).
- **Window.** Drop an archive, browse folders, open a file by double click, extract everything or a selection, test the archive. Drag files out of the window to Explorer, the desktop or another app.
- **Smart extraction.** If the archive holds one folder, it goes next to the archive as is; otherwise everything goes into a folder named after the archive. Nothing is overwritten (`x (2)`).
- **Explorer menu.** "Extract here", "Extract to...", "Open in Raspak", "Test archive". On Windows 11 in the new menu (a "Raspak" submenu), on Windows 10 and under "Show more options" in the classic one.
- **Batches.** Select 20 archives and press "Extract here" - one progress window for all of them.
- **Multi-volume archives.** Whichever volume you open, extraction starts from the first one, and volumes of one set selected together are extracted once. Raspak checks that all volumes are there and warns about unfinished downloads (`.part`, `.crdownload`, `.!qB` and so on).
- **Protection from harmful archives.** Paths like `..\..\` and `C:\...` cannot leave the target folder, links are not created, names like `CON`, `aux.txt` and characters `<>:"|?*` are fixed.
- **Packing.** Right click on any file or folder: "Add to "Name.zip"" right away, or "Add to archive..." with a small dialog (name, ZIP / 7Z / TAR.GZ / TAR.ZST, level, AES-256 password for zip). The dialog shows the expected archive size. The "Ultra" level for 7z and tar.zst uses a large window and finds repeats across files.
- **Adding to an archive.** Open the archive, go to a folder and drop files there. ZIP is appended without recompressing old data (new files in a password zip get the same password); tar and compressed tar are rebuilt with the same method. 7z, rar and multi-volume archives are not changed. The archive is replaced only at the end, so a cancel or an error does not damage it.
- **Updates** from GitHub releases.

## Speed

- zip is extracted in several threads: files in a zip are independent.
- 7z: its own parallel LZMA2 decoder. Multi-threaded 7-Zip splits the stream into blocks with a dictionary reset; Raspak decodes them at the same time and writes data as soon as it is ready.
- rar and tar.*: decoding in one thread, file writing runs in parallel; gzip is multi-threaded.

Benchmark: 1500 files, 286 MB, same disk, three runs each:

| archive | Raspak | 7-Zip 24 (`7z x`) |
|---|---|---|
| zip (deflate) | 1.07-1.32 s | 1.16-1.18 s |
| 7z (LZMA2, multi-threaded) | 1.5-1.9 s | 1.19-1.27 s |
| tar.gz | 1.16-1.32 s | - |

Packing the same 286 MB: zip - 0.11 s (7-Zip `a -tzip -mx5` - 0.9 s), tar.gz - 0.25 s, tar.zst - 0.36 s. zip is compressed on all cores; already compressed files (jpg, mp4, zip...) are stored as is.

## Windows 11 menu

The new Windows 11 context menu shows only extensions from signed MSIX packages. Raspak builds such a package itself when the option is turned on:

1. creates a self-signed certificate `CN=Raspak Local` on the PC;
2. builds a sparse package without the program files (manifest, COM server, images) and signs it;
3. deletes the private key, so nothing else can be signed with it;
4. trusts the certificate and registers the package for the current user. If Windows does not trust the user store, it asks for admin rights once.

Explorer reads the menu at start, so Raspak offers to restart Explorer after installing. Turning the option off removes the package and the certificate.

## Tech stack

- **Go**, built with `CGO_ENABLED=0`.
- [lxn/walk](https://github.com/lxn/walk) - the main window with native Windows controls; the packing dialog uses WebView2 ([go-webview2](https://github.com/jchv/go-webview2)).
- [mholt/archives](https://github.com/mholt/archives), [bodgit/sevenzip](https://github.com/bodgit/sevenzip), [nwaples/rardecode](https://github.com/nwaples/rardecode), [yeka/zip](https://github.com/yeka/zip) - reading archives.
- [klauspost/compress](https://github.com/klauspost/compress), [pgzip](https://github.com/klauspost/pgzip), [ulikunitz/xz](https://github.com/ulikunitz/xz) - compression.
- Own parallel LZMA2 decoder and 7z writer.
- Windows COM and MSIX (sparse package) for the Windows 11 menu.

## Requirements

Windows 10 or 11, x64.

## Getting started

Download `Raspak.exe` from [Releases](https://github.com/Trashfallen/Raspak/releases/latest) and run it. On the first run Raspak asks whether to add itself to Explorer. Settings (gear button): Explorer menu, opening archives by double click, opening the folder after extraction, update checks. Data is kept in `%APPDATA%\Raspak`.

Command line:

```bat
Raspak.exe archive.zip                 :: open in the window
Raspak.exe --extract a.zip b.rar       :: extract next to the archive
Raspak.exe --extract-to a.zip          :: ask for a folder
Raspak.exe --test a.7z                 :: test
```

## Building

Requires Go (version in `go.mod`).

```
build.cmd        # dist\Raspak.exe
go test ./...
```
