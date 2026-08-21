// Package billing — invoice PDF generation via a local headless browser.
//
// FR: FR-BIL-007 | DDS §5.6 | MDS §4.3
//
// This rendered invoices by POSTing HTML to a Gotenberg container's
// /forms/chromium/convert/html endpoint, which ran its own bundled Chromium
// inside Docker. The native Windows install has nowhere to run that
// container, so GeneratePDF now drives a Chromium-based browser already on
// the machine directly over the Chrome DevTools Protocol (see pkg/chromium
// for how that browser is found — normally Microsoft Edge, which ships with
// Windows itself).
package billing

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/shopspring/decimal"
)

// InvoiceData carries all fields rendered into the PDF template.
type InvoiceData struct {
	InvoiceNumber   string
	InvoiceDate     time.Time
	DueDate         time.Time
	SubscriberName  string
	MobileNumber    string
	RegisteredState string
	PlanName        string
	PlanPeriod      string // e.g. "June 2025"

	// Billing amounts
	BaseAmount  decimal.Decimal
	CGSTRate    decimal.Decimal
	CGSTAmount  decimal.Decimal
	SGSTRate    decimal.Decimal
	SGSTAmount  decimal.Decimal
	IGSTRate    decimal.Decimal
	IGSTAmount  decimal.Decimal
	TotalAmount decimal.Decimal

	// Usage summary block (FR-BIL-007 plain-language requirement)
	GBUsed      decimal.Decimal
	GBIncluded  decimal.Decimal
	SpeedActive string // e.g. "100 Mbps / 100 Mbps"
	FUPApplied  bool
}

// invoicePDFTimeout bounds one GeneratePDF call: launching the browser,
// loading the invoice HTML and printing it to PDF. 30s matches the timeout
// the old Gotenberg HTTP client used for the same round trip.
const invoicePDFTimeout = 30 * time.Second

// InvoicePDFClient generates GST-compliant PDF invoices by driving a local
// Chromium-based browser over the Chrome DevTools Protocol.
type InvoicePDFClient struct {
	execPath string
}

// NewInvoicePDFClient constructs an InvoicePDFClient that drives the
// browser at execPath — see pkg/chromium.Locate for how the caller finds
// one.
func NewInvoicePDFClient(execPath string) *InvoicePDFClient {
	return &InvoicePDFClient{execPath: execPath}
}

// GeneratePDF renders the invoice HTML template and prints it to PDF using a
// short-lived headless browser instance, returning the raw PDF bytes.
//
// A fresh browser process per call, not a pooled or long-lived one: invoice
// PDFs are generated at most a handful of times a minute even on the largest
// deployment this system targets, and a process that exits the moment
// GeneratePDF returns cannot accumulate the tab and memory growth a
// long-running headless Chrome is prone to under sustained use — nothing
// here calls this often enough for pooling to be worth its complexity.
//
// FR: FR-BIL-007 | DDS §5.6
func (c *InvoicePDFClient) GeneratePDF(ctx context.Context, data InvoiceData) ([]byte, error) {
	html, err := renderInvoiceHTML(data)
	if err != nil {
		return nil, fmt.Errorf("billing: render invoice HTML: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, invoicePDFTimeout)
	defer cancel()

	// The browser is launched here and connected to by URL, rather than
	// handed to chromedp.NewExecAllocator to launch itself.
	//
	// NewExecAllocator decides the browser is ready by reading its stderr
	// until a "DevTools listening on ws://..." line appears. Microsoft Edge
	// only writes that line when it has a console attached: launched from a
	// Go process (no console, stderr on a pipe) it prints nothing at all,
	// the pipe closes, and chromedp reports "chrome failed to start:" with
	// no further detail while the browser is in fact running perfectly.
	// Confirmed by reproducing it outside chromedp entirely - a bare
	// os/exec launch reading StderrPipe gets zero bytes, while the same
	// flags under a console print the line - and by watching the browser
	// write a valid DevToolsActivePort within half a second of the launch
	// chromedp had already given up on.
	//
	// launchBrowser waits on that file instead, which the browser writes on
	// every platform whether or not anyone is listening to stderr.
	wsURL, stop, err := launchBrowser(ctx, c.execPath)
	if err != nil {
		return nil, fmt.Errorf("billing: start browser: %w", err)
	}
	defer stop()

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, wsURL)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	var pdf []byte
	err = chromedp.Run(browserCtx,
		chromedp.Navigate("about:blank"),
		// The invoice HTML is handed to the browser directly as a document
		// rather than served from a URL: it exists only as a Go string, and
		// this is the one already-loaded blank page's content being
		// replaced — no temp file, no HTTP server, no data: URL length
		// limit to worry about.
		chromedp.ActionFunc(func(ctx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return fmt.Errorf("get frame tree: %w", err)
			}
			return page.SetDocumentContent(frameTree.Frame.ID, html).Do(ctx)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			buf, _, printErr := page.PrintToPDF().WithPrintBackground(true).Do(ctx)
			if printErr != nil {
				return fmt.Errorf("print to PDF: %w", printErr)
			}
			pdf = buf
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("billing: render PDF: %w", err)
	}
	return pdf, nil
}

// launchBrowser starts a headless browser and returns the DevTools
// WebSocket URL to drive it through, plus a stop function that kills the
// process and removes its profile directory.
//
// Readiness is taken from the DevToolsActivePort file the browser writes
// into its own profile directory - two lines, the port and the browser's
// WebSocket path - rather than from its stderr. See GeneratePDF's own note
// for why stderr is not dependable here.
func launchBrowser(ctx context.Context, execPath string) (wsURL string, stop func(), err error) {
	// A fresh profile per launch. It doubles as the location of the
	// DevToolsActivePort file read below, so it must be unique or two
	// concurrent renders would read each other's port.
	userDataDir, err := os.MkdirTemp("", "isp-invoice-pdf-")
	if err != nil {
		return "", nil, fmt.Errorf("create browser profile dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(userDataDir) }

	// Port 0 asks the browser to pick a free one and report it back in
	// DevToolsActivePort, which avoids racing another process for a fixed
	// port. The rest mirror chromedp's own defaults for a headless render:
	// no first-run UI, no background chatter, no GPU.
	cmd := exec.CommandContext(ctx, execPath,
		"--headless",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-dev-shm-usage",
		"--mute-audio",
		"--remote-debugging-port=0",
		"--user-data-dir="+userDataDir,
	)
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("start %s: %w", execPath, err)
	}

	stop = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		cleanup()
	}

	portFile := filepath.Join(userDataDir, "DevToolsActivePort")
	for {
		if body, readErr := os.ReadFile(portFile); readErr == nil {
			// Two lines: the port, then the browser's own WebSocket path.
			// Anything else means the browser is still mid-write, so keep
			// waiting rather than parsing a half-written file.
			if lines := strings.SplitN(strings.TrimSpace(string(body)), "\n", 2); len(lines) == 2 {
				return fmt.Sprintf("ws://127.0.0.1:%s%s",
					strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])), stop, nil
			}
		}
		select {
		case <-ctx.Done():
			stop()
			return "", nil, fmt.Errorf("browser did not report a DevTools port: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// invoiceHTMLTemplate is the GST-compliant plain-language invoice layout.
//
// html/template, not text/template: SubscriberName and the other fields
// below come from subscriber-supplied KYC data, and this HTML is now
// executed by a real browser (WithPrintBackground included) rather than
// just handed to a converter — html/template's context-aware escaping is
// what keeps a subscriber name containing "<" or "&" from being interpreted
// as markup instead of rendered as text.
var invoiceHTMLTemplate = template.Must(template.New("invoice").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  body { font-family: Arial, sans-serif; font-size: 12px; margin: 40px; color: #222; }
  h1   { font-size: 20px; }
  table { border-collapse: collapse; width: 100%; }
  th, td { border: 1px solid #ccc; padding: 6px 10px; text-align: left; }
  th { background: #f5f5f5; }
  .total td { font-weight: bold; background: #eef; }
  .usage-box { background: #f0f8ff; border: 1px solid #90caf9; border-radius: 6px;
               padding: 12px 16px; margin: 24px 0; }
  .usage-box h3 { margin: 0 0 8px; font-size: 13px; color: #1565c0; }
  .usage-row { display: flex; justify-content: space-between; margin: 4px 0; }
  .footer { margin-top: 24px; font-size: 10px; color: #666; }
</style>
</head>
<body>
  <h1>GST Tax Invoice</h1>
  <p><strong>Invoice Number:</strong> {{.InvoiceNumber}} &nbsp;&nbsp;
     <strong>Date:</strong> {{.InvoiceDate.Format "02 Jan 2006"}} &nbsp;&nbsp;
     <strong>Due Date:</strong> {{.DueDate.Format "02 Jan 2006"}}</p>

  <table style="margin-bottom:16px">
    <tr><th>Subscriber Name</th><td>{{.SubscriberName}}</td>
        <th>Mobile</th><td>{{.MobileNumber}}</td></tr>
    <tr><th>Plan</th><td>{{.PlanName}}</td>
        <th>Period</th><td>{{.PlanPeriod}}</td></tr>
    <tr><th>State (for GST)</th><td colspan="3">{{.RegisteredState}}</td></tr>
  </table>

  <table>
    <tr><th>Description</th><th>Amount (₹)</th></tr>
    <tr><td>Internet Plan — {{.PlanName}}</td><td>{{.BaseAmount.StringFixed 2}}</td></tr>
    {{if gt .CGSTRate.Sign 0}}
    <tr><td>CGST @ {{.CGSTRate}}%</td><td>{{.CGSTAmount.StringFixed 2}}</td></tr>
    <tr><td>SGST @ {{.SGSTRate}}%</td><td>{{.SGSTAmount.StringFixed 2}}</td></tr>
    {{else}}
    <tr><td>IGST @ {{.IGSTRate}}%</td><td>{{.IGSTAmount.StringFixed 2}}</td></tr>
    {{end}}
    <tr class="total"><td>Total</td><td>₹{{.TotalAmount.StringFixed 2}}</td></tr>
  </table>

  <!-- Usage Summary Block — FR-BIL-007 plain-language requirement -->
  <div class="usage-box">
    <h3>Data Usage Summary</h3>
    <div class="usage-row">
      <span>Data used this cycle:</span>
      <strong>{{.GBUsed.StringFixed 0}} GB of {{.GBIncluded.StringFixed 0}} GB included</strong>
    </div>
    <div class="usage-row">
      <span>Speed applied:</span>
      {{if .FUPApplied}}
      <strong style="color:#c62828">FUP throttle active — {{.SpeedActive}}</strong>
      {{else}}
      <strong style="color:#2e7d32">{{.SpeedActive}} (full speed)</strong>
      {{end}}
    </div>
  </div>

  <p class="footer">
    This is a computer-generated invoice. For queries, contact support.<br>
    HSN/SAC: 998432 (Internet Telecommunication Services) | GST applies under the reverse-charge mechanism for B2B.
  </p>
</body>
</html>
`))

func renderInvoiceHTML(data InvoiceData) (string, error) {
	var buf bytes.Buffer
	if err := invoiceHTMLTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
