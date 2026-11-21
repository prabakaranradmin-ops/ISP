package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Named subscriber groups — FR-GRP-001.
//
// GroupManager is satisfied by *api.Handler, so the console validates and
// audits exactly as the API does rather than reaching the store directly.
type GroupManager interface {
	CreateSubscriberGroup(ctx context.Context, in revenue.SubscriberGroup) (*revenue.SubscriberGroup, error)
	RenameSubscriberGroup(ctx context.Context, id int, in revenue.SubscriberGroup) error
	SetSubscriberGroupActive(ctx context.Context, id int, active bool) error
	DeleteSubscriberGroup(ctx context.Context, id int) error
	ListSubscriberGroups(ctx context.Context, includeRetired bool) ([]revenue.SubscriberGroup, error)
	AddToGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error)
	RemoveFromGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error)
}

// groupMessages words each outcome carried back in the URL.
var groupMessages = map[string]string{
	"created":  "Group added. Put subscribers in it from the directory, or paste their ids below.",
	"renamed":  "Group saved.",
	"retired":  "Group retired. It is out of the filters and the announcement dropdown; its members and any announcement already addressed to it are unchanged.",
	"restored": "Group restored and offered again.",
	"deleted":  "Group deleted, along with its memberships. The subscribers themselves are untouched.",

	"duplicate": "Not saved: another active group already has that name.",
	"inuse":     "Not deleted: an announcement is addressed to this group. Retire it instead — deleting it would change who that announcement reaches.",
	"missing":   "No group with that id.",
	"failed":    "That did not go through. Nothing was changed; try again.",
}

var groupSuccess = map[string]bool{
	"created": true, "renamed": true, "retired": true, "restored": true, "deleted": true,
	"members": true,
}

type groupData struct {
	Groups         []revenue.SubscriberGroup
	IncludeRetired bool
	CanManage      bool
}

// canManageGroups: wider than the catalogue screens, deliberately.
//
// A group is a label an operator puts on their own customers — a locality, an
// apartment block, the twenty on one feeder cable — not a commercial or a
// network decision. The front desk is who knows which customers belong
// together, and the NOC needs to name the set behind a fault. Nothing about a
// group changes what anybody is billed or what speed they get; what it does
// change is who an announcement reaches, which is why every change here is
// audited.
func canManageGroups(role string) bool {
	switch role {
	case "isp_owner", "billing_admin", "csr", "noc_engineer":
		return true
	}
	return false
}

func (h *Handler) groupGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "groups")
	if !ok {
		return s, false
	}
	if h.groups == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Subscriber groups are not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) requireGroupManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.groupGate(w, r)
	if !ok {
		return s, false
	}
	if !canManageGroups(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Your role cannot change subscriber groups.")
		return s, false
	}
	return s, true
}

// Groups handles GET /staff/groups.
func (h *Handler) Groups(w http.ResponseWriter, r *http.Request) {
	s, ok := h.groupGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Subscriber groups", "groups")
	q := r.URL.Query()

	includeRetired := q.Get("retired") == "true"
	groups, err := h.groups.ListSubscriberGroups(r.Context(), includeRetired)
	if err != nil {
		h.renderError(w, r, s, http.StatusInternalServerError,
			"Could not read the subscriber groups.")
		return
	}

	if code := q.Get("m"); code != "" {
		if m, ok := groupMessages[code]; ok {
			if groupSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	// A validation sentence, or a membership count, is specific to what the
	// operator did, so it is carried verbatim rather than mapped to a code.
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}
	if note := q.Get("note"); note != "" {
		d.Message = note
	}

	d.Data = groupData{
		Groups:         groups,
		IncludeRetired: includeRetired,
		CanManage:      canManageGroups(s.Role),
	}
	h.render(w, "groups", d)
}

// CreateGroupForm handles POST /staff/groups.
func (h *Handler) CreateGroupForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireGroupManager(w, r)
	if !ok {
		return
	}
	in, err := groupFromForm(r)
	if err != nil {
		h.redirectGroups(w, r, "", err.Error(), "")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.groups.CreateSubscriberGroup(ctx, in); err != nil {
		h.groupFailure(w, r, err)
		return
	}
	h.redirectGroups(w, r, "created", "", "")
}

// RenameGroupForm handles POST /staff/groups/{id}.
func (h *Handler) RenameGroupForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireGroupManager(w, r)
	if !ok {
		return
	}
	id, ok := h.groupID(w, r, s)
	if !ok {
		return
	}
	in, err := groupFromForm(r)
	if err != nil {
		h.redirectGroups(w, r, "", err.Error(), "")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.groups.RenameSubscriberGroup(ctx, id, in); err != nil {
		h.groupFailure(w, r, err)
		return
	}
	h.redirectGroups(w, r, "renamed", "", "")
}

// SetGroupActiveForm handles POST /staff/groups/{id}/active.
func (h *Handler) SetGroupActiveForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireGroupManager(w, r)
	if !ok {
		return
	}
	id, ok := h.groupID(w, r, s)
	if !ok {
		return
	}
	active := r.PostFormValue("active") == "true"
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.groups.SetSubscriberGroupActive(ctx, id, active); err != nil {
		h.groupFailure(w, r, err)
		return
	}
	if active {
		h.redirectGroups(w, r, "restored", "", "")
		return
	}
	h.redirectGroups(w, r, "retired", "", "")
}

// DeleteGroupForm handles POST /staff/groups/{id}/delete.
func (h *Handler) DeleteGroupForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireGroupManager(w, r)
	if !ok {
		return
	}
	id, ok := h.groupID(w, r, s)
	if !ok {
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.groups.DeleteSubscriberGroup(ctx, id); err != nil {
		h.groupFailure(w, r, err)
		return
	}
	h.redirectGroups(w, r, "deleted", "", "")
}

// GroupMembersForm handles POST /staff/groups/{id}/members.
//
// Takes a pasted list of subscriber ids, because that is how the lists
// operators already have arrive: out of a spreadsheet, a survey, or a
// WhatsApp message from a field technician. Adding from the directory's
// checkboxes is the other route and both end here.
func (h *Handler) GroupMembersForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireGroupManager(w, r)
	if !ok {
		return
	}
	id, ok := h.groupID(w, r, s)
	if !ok {
		return
	}
	ids, err := parseSubscriberIDList(r.PostFormValue("subscriber_ids"))
	if err != nil {
		h.redirectGroups(w, r, "", err.Error(), "")
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	remove := r.PostFormValue("action") == "remove"

	var (
		n    int
		verb string
	)
	if remove {
		n, err = h.groups.RemoveFromGroup(ctx, id, ids)
		verb = "removed from"
	} else {
		n, err = h.groups.AddToGroup(ctx, id, ids)
		verb = "added to"
	}
	if err != nil {
		h.groupFailure(w, r, err)
		return
	}

	// The count is reported rather than assumed: adding is idempotent and
	// skips ids that do not exist, so "20 pasted, 17 added" is the honest
	// answer and the three missing are what the operator needs to chase.
	note := fmt.Sprintf("%d of %d %s the group.", n, len(ids), verb)
	if n < len(ids) && !remove {
		note += " The rest were already in it, or are not subscriber ids."
	}
	if n < len(ids) && remove {
		note += " The rest were not in it."
	}
	h.redirectGroups(w, r, "", "", note)
}

func (h *Handler) groupID(w http.ResponseWriter, r *http.Request, s Session) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid group id.")
		return 0, false
	}
	return id, true
}

// groupFromForm reads a group out of the posted form.
func groupFromForm(r *http.Request) (revenue.SubscriberGroup, error) {
	in := revenue.SubscriberGroup{
		Name:        r.PostFormValue("name"),
		Description: r.PostFormValue("description"),
	}
	// Normalise and validate here as well as in the store, so the operator
	// gets the sentence rather than a generic failure.
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return in, err
	}
	return in, nil
}

// parseSubscriberIDList reads a pasted list of subscriber ids.
//
// Separated on commas, whitespace and newlines alike: the list arrives
// pasted out of a spreadsheet column (newlines), a message (commas, spaces)
// or both, and rejecting one of those forms would just mean the operator
// reformats it by hand.
func parseSubscriberIDList(raw string) ([]int, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	if len(fields) == 0 {
		return nil, errors.New("paste at least one subscriber id")
	}
	// A list long enough to be a mistake rather than an intention. A paste of
	// a whole exported column would otherwise become one audit entry naming
	// thousands of people.
	const maxIDs = 2000
	if len(fields) > maxIDs {
		return nil, fmt.Errorf("that is %d ids; add at most %d at a time", len(fields), maxIDs)
	}

	seen := make(map[int]bool, len(fields))
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		id, err := strconv.Atoi(f)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a subscriber id", f)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

func (h *Handler) redirectGroups(w http.ResponseWriter, r *http.Request, code, problem, note string) {
	url := "/staff/groups"
	switch {
	case problem != "":
		url += "?err=" + neturl.QueryEscape(problem)
	case note != "":
		url += "?note=" + neturl.QueryEscape(note)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// groupFailure maps a store error to one of the worded outcomes.
func (h *Handler) groupFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, revenue.ErrGroupNameTaken):
		h.redirectGroups(w, r, "duplicate", "", "")
	case errors.Is(err, revenue.ErrGroupInUse):
		h.redirectGroups(w, r, "inuse", "", "")
	case strings.Contains(err.Error(), "not found"):
		h.redirectGroups(w, r, "missing", "", "")
	default:
		// Anything else is unexpected: groupFromForm already ran the same
		// Normalise/Validate the store runs, so a validation message cannot
		// reach here. Showing the raw error would leak storage detail to an
		// operator who can do nothing with it.
		h.redirectGroups(w, r, "failed", "", "")
	}
}

// atoiOrZero reads an optional numeric query parameter.
//
// Zero for anything unparseable, which every filter that uses it reads as
// "not set". A hand-edited or truncated URL then shows the unfiltered list
// rather than an error page, and the caller reports the id it could not
// honour where it matters.
func atoiOrZero(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 0 {
		return 0
	}
	return v
}
