# plugin-amcrest

`plugin-amcrest` is a SlideBolt plugin for Amcrest cameras. It is intentionally
manual: cameras are provisioned as normal SlideBolt devices and then reconciled
by the plugin. The plugin does not scan the network for cameras on startup.

## Connection Overview

**Protocol**: HTTP with Digest Authentication  
**Port**: 80 (HTTP) or 443 (HTTPS)  
**Authentication**: HTTP Digest (not Basic Auth)

## Quick Start

```bash
# Run unit tests
go test ./...

# Run the real-camera integration test
go test -tags integration -run TestDiscovery_FindCameras ./cmd/plugin-amcrest
```

The integration test reads local camera credentials from `.env.local`, seeds a
provisioned `Device` + `Entity`, and then starts the plugin against the shared
test harness.

## API Endpoints

### Get System Info
```bash
GET /cgi-bin/magicBox.cgi?action=getSystemInfo
```
Returns: `deviceType=IPCamera`, `serialNumber=...`, etc.

### Get Software Version
```bash
GET /cgi-bin/magicBox.cgi?action=getSoftwareVersion
```
Returns: `version=2.800.0000000.18.R`

### Get Snapshot
```bash
GET /cgi-bin/snapshot.cgi
```
Returns: JPEG image data

### Get Event Codes
```bash
GET /cgi-bin/eventManager.cgi?action=getEventIndexes&code=All
```
Returns available events like: `VideoMotion`, `VideoLoss`, `VideoBlind`, `_DoorbellPress_`

### Subscribe to Events (Server-Sent Events)
```bash
GET /cgi-bin/eventManager.cgi?action=attach&codes=[VideoMotion,_DoorbellPress_]
```
Returns: Multipart stream with events

### Get All Config
```bash
GET /cgi-bin/configManager.cgi?action=getConfig&name=All
```
Returns: All camera configuration as key=value pairs

## Authentication Flow

1. First request returns `401 Unauthorized` with `WWW-Authenticate: Digest ...` header
2. Client computes MD5 hash digest:
   - `HA1 = MD5(username:realm:password)`
   - `HA2 = MD5(method:uri)`
   - `response = MD5(HA1:nonce:nc:cnonce:qop:HA2)`
3. Second request includes `Authorization: Digest ...` header

## Example Event Format

```
Code=VideoMotion;action=Start;index=0;data=...
Code=_DoorbellPress_;action=Start;index=0;data=...
```

## Local Integration Environment

```bash
AMCREST_HOST=192.168.88.121      # Required for local integration test only
AMCREST_USERNAME=admin           # Required for local integration test only
AMCREST_PASSWORD=secret          # Required for local integration test only
AMCREST_SCHEME=http              # Optional
AMCREST_TIMEOUT_MS=3000          # Optional
```

Those values are not runtime plugin startup config. They are only used by the
local integration test to seed a provisioned camera.

## Provisioning Model

- **Per-device**: Each camera is a separate SlideBolt device
- **Manual**: Cameras are added explicitly; there is no LAN discovery
- **Digest Auth**: Uses HTTP Digest authentication (not Basic Auth)
- **Provisioned**: The plugin reconciles already-stored device metadata into
  live entities
