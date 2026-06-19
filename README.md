# File Dedup

`file-dedup` is a Go command-line tool for tidying backup directories. It can 
extract ZIP archives, normalize filenames, flatten directory trees, group files by
extension, remove duplicate content, write manifests, and undo completed
operations.

## Story

I created this tool to clean up my own backup archive, from 15 years of period. I
had ended up just taking backups of old computers by copying some file path and 
storing that to external drive. This ended up creating a huge blob of data, with
data duplication, and frankly - it was completely unusable for me. I started 
to create this simple tool to rename files, flatten directories, sorting stuff by
filetype and remove duplicate data. Shortly I learned that Go's standard library
didn't have support for deflate64 compression out of the box, which is or was 
used in Windows to compress larger archives. 

That opened a compression rabbit hole. Some large archives in my backups,
from around 2015, used deflate64. I could have solved that with 7-Zip or one of
many available C-based tools, but I wanted a pure Go solution and wanted to
learn more about compression. I ended up forking Go's `compress/flate` work into
`github.com/haapjari/flate` to support zip method 9 without shelling out.

## Quick Start

Build the CLI:

```bash
make build
```

Preview before applying changes:

```bash
./file-dedup unzip --dry-run /path/to/archive
./file-dedup rename --dry-run /path/to/archive
./file-dedup flatten --dry-run /path/to/archive
./file-dedup organize --dry-run /path/to/archive
./file-dedup duplicate --dry-run /path/to/archive
```

Apply one step at a time:

```bash
./file-dedup unzip /path/to/archive
./file-dedup rename /path/to/archive
./file-dedup flatten /path/to/archive
./file-dedup organize /path/to/archive
./file-dedup duplicate /path/to/archive
```

## Commands

| Command | Purpose |
| --- | --- |
| `unzip` | Extract ZIP archives recursively and move extracted archives to trash. |
| `rename` | Rename files with date-prefixed, sanitized names. |
| `flatten` | Move files to the target root and trash duplicate content. |
| `organize` | Group files into subdirectories by extension. |
| `duplicate` | Trash duplicate files by content hash. |
| `manifest` | Write a SHA256 inventory of files. |
| `undo` | Reverse the latest journaled operation, or a selected run. |
| `purge` | Permanently delete trash. This is the only irreversible command. |

## Safety Model

The tool is designed to avoid accidental data loss, but it is not a substitute
for a real backup.

- Dry-run is available for all mutating commands.
- Normal delete paths move files to `.file-dedup/trash/<run-id>/`.
- `purge` is the only command that permanently deletes trash.
- Mutations are constrained to the target directory with path validation.
- Symlink escapes and ZIP path traversal are rejected.
- Duplicate removal uses SHA256 content hashes and re-checks files before
  trashing them.
- Non-dry-run mutating commands create manifest snapshots by default.
- Completed operations write journals that `undo` can use.
- A `.file-dedup/lock` file prevents concurrent `file-dedup` runs in the same
  target.

Important limits:

- Journals are written after operations complete. They support undo and audit,
  not full crash recovery for a power loss in the middle of a run.
- The lock only coordinates this tool. It does not stop other programs from
  changing files during a run.
- Do not run `purge` until you have verified the result.

## Metadata

`file-dedup` stores recovery data inside the target directory:

```text
.file-dedup/
  lock
  trash/<run-id>/...
  manifests/<run-id>.json
  journal/<run-id>.jsonl
  journal/<run-id>.rolled-back.jsonl
```

Keep this directory until you are sure you no longer need undo or audit data.

## ZIP Support

Supported ZIP compression methods:

- store (`0`)
- deflate (`8`)
- Deflate64 (`9`)

Unsupported ZIP methods are skipped without deleting the source archive.

## Development

Run tests:

```bash
make test
go test ./cmd -count=1
go test ./e2e -count=1
```

Run the main local gate:

```bash
make pre-commit
```

## AI Assistance Notice

This codebase has been written with AI assistance. Human review, testing, and
local validation are still required before using it on important data.

## License

MIT
