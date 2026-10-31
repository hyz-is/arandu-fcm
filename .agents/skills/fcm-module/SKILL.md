---
name: fcm-module
description: Change what this Arandu package registers — its routes, handlers, configuration, response shape or schema. Use when the request is to "add a route", "add an endpoint", "add a handler", "add a config option", "change the prefix", "add a field to the response", "add a column", "write a migration", "return the tenant id too", "add a background job to the package", "make it run something at boot", "add a scope", "add a rule to the entity", "regenerate the query", "who is the current user in a handler", "return 404", or when a change touches module.go, config.go, model.go or DeviceQuery.go. Covers foundation.Module and the five optional interfaces beside it, why handlers are thin, the RequireAuth guard and ctx.User, returning errors for the router to answer, the generated query and aru model:build, where the rules of the entity live, the migration name that carries the order, and what Resource is for.
license: MIT
---

# The module contract

`foundation.Module` is `Name()` and `Routes()`, and nothing else. That pair is
the whole public contract between a package and the framework: an application
calls `New`, gets something that satisfies it, and registers it by hand.

```go
var (
	_ foundation.Module      = (*Module)(nil)
	_ foundation.Migratable  = (*Module)(nil)
	_ foundation.Schedulable = (*Module)(nil)
)
```

Those two lines are compile-time proof and they belong at the top of
`module.go`. Add one for every contract the module takes on, so the failure lands
here rather than at the registration in somebody else's repository.

## Taking on more than routes

Six interfaces sit beside `Module` in `framework/foundation`, two of which this
package already implements, and each is opted into the same way — by
implementing it. Confirm the set for the version in `go.mod` with:

```sh
export GOWORK=off
go doc github.com/arandu-io/framework/foundation
```

| interface | what it is for |
| --- | --- |
| `Bootable` | prepare state once, at boot |
| `Background` | run a loop of the module's own |
| `Schedulable` | declare work for the scheduler — this package implements it, for the clean of stale devices |
| `Migratable` | own tables — this package implements it |
| `Health` | report on the storage it depends on |
| `Closable` | give resources back at shutdown |

Two of them change what `arandu.mod.toml` has to say. A `Background` loop that
calls out needs `network = true` (this package declares it already, for
Firebase); anything that reads or writes a file needs `filesystem = true` (this
package reads the service account file). Declare it in the same commit as the code, or
`TestTheDeclaredCapabilitiesAreWhatTheCodeDoes` fails and names which of the
four it was. Nothing downstream would have caught it: `aru doctor` reads the
application's own tree and never opens an installed package.

## Adding a route

**1. Register it in `Routes`, with a name.**

```go
	r.Action(stdhttp.MethodGet, p+"/{id}", m.show).Name("fcm.devices.show")
```

Below the line that is already there, and nowhere above it (the early return for
`Config.Routes` off stays above both):

```go
	r = r.Group("", middleware.RequireAuth(m.sessions))
```

That line replaces `r` with the guarded group before the first registration, so
a route added below it needs a session like every other, and there is no
unguarded router left in scope to add one to by mistake.
`TestHandlersLeaveTheSubjectAndTheStatusToTheFramework` fails if a route is
registered before the guard is mounted.

The name is what a URL is built from. Two spellings of one address disagree, and
the failure when they do is a link to a 404. The prefix is `m.cfg.Prefix` and
never a literal: the application decides where the package is mounted, and
`TestTheRoutesAreUnderThePrefixAndOutsideTheFrameworkNamespace` in
`tests/Feature/routes_test.go` mounts it at `/mobile/devices` and fails if any
route came out anywhere else, or if `everyRoute` does not reach it. It also
asserts every route is tagged with the module name, which is what
`aru route:list` groups by.

**2. Write the handler thin.** Read the input and who is asking, ask the
service, answer:

```go
func (m *Module) destroy(ctx *fhttp.Context) error {
	who, _ := ctx.User()
	if err := m.devices.UnregisterID(ctx.Ctx(), who, ctx.Param("id")); err != nil {
		return err
	}
	return ctx.Status(stdhttp.StatusNoContent)
}
```

`ctx.User()` reads the subject `RequireAuth` put on the request. Nothing in the
module loads the session again, and no handler takes an identity from the input.
The second value it answers is not consulted: behind the guard there is always a
subject, and the zero one it would answer otherwise is refused by
`security.Authorize` before any policy runs.

`ctx.Status` and not `ctx.JSON` for an empty answer: `JSON` calls `ToArray()` on
what it is handed, so a nil resource panics inside the framework rather than
answering 204.

No rule and no Model construction lives in a handler. A handler that held the
database or called `Devices` would bypass the only place the Policy is
guaranteed to run. Read `fcm-policy` before writing the Service method.

**3. Return the error; the router answers it.** The handler writes `return err`
and nothing else with what the service returned. The framework's action adapter
answers it, in one place, through the same refusal path the route guards use:

| the service returned | the router answers |
| --- | --- |
| `validation.Errors` | 303 back where the request came from, with the messages and what was typed in the flash; to a request that wants JSON, 422 `application/problem+json` with the messages in `errors`, keyed by field |
| `ErrNotFound`, or any `model.ErrModelNotFound` | 404 |
| `security.ErrForbidden`, which a policy refusal is | 403 |
| `security.ErrCSRF` | 419 |
| `database.ErrUniqueViolation` | 409 |
| an error with an `HTTPStatus() int` method | that status |
| anything else | the error page in development, 500 in production |

A request wants JSON when its `Accept` names `application/json`, or when it is an
XHR that is not htmx; it gets every status above as a problem document rather
than a page.

The answer is the status and its standard sentence, never the error's own text:
telling a client why a policy said no tells it what exists, one request at a
time, and the reason is in the log, where the person operating the system reads
it and the person probing it does not.

`ErrNotFound` is a 404 because it wraps `model.ErrModelNotFound`;
`TestTheMissingDeviceIsTheModelsNotFound` holds that. A failure of the module's
own that should answer some other status is an error type with an
`HTTPStatus() int` method, never a `switch` in the handler.
`TestHandlersLeaveTheSubjectAndTheStatusToTheFramework` fails on a handler that
calls `errors.Is` or `errors.As`, `fhttp.Refuse`, `fhttp.Reject` or
`http.Error`.

**4. Add the case to the route tests.** `everyRoute` in
`tests/Feature/routes_test.go` is a table of every route.
`TestAVisitorWithNoSessionIsSentToSignIn` asserts each one sends a request with
no session to `/auth/login`, and `TestASubjectWithoutTheActionGets403OnTheRoutes`
asserts each one answers a signed-in subject that carries no action 403 from the
policy. The feature suite runs on a real SQLite database, so a route that
writes something it should not is caught by the rows it leaves.

## Where the subject comes from

`RequireAuth`, mounted once at the top of `Routes`, loads the session. A request
with none is sent to the sign-in screen and never reaches a handler; a request
with one carries its subject on the context, and the handler reads it with
`who, _ := ctx.User()`. Only the subject travels on the context; the Grant is
still issued by the Policy, per call.

There is no guest on these routes and no tenant in `Config`: the tenant of every
statement is the one on the Grant, which came from the session of whoever is
signed in. A public route that wants to know who is looking without requiring a
session mounts `middleware.LoadSubject` instead, and a reader the package means
to serve without any session is a `security.Guest` the code declares on purpose
— open it in the policy first, as `fcm-policy` says.

## Changing the Model

`Device` embeds `model.Model`, and `deviceTable` declares its table once,
beside it in `model.go`. Keep the application-generated key and the tenant
default visible there:

```go
var deviceTable = model.NewTable(model.TableSpec{
	Name:      "fcm_devices",
	New:       func() model.Entity { return new(Device) },
	ManualKey: true,
})
```

`ManualKey` because the key is text the service writes from `data.NewID()`: the
database neither increments it nor fills it. Do not set `Global: true`: this
package owns tenant data. Model terminals require a Grant and apply `tenant_id`;
the Service still calls `security.Authorize` first because the Model does not
decide which Policy action the Grant represents.

`DeviceQuery.go` is generated from that declaration: `Devices(db)`, the
`*DeviceQuery` it returns, and `DeviceCollection`. It is never edited by
hand. After changing the struct or the table, run

```sh
aru model:build
```

and commit what it rewrote. `aru model:build --check` writes nothing and exits 1
when the file is missing, stale or hand-edited.

Every query starts at `Devices(s.db)`. The constructor returns one mutable
query, so a second chain begun from a value held in a variable carries the
clauses of the first — and compiles.

Keep rows as pointers after `New`, `First`, `Find` or `Get`. A copy keeps the
model of the row it was taken from, and the model refuses to write through it
with `model.ErrUnwired`. For the same reason a row from `New()` is filled field
by field: assigning a whole struct over it replaces its model with the unwired
one of the literal.

The rules of the entity itself go in the custom block of `model.go`: an
invariant, a derived value, a transition that changes only the fields of the
row, written as a pure method — no database, no network, no clock read inside,
no Grant; a time it stamps arrives as a parameter. A guard and the transition it
protects come as a pair when there is a guard; `Reassign` is the transition
registering a known token performs:

```go
// arandu:begin custom

// Reassign gives the device to another user and refreshes what the
// installation says about itself, as registering a token already known does.
func (d *Device) Reassign(userID string, platform Platform, app, locale string, now time.Time) {
	d.UserID = userID
	d.Platform = platform
	d.App = app
	d.Locale = locale
	d.LastSeenAt = now
	d.UpdatedAt = now
}

// arandu:end custom
```

A local scope is a method on `*DeviceQuery` in the same block, and a relation
is registered on `deviceTable` from an `init` function:

```go
// ForApp narrows the query to the devices registered by this application id.
func (q *DeviceQuery) ForApp(app string) *DeviceQuery {
	return q.Where("app", "=", app)
}
```

The Service orchestrates around them: it validates the request, asks the Policy
for the Grant, calls the rule, and saves with that Grant.

The Hesape Model stamps `created_at` and `updated_at` when it saves, because the
entity declares both columns; `last_seen_at` is the module's own and is set from
the configured clock, so the tests move it.

## Adding a configuration field

`Config` is a typed struct, and that is the point: a misspelled key in a map is
a setting that silently keeps its default, and here a field that does not exist
does not compile.

Three things move together for every new field.

- **A doc comment on the field**, saying what it means and where it may come
  from.
- **A rule in `Validate`** if there is a value it cannot be. `New` calls
  `Validate` before anything else, so a setting that cannot work fails where it
  is wired rather than on the first request that needed it.
- **A default in `withDefaults`**, as a named constant beside `DefaultPrefix`
  and `DefaultTries`, if zero is meant to mean something. `withDefaults` runs
  *after* `Validate` and never before: filling a default in first hides the
  value somebody actually wrote from the check that would have refused it.

A value out of range is refused rather than clamped. A number somebody wrote and
did not get is worse than a number somebody wrote and was told about — that is
why `MaxDevicesPerUser` above `MaxDevicesPerUserLimit` is an error and not a
silent 50.

`Config` holds secrets, so a field that holds one is also left out of
`Config.summary`, which is what every printed, logged and encoded form is built
from, or is put there as `[redacted]`. Add the case to
`TestTheConfigurationRefusesWhatCannotWork` in `tests/Unit/policy_test.go`,
which is a map of named bad configurations, and to
`TestNewRefusesAWiringThatCannotWork` in `tests/Feature/routes_test.go` if the
field can make `New` fail.

## What may leave in a response

`DeviceResource` and `DevicesResource` in `model.go` are declared snapshots, not
direct encoding of the entity. This also defines the safe copy boundary:
Model-backed Service results stay as `*Device`/`[]*Device`, because a copied row keeps
the model of the original and refuses to be written. A response snapshot reads
only the explicit fields and cannot be saved.

An encoder handed the entity would answer with whatever fields it happens to
have, including the embedded Model and anything added later without opening the
handler. `TenantID` names another customer's identifier, and `Token` and
`TokenHash` are the sealed registration token and its digest; none belongs in a
response.

So a new column that should be visible is added in two places: the struct in
`model.go`, and the map `ToArray` returns. A column that should not be visible
is added in one.

`With()` is what goes *beside* the fields at the top level. `ctx.JSON` puts what
`ToArray` returns under a fixed `"data"` key and merges what `With` returns next
to it, so `DevicesResource` answers `{"data": {"items": [...]}}`. A resource with
nothing to add returns nil. The list of one user's phones has no next page, so it
carries no cursor; a listing that could grow past one page would add a
`next_cursor` here, offered only for a full page.

## Adding a migration

Append it to the slice `Migrations()` returns, and give it a name that sorts
after the last one:

```go
func (createDevices) GetName() string { return "20261031_0001_create_fcm_devices" }
```

**The name carries the order and nothing else does.**
`TestTheModuleDeclaresItsSchemaAndRollsItBack` in `tests/Feature/devices_test.go`
holds the name of the migration, the compile-time assertion in `module.go`
requires every one to satisfy `migrations.ReversibleMigration` — the migrator finds `Down` by type assertion,
so a `Down` with the wrong signature is a rollback that silently does nothing.

Three rules the existing migration keeps:

- **A name is fixed once the package is published.** Changing what an applied
  name means leaves the change missing everywhere it already ran, and nothing
  says so.
- **Types that spell the same in SQLite, PostgreSQL and MySQL.** Identifier
  columns are `VARCHAR(255)` rather than `TEXT` because they take part in a key
  and MySQL refuses `TEXT` in one without a prefix length. Timestamps get no
  database default: the value comes from Go, for the same reason ids do —
  `gen_random_uuid`, `UUID()` and `randomblob` are three spellings of one idea.
- **An index that matches the `ORDER BY` of the listing, tenant first.** Without
  it every page is a scan of every customer's rows.

Migrations do not run at boot. `aru migrate` is a step in the installer's
pipeline, and every migration has to be compatible with the previous binary
during a rollout: a new column is nullable or has a default, and removing one
takes two releases.
