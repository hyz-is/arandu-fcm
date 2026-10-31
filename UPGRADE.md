# Upgrade Guide

## Unreleased

## v0.1.0

First release, nothing to upgrade from. To install it:

1. `go get github.com/hyz-is/arandu-fcm@v0.1.0`. It requires Framework `v0.67.0`
   and Hesape `v0.89.2`, and declares `framework = ">= 0.67"` in
   `arandu.mod.toml`.
2. Wire it in `bootstrap/app.go` as the README shows, and run `aru migrate`: the
   migration `20261031_0001_create_fcm_devices` creates the `fcm_devices` table.
   The jobs table and hesape's outbox have to be migrated too, as they are for
   any module that queues work and stores events.
3. Set `FCM_CREDENTIALS_JSON` (or `FCM_CREDENTIALS_BASE64`, or a file named by
   `FCM_CREDENTIALS_FILE`) in the deployment's secrets, and give Firebase the APNs
   key for iOS.
4. Add `fcm.Actions()` to what arandu-permission offers, and register the job
   handler with the worker when `Config.Queue` is set.

