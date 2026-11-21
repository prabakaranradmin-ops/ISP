// Package portalui implements the server-rendered subscriber self-service
// portal (MOD-PORTAL, MDS §4.9): Dashboard, Usage history, Invoices &
// payments, Renew plan, Support tickets, and Notification history — the
// browsable HTML frontend for the JSON API in internal/portal.
//
// It shares the subscriber-scoped JWT with the JSON API. Browsers carry it
// in an HttpOnly cookie (Path=/ui) instead of an Authorization header,
// bridged to the existing middleware.JWTMiddleware by cookieBridge (see
// auth.go for why this is a bridge in front of that middleware rather than a
// change to it).
package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

// Deps are portalui.Handler's dependencies. Fields are added incrementally
// as each page's phase is built, rather than pre-declaring the full set
// MOD-PORTAL will eventually need.
//
// Invoices and PDF reuse internal/api's own LedgerQuerier/InvoiceQuerier/
// PDFGenerator interfaces directly rather than declaring parallel ones here:
// *db.BillingStore already satisfies them, and internal/api never imports
// internal/portal or internal/portalui, so this import creates no cycle.
type Deps struct {
	Subscribers    portal.PortalSubscriberQuerier
	Sessions       portal.PortalSessionQuerier
	SessionHistory portal.PortalSessionHistoryQuerier
	Invoices       api.InvoiceQuerier
	PDF            api.PDFGenerator
	// Payments, ReceiptPDF and Supplier give subscribers their own payment
	// receipts (FR-BIL-021).
	Payments      PaymentLister
	ReceiptPDF    api.ReceiptPDFGenerator
	Supplier      billing.Supplier
	Razorpay      portal.RazorpayOrderCreator
	Tickets       portal.PortalTicketQuerier
	Notifications portal.PortalNotificationQuerier
	JWTSecret     string
}

// Handler is the portal UI's HTTP handler.
type Handler struct {
	subscribers    portal.PortalSubscriberQuerier
	sessions       portal.PortalSessionQuerier
	sessionHistory portal.PortalSessionHistoryQuerier
	invoices       api.InvoiceQuerier
	pdfGen         api.PDFGenerator
	payments       PaymentLister
	receiptPDF     api.ReceiptPDFGenerator
	supplier       billing.Supplier
	razorpay       portal.RazorpayOrderCreator
	tickets        portal.PortalTicketQuerier
	notifications  portal.PortalNotificationQuerier
	jwtSecret      string
}

// NewHandler constructs the portal UI Handler.
func NewHandler(deps Deps) *Handler {
	return &Handler{
		subscribers:    deps.Subscribers,
		sessions:       deps.Sessions,
		sessionHistory: deps.SessionHistory,
		invoices:       deps.Invoices,
		pdfGen:         deps.PDF,
		payments:       deps.Payments,
		receiptPDF:     deps.ReceiptPDF,
		supplier:       deps.Supplier,
		razorpay:       deps.Razorpay,
		tickets:        deps.Tickets,
		notifications:  deps.Notifications,
		jwtSecret:      deps.JWTSecret,
	}
}

// RegisterRoutes wires all portal UI routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/login", h.LoginPage)
	mux.HandleFunc("POST /ui/login", h.Login)
	mux.HandleFunc("POST /ui/logout", h.Logout)
	mux.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
	})

	auth := middleware.JWTMiddleware(h.jwtSecret)
	self := func(next http.HandlerFunc) http.Handler {
		return cookieBridge(auth(middleware.RequireRole("subscriber")(next)))
	}
	mux.Handle("GET /ui/dashboard", redirectToLoginOn401(self(h.Dashboard)))
	mux.Handle("GET /ui/dashboard/session", self(h.DashboardSessionFragment))
	mux.Handle("GET /ui/usage", redirectToLoginOn401(self(h.Usage)))
	mux.Handle("GET /ui/invoices", redirectToLoginOn401(self(h.Invoices)))
	mux.Handle("GET /ui/invoices/{id}/pdf", self(h.InvoicePDF))
	mux.Handle("GET /ui/payments/{id}/receipt", self(h.ReceiptPDF))
	mux.Handle("GET /ui/renew", redirectToLoginOn401(self(h.RenewPage)))
	mux.Handle("POST /ui/renew", self(h.requireCSRF(h.Renew)))
	mux.Handle("GET /ui/tickets", redirectToLoginOn401(self(h.Tickets)))
	mux.Handle("POST /ui/tickets", self(h.requireCSRF(h.CreateTicket)))
	mux.Handle("POST /ui/tickets/{id}/rating", self(h.requireCSRF(h.RateTicket)))
	mux.Handle("GET /ui/notifications", redirectToLoginOn401(self(h.Notifications)))

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(staticFS)))
}
