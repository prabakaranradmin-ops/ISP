package reporting

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"
)

// CSV encoders for the FR-RPT-004 reports.
//
// Byte-identical between the synchronous download, the console's own route
// and the scheduled export, because all three call WriteCSV — a scheduled
// report that differs from the one on screen is a support ticket waiting to
// happen.

// day renders a date-only value. Deliberately not the full timestamp: an
// expiry is a date, and printing "2026-12-01T00:00:00Z" in a spreadsheet
// column invites the reader to wonder what happened at midnight.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// optDay renders a nullable timestamp, empty for nil.
func optDay(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// gb renders a byte count in GB to two decimals, because an ISP's allowances
// and tariffs are quoted in GB and nobody reads 1099511627776.
func gb(bytes int64) string {
	if bytes == 0 {
		return "0.00"
	}
	return strconv.FormatFloat(float64(bytes)/(1024*1024*1024), 'f', 2, 64)
}

func writeRows(w io.Writer, what string, header []string, rows [][]string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("reporting: write %s header: %w", what, err)
	}
	for _, r := range rows {
		if err := cw.Write(r); err != nil {
			return fmt.Errorf("reporting: write %s row: %w", what, err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteExpiringCSV streams the expiring-subscribers report.
func WriteExpiringCSV(w io.Writer, rows []ExpiringRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			strconv.Itoa(r.SubscriberID), r.Username, r.MobileNumber, r.PlanName, r.Status,
			optInt(r.FranchiseID), day(r.PlanExpiry), strconv.Itoa(r.DaysLeft),
			r.PlanPrice.StringFixed(2), r.WalletBalance.StringFixed(2), r.AmountDue().StringFixed(2),
		})
	}
	return writeRows(w, "expiring subscribers", []string{
		"subscriber_id", "username", "mobile_number", "plan_name", "status", "franchise_id",
		"plan_expiry", "days_left", "plan_price", "wallet_balance", "amount_due",
	}, out)
}

// WritePlanChangesCSV streams the plan-change-history report.
func WritePlanChangesCSV(w io.Writer, rows []PlanChangeRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			strconv.Itoa(r.SubscriberID), r.Username, r.FromPlan, r.ToPlan,
			day(r.EffectiveOn), r.Status, r.Reason, r.ScheduledBy,
			r.ScheduledAt.UTC().Format(time.RFC3339), optDay(r.AppliedAt),
			optDay(r.CancelledAt), r.CancelledBy,
		})
	}
	return writeRows(w, "plan change history", []string{
		"subscriber_id", "username", "from_plan", "to_plan", "effective_on", "status",
		"reason", "scheduled_by", "scheduled_at", "applied_at", "cancelled_at", "cancelled_by",
	}, out)
}

// WritePaymentsByMethodCSV streams the payments-by-method report.
func WritePaymentsByMethodCSV(w io.Writer, rows []PaymentMethodRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			month(r.Month), r.Method, strconv.Itoa(r.Payments), r.Total.StringFixed(2),
		})
	}
	return writeRows(w, "payments by method",
		[]string{"month", "method", "payments", "total"}, out)
}

// WriteCollectionByStaffCSV streams the collection-by-staff report.
func WriteCollectionByStaffCSV(w io.Writer, rows []StaffCollectionRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			month(r.Month), r.RecordedBy, strconv.Itoa(r.Payments),
			r.CashTotal.StringFixed(2), r.DigitalTotal.StringFixed(2), r.Total.StringFixed(2),
		})
	}
	return writeRows(w, "collection by staff", []string{
		"month", "recorded_by", "payments", "cash_total", "digital_total", "total",
	}, out)
}

// WriteInvoiceRegisterCSV streams the invoice register.
func WriteInvoiceRegisterCSV(w io.Writer, rows []InvoiceRegisterRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			strconv.Itoa(r.InvoiceID), day(r.InvoiceDate), strconv.Itoa(r.SubscriberID),
			r.Username, r.PlanName, optInt(r.FranchiseID), r.Description,
			r.BaseAmount.StringFixed(2), r.CGST.StringFixed(2), r.SGST.StringFixed(2),
			r.IGST.StringFixed(2), r.Total.StringFixed(2),
		})
	}
	return writeRows(w, "invoice register", []string{
		"invoice_id", "invoice_date", "subscriber_id", "username", "plan_name", "franchise_id",
		"description", "base_amount", "cgst_amount", "sgst_amount", "igst_amount", "total_amount",
	}, out)
}

// WriteTaxChargedCSV streams the tax-charged report.
func WriteTaxChargedCSV(w io.Writer, rows []TaxRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			month(r.Month), r.CGSTRate.StringFixed(2), r.SGSTRate.StringFixed(2),
			r.IGSTRate.StringFixed(2), strconv.Itoa(r.Invoices),
			r.TaxableValue.StringFixed(2), r.CGST.StringFixed(2), r.SGST.StringFixed(2),
			r.IGST.StringFixed(2), r.TaxTotal.StringFixed(2),
		})
	}
	return writeRows(w, "tax charged", []string{
		"month", "cgst_rate", "sgst_rate", "igst_rate", "invoices",
		"taxable_value", "cgst_amount", "sgst_amount", "igst_amount", "tax_total",
	}, out)
}

// WriteTurnaroundCSV streams the ticket-turnaround report.
func WriteTurnaroundCSV(w io.Writer, rows []TurnaroundRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			month(r.Month), r.Category, r.Priority,
			strconv.Itoa(r.Raised), strconv.Itoa(r.Resolved),
			optFloat(r.MedianHours), optFloat(r.P90Hours), optFloat(r.MaxHours),
			strconv.Itoa(r.StillOpen),
		})
	}
	return writeRows(w, "ticket turnaround", []string{
		"month", "category", "priority", "raised", "resolved",
		"median_resolution_hours", "p90_resolution_hours", "max_resolution_hours", "still_open",
	}, out)
}

// WriteTopDataUsersCSV streams the top-data-users report.
func WriteTopDataUsersCSV(w io.Writer, rows []DataUserRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		// Bytes and GB both: bytes so the column can be summed or sorted
		// exactly, GB so it can be read.
		out = append(out, []string{
			strconv.Itoa(r.SubscriberID), r.Username, r.PlanName, optInt(r.FranchiseID),
			strconv.Itoa(r.Sessions),
			strconv.FormatInt(r.InputOctets, 10), strconv.FormatInt(r.OutputOctets, 10),
			strconv.FormatInt(r.TotalBytes, 10), gb(r.TotalBytes),
			strconv.FormatInt(r.AllowanceBytes, 10), gb(r.AllowanceBytes),
		})
	}
	return writeRows(w, "top data users", []string{
		"subscriber_id", "username", "plan_name", "franchise_id", "sessions",
		"input_octets", "output_octets", "total_bytes", "total_gb",
		"allowance_bytes", "allowance_gb",
	}, out)
}

// WriteFUPExceededCSV streams the over-fair-use report.
func WriteFUPExceededCSV(w io.Writer, rows []FUPExceededRow) error {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			strconv.Itoa(r.SubscriberID), r.Username, r.PlanName, optInt(r.FranchiseID),
			strconv.FormatInt(r.UsedBytes, 10), gb(r.UsedBytes),
			strconv.FormatInt(r.AllowanceBytes, 10), gb(r.AllowanceBytes),
			strconv.FormatInt(r.TopupBytes, 10), gb(r.TopupBytes),
			optFloat(r.OveragePct()), r.AppliedRate, strconv.FormatBool(r.FUPActive),
		})
	}
	return writeRows(w, "over fair-use", []string{
		"subscriber_id", "username", "plan_name", "franchise_id",
		"used_bytes", "used_gb", "allowance_bytes", "allowance_gb",
		"topup_bytes", "topup_gb", "overage_pct", "applied_rate", "fup_active",
	}, out)
}
