# Document 5: Detailed Design Specification (DDS)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** DDS
**Traces From:** [MDS](04_MDS_Module_Design.md) → [SRS](02_SRS_System_Requirements.md)
**Traces To:** [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 5.1 RADIUS Worker Pool (MOD-AAA)
**FR:** FR-AAA-001..004 | **NFR:** NFR-PERF-001, NFR-SCAL-001

Fixed 128-worker pool prevents unconstrained goroutine creation during authentication storms.

```go
// internal/radius/daemon.go
func (d *RadiusDaemon) Start() error {
    udpAddr, _ := net.ResolveUDPAddr("udp", d.addr)
    conn, _ := net.ListenUDP("udp", udpAddr)
    for i := 0; i < 128; i++ {
        go d.workerPoolConsumer()
    }
    server := radius.PacketServer{
        Addr:         d.addr,
        SecretSource: radius.StaticSecretSource(d.secret),
        Handler: radius.HandlerFunc(func(w radius.ResponseWriter, r *radius.Request) {
            d.packetQueue <- r
        }),
    }
    return server.Serve(conn)
}
```

Each worker records latency on every request:
```go
start := time.Now()
d.handleRequest(w, r)
radiusAuthDuration.Observe(time.Since(start).Seconds())
```

---

## 5.2 Accounting Deduplication (MOD-AAA)
**FR:** FR-AAA-003

Prevents artificial FUP breaches from MikroTik packet retransmissions.

```go
// internal/radius/handlers.go
dedupKey := "acct_dedup:" + acctSessionID + ":" + strconv.FormatUint(uint64(inputOctets), 10)
isNew, err := d.pools.Redis.SetNX(ctx, dedupKey, "1", 30*time.Second).Result()
if err != nil || !isNew {
    r.ResponseWriter.Write(r.Response(radius.CodeAccountingResponse))
    radiusDedupSkipped.Inc()
    return
}
```

---

## 5.3 CoA/PoD Reliability & Dead-Letter Monitor (MOD-FUP)
**FR:** FR-FUP-002..003

```go
// internal/tasks/coa_task.go
func SendReliableCoA(ctx context.Context, nasIP string, packet *radius.Packet) error {
    conn, _ := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(nasIP), Port: 1700})
    defer conn.Close()
    conn.SetDeadline(time.Now().Add(3 * time.Second))
    conn.Write(encodedPacket)
    buf := make([]byte, 4096)
    n, err := conn.Read(buf)
    if err != nil {
        return fmt.Errorf("await CoA-ACK: %w", err) // Asynq retries on non-nil
    }
    response, _ := radius.ParsePacket(buf[:n], d.secret)
    if response.Code != radius.CodeCoAACK {
        return fmt.Errorf("CoA-NAK from %s", nasIP)
    }
    coaAckTotal.WithLabelValues("ack").Inc()
    return nil
}
```

Dead-letter monitor (polls every 30s, alerts if depth > 0):
```go
// internal/tasks/deadletter_monitor.go
func (m *Monitor) Run(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        inspector := asynq.NewInspector(m.redisOpt)
        info, _ := inspector.GetQueueInfo("network_commands")
        deadLetterQueueDepth.Set(float64(info.Dead))
        if info.Dead > 0 {
            m.alerter.Trigger("dead_letter_queue_non_empty", info.Dead)
        }
    }
}
```

---

## 5.4 GST Computation & Banker's Rounding (MOD-BIL)
**FR:** FR-BIL-001..002

```go
// internal/billing/gst.go
func CalculateGstInvoice(baseAmount decimal.Decimal, state string, rate db.GstRate) db.Invoice {
    var cgst, sgst, igst decimal.Decimal
    if state == "TN" {
        cgst = baseAmount.Mul(rate.CgstRate).Div(decimal.NewFromInt(100)).Round(2)
        sgst = baseAmount.Mul(rate.SgstRate).Div(decimal.NewFromInt(100)).Round(2)
        igst = decimal.Zero
    } else {
        igst = baseAmount.Mul(rate.IgstRate).Div(decimal.NewFromInt(100)).Round(2)
        cgst, sgst = decimal.Zero, decimal.Zero
    }
    total := baseAmount.Add(cgst).Add(sgst).Add(igst)
    return db.Invoice{BaseAmount: baseAmount, CgstAmount: cgst, SgstAmount: sgst, IgstAmount: igst, TotalAmount: total}
}
```

---

## 5.5 AES-GCM-256 PII Encryption with Key Versioning (MOD-AUTH)
**FR:** FR-SEC-002..003 | **CRD:** CRD-REG-002

Ciphertext format: `{version_id}:{base64(nonce+ciphertext)}` — e.g. `v3:Zm9vYmFy...`

```go
// pkg/crypto/encryption.go
func (e *AESEncryptor) Encrypt(plaintext string) (string, error) {
    block, _ := aes.NewCipher(e.key)
    gcm, _ := cipher.NewGCM(block)
    nonce := make([]byte, gcm.NonceSize())
    io.ReadFull(rand.Reader, nonce)
    ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
    return e.keyVersion + ":" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

func Decrypt(versionedCiphertext string, keyStore KeyStore) (string, error) {
    parts := strings.SplitN(versionedCiphertext, ":", 2)
    key, _ := keyStore.GetKey(parts[0])
    raw, _ := base64.StdEncoding.DecodeString(parts[1])
    block, _ := aes.NewCipher(key)
    gcm, _ := cipher.NewGCM(block)
    nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
    plaintext, _ := gcm.Open(nil, nonce, ct, nil)
    return string(plaintext), nil
}
```

---

## 5.6 Webhook HMAC Validation & Wallet Idempotency (MOD-BIL)
**FR:** FR-SEC-004, FR-BIL-005 | **CRD:** CRD-PAY-001

```go
// internal/billing/webhook.go
func ValidateRazorpaySignature(payload []byte, sig, secret string) error {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(payload)
    if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
        webhookHMACFailures.WithLabelValues("razorpay").Inc()
        return fmt.Errorf("invalid webhook signature")
    }
    return nil
}
```

Wallet idempotency (return original tx on duplicate token):
```go
// internal/billing/wallet.go
func (s *WalletService) Recharge(ctx context.Context, req RechargeRequest) (*Transaction, error) {
    existing, err := s.db.GetTransactionByToken(ctx, req.TransactionToken)
    if err == nil { return existing, nil } // idempotent return
    // ... execute double-entry transaction
}
```

---

## 5.7 JWT Middleware & RBAC (MOD-AUTH)
**FR:** FR-SEC-005

```go
// internal/middleware/auth.go
func RequireRole(roles ...string) func(http.Handler) http.Handler {
    allowed := make(map[string]bool)
    for _, r := range roles { allowed[r] = true }
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            role, _ := r.Context().Value(ctxKeyRole).(string)
            if !allowed[role] { http.Error(w, "forbidden", http.StatusForbidden); return }
            // Emit audit log entry
            auditLog(r, role)
            next.ServeHTTP(w, r)
        })
    }
}
```

---

## 5.8 WhatsApp Business API Notification Dispatcher *(new — MOD-NOTIF)*
**FR:** FR-NOTIF-001..011 | **CRD:** CRD-NOTIF-001..002

`internal/notifications/whatsapp.go`

```go
type WhatsAppClient struct {
    phoneNumberID string // Meta Business phone number ID
    accessToken   string // Bearer token from secret manager
    baseURL       string // https://graph.facebook.com/v17.0
}

func (c *WhatsAppClient) SendTemplate(ctx context.Context, req TemplateMessage) error {
    payload := map[string]any{
        "messaging_product": "whatsapp",
        "to":                req.ToPhoneE164,
        "type":              "template",
        "template": map[string]any{
            "name":     req.TemplateName,
            "language": map[string]string{"code": "en"},
            "components": buildComponents(req.Variables),
        },
    }
    body, _ := json.Marshal(payload)
    httpReq, _ := http.NewRequestWithContext(ctx, "POST",
        fmt.Sprintf("%s/%s/messages", c.baseURL, c.phoneNumberID),
        bytes.NewReader(body))
    httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := c.http.Do(httpReq)
    if err != nil { return fmt.Errorf("whatsapp send: %w", err) }
    defer resp.Body.Close()

    var result struct{ Messages []struct{ ID string `json:"id"` } }
    json.NewDecoder(resp.Body).Decode(&result)

    // Write notification_log with provider_message_id (FR-NOTIF-009)
    c.db.CreateNotificationLog(ctx, db.NotificationLog{
        SubscriberID:      req.SubscriberID,
        Channel:           "whatsapp",
        TemplateID:        req.TemplateID,
        TriggeredByEvent:  req.TriggerEvent,
        ProviderMessageID: result.Messages[0].ID,
        DeliveryStatus:    "sent",
        SentAt:            time.Now(),
    })
    notificationDispatchTotal.WithLabelValues("whatsapp", req.TemplateID, "sent").Inc()
    return nil
}
```

**WhatsApp Delivery Webhook Handler (FR-NOTIF-011):**
```go
// POST /webhooks/whatsapp — receives Meta delivery status callbacks
func (h *Handler) WhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
    // Verify webhook with X-Hub-Signature-256 header
    // Parse statuses array
    for _, status := range payload.Entry[0].Changes[0].Value.Statuses {
        h.db.UpdateNotificationLogStatus(ctx, status.ID, status.Status) // sent/delivered/read/failed
        whatsappDeliveryStatusTotal.WithLabelValues(status.Status).Inc()
    }
    w.WriteHeader(http.StatusOK)
}
```

**DND Check (FR-NOTIF-008):**
```go
// internal/notifications/dispatcher.go
func (d *Dispatcher) Dispatch(ctx context.Context, task NotificationTask) error {
    sub, _ := d.db.GetSubscriber(ctx, task.SubscriberID)
    if sub.DndOptOut && task.Class == "marketing" {
        d.db.CreateNotificationLog(ctx, db.NotificationLog{
            DeliveryStatus: "suppressed_dnd",
        })
        return nil // not an error; intentional suppression
    }
    // proceed with dispatch
}
```

---

## 5.9 Subscriber Health API *(new — MOD-PORTAL, SAD-COMP-008)*
**FR:** FR-OBS-004 | **CRD:** PER-002, PER-004

Single-call endpoint aggregating Redis session state + DB metadata for CSR/NOC use. Target response time: ≤ 200ms (NFR-PERF-002).

```go
// internal/health/subscriber.go
type SubscriberHealth struct {
    SubscriberID    int            `json:"subscriber_id"`
    Username        string         `json:"username"`
    Status          string         `json:"status"`           // active / suspended / grace_period
    WalletBalance   string         `json:"wallet_balance"`
    PlanExpiry      time.Time      `json:"plan_expiry"`
    ActiveSession   *SessionSummary `json:"active_session"`  // from Redis; nil if no session
    FupStatus       FUPStatus      `json:"fup_status"`       // below/warning/throttled
    LastCoaResult   string         `json:"last_coa_result"`  // ack/nak/pending/none
    OpenTickets     int            `json:"open_tickets"`
    LastNotification *NotifSummary `json:"last_notification"` // from notification_log
}

type SessionSummary struct {
    SessionID     string `json:"session_id"`
    NasIP         string `json:"nas_ip"`
    AssignedIP    string `json:"assigned_ip"`
    BytesUsed     int64  `json:"bytes_used"`
    BytesTotal    int64  `json:"bytes_total"`
    PctUsed       int    `json:"pct_used"`
    SpeedProfile  string `json:"speed_profile"` // "100M/100M" or "10M/10M (FUP)"
    SessionAge    string `json:"session_age"`   // human-readable e.g. "2h 14m"
}

func (h *Handler) GetSubscriberHealth(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "id")
    // Fan out: Redis session fetch + DB query run concurrently
    var wg sync.WaitGroup
    var session *SessionSummary
    var sub db.Subscriber
    wg.Add(2)
    go func() { defer wg.Done(); session = h.redis.GetActiveSession(ctx, id) }()
    go func() { defer wg.Done(); sub, _ = h.db.GetSubscriberWithMeta(ctx, id) }()
    wg.Wait()
    // assemble and return SubscriberHealth
}
```

---

## 5.10 Revenue Assurance Jobs (MOD-REV)
**FR:** FR-REV-001..004 | **CRD:** CRD-REV-001..002

Nightly task `revenue:reconcile` runs at 02:00 IST:

```go
// internal/revenue/reconcile.go
func (j *ReconcileJob) Run(ctx context.Context) error {
    // 1. Unbilled subscriber report → write to revenue_snapshots table
    unbilled, _ := j.db.GetUnbilledActiveSubscribers(ctx)
    // 2. Ledger variance check → alert if ABS(variance) > 0.01
    variance, _ := j.db.GetLedgerVariance(ctx)
    if variance.Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
        j.alerter.Trigger("ledger_variance_detected", variance)
    }
    // 3. Collections forecast → write to collections_forecast table
    forecast, _ := j.db.BuildCollectionsForecast(ctx, 30)
    j.db.UpsertCollectionsForecast(ctx, forecast)
    revenueReconcileLastRun.SetToCurrentTime()
    return nil
}
```
