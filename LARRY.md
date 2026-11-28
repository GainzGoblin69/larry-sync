# larry-sync

The headless half of Larry Boy Advanced: grout's RomM catalog, downloads, BIOS
handling and save sync as a command with no screen. A launcher runs it as a
child process and reads its output.

It is built from this fork of [grout](https://github.com/rommapp/grout) and
shares grout's `config.json` and catalog database. A device paired in grout is
paired here, and the reverse.

## Installing on a TrimUI Brick running NextUI

1. Install the Grout pak as usual (`/mnt/SDCARD/Tools/tg5040/Grout.pak`).
2. Copy `larry-sync` and `larry-sync.sh` into that folder.
3. Run commands through the wrapper, which sets the firmware and library path:

   ```sh
   cd /mnt/SDCARD/Tools/tg5040/Grout.pak
   ./larry-sync.sh status
   ```

## Commands

| Command | What it does | Needs network |
|---|---|---|
| `pair --url URL [--name NAME] [--insecure]` | Links the device to a RomM server. Prints the address to approve it at, then waits. | Yes |
| `status` | Reports whether the device is paired and the server answers. | No |
| `sync [--no-art]` | Refreshes the whole catalog and cover art, then writes the catalog files. | Yes |
| `export` | Rewrites the catalog files from the local catalog. | No |
| `fetch --rom ID [--file ID] [--no-bios] [--no-saves]` | Downloads a game, the BIOS files its platform needs, and its newest save. For a game already on the device it only checks BIOS and saves. | For new games |
| `remove --rom ID` | Deletes a downloaded game. Saves are kept. | No |
| `bios --platform FS_SLUG` | Downloads any BIOS files missing for a platform. | Yes |
| `saves [--rom ID] [--direction both\|pull\|push]` | Syncs saves. Conflicts are reported and left alone on both sides. | Yes |

## Output

Standard output carries one event per line, fields separated by tabs. Logs go
to standard error and `logs/larry-sync.log`.

| Line | Meaning |
|---|---|
| `STAGE <name> <detail>` | A step started: `connect`, `approve`, `platforms`, `games`, `art`, `rom`, `extract`, `bios`, `saves`. |
| `PROGRESS <percent> <done> <total>` | Progress of the current step. `done` and `total` are bytes for a download and item counts otherwise. |
| `INFO <message>` | Something worth showing that needs no action. |
| `RESULT <key> <value>` | An outcome, such as `rom_path` after `fetch` or `pair_url` during `pair`. |
| `ERROR <message>` | Why the command failed. |

Exit codes: `0` ok, `1` failed, `2` bad usage, `3` not paired, `4` server
unreachable.

## Catalog files

`sync`, `export`, `fetch` and `remove` write these under `larry/` beside
`config.json` (or `$LARRY_DIR`). Fields are tab-separated and never contain a
tab.

`platforms.tsv`, one platform per line:

```
fs_slug  name  rom_directory  games  downloaded
```

`games/<fs_slug>.tsv`, one game per line, sorted by name:

```
rom_id  name  downloaded  local_path  art_path  size_bytes  file_name
```

`downloaded` is `1` or `0`. `local_path` is the file to launch and `art_path`
is the cover; each is empty when there is no file. Every game on the server is
listed for every platform the firmware can run, whether or not it has been
downloaded.
