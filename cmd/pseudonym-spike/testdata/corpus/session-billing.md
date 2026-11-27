Session transcript 2026-11-20 — billing integration debugging.

Maria Silva opened the session: "Acme Corp's production key sk-live-9f4a7b2c
was rotated by the platform team and now every request from our billing job
against acme.com/api/v2 returns 401."

Investigation: the job reads the key from Acme Corp's vault path
secret/acme/billing/prod, but the deploy pipeline still pinned the previous
value. Maria Silva re-rendered the template, restarted the worker, and the
acme.com health endpoint returned 200 again.

Follow-up: Acme Corp finance asked whether the acme.com invoice for November
reflects the new plan tier. Maria Silva will confirm with Acme Corp's
billing contact before month-end close.

Note for the runbook reviewer: this document deliberately mixes exact-case
occurrences ("Acme Corp", "Maria Silva", "acme.com") with a possessive
("Acme Corp's") and a case variant if any slide decks lower-case the brand.
It is the reference fixture for the apply-rate arithmetic.
