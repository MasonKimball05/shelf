# shelf

Streams the media library on my Windows desktop to my
[Media Player](https://github.com/MasonKimball05/Custom-Mac-Media-Player) over
Tailscale: browse folders, search, and play movies, TV and music with seeking and
subtitles, from home or anywhere. Supervised by
[homebase](https://github.com/MasonKimball05/homebase) like my other self-hosted apps.

Go, standard library only. One `.exe`. Files are played as they are, with no
transcoding, because mpv on the Mac side plays nearly every format.

## How it works

1. On startup (and every 30 minutes) shelf scans the configured folders and
   gives every media file an **opaque ID**: a hash of its folder and path.
2. Media Player browses and searches over a JSON API that requires a **bearer token**.
3. To play a file, the app asks for a **signed link**:
   `/stream/<id>/<name>?exp=<time>&sig=<HMAC>`. mpv and AVFoundation can't
   easily send auth headers, so the link carries its own proof, valid for 12 hours.
4. The stream endpoint verifies the signature and serves the file with
   **Range support**, so seeking and resuming work like a local file.

## Security

- **No path from a request ever touches the disk.** Streams are served by ID only,
  and browsing uses the path purely as an index lookup, so `../` traversal is
  impossible by construction. Symlinks and junctions inside the library are not followed.
- **Links can't be forged, extended or redirected** to another file: the HMAC
  covers the file ID and the expiry, and is checked in constant time. A link that
  leaks into a history or log stops working when it expires.
- **Tailscale only.** A Windows firewall rule allows the port from `100.64.0.0/10`
  only, like the other apps homebase runs.
- **Secrets live in the environment**, via homebase's `env_file`, never in the config.

## Configuration

`shelf.json` (see `shelf.example.json`):

```json
{
  "listen": "0.0.0.0:8095",
  "public_url": "http://arkans-pc1:8095",
  "roots": [
    { "name": "Movies", "path": "D:\\Media\\Movies" },
    { "name": "Music",  "path": "D:\\Media\\Music" }
  ],
  "link_ttl_hours": 12,
  "rescan_minutes": 30
}
```

`shelf.env` (never committed):

```
SHELF_TOKEN=<24+ random characters: pasted into Media Player's Settings → Library>
SHELF_SECRET=<32+ random characters: signs stream links, never leaves the desktop>
```

## Build and test

```bash
go test ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o shelf.exe .
```

## API

| Method | Path | Auth |
|---|---|---|
| GET | `/api/roots` | token |
| GET | `/api/browse?root=&path=` | token |
| GET | `/api/search?q=` | token |
| POST | `/api/link?id=` | token: returns a signed stream link (plus subtitle links) |
| POST | `/api/rescan` | token |
| GET | `/stream/{id}/{name}?exp=&sig=` | signature |
| GET | `/healthz` | none (for homebase) |
