Incident review — vendor onboarding leaks.

During vendor onboarding, an intern pasted Acme Corp's vendor list, including
contact rows for Maria Silva (maria.silva@acme.com) and two other coordinators,
into a public ticket. The ticket body carried Acme Corp's internal contract
IDs and one staging key shaped like sk-live-0000000000 (redacted here).

What worked: the ticket was caught by review before indexing; Acme Corp's
security team rotated the staging key within an hour; Maria Silva filed the
postmortem.

What did not: nobody could answer which OTHER documents mentioned acme.com
contract terms, because full-text search matched only exact-case acme.com
spellings — the deck that spelled it "ACME.COM" was missed until the second
pass.

Action items: shortlist a declared-term list for outbound review; measure how
often case variants and possessives appear in real session traffic (this
spike's corpus measures exactly that); decide whether patch-then-restore is
compatible with provider prompt caches before shipping.
