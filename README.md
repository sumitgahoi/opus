# Opus

Personal music library server. The durable catalog, song bytes, artwork, and
append-only action log. Clients (Hush on iPhone, music-daemon on a speaker host)
sync against it; Opus does not play audio or search.

## Install

Download the latest installer and run it. The same command installs a
new server or upgrades one that is already installed.

```
wget -O /tmp/install.sh https://github.com/sumitgahoi/music-server/releases/latest/download/install.sh
sudo bash /tmp/install.sh
```

On a new install it asks for the music folder. It then downloads the
binary for this machine (linux/amd64 or linux/arm64), installs the
systemd `opus` service, and starts it. The service starts on boot and
restarts after a crash. It listens on port **80** (`http://host` with no
port in the client URL).

Uninstall with the script installed next to the binary:

```
sudo /usr/local/bin/uninstall.sh
```

It asks whether to leave the database and artwork, or remove them.
The music folder stays.

```
sudo /usr/local/bin/uninstall.sh --keep-data
sudo /usr/local/bin/uninstall.sh --purge
```

## Paths

| Path | Role |
| --- | --- |
| `/usr/local/bin/opus` | Server binary |
| `/etc/opus/opus.conf` | Site config (`music_root`, sign-in) |
| `/var/lib/opus/` | Database and artwork cache |
| `opus.service` | systemd unit (user/group `opus`) |

## Listen address

The binary and the shipped unit listen on `0.0.0.0:80`. The unit grants
`CAP_NET_BIND_SERVICE` so the `opus` user can bind that port. Override with
`-addr` when you need a different address.

Stay on plain HTTP while Opus is only on the trusted LAN. Add HTTPS (443)
when you have a real hostname and certificates—typically a reverse proxy
(Caddy or nginx) in front, not TLS inside the Go process. Use HTTPS before
exposing Opus beyond the home network. Hush accepts both `http://` and
`https://` server URLs.
