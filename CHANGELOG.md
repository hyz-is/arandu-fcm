# Changelog

Everything worth knowing about a release of Arandu FCM is recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A published module version is immutable: Go serves it from the proxy forever, so
a release is corrected by another release and never by moving a tag.

## [Unreleased]

## [0.1.0] - 2026-10-31

First release.

### Added

- `Device`, the phone of a user of a tenant, in the `fcm_devices` table created
  by the migration `20261031_0001_create_fcm_devices`. The registration token is
  sealed at rest and found by a keyed digest that includes the tenant; the row is
  unique per `(tenant_id, token_hash)`.
- `DeviceService` with `Register` (idempotent by token; the token may change
  hands), `Unregister` (idempotent), `UnregisterID` and `ListForUser`. A user
  keeps at most `Config.MaxDevicesPerUser` phones; the one seen least recently
  goes first.
- `DevicePolicy` and the actions `DeviceRegister`, `DeviceUnregister`,
  `DeviceList` and `DeviceManage`, offered by `Actions()`, and `DeviceClean`,
  which nobody holds: the scheduled clean issues its own Grant for each tenant.
- `Channel`, the hesape `notifications.Channel` named `ChannelFCM`, with `Push`
  (title, body, https image, data pairs, priority, collapse key, ttl, click
  action) and the interfaces `PushNotification` and
  `LocalizedPushNotification` a notification implements.
- `Client`, the FCM HTTP v1 sender: OAuth2 with the service account (a JWT signed
  RS256 traded for an access token, cached until five minutes before it
  expires), the `android` and `apns` blocks, and `SendError`, which says whether
  the token is dead, whether to retry and after how long. `Sender` is the seam
  and `Fake` the double for an application's tests.
- `Credentials`, read with `ParseCredentials`, `CredentialsFromFile` or
  `CredentialsFromEnv` (`FCM_CREDENTIALS_JSON`, `FCM_CREDENTIALS_BASE64`,
  `FCM_CREDENTIALS_FILE`, `GOOGLE_APPLICATION_CREDENTIALS`). `Credentials`,
  `Config` and `Outgoing` print, log and encode without their secrets.
- Delivery through the host queue: with `Config.Queue` one job `fcm.push` per
  phone on the `fcm` queue, registered with `Module.RegisterJobHandlers`; a 429
  or a 5xx is retried by the worker with backoff and at least the `Retry-After`
  Firebase asked for, up to `Config.Tries`; a refused message is parked at once.
  Without a queue the channel sends inline and does not retry.
- A token Firebase reports as `UNREGISTERED`, or as `INVALID_ARGUMENT` about
  `message.token`, removes its device. `Module.Schedule` declares a daily clean
  of the phones that have not registered for `Config.StaleAfterDays`
  (`Module.Clean` runs it).
- Optional JSON routes, off unless `Config.Routes` is set: `GET`, `POST`,
  `POST .../unregister` and `DELETE .../{id}` under `Config.Prefix`.
- Events in the application's outbox for every change: `fcm.device.registered`,
  `fcm.device.unregistered` and `fcm.device.removed`, with no token in them.

