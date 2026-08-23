package staffui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// Staff account management (owner-only) — closes the gap where creating a
// staff account, changing a password, or deactivating someone required a
// direct SQL statement against staff_users.

// staffRoles are the roles this screen can assign. The franchise-scoped
// roles staff_users.chk_staff_role also permits (lco, franchise_admin,
// franchise_staff — migration 024) are deliberately excluded: those need a
// franchise_id binding this screen has no UI to collect, and the console's
// own role model (AllowedSections) does not recognise them either.
var staffRoles = []string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}

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
	Accounts []StaffAccount
	Roles    []string
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

	d.Data = accountsData{Accounts: accounts, Roles: staffRoles}
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
	if !isStaffRole(role) {
		h.renderStaffAccounts(w, r, s, "", "Role must be one of: "+strings.Join(staffRoles, ", ")+".")
		return
	}
	if len(password) < minStaffPasswordLength {
		h.renderStaffAccounts(w, r, s, "",
			fmt.Sprintf("Password must be at least %d characters.", minStaffPasswordLength))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		log.Error().Err(err).Msg("staffui: hash new staff credential failed")
		h.renderStaffAccounts(w, r, s, "", "Could not create that account.")
		return
	}

	created, err := h.staff.CreateStaff(r.Context(), username, fullName, string(hash), role, leaAccess)
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
	if !isStaffRole(role) {
		h.renderStaffAccounts(w, r, s, "", "Role must be one of: "+strings.Join(staffRoles, ", ")+".")
		return
	}
	leaAccess := r.PostFormValue("lea_access") == "on"
	active := r.PostFormValue("active") == "on"

	if blocked, msg := h.blockLastOwnerLockout(r, id, role, active); blocked {
		h.renderStaffAccounts(w, r, s, "", msg)
		return
	}

	updated, err := h.staff.UpdateStaff(r.Context(), id, &role, &leaAccess, &active)
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
