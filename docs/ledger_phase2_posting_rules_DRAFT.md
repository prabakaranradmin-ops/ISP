# General Ledger — what already posts, and the one real gap

**Status: for review. Needs an accountant's answer to one question before any
code is written.**

Drafted 2026-11-23. **Rewritten the same day, because the first version was
wrong about the most important thing.** It said the ledger was manual-entry
only and that nothing posted automatically. That is not true: three automated
postings have been in production for months. The correction matters, because
the first version would have had you commission work that is already done.

---

## 1. What already posts automatically

Three event kinds write journal entries today, each tagged with the row that
caused it. All of this works and is covered by integration tests.

### 1.1 Wallet movements — `source_type = 'wallet_ledger'`

Every recharge, refund, adjustment and cheque reversal posts two legs, mapped
by `billing.GLAccountCode`:

| Account | Used for |
|---|---|
| 1000 Cash / Bank | money in or out through the gateway or the counter |
| 1200 Subscriber Wallet Liability | the balance owed back to the subscriber |
| 4000 Subscription Revenue | the revenue leg when a plan charge is taken |
| 5200 Wallet Adjustments & Refunds | a goodwill credit nobody paid for |
| 1300 TDS Receivable | tax a business customer deducted at source |
| 2200 GST Payable | the output-tax split, via `splitTaxLeg` |

The GST split already happens here, so output tax lands in a liability rather
than being buried in revenue.

### 1.2 Purchase orders received — `source_type = 'purchase_order'`

| Account | Debit | Credit |
|---|---|---|
| 5100 Operating Expenses | full amount | |
| 2000 Accounts Payable | | full amount |

### 1.3 Franchise commission — `source_type = 'lco_commission'`

| Account | Debit | Credit |
|---|---|---|
| 5000 Franchise Commission Expense | commission | |
| 2100 Commission Payable to Partners | | commission |

Posted when the commission is earned on a collection, and a zero-rate partner
gets its `lco_ledger` row with no journal entry — a zero line would fail
`chk_gl_line_nonzero`.

### 1.4 Idempotency — closed 2026-11-23

These all arrive through retrying paths, and nothing stopped the same event
posting twice. Migration 075 adds a partial unique index on
`(source_type, source_id)`, so a retry is refused and comes back as
`ledger.ErrAlreadyPosted` — which a handler treats as "already done" rather
than retrying to the dead-letter queue. Manual entries carry no `source_id`
and are deliberately exempt.

---

## 2. The one real gap: revenue is recognised on cash, not on invoice

**Raising an invoice posts nothing to the general ledger.** Revenue reaches
4000 when money moves through the wallet, not when the invoice is issued. In
accounting terms the automated postings are **cash-basis**, and that is why
there is no Accounts Receivable account in the chart — nothing ever needed
one.

For a GST-registered company this is the question worth your accountant's
time, because output GST becomes payable when the invoice is raised, not when
the customer pays. Today 2200 GST Payable is credited at the point of
collection.

**Question 1 — and it is the only one that blocks code.** Should invoicing
move to accrual?

**If yes**, it needs one new account and one new posting:

| Proposed | Name | Type |
|---|---|---|
| **1100** | Accounts Receivable — Subscribers | asset / debit |

Invoice raised — ₹500 base, ₹90 GST:

| Account | Debit | Credit |
|---|---|---|
| 1100 Accounts Receivable | 590.00 | |
| 4000 Subscription Revenue | | 500.00 |
| 2200 GST Payable | | 90.00 |

Settled from the wallet:

| Account | Debit | Credit |
|---|---|---|
| 1200 Subscriber Wallet Liability | 590.00 | |
| 1100 Accounts Receivable | | 590.00 |

The wallet posting in 1.1 would then stop crediting 4000 directly, or revenue
would be counted twice. **That is the part that makes this a decision rather
than an addition**: it changes an existing, working, tested posting path.

**If no** — cash-basis is a deliberate choice for a prepaid ISP, and it is a
defensible one when almost every subscriber pays before service — then
nothing here needs doing, and the chart is correct as it stands. Say so and I
will note it in the CRD so this does not get re-raised.

---

## 3. Smaller gaps, none of them blocking

Each is additive and independent of Question 1.

| Proposed | Name | Why | Question |
|---|---|---|---|
| **1310** | GST Input Tax Credit | 1.2 puts the whole PO amount including GST into 5100 Operating Expenses, so reclaimable input tax is expensed and the ITC trail GSTR-3B needs is lost | **2.** Do your POs record GST separately? If they do not, this cannot be fixed by accounting alone |
| **1400** | Inventory — CPE & Equipment | 1.2 expenses a router on receipt. If stock is held before issue it is an asset until issued | **3.** Expense on receipt (simplest, what happens now), hold as inventory, or capitalise and depreciate? |
| **5300** | Bad Debts Written Off | A terminated subscriber's dues have nowhere to go. Only needed if Question 1 is "yes" — with no receivable there is nothing to write off | **4.** Automatic on termination, or always a manual decision? I would say manual: an automatic write-off makes bad debt invisible |
| **5400** | Bank & Gateway Charges | Razorpay's cut and cheque-return charges reduce revenue today instead of being their own expense | **5.** Post the fee at collection, or reconcile against the gateway settlement? The second is more common |
| 2210/2220/2230 | CGST / SGST / IGST | 2200 is one account, while `billing.CalculateGstInvoiceFrom` already computes the three-way split. Splitting the accounts would let GSTR-1 be filled from the trial balance | **6.** Worth the extra accounts? I would say yes |

---

## 4. One engineering note, not for the accountant

A posting shares a transaction with the event that caused it in all three
paths above — checked, not assumed. That is what keeps the trial balance
tied to the subledgers, and it is the property to preserve in anything added
here.

---

## 5. What I need back

**Question 1 only**, to start. The rest can be answered later without rework,
and two of them (2 and 3) may be moot depending on how your POs record tax.

If Question 1 is "no", this document closes and the ledger is finished as
designed.
