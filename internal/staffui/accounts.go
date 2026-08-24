package staffui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// Staff account management (owner-only) — closes the gap where creating a
// staff account, changing a password, or deactivating someone required a
// direct SQL statement against staff_users.

// staffRoles are the ISP-wide roles this screen can assign — no franchise_id
// involved, and chk_staff_franchise_binding requires that column stay NULL
// for every one of them.
var staffRoles = []string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}

// franchiseRoles are the three roles staff_users.chk_staff_role also permits
// (migration 024) that instead require a franchise_id binding — a franchise
// partner's own staff. Kept separate from staffRoles rather than merged so
// the "assignable roles" dropdown can group them, and so isStaffRole (the
// ISP-wide check used by lockout/self-service logic elsewhere) is not
// accidentally widened by adding these.
var franchiseRoles = []string{"lco", "franchise_admin", "franchise_staff"}

// allAssignableRoles is every role this screen's dropdowns offer.
func allAssignableRoles() []string {
	out := make([]string, 0, len(staffRoles)+len(franchiseRoles))
	out = append(out, staffRoles...)
	out = append(out, franchiseRoles...)
	return out
}

func isFranchiseRole(role string) bool {
	for _, r := range franchiseRoles {
		if r == role {
			return true
		}
	}
	return false
}

func isAssignableRole(role string) bool {
	return isStaffRole(role) || isFranchiseRole(role)
}

// franchiseIDForRole parses and validates a franchise_id form value against
// the role it will be paired with, matching chk_staff_franchise_binding's
// own rule (franchise-scoped roles require one, every other role forbids
// one) without waiting for the constraint to reject a mismatch as a raw
// 500.
//
// required distinguishes the two callers: account creation has no existing
// binding to fall back to, so a franchise-scoped role must name one right
// now (required=true). Editing an existing account may leave the field
// blank to mean "keep whatever is already bound" (required=false) — the
// store's own UPDATE resolves that per-role, including clearing the
// binding entirely when the edit moves the account to an ISP-wide role.
func franchiseIDForRole(role, raw string, required bool) (*int, error) {
	if !isFranchiseRole(role) {
		// An ISP-wide role is never bound, whatever was submitted — the
		// caller does not even need to have hidden the field via JS for
		// this to be safe, since it is ignored either way.
		return nil, nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return nil, fmt.Errorf("A franchise must be selected for this role.")
		}
		return nil, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("Franchise selection is invalid.")
	}
	return &id, nil
}

// minStaffPasswordLength is a console-side floor, not a schema constraint.
// Higher than a subscriber's (unvalidated) password: a console account can
// disconnect sessions, credit wallets and resolve a law-enforcement lookup.
const minStaffPasswordLength = 10

// isDuplicateUsername reports whether err looks like staff_users' unique
// constraint on username firing. A local, string-matching equivalent of
// internal/api/routes.go's isUniqueViolation — that one is unexported in a
// different package, and duplicating a three-line check here is simpler
// than exporting it just for this one call site.
func isDuplicateUsername(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}

func isStaffRole(role string) bool {
	for _, r := range staffRoles {
		if r == role {
			return true
		}
	}
	return false
}

type accountsData struct {
	Accounts   []StaffAccount
	Roles      []string
	Franchises []revenue.FranchiseRecord
}

// StaffAccounts lists every account and hosts the create-account form.
func (h *Handler) StaffAccounts(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "accounts")
	if !ok {
		return
	}
	h.renderStaffAccounts(w, r, s, "", "")
}

func (h *Handler) renderStaffAccounts(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Staff Accounts", "accounts")
	d.Message, d.Error = message, errMsg

	if h.staff == nil {
		d.Error = "Staff account management is not configured on this deployment."
		h.render(w, "accounts", d)
		return
	}

	accounts, err := h.staff.ListStaff(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list staff failed")
		if d.Error == "" {
			d.Error = "Could not load staff accounts."
		}
		h.render(w, "accounts", d)
		return
	}

	// Franchises are optional context, not a hard dependency: a deployment
	// with no franchise store configured (or simply none onboarded yet)
	// still gets the ISP-wide half of this screen; the franchise-role
	// options in the dropdown just have nothing to bind to yet.
	var franchises []revenue.FranchiseRecord
	if h.franchises != nil {
		franchises, err = h.franchises.ListFranchises(r.Context(), nil)
		if err != nil {
			log.Error().Err(err).Msg("staffui: list franchises (for account form) failed")
		}
	}

	d.Data = accountsData{Accounts: accounts, Roles: allAssignableRoles(), Franchises: franchises}
	h.render(w, "accounts", d)
}

// CreateStaffAccount registers a new console operator.
func (h *Handler) CreateStaffAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "accounts")
	if !ok {
		return
	}
	if h.staff == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Staff account management is not configured.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	fullName := strings.TrimSpace(r.PostFormValue("full_name"))
	password := r.PostFormValue("password")
	role := r.PostFormValue("role")
	leaAccess := r.PostFormValue("lea_access") == "on"

	if username == "" || fullName == "" {
		h.renderStaffAccounts(w, r, s, "", "Username and full name are required.")
		return
	}
	if !isAssignableRole(role) {
		h.renderStaffAccounts(w, r, s, "", "Role must be one of: "+strings.Join(allAssignableRoles(), ", ")+".")
		return
	}
	if len(password) < minStaffPasswordLength {
		h.renderStaffAccounts(w, r, s, "",
			fmt.Sprintf("Password must be at least %d characters.", minStaffPasswordLength))
		return
	}

	// required=true: creation has no existing binding to fall back to, so a
	// franchise-scoped role must name one right now.
	franchiseID, ferr := franchiseIDForRole(role, r.PostFormValue("franchise_id"), true)
	if ferr != nil {
		h.renderStaffAccounts(w, r, s, "", ferr.Error())
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		log.Error().Err(err).Msg("staffui: hash new staff credential failed")
		h.renderStaffAccounts(w, r, s, "", "Could not create that account.")
		return
	}

	created, err := h.staff.CreateStaff(r.Context(), username, fullName, string(hash), role, leaAccess, franchiseID)
	if err != nil {
		if isDuplicateUsername(err) {
			h.renderStaffAccounts(w, r, s, "", "That username is already in use.")
			return
		}
		log.Error().Err(err).Str("username", username).Msg("staffui: create staff failed")
		h.renderStaffAccounts(w, r, s, "", "Could not create that account.")
		return
	}
	h.renderStaffAccounts(w, r, s, fmt.Sprintf("Account %q created.", created.Username), "")
}

// UpdateStaffAccount edits a role, LEA access, active status, and
// optionally resets the password — one form, matching the Routers screen's
// edit-plus-optional-secret-rotation pattern.
func (h *Handler) UpdateStaffAccount(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "accounts")
	if !ok {
		return
	}
	if h.staff == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Staff account management is not configured.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderStaffAccounts(w, r, s, "", "Invalid account id.")
		return
	}

	role := r.PostFormValue("role")
	if !isAssignableRole(role) {
		h.renderStaffAccounts(w, r, s, "", "Role must be one of: "+strings.Join(allAssignableRoles(), ", ")+".")
		return
	}
	leaAccess := r.PostFormValue("lea_access") == "on"
	active := r.PostFormValue("active") == "on"

	if blocked, msg := h.blockLastOwnerLockout(r, id, role, active); blocked {
		h.renderStaffAccounts(w, r, s, "", msg)
		return
	}

	// required=false: editing an existing account, so a blank franchise_id
	// for a franchise-scoped role means "keep whatever is already bound" —
	// the store's own UPDATE resolves that, not this check. A role change
	// away from a franchise-scoped role always clears the binding
	// regardless of what was submitted here (see UpdateStaff's own doc).
	franchiseID, ferr := franchiseIDForRole(role, r.PostFormValue("franchise_id"), false)
	if ferr != nil {
		h.renderStaffAccounts(w, r, s, "", ferr.Error())
		return
	}

	updated, err := h.staff.UpdateStaff(r.Context(), id, &role, &leaAccess, &active, franchiseID)
	if err != nil {
		log.Error().Err(err).Int("staff_id", id).Msg("staffui: update staff failed")
		h.renderStaffAccounts(w, r, s, "", "Could not save that account.")
		return
	}
	if updated == nil {
		h.renderStaffAccounts(w, r, s, "", "No account with that id.")
		return
	}

	if newPassword := r.PostFormValue("password"); newPassword != "" {
		if len(newPassword) < minStaffPasswordLength {
			h.renderStaffAccounts(w, r, s, "",
				fmt.Sprintf("Account saved, but the password was not changed: it must be at least %d characters.", minStaffPasswordLength))
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
		if err != nil {
			log.Error().Err(err).Msg("staffui: hash reset credential failed")
			h.renderStaffAccounts(w, r, s, "", "Account saved, but the password reset failed.")
			return
		}
		if err := h.staff.SetStaffPassword(r.Context(), id, string(hash)); err != nil {
			log.Error().Err(err).Int("staff_id", id).Msg("staffui: reset staff credential failed")
			h.renderStaffAccounts(w, r, s, "", "Account saved, but the password reset failed.")
			return
		}
	}

	h.renderStaffAccounts(w, r, s, fmt.Sprintf("Account %q updated.", updated.Username), "")
}

// blockLastOwnerLockout refuses an update that would leave the console with
// no active isp_owner account — deactivating the last one, or demoting them
// to another role, would make the Staff Accounts screen itself (owner-only)
// unreachable by anyone, with no SQL-free way back in.
//
// Computed by re-tallying every account with id's row substituted for its
// post-update state, rather than short-circuiting on "is this edit moving
// *a* role away from owner" — that shortcut would wrongly flag every edit
// to a non-owner account too (a csr being deactivated is not a lockout risk
// at all, since it was never contributing to the owner count).
func (h *Handler) blockLastOwnerLockout(r *http.Request, id int, newRole string, newActive bool) (bool, string) {
	accounts, err := h.staff.ListStaff(r.Context())
	if err != nil {
		// Fails closed: if the current roster can't even be read, refusing
		// the change is safer than risking the last owner over a query error.
		log.Error().Err(err).Msg("staffui: list staff (lockout check) failed")
		return true, "Could not verify this wouldn't remove the last owner account — try again."
	}

	remainingActiveOwners := 0
	for _, a := range accounts {
		role, active := a.Role, a.Active
		if a.ID == id {
			role, active = newRole, newActive
		}
		if role == "isp_owner" && active {
			remainingActiveOwners++
		}
	}
	if remainingActiveOwners == 0 {
		return true, "This would leave no active owner account — deactivate or change its role only after another owner account exists."
	}
	return false, ""
}
