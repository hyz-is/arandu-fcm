---
name: fcm-package
description: Install, wire and use the Arandu FCM package (Go, Arandu) in an application. Use when the request is to "install Arandu FCM", "add push notifications to the app", "send a push to the phone", "Firebase Cloud Messaging", "go get github.com/hyz-is/arandu-fcm", "wire it into bootstrap/app.go", "register the module", "register the device token", "the iPhone gets nothing", "the token expired", "everything under /fcm/devices returns 403", "it redirects to /auth/login", "the table does not exist", "no such table", "let members register a phone", or when a project's go.mod already requires github.com/hyz-is/arandu-fcm. Covers the lines of wiring and where each one goes, the service account in the environment, the notifications channel and the Push message, the worker and the queue, the optional device routes, the actions to give people, and the migration step that is not optional.
license: MIT
metadata:
  audience: app
---

# Using Arandu FCM

An Arandu package that pushes to Android and iOS phones through Firebase Cloud
Messaging. It keeps the devices of the tenant's users in its own table, and it is
a channel of the hesape notifier. It is registered by hand — there is no service
provider, no container and no discovery, so if a line is not written in the
application, it does not happen.

## Install

```bash
go get github.com/hyz-is/arandu-fcm
```

## The lines of wiring, and where each one goes

All of them are in `bootstrap/app.go`. Nothing else in the application changes,
and `aru make:module` does not edit this file for you.

The imports, with the other module imports:

```go
import (
	"os"

	"github.com/arandu-io/hesape/notifications"
	"github.com/arandu-io/hesape/notifications/channels"

	fcm "github.com/hyz-is/arandu-fcm"
)
```

The construction, in `Build`, after the database, the session store and the queue
exist and before `k.Register`:

```go
	credentials, err := fcm.CredentialsFromEnv(os.LookupEnv)
	if err != nil {
		return App{}, err
	}
	fcmModule, err := fcm.New(fcm.Config{
		Credentials: credentials,
		Key:         appKey, // at least 32 bytes
		Queue:       jobs,   // the application's queue.Queue; nil sends inline, with no retry
		Routes:      true,   // the JSON routes the mobile app calls; off by default
	}, db, sessions)
	if err != nil {
		return App{}, err
	}
```

The channel, in the notifier the application already builds:

```go
	notifier := notifications.New([]notifications.Channel{
		channels.NewDatabase(store),
		fcmModule.Channel(),
	})
```

And the registration, inside the `k.Register(...)` call already there:

```go
		fcmModule,
```

`New` returns an error rather than starting half-wired. It refuses a
configuration that cannot work, a missing service account, a short key, a nil
database handle, and a nil session store when the routes are on, so a wiring
mistake costs one restart instead of arriving as a nil dereference on the first
request that needed it.

## The credential

The service account JSON comes from the environment, never from the repository:
`FCM_CREDENTIALS_JSON`, or `FCM_CREDENTIALS_BASE64`, or a file named by
`FCM_CREDENTIALS_FILE` (or `GOOGLE_APPLICATION_CREDENTIALS`), in that order. It
is generated in the Firebase console (Project settings, Service accounts,
Generate new private key) and its account needs the Firebase Cloud Messaging API
Admin role, with the FCM API (V1) enabled in the Google Cloud project. The
package never prints it: not in a log, an error, `String()` or JSON. Do not add
a line that does.

For iOS, Firebase needs Apple's key, once: in Firebase, Project settings, Cloud
Messaging, upload the APNs Authentication Key (`.p8`) with its Key ID and the
Team ID. The Xcode target needs the Push Notifications capability and Remote
notifications in Background Modes. **Symptom to recognise:** Android works and an
iPhone gets nothing while Firebase accepts the token — the APNs key is missing
or belongs to another team.

## Then run the migrations, and the worker

```bash
aru migrate
aru queue:work --queue=fcm
```

Not optional, and not automatic. The package owns a table, `fcm_devices`, which
is why its `arandu.mod.toml` declares `migrations = true`. `aru migrate` is a step
in the pipeline and never a call at the start of the process: with N replicas, N
migrations race each other. The jobs table and hesape's outbox (where the
module stores who registered and removed which phone) are migrated by the
application too.

**Symptom to recognise:** `no such table`, or `relation does not exist`, from a
route that authorized correctly. The migration has not run.

With a `Queue`, the handler has to be known to the worker, beside the handlers of
the other modules:

```go
	if err := fcmModule.RegisterJobHandlers(worker); err != nil {
		return err
	}
```

**Symptom to recognise:** the `fcm` queue only grows. No worker is running for it,
or it was started without the handler.

## Send a push

A notification implements `ToPush` as it implements `ToMail`:

```go
func (OrderShipped) Via(notifications.Notifiable) []notifications.ChannelName {
	return []notifications.ChannelName{fcm.ChannelFCM}
}

func (n OrderShipped) ToPush(to notifications.Notifiable) fcm.Push {
	return fcm.NewPush().Title("Pedido enviado").Body("A caminho.").
		With("deeplink", "app://orders/"+n.ID)
}
```

The recipient is the user whose id the notifiable carries (`NotifiableType` is
`"user"`, or `RouteFor(fcm.ChannelFCM)` names the user). A user with no phone is
not reachable on this channel, and that is not an error. The grant given to
`notifier.Send` is the one hesape asks for, issued for `notifications.ActionSend`.
A token Firebase says is gone removes its device; a 429 or a 5xx is retried by
the queue's worker after at least the `Retry-After`; a refused message is parked.

For the application's own tests, `fcm.Config{Sender: &fcm.Fake{}}` needs no
credentials and records what would have been sent.

## Register the phone

The mobile app registers on every launch and when Firebase hands it a new token,
and unregisters on sign-out. With `Routes: true`, behind `RequireAuth`:

| method | path | name | body |
| --- | --- | --- | --- |
| `GET` | `/fcm/devices` | `fcm.devices.index` | |
| `POST` | `/fcm/devices` | `fcm.devices.store` | `{"token", "platform", "app", "locale"}` |
| `POST` | `/fcm/devices/unregister` | `fcm.devices.unregister` | `{"token"}` |
| `DELETE` | `/fcm/devices/{id}` | `fcm.devices.destroy` | |

`platform` is `android` or `ios`. The tenant and the owner are the session's: a
tenant or a user in the body is ignored. Registering the same token again updates
the row; the same token from another user moves the device to them. An
application that wants other routes leaves `Routes` off and calls
`fcmModule.Devices()` (`Register`, `Unregister`, `ListForUser`) from its own.

Every route sits behind `RequireAuth`, mounted by the package itself. A request
with no session is sent to `/auth/login` — 303, or `HX-Redirect` for an htmx
request. That is the sign-in screen the starter kit publishes; an application
without one sees a 404 there. **Symptom to recognise:** every route of the
package redirects to `/auth/login`. Nobody is signed in on that request: the
session cookie did not arrive, or the session store passed to `New` is not the
one the application signs people in with.

Build URLs from the names, never by writing the path a second time. `aru
route:list` shows them grouped by module. Under a custom `Prefix` the paths move
and the names do not.

## Give people the actions

The policy denies every action a subject was not given, and it is not a bug to
work around. Offer `fcm.Actions()` in arandu-permission: `fcm.DeviceRegister`,
`fcm.DeviceUnregister` and `fcm.DeviceList` for a member's own phone,
`fcm.DeviceManage` for whoever administers the tenant. A subject with none of
them answers 403 on every route, on day one.

**Do not open it from the application.** There is no hook, no override and no
config flag for the policy, and adding one would be a second place where
authorization is decided. If the rules this application needs are not the rules
the package ships, the change belongs in the package.

## What the answers mean

| status | what happened |
| --- | --- |
| `303` to `/auth/login` | there is no session on the request |
| `403` | the policy refused: the subject does not carry the action |
| `404` | no device with that id for this user in this tenant |
| `422` `application/problem+json` | the input was rejected; the messages are in `errors`, keyed by field |

The package does not choose these: its handlers return what the service
returned, and the framework's router answers it. A refusal carries no detail
beyond the status and its standard sentence. Telling a client why a policy said
no tells it what exists and what does not, one request at a time. The reason is
in the log.

## Reporting a problem

Issues and pull requests go to the repository the module path names,
`github.com/hyz-is/arandu-fcm`, which belongs to `hyz-is`. A vulnerability goes to the
private advisory form named in that repository's `SECURITY.md`, and never into
an issue.

MIT licensed. Copyright HYZIS - SERVICOS DIGITAIS LTDA - EPP.
