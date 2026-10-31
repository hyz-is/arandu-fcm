package fcm

import (
	"context"
	"fmt"

	"github.com/arandu-io/framework/security"
)

// The actions of Fcm. Constants rather than strings at the call site: a
// typo in an action name would silently authorize nothing, or worse, everything.
//
// They carry the entity in the name because an application registers many
// packages, and the name of an action shows up in logs and in audit trails
// where "view" on its own says nothing about what was viewed.
const (
	// FcmView is reading one record.
	FcmView security.Action = "fcm.view"
	// FcmList is paging through the records.
	FcmList security.Action = "fcm.list"
	// FcmCreate is adding one.
	FcmCreate security.Action = "fcm.create"
	// FcmUpdate is changing one.
	FcmUpdate security.Action = "fcm.update"
	// FcmDelete is removing one.
	FcmDelete security.Action = "fcm.delete"
)

// FcmPolicy is the only authority over who does what with a Fcm.
//
// IT DENIES EVERYTHING, and that is the state to start from rather than a
// placeholder to delete. A policy shipped with a branch that allows every
// action is a hole in every application that installs the package, and the hole
// looks like working code until somebody reads it.
//
// There is deliberately no such branch to remove. Open one action at a time,
// inside the custom block below, saying who may take it and on which record --
// what is not written there stays closed, including every action added later.
type FcmPolicy struct{}

// Compile-time proof that the policy answers about this entity and no other. A
// policy that drifted onto another type would leave this one unguarded while
// the Model path still compiled.
var _ security.Policy[Fcm] = FcmPolicy{}

// Can decides whether the subject may perform the action on the record.
//
// It is the only place that decides. The service reaches the Model only after
// Authorize turns this method's nil result into a Grant.
func (FcmPolicy) Can(ctx context.Context, s security.Subject, a security.Action, record Fcm) error {
	// Tenant isolation comes first and applies to every action. Without it every
	// check below would be pointless in a multi-tenant system: a rule that
	// allows an owner to read their own record would allow it across customers
	// as soon as two of them have a record with the same identifier.
	//
	// The empty id is the candidate that has not been stored yet, which belongs
	// to nobody until it is written with the tenant off the Grant.
	if record.ID != "" && record.TenantID != s.Tenant {
		return fmt.Errorf("fcm belongs to another tenant")
	}

	// arandu:begin custom
	// The rules of this package go here, one action at a time. A rule that
	// depends on the record and not only on the role is written the same way:
	//
	//	if a == FcmView && (s.ID == record.ID || s.HasRole("admin")) {
	//		return nil
	//	}
	//
	// The routes of this package sit behind RequireAuth, so none of them asks
	// about a guest. An application that calls the service from a public route
	// of its own may, with security.Guest: a reader it declared anonymous on
	// purpose, and the only subject that arrives without an id. Answer it
	// explicitly or it falls through to the refusal below, which is the safe
	// direction:
	//
	//	if a == FcmView && s.IsGuest() && record.Published {
	//		return nil
	//	}
	// arandu:end custom

	return fmt.Errorf("no rule allows %s on fcm", a)
}
