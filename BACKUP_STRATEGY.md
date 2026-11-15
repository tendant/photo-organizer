# Backup and De-duplication Strategy

How to consolidate scattered photos and videos into one de-duplicated library and back
it up safely. Sections say whether something is a **decision** or a **proposal** (not
yet done). Paths and names below are placeholders; keep machine-specific notes outside
this repository.

## Goal

Photos and videos end up scattered across many folders and disks, copied at different
times. The aim is:

1. one clean, de-duplicated **library** per media kind,
2. a real, independent **backup** of each library,
3. the old scattered copies retired only after both exist and are verified.

## The three roles (decision)

| Role | What it is | Who writes to it |
|---|---|---|
| **Source** | Existing scattered folders and old backups | nobody new; the tool only reads them |
| **Library** | One de-duplicated copy, laid out `YYYY/YYYY-MM-DD/` | only `organize` |
| **Backup** | A copy of the library on a different disk or machine | only `backup` |

```
sources (messy, many) --organize--> library (one clean copy) --backup--> backup (other disk)
```

Old backups taken at different times are **sources**, not backups in this scheme: they
overlap, and merging them into the library collapses the duplicates.

## Principles (decision)

- **Copy first, delete last.** Sources are never changed by the tool. Delete a source only
  after the library copy is verified and the library is backed up. Deletion is always manual.
- **A backup is an independent copy.** Do not use hardlinks between library and sources: a
  hardlink is a second name for the same data, so one disk failure or one in-place edit hits
  both, and backup tools handle them badly. Do not rely on filesystem block cloning either.
- **Verify by content, not by name.** Use `organize --verify full` for the real run. The scan
  hash only samples the first and last 32 KB, so duplicate groups stay "unconfirmed" until
  whole files are compared.
- **Keep photos and videos apart.** They differ in size, value per GB and how they should be
  verified and backed up. Videos are laid out by date, like photos.
  - `organize --type photo --dest <photo-library> --verify full`
  - `organize --type video --dest <video-library> --verify auto`
  - A same-name group is classified whole: any photo makes it a photo group, so a RAW+JPG pair
    or a Live Photo's `.mov` stays with its photo. The two runs never overlap, and together they
    equal one run without `--type`. Audio is only handled without `--type`.
- **Manifests must tell the truth.** `scan` prunes rows for deleted files by default
  (`--no-prune` to opt out), so `dups` and `check-backup` never count a deleted file as a copy.
  Pruning is skipped if the scan could not read some folders, or would remove over half the rows.
- **Removable disks get a stable ID.** Scan them with `--media-id <name>`. It writes a
  `machine-id` file to the disk root once (`--no-write-media-id` keeps a disk untouched).

## Workflow

1. **Check free space** on every disk involved before planning. A library needs room, and a
   nearly full disk (or ZFS pool) slows down and leaves no space for the backup.
2. **Scan** each top-level root, read-only: `photo-organizer scan <root>`. One manifest per
   root. Scanning a parent and a child is safe (reports dedupe by absolute path) but wasteful;
   prefer the top folder. Re-scan existing roots rather than new bigger ones, so cached hashes
   are reused. Refresh old scans before trusting them.
3. **Look**: `dups`, `dup-folders --top 20`.
4. **Preview** a library build; it copies nothing and can save the exact plan:
   `organize <sources> --type photo --dest <library> --verify full --save-plan <plan.json>`
   Read the summary: copy size, duplicates, dates, warnings. Keep the plan file somewhere permanent.
5. **Apply** exactly what was previewed: `organize --plan <plan.json> --apply`.
6. **Verify**: preview again; it should report nothing left to copy. Then `check-backup`.
7. **Back up** the library: `backup <library> --dest <target>`. `backup` mirrors into a folder of
   the same name inside the target, so name the parent folder, not the final one.
8. **Retire** a source (manual delete or `archive`) only after steps 6 and 7 pass.

When space is tight, work in **slices** (one source subfolder at a time: copy, verify, back up,
then free that slice), so the peak extra space is one slice, not the whole library.

`organize` keeps the first copy of each piece of content it meets and visits sources in path
order. If two trees share content, run the one you want to keep as the origin first.

## Choosing where the backup lives

- It must be a different disk, ideally a different machine, with enough free space for the
  whole library.
- A second dataset on the same disks only protects against deletion, not disk or pool failure.
- A single external disk is a good second copy but not a safe only copy.
- NTFS and similar filesystems reject some filenames and are case-insensitive; watch `backup`
  for errors on odd names.
- A disk that already holds old data should be scanned and compared with the rest first, so you
  know whether it is itself backed up before anything is written to it.

## Multiple machines (proposal)

One hub holds the library and its own backup; every other machine only pushes to it:
`scan`, `backup <root> --dest <hub>:<incoming>`, `collect`, `check-backup`. Needs SSH keys,
the hub registered with `collect --add`, and `photo-organizer` installed on the remote.

## Things to check before applying a plan

- Files dated by an unset camera clock (many files on January 1st) land in one folder.
- Files dated only by modified time are not trustworthy after a copy or restore.
- Duplicate counts based on the partial hash are estimates until `--verify full`.
- Manifest staleness warnings: a manifest that is months old can still list deleted files.

## Tool changes made for this strategy

- `organize --type photo|video|all`.
- `scan` prunes by default; `--no-prune` opts out; scans that hit unreadable paths skip the prune.
- `make install` installs to `/usr/local/bin`; `make install-user` is the no-sudo `~/bin` option
  (a copy in `~/bin` shadows the system one on `PATH`).
