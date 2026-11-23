# General Ledger Phase 2 — posting rules, for review

**Status: draft, not implemented. Needs an accountant's sign-off before any
code is written.**

Drafted 2026-11-23. Phase 1 shipped the ledger as manual entries only: the
console can create accounts, post a journal entry by hand, and read a trial
balance, income statement and balance sheet. Nothing posts automatically.

Phase 2 is the automatic posting — when a subscriber recharges their wallet,
when an invoice is raised, when a partner earns commission, when a purchase
order is billed, the ledger should record it without anybody typing a journal
entry. This document is the list of what debits what. **Get this wrong and
the books are quietly wrong**, which is why it is a review document and not a
commit.

---

## 1. The chart of accounts that already exists

Migration 043 seeded eleven accounts. They are sensible for an Indian ISP and
this draft keeps all of them:

| Code | Name | Type | Normal balance |
|---|---|---|---|
| 1000 | Cash / Bank | asset | debit |
| 1200 | Subscriber Wallet Liability | liability | credit |
| 1300 | TDS Receivable | asset | debit |
| 2000 | Accounts Payable | liability | credit |
| 2100 | Commission Payable to Partners | liability | credit |
| 2200 | GST Payable | liability | credit |
| 3000 | Owner's Equity | equity | credit |
| 4000 | Subscription Revenue | income | credit |
| 5000 | Franchise Commission Expense | expense | debit |
| 5100 | Operating Expenses | expense | debit |
| 5200 | Wallet Adjustments & Refunds | expense | debit |

## 2. Accounts this draft needs added — please confirm each

The posting rules below cannot be written without these. Each is a question
for you, not a decision I should take.

| Proposed | Name | Type | Why it is needed |
|---|---|---|---|
| **1100** | Accounts Receivable — Subscribers | asset | debit | An invoice raised is money owed before it is money received. Without it there is nowhere to debit when an invoice is created, and revenue would have to be recognised only on payment — which is cash-basis accounting and not what a GST-registered company files. **This is the important one.** |
| **1310** | GST Input Tax Credit | asset | debit | GST paid to vendors is reclaimable. Posting it into Operating Expenses instead would overstate expenses and lose the ITC trail your GSTR-3B depends on. |
| **1400** | Inventory — CPE & Equipment | asset | debit | The system already tracks procurement and CPE devices. A router bought for stock is an asset until issued, not an expense on the day it arrives. |
| **5300** | Bad Debts Written Off | expense | debit | A terminated subscriber with dues has to leave the receivables ledger somehow. |
| **5400** | Bank & Gateway Charges | expense | debit | Razorpay's cut and cheque-return charges are an expense in their own right, not a reduction of revenue. |

**Question 1:** add all five, or only some? If you would rather not carry
Inventory (1400) and expense equipment on purchase, say so — it is a
legitimate choice for a small operator, and it simplifies the procurement
posting considerably.

---

## 3. The posting rules

Every row is a complete, balanced entry. Amounts are illustrative; a plan at
₹500 plus 18% GST is used throughout.

### 3.1 Subscriber recharges their wallet — ₹1,000 by UPI

| Account | Debit | Credit |
|---|---|---|
| 1000 Cash / Bank | 1,000.00 | |
| 1200 Subscriber Wallet Liability | | 1,000.00 |

Money has arrived but nothing has been earned yet, so it sits as a liability:
the ISP owes the subscriber either service or a refund. Revenue is recognised
in 3.2, not here.

**Question 2:** if payment arrives through Razorpay and they deduct a fee, do
you want the fee posted at the moment of collection (debit 5400, credit 1000
for the fee) or reconciled separately against the gateway settlement? The
second is more common and less noisy, but it means the ledger briefly shows
the gross amount in the bank.

### 3.2 Monthly invoice raised — ₹500 base, ₹90 GST, ₹590 total

| Account | Debit | Credit |
|---|---|---|
| 1100 Accounts Receivable | 590.00 | |
| 4000 Subscription Revenue | | 500.00 |
| 2200 GST Payable | | 90.00 |

The GST becomes payable when the invoice is raised, not when the customer
pays — this is what makes 1100 necessary.

**Question 3:** for intra-state supply this should split into CGST and SGST
for the return, and inter-state is IGST. The system already calculates the
split (`billing.CalculateGstInvoiceFrom`). Do you want three GST accounts
(2210 CGST, 2220 SGST, 2230 IGST) rather than one 2200, so GSTR-1 can be
filled straight from the trial balance? **I would recommend yes**, but it
multiplies the accounts and is your call.

### 3.3 Invoice settled from the wallet — ₹590

| Account | Debit | Credit |
|---|---|---|
| 1200 Subscriber Wallet Liability | 590.00 | |
| 1100 Accounts Receivable | | 590.00 |

No cash moves. The liability the ISP owed is discharged by the receivable the
subscriber owed.

### 3.4 Customer pays directly and deducts TDS — ₹590 invoice, ₹584 received

| Account | Debit | Credit |
|---|---|---|
| 1000 Cash / Bank | 584.00 | |
| 1300 TDS Receivable | 6.00 | |
| 1100 Accounts Receivable | | 590.00 |

Applies to business customers who deduct tax at source. The existing 1300
account is for exactly this.

**Question 4:** which TDS rate and section applies to your service — 194C,
194J, or 194-I if you lease bandwidth? The rate is not needed for the posting
rule, but it is needed for the certificate reconciliation that follows.

### 3.5 Franchise / LCO commission earned — ₹50 on a collection

| Account | Debit | Credit |
|---|---|---|
| 5000 Franchise Commission Expense | 50.00 | |
| 2100 Commission Payable to Partners | | 50.00 |

And when it is paid out:

| Account | Debit | Credit |
|---|---|---|
| 2100 Commission Payable to Partners | 50.00 | |
| 1000 Cash / Bank | | 50.00 |

**Question 5:** is commission earned when the subscriber is invoiced, or when
the subscriber actually pays? Accrual accounting says the former, but most
ISPs pay partners on collection, and posting it on invoice would show a
liability for commission on money that never arrived. **I would recommend on
collection**, which also matches how `lco_ledger` already behaves.

### 3.6 Purchase order billed by a vendor — ₹10,000 of routers plus ₹1,800 GST

| Account | Debit | Credit |
|---|---|---|
| 1400 Inventory — CPE & Equipment | 10,000.00 | |
| 1310 GST Input Tax Credit | 1,800.00 | |
| 2000 Accounts Payable | | 11,800.00 |

And when a router is issued to a subscriber, if you treat CPE as a cost of
service rather than a rental asset:

| Account | Debit | Credit |
|---|---|---|
| 5100 Operating Expenses | 1,200.00 | |
| 1400 Inventory — CPE & Equipment | | 1,200.00 |

**Question 6:** is a router given to a subscriber (a) expensed on issue, as
above, (b) kept as a fixed asset you depreciate because it remains your
property, or (c) sold to them as revenue? All three happen in this industry
and they post completely differently. **This is the question I least want to
guess at.**

### 3.7 Refund to a subscriber — ₹300 from wallet balance

| Account | Debit | Credit |
|---|---|---|
| 1200 Subscriber Wallet Liability | 300.00 | |
| 1000 Cash / Bank | | 300.00 |

A refund of unused balance returns a liability, so it is not an expense.
5200 Wallet Adjustments & Refunds is for the other case — a goodwill credit
that was never paid for:

| Account | Debit | Credit |
|---|---|---|
| 5200 Wallet Adjustments & Refunds | 100.00 | |
| 1200 Subscriber Wallet Liability | | 100.00 |

### 3.8 Cheque bounced — reversing a ₹1,000 credit, with ₹50 of bank charges

| Account | Debit | Credit |
|---|---|---|
| 1200 Subscriber Wallet Liability | 1,000.00 | |
| 1000 Cash / Bank | | 1,000.00 |
| 5400 Bank & Gateway Charges | 50.00 | |
| 1000 Cash / Bank | | 50.00 |

FR-BIL-014 already reverses the wallet credit; this is its ledger shadow.

**Question 7:** do you recover the bounce charge from the subscriber? If so
it is a receivable against them rather than your expense, and the rule
changes.

### 3.9 Writing off a terminated subscriber's dues — ₹590

| Account | Debit | Credit |
|---|---|---|
| 5300 Bad Debts Written Off | 590.00 | |
| 1100 Accounts Receivable | | 590.00 |

**Question 8:** should this be automatic on termination, or always a manual
decision? **I would recommend manual** — an automatic write-off makes bad
debt invisible, and it is a number an owner should have to look at.

---

## 4. Two engineering notes

**Every posting must be idempotent.** `gl_journal_entries` carries
`source_type` and `source_id`, which is how a posting is tied to the invoice
or payment that caused it. There is currently **no uniqueness constraint** on
that pair and **no CHECK** on `source_type`, so a retried billing run would
post the same entry twice and silently double the books. Phase 2 should add a
unique index on `(source_type, source_id)` before it posts anything.

**Nothing should post outside a transaction with the event itself.** If an
invoice is created and the ledger posting fails, the two must fail together,
or the trial balance stops tying to the invoice table and no report will ever
reconcile again.

---

## 5. What I need back

The eight numbered questions. Questions **1, 3 and 6** are the ones that
change the structure rather than a detail — Accounts Receivable, whether GST
splits into CGST/SGST/IGST, and how subscriber routers are treated. The other
five can be answered later without rework.

Once those are settled the implementation is roughly 1–2 days: the account
additions as a migration, a posting service with the rules above, the unique
index, and tests that assert every entry balances and that each event posts
exactly once.
