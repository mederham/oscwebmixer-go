# OSCWebMixer Go

A complete rewrite of [castles/OSCWebMixer2](https://github.com/castles/OSCWebMixer2) in Go.



OSCWebMixer-Go turns a phone or tablet into a personal monitor mixer: it speaks OSC directly to DiGiCo Quantum and SD-series consoles, so performers can shape their own monitor mix from a browser without installing an app.

## Features

> [!warning]
> This section is currently a placeholder, its content is AI-generated and incomplete.


- Mobile-first mixing UI with dedicated portrait and landscape layouts for level, pan, and aux-select views
- Talks OSC directly to DiGiCo Quantum and SD-series consoles over UDP
- Relays OSC to external devices (iPads, other consoles) via broadcast or loopback
- [htmx](https://htmx.org/)-based admin panel for network settings, external devices, sources, auxes, presets, and general settings
- Drag-and-drop ordering of sources and auxes, with instrument icons for quick identification
- Save and recall permixing presets
- Auto-hide channel, group, and aux name patterns to declutter each performer's view
- Real-time UI updates over WebSocket, no page reloads
- Single static Go binary running in a `scratch` container, JSON config on disk, no external database
- Built-in HTTP healthcheck for container orchestration

## Screenshots

Admin panel | Mixer frontend
:---:|:---:
![Admin sources](docs/screenshots/oscwebmixer-go_admin_3_sources.png) | ![Mixer level view](docs/screenshots/oscwebmixer-go_mixer_level_landscape.png)

More screenshots: [docs/screenshots.md](docs/screenshots.md)

## Installation

The easiest way to run OSCWebMixer-Go is via Docker. Currently only this method is documented here.

### Using Docker CLI

```bash
docker run -p 80:80 -p 8000:8000/udp -v ./config:/webmixer/config registry.mederham.at/audio/oscwebmixer-go:latest
```

This command runs the container image with the web interface at `http://<host-ip>` and UDP port 8000 as the OSC listen port. To configure the desk connection, visit the admin panel under `http://<host-ip>/admin`.

### Using Docker-Compose

```yaml
services:
  oscwebmixer:
    image: registry.mederham.at/audio/oscwebmixer-go:latest
    restart: unless-stopped
    ports:
      - "8080:80"           # admin panel + mixer UI
      - "8000:8000/udp"     # OSC listen port
    volumes:
      - ./config:/webmixer/config
    environment:
      OSCWEBMIXER_DESK_IP: <your_desk_ip>
      OSCWEBMIXER_DESK_PORT: <your_desk_port>
      # OSCWEBMIXER_DESK_TRUSTED_IPS: 192.168.65.1
```

When the environment variables `OSCWEBMIXER_DESK_IP` and `OSCWEBMIXER_DESK_PORT` are set, the application reconfigures the desk connection on every restart to use the values provided. They can still be changed in the admin panel, but reset once the container is restarted.

Docker Desktop (Mac/Windows) masks the desk's real IP with its VM gateway address. The masked IP address is not trusted by OSCWebMixer per default.
Uncomment the `OSCWEBMIXER_DESK_TRUSTED_IPS` variable to trust the VM gateway IP(s) if you use Docker Desktop (default is `192.168.65.1`).
