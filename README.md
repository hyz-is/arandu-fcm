<p align="center">
  <img src=".github/logo.svg" alt="Arandu" width="180">
</p>

# Arandu FCM

An Arandu package that pushes to phones, Android and iOS, through Firebase Cloud
Messaging.

- It **registers devices**: the mobile application hands over the token Firebase
  gave the installation, and the package keeps it, sealed, for the user and the
  tenant of whoever registered it.
- It is a **channel of the hesape `Notifier`**: a notification that names
  `fcm.ChannelFCM` in `Via` and builds a `fcm.Push` reaches every phone of its
  recipient, next to the mail and the database channels.
- It **sends through the FCM HTTP v1 API**, signing in with a service account
  (a JWT signed RS256, traded for an access token that is kept until shortly
  before it expires), with the Android and the APNs blocks that each platform
  needs.
- It **cleans up after Firebase**: a token Firebase says is gone removes its
  device, and a phone that stops registering itself is removed by a scheduled
  clean.
- It never prints, logs or encodes a secret.

## Install

```bash
go get github.com/hyz-is/arandu-fcm
```

## Wire it

An Arandu application registers a module explicitly. There is no service
provider, no container and no discovery, so these are the lines to paste into
`bootstrap/app.go` and there are no others.

The imports, with the other imports:

```go
import (
	"os"

	"github.com/arandu-io/hesape/notifications"
	"github.com/arandu-io/hesape/notifications/channels"

	fcm "github.com/hyz-is/arandu-fcm"
)
```

The construction, in `Build`, after the database, the session store and the
queue exist and before `k.Register`. The service account comes from the
environment, and the key is the application's own:

```go
	credentials, err := fcm.CredentialsFromEnv(os.LookupEnv)
	if err != nil {
		return App{}, err
	}
	fcmModule, err := fcm.New(fcm.Config{
		Credentials: credentials,
		Key:         appKey,   // the application's key: at least 32 bytes
		Queue:       jobs,     // the application's queue.Queue; nil sends inline, with no retry
		Routes:      true,     // the JSON routes for the mobile application; off by default
	}, db, sessions)
	if err != nil {
		return App{}, err
	}
```

The channel goes into the notifier the application already builds, beside the
others:

```go
	notifier := notifications.New([]notifications.Channel{
		channels.NewDatabase(store),
		fcmModule.Channel(),
	})
```

The registration, inside the `k.Register(...)` call already there:

```go
		fcmModule,
```

The worker. When `Queue` is set, the package puts one job per phone on the
`fcm` queue, and the application's worker has to know the handler. Register it
beside the handlers of the other modules, and run a worker for the queue:

```go
	if err := fcmModule.RegisterJobHandlers(worker); err != nil {
		return err
	}
```

```bash
aru queue:work --queue=fcm
```

Then, once, before the application serves:

```bash
aru migrate
```

This package owns a table, which is why the migration step is not optional and
why `arandu.mod.toml` says `migrations = true`. The events it stores go to the
outbox of hesape's `events` module, which the application migrates too, so
arandu-activitylog's recorder writes who registered and removed which phone.

Give people the actions with arandu-permission: `fcm.Actions()` is the list to
offer. `fcm.DeviceRegister`, `fcm.DeviceUnregister` and `fcm.DeviceList` are
what an ordinary member needs for the app on their own phone; `fcm.DeviceManage`
is for whoever administers the tenant.

## Environment variables

| variable | meaning |
| --- | --- |
| `FCM_CREDENTIALS_JSON` | the service account JSON itself, as downloaded from Firebase |
| `FCM_CREDENTIALS_BASE64` | the same JSON in standard base64, for a deployment system that mangles a multi-line value |
| `FCM_CREDENTIALS_FILE` | the path of the service account JSON file |
| `GOOGLE_APPLICATION_CREDENTIALS` | the variable Google's own tools read for the same path; the last resort |

`CredentialsFromEnv` asks them in that order and uses the first that is set. The
package reads the environment only through the lookup the application hands it,
so nothing is read behind your back. The key and the JSON live in the
deployment's secrets, never in the repository, the logs or the chat.

Generate the JSON in the Firebase console: **Project settings -> Service
accounts -> Generate new private key**. The account needs the **Firebase Cloud
Messaging API Admin** role (`roles/firebasecloudmessaging.admin`), and the
**Firebase Cloud Messaging API (V1)** has to be enabled in the Google Cloud
project.

## Register the APNs key in Firebase, for iOS

Firebase delivers to an iPhone through Apple's push service, and it can only do
that once it holds a key Apple issued. Nothing in this package or in your
application server touches APNs: the key is given to Firebase once.

1. In the Apple Developer account, open **Certificates, Identifiers & Profiles
   -> Keys** and create a key with **Apple Push Notifications service (APNs)**
   enabled. Download the `.p8` file (Apple lets you do it once) and note the
   **Key ID**. Note the **Team ID** from the membership page.
2. In the Firebase console, open **Project settings -> Cloud Messaging**. Under
   **Apple app configuration**, in the iOS app registered with your bundle id,
   choose **APNs Authentication Key -> Upload**, and give it the `.p8` file, the
   Key ID and the Team ID. One key serves the development and the production
   environments and every app of the team; a certificate (`.p12`) works too, but
   expires every year.
3. In Xcode, the app target needs the **Push Notifications** capability and
   **Background Modes -> Remote notifications**. The app asks the user for
   permission, and then asks Firebase for the registration token and sends it to
   the route below.
4. For a push with an image, the app needs a **Notification Service Extension**
   that downloads the image from `fcm_options.image`; this package sets
   `mutable-content` on a push that has one, which is what wakes the extension.

An iPhone that never receives anything, with the token accepted by Firebase, is
nearly always a missing or wrong APNs key: check the Cloud Messaging tab first.

## Send a push

A notification is hesape's `notifications.Notification` with one more method,
`ToPush`, which builds the message for the recipient. It is to push what
`ToMail` is to e-mail.

```go
type OrderShipped struct{ OrderID, Number string }

func (OrderShipped) Key() notifications.Key { return "orders.shipped" }

func (OrderShipped) Via(notifications.Notifiable) []notifications.ChannelName {
	return []notifications.ChannelName{notifications.ChannelDatabase, fcm.ChannelFCM}
}

func (n OrderShipped) ToPush(to notifications.Notifiable) fcm.Push {
	return fcm.NewPush().
		Title("Pedido enviado").
		Body("O pedido " + n.Number + " saiu para entrega.").
		Image("https://cdn.example.com/orders/" + n.OrderID + ".png").
		With("deeplink", "app://orders/"+n.OrderID). // the data the app opens the screen from
		CollapseKey("order-" + n.OrderID).          // a later push replaces this one if it was not delivered
		TTL(time.Hour)                              // drop it if the phone stays offline for longer
}
```

```go
	err := notifier.Send(ctx, grant, user, OrderShipped{OrderID: "42", Number: "A-42"})
```

The grant is the one the notifier asks for, issued for
`notifications.ActionSend`. The recipient's phones are those of the user named by
`to.RouteFor(fcm.ChannelFCM)`, or else by `to.NotifiableID()` when
`to.NotifiableType()` is `"user"` (`Config.NotifiableType` changes the word). A
recipient with no phone is `notifications.ErrNotAddressed`: the notifier carries
on with the other channels and reports nothing.

A notification that words its push differently per language implements
`ToPushIn(to, locale)` as well (`fcm.LocalizedPushNotification`); the channel
asks it once per language its recipient's phones use, and uses `ToPush` for a
phone that did not say.

### The message

| `Push` setter | what it does | on Android | on iOS |
| --- | --- | --- | --- |
| `Title`, `Body` | the alert; none of the two makes a data-only push | `notification` | `notification` |
| `Image(url)` | an https image | `android.notification.image` | `apns.fcm_options.image` and `mutable-content: 1` |
| `With`, `Data` | key-value strings for the app, the usual deep link | `data` | `data` |
| `Normal()` | lowers the priority (high is the default) | `priority: NORMAL` | `apns-priority: 5` |
| `CollapseKey` | a later push replaces this one | `collapse_key` | `apns-collapse-id` |
| `TTL` | how long Firebase keeps it for an offline phone | `ttl` | `apns-expiration` |
| `ClickAction` | the intent a tap opens | `android.notification.click_action` | not used |

Every alert goes with `priority: HIGH` (`apns-priority: 10`) and the `default`
sound. A data-only push is delivered as `content-available` with
`apns-push-type: background` and `apns-priority: 5`, as APNs requires of it.

`Push.Validate` runs before anything is sent and refuses what Firebase would:
nothing to show or carry, an image that is not https, a ttl over four weeks, a
reserved data key (`from`, `message_type`, `google*`, `gcm*`), more than 50 pairs
or more than 4096 bytes of title, body and data.

## How a push is delivered, and when it is retried

**With a `Queue`** the channel puts one job per phone on the queue and returns;
the worker delivers. The job names the device and never carries its token. A
failure that can pass (429, any 5xx, a call that did not complete) is returned
to the worker, which releases the job after its backoff (10 s, 30 s, 2 min,
10 min) or after the `Retry-After` Firebase asked for when that is longer,
whichever is more, until the job's `Tries` (5 by default) are spent; then it is
parked once. A failure that cannot pass (a message Firebase refuses, a rejected
credential) is parked at once and never retried. A push that comes up after its
ttl, or after an hour when it has none, is dropped instead of sent late.

**Without a `Queue`** the channel sends before it returns, one call per phone,
and a failure is returned to the notifier as a `*fcm.SendError`. There is no
retry: nothing here loops, sleeps or starts a goroutine.

The one repeat the package makes by itself is for a 401: the cached access token
is replaced, the send is made once more, and a second 401 is a failure.

**A token Firebase says is dead removes its device**, and it is not a failure of
the notification (the user's other phones are still reached):

| answer | meaning | effect |
| --- | --- | --- |
| `UNREGISTERED` | the app was uninstalled or the token expired | the device is removed |
| `INVALID_ARGUMENT` with a violation on `message.token` | the token is not a registration token | the device is removed |
| `INVALID_ARGUMENT` anywhere else | Firebase refuses the *payload* | the device is kept; the job is parked |
| `SENDER_ID_MISMATCH`, `THIRD_PARTY_AUTH_ERROR` | the project or the APNs key is wrong | the device is kept: a misconfiguration must not erase every user's phone |

## Devices

The mobile application registers on every launch (that is what keeps a phone
from going stale) and whenever Firebase hands it a new token, and unregisters
when the user signs out.

```go
	service := fcmModule.Devices()

	device, err := service.Register(ctx, subject, fcm.RegisterRequest{
		Token: token, Platform: "android", App: "com.example.app", Locale: "pt-BR",
	})
	err = service.Unregister(ctx, subject, token)
	phones, err := service.ListForUser(ctx, subject, "") // "" is the subject themselves
```

- **`Register` is idempotent by token.** The same token again updates the row
  and does not add one. The token may change hands: a phone passed to another
  user, or signed into another account, registers the same token, and the row now
  belongs to the one who presented it. The owner is always the subject asking and
  the tenant always the Grant's; neither is read from the request.
- **`Unregister` is idempotent**: a token the subject does not hold is already
  not theirs, and nothing is removed or revealed.
- **`ListForUser`** answers the subject's own phones. Another user's takes
  `fcm.DeviceManage`, and so does removing another user's phone.
- A user keeps at most `MaxDevicesPerUser` phones (10); registering another
  removes the one seen least recently.
- The same token can exist in two tenants as two devices, and nothing links
  them: the digests the rows are found by include the tenant.

### The routes

Off by default. With `Config.Routes: true` the package mounts these under
`Config.Prefix` (default `/fcm/devices`), all behind `RequireAuth`:

| method | path | name | body |
| --- | --- | --- | --- |
| `GET` | `/fcm/devices` | `fcm.devices.index` | |
| `POST` | `/fcm/devices` | `fcm.devices.store` | `{"token", "platform", "app", "locale"}` |
| `POST` | `/fcm/devices/unregister` | `fcm.devices.unregister` | `{"token"}` |
| `DELETE` | `/fcm/devices/{id}` | `fcm.devices.destroy` | |

`store` answers 200 with the device, without its token; `unregister` and
`destroy` answer 204; `GET` answers the subject's devices. A visitor with no
session is sent to `/auth/login`, a subject without the action gets 403, another
user's or another tenant's device is 404, and a rejected input is 422 with the
messages keyed by field. A tenant or a user in the body is ignored. An
application that wants other routes leaves `Routes` off and wraps
`fcmModule.Devices()` in its own.

## Configuration

| field | required | meaning |
| --- | --- | --- |
| `Credentials` | yes, unless `Sender` is set | the service account; read it with `CredentialsFromEnv` |
| `Key` | yes | the application's key, at least 32 bytes; the tokens are sealed and digested with keys derived from it for this module alone |
| `Queue` | no | where pushes are put for a worker. Nil sends inline, with no retry |
| `QueueName` | no | the queue the pushes go on. Defaults to `fcm` |
| `Tries` | no | deliveries a push job gets before it is parked. Defaults to 5 |
| `Routes` | no | mounts the JSON routes. Off by default |
| `Prefix` | no | where the routes are mounted. Defaults to `/fcm/devices`; never inside `/_arandu` |
| `NotifiableType` | no | the `NotifiableType` of the recipients whose id is a user that owns phones. Defaults to `user` |
| `MaxDevicesPerUser` | no | phones kept per person. Defaults to 10, refused above 50 |
| `StaleAfterDays` | no | days without registering before the clean removes a phone. Defaults to 270, the age at which Firebase expires a token |
| `CleanSpec` | no | the cron of the clean. Defaults to `40 3 * * *`; `-` schedules none |
| `APIBase` | no | where Firebase answers; for an emulator or a test. https unless the host is loopback |
| `HTTPClient` | no | carries the calls; a client with a timeout that follows no redirect is used when nil |
| `Sender` | no | replaces the client that reaches Firebase, for the application's tests |
| `Logger` | no | where the module logs. Defaults to `slog.Default()` |
| `Clock` | no | the time the module reads. Defaults to `time.Now` |

There is no tenant to set: the tenant of every statement is the Grant's, from the
session of whoever is registering or from the Grant the notification was
authorized with. `New` returns an error rather than starting half-wired, so a
setting that cannot work fails where it is written.

The scheduled clean (`Module.Schedule`, also callable as `Module.Clean`) removes,
tenant by tenant, the phones that have not registered themselves for
`StaleAfterDays`.

## The table

`aru migrate` creates `fcm_devices`:

| column | |
| --- | --- |
| `id` | primary key, generated by the application |
| `tenant_id` | from the Grant, never from a request |
| `user_id` | the subject who registered the phone |
| `token` | the registration token, **sealed** (AES-256-GCM) |
| `token_hash` | the keyed digest (HMAC-SHA-256 over the tenant and the token) the row is found by; unique with `tenant_id` |
| `platform` | `android` or `ios` |
| `app` | the application id (package or bundle) |
| `locale` | the language of the installation |
| `last_seen_at` | the last registration |
| `created_at`, `updated_at` | |

Indexes: unique `(tenant_id, token_hash)`, `(tenant_id, user_id, created_at)`
and `(tenant_id, last_seen_at)`.

## How the credentials are protected

- The service account is read from the environment (or a file), parsed once,
  and held in a value whose `String`, `GoString`, every `fmt` verb, `slog`,
  JSON and text encodings answer `[redacted]` where the key would be. So do the
  `Config` that holds it and the key, and the `Outgoing` message that holds a
  token in clear. The accessors that remain answer the project and the account's
  address, which are not secrets.
- An error from parsing a credential names the field that is wrong and never any
  of its content.
- An error or a log line built from text Firebase or the token address sent
  passes through a scrubber first: the registration token, the assertion, the
  access token and the key (whole and line by line) are replaced, and the text is
  cut short. The address of a failed call is not carried either: a transport
  error repeats it, and it is nobody's business in a log line.
- The registration tokens are sealed at rest with a key derived from the
  application's key for this module alone, and the sealed text carries the tenant
  and the device it was sealed for: a value copied into another row does not
  open there. The job on the queue carries the device id and never the token.
- The key and the service account are the deployment's secrets: the repository,
  the logs and the chat never hold one.
- `APIBase` and the service account's `token_uri` have to be https, except a
  loopback address; the client follows no redirect, so the access token never
  travels to wherever a redirect points.

## Test your application without Firebase

`fcm.Fake` is a `Sender` that reaches nothing. It records every message it is
asked to send, and `Fail` scripts the outcome:

```go
	fake := &fcm.Fake{}
	module, err := fcm.New(fcm.Config{Sender: fake, Key: appKey}, db, sessions)
	// ... send a notification ...
	sent := fake.Sent() // []fcm.Outgoing: platform and push; the token prints as [redacted]
```

## The data path

`Device` embeds `model.Model`, and `deviceTable`, beside it in `model.go`,
declares its table once. `aru model:build` generates `DeviceQuery.go` from it:
`Devices(db)`, which starts a `*DeviceQuery`, and `DeviceCollection`. The file is
committed and never edited by hand; regenerate it after changing the entity, and
`aru model:build --check` fails when it is out of date.

`DeviceService` owns `*data.DB` and follows
`validate -> security.Authorize -> Grant -> Model terminal`; handlers never hold
the database or start a query. The work done for a push or the clean checks the
Grant it was handed instead. Every query starts at `Devices(s.db)`: the
constructor returns one mutable query, so a second chain begun from a value held
in a variable would carry the clauses of the first.

Terminals return `*Device` and `[]*Device`. Keep those pointers: a copy of a row
keeps the model of the original, and the model refuses to write through it with
`model.ErrUnwired`.

There is no CRUD Repository. Add one only for a complex query, read model,
report, export or raw SQL contract that the generated query cannot express.

## Layout

```
module.go         registration, routes, handlers, jobs, schedule and migrations
config.go         what the application passes in
credentials.go    the service account, read from the environment, never printed
model.go          the Device entity, its table, its own rules, and what it may answer with
DeviceQuery.go    the query aru model:build generates from model.go
policy.go         who may do what
service.go        DeviceService: register, unregister, list, and the work done for a push
message.go        Push, the message of the channel
channel.go        the hesape notifications.Channel
push.go           delivering a push to a device, directly or as a queue job
client.go         FCM v1 and the OAuth2 exchange: the only file that speaks HTTP
fake.go           a Sender for the application's tests
seal.go           sealing the tokens at rest
audit.go          the events stored in the application's outbox
```

## What is already correct, and has to stay that way

**The policy denies everything it was not told to allow.** There is no permit-all
branch. An action opens only for a subject that carries it, and the clean opens
for nobody.

**Authorization precedes the Model.** The package audit checks that order in
every function that reaches it. A Model terminal enforces tenant scope; the
preceding decision says whether the action itself is allowed.

**The tenant comes from `data.Tenant(g)`.** Never from the path, the body, the
query string or a header. The value on the Grant came from the session or from
the authorized notification; a value that arrived with the request is a value the
caller chose.

**Who is asking comes from the guard.** `RequireAuth` loads the session once and
puts the subject on the request; nothing in the module loads it again.

**`arandu.mod.toml` declares what the package does** — network (Firebase and its
OAuth2 address), filesystem (the credential file), migrations — and the suite
compares the declaration against what the code *calls*, not against what it
imports.

## Tests

```bash
go test -race ./...
FCM_TEST_DATABASE_URL='postgres://user@/db?host=/tmp&sslmode=disable' go test -race ./...
```

The second runs the feature suite on a PostgreSQL server, one schema per test.

The unit suite holds the policy, the audit of the source, the credentials and the
redaction, and the validation of a push. The feature suite runs the module over a
real SQLite database with the jobs table and the outbox, a worker, a notifier and
a **fake Firebase** (`httptest`) that serves both the token address and the send
endpoint: the Android and iOS bodies and headers, the token exchange and its
cache, the dead tokens, the 429 with `Retry-After` rescheduled by the worker, the
tries that bound a retry, tenant isolation, idempotent registration, the routes,
the clean, and the secrets that must not appear in an error or a log.

## Licence

MIT. See [LICENSE.md](LICENSE.md). Copyright HYZIS - SERVICOS DIGITAIS LTDA - EPP.
