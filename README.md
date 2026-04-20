# Plex Sonic Similar Songs — Navidrome Plugin

A [Navidrome](https://navidrome.org) plugin that uses Plex's Sonic Analysis to power **Instant Mix** and similar-songs features. When you hit "Instant Mix" on a track, this plugin searches your Plex server for the same song, retrieves its sonically similar tracks, and maps them back to your Navidrome library.

## How It Works

1. **Cache check** — Looks up the track in the local KVStore (persists across restarts, 7-day TTL).
2. **Forward search** — Searches Plex's `/hubs/search` endpoint to find the matching track by artist + title.
3. **Sonic analysis** — Calls Plex's `/library/metadata/{id}/nearest` to get sonically similar tracks.
4. **Reverse matching** — Converts the Plex results into `SongRef` objects (name, artist, album, duration) so Navidrome can reconcile them against its own library using fuzzy string matching.
5. **Cache & return** — Stores the result in KVStore and returns the similar songs.

## Requirements

- [Navidrome](https://navidrome.org) with plugin support enabled
- A [Plex Media Server](https://plex.tv) with a music library (Sonic Analysis enabled)
- A Plex authentication token ([how to find your token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/))

## Building

### Prerequisites

- [TinyGo](https://tinygo.org/getting-started/install/) (0.40+)
- Go 1.24 or 1.25 (TinyGo 0.40 does not yet support Go 1.26)
- [Task](https://taskfile.dev/installation/) (optional, for task runner)

### Build & Package

```sh
# Using Task
task package

# Or manually
tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
zip -j plex-similar-songs.ndp manifest.json plugin.wasm
```

## Installation

1. Copy `plex-similar-songs.ndp` to your Navidrome plugins folder.
2. Enable plugins in `navidrome.toml`:
   ```toml
   [Plugins]
   Enabled = true
   Folder = "/path/to/plugins"
   ```
3. Add the plugin to your agents list:
   ```toml
   Agents = "lastfm,plex-similar-songs"
   ```
4. Restart Navidrome, then go to **Settings → Plugins** and configure:

   | Setting | Description |
   |---------|-------------|
   | **Plex Server URL** | Base URL of your Plex server (e.g. `http://192.168.1.100:32400`) |
   | **Plex Token** | Your `X-Plex-Token` authentication token |
   | **Match Threshold** | Minimum fuzzy-match confidence (0–100, default 85) |

## Configuration Details

### Match Threshold

The plugin uses bigram (Dice coefficient) string similarity to match Plex track names back to your Navidrome library. Common noise like "Remastered", "Deluxe Edition", etc. is stripped before comparison. Lower the threshold if you're getting too few matches; raise it if you're getting false positives.

### Caching

Results are cached in Navidrome's persistent KVStore with a 7-day TTL. The cache is keyed by MusicBrainz Recording ID (if available) or a hash of artist + title. To force a refresh, you can wait for the TTL to expire or reinstall the plugin (which resets the KVStore).

## Project Structure

```
plex-similar-songs/
├── main.go          # Plugin implementation
├── manifest.json    # Plugin metadata, permissions, and config schema
├── go.mod           # Go module
├── go.sum           # Go checksums
└── Taskfile.yml     # Build tasks
```

## Capabilities

| Export | Description |
|--------|-------------|
| `nd_get_similar_songs_by_track` | Returns sonically similar songs for a given track |
| `nd_get_similar_songs_by_artist` | Returns sonically similar songs for an artist |

## License

MIT
