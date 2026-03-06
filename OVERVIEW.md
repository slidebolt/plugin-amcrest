# plugin-amcrest

Amcrest camera plugin for Slidebolt using manual device onboarding.

## Onboarding model

Devices are created manually via `POST /api/plugins/plugin-amcrest/devices`.

At create time, include credentials in device labels:

- `amcrest_host`
- `amcrest_username`
- `amcrest_password`
- optional: `amcrest_scheme` (`http`/`https`)
- optional: `amcrest_event_codes` (comma-separated)

The plugin immediately validates connectivity using the supplied host/user/pass.
If connection succeeds, credentials are persisted in plugin `RawStore` for that device.
Sensitive labels are scrubbed from canonical device output.

## Plugin env

- `AMCREST_DEFAULT_SCHEME` (`http` default)
- `AMCREST_DEFAULT_EVENT_CODES` (`_DoorbellPress_,VideoMotion` default)
- `AMCREST_CONNECT_TIMEOUT_MS` (`3000` default)
