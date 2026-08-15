"""
Synthetic labelled test set for mailbox's AI categoriser.

SAFE TO COMMIT — and verified, not asserted. An earlier version of this file was
written with a real mailbox open alongside it and reproduced large amounts of it
while claiming not to; a name scan passed because the names had been changed and
the prose had not. This version was written from the category definitions alone,
and the acceptance test is a 6-word-shingle comparison against that real corpus
returning ZERO matches. If you extend this file, re-run that check rather than
looking for names.

Everything below is invented: a fictional recipient (Robin) at a fictional
company building a document-processing API, fictional colleagues, clients and
vendors. What is carried over from reality is only *structure* — which
archetypes fill an automated inbox, where the category boundaries sit, and which
decoys make a classifier fail.

FORMAT
    (from_display, subject, snippet, label, difficulty, probe)

    label       one of ai.EmailCategories, or "" for "no tag"
    difficulty  easy  a competent model must get this right
                med   ordinary judgement
                hard  probes a specific boundary rule; weak models fail here
    probe       what rule this case tests, or None

BOUNDARY CONVENTIONS (mirroring the production prompt, so ground truth is fair)
    Receipt vs Finance      money already PAID -> Receipt; money OWED/DUE -> Finance
    Calendar vs Newsletter  a personal commitment on YOUR calendar -> Calendar;
                            a public webinar/conference promoted to a mailing
                            list -> Newsletter, even when it names a date
    Calendar vs NeedsReply  a specific time to accept/attend -> Calendar
    Newsletter vs Notification  editorial content for general readers ->
                            Newsletter; automated activity about YOUR OWN
                            account/profile/connections -> Notification
    Security vs Notification  account access & credentials -> Security;
                            infrastructure and app alerts -> Notification
    Discount vs Newsletter  a redeemable code or a specific limited-time offer
                            -> Discount; generic brand marketing -> Newsletter
    ""                      no action, no transaction, no event; also the user's
                            OWN sent mail and closing pleasantries

Class sizes are deliberately uneven. Notification carries ~87% of INBOX_MIX, so
it gets 120 of the 272 cases: that puts one case at ~0.7pp of the weighted score
instead of ~2.9pp, which is the resolution needed to rank models that sit a few
points apart.

Multilingual cases (Dutch, German, Russian) are deliberate: a European inbox is
not English-only, and small models degrade on non-English input.
"""

# --------------------------------------------------------------------------
# Aggregate inbox mix for the frequency-WEIGHTED score. Rounded proportions of a
# typical developer/founder inbox: automated notifications outnumber everything
# else by an order of magnitude, which is why raw and weighted accuracy must
# both be reported. A model that blindly answers "Notification" scores ~87%
# weighted and ~17% raw.
# --------------------------------------------------------------------------
INBOX_MIX = {
    "Notification": 870, "Needs reply": 40, "Newsletter": 26, "Security": 18,
    "Receipt": 12, "Discount": 11, "Finance": 8, "Calendar": 7, "": 5,
    "Travel": 3,
}

CASES = [
    # =====================================================================
    # Notification — group 1: CI/CD and code hosting (12)
    # =====================================================================
    ("Pipeline Bot", "run 4412 stopped at stage 'package'",
     "Stage package exited 2 after 3m48s. Downstream stages were skipped.",
     "Notification", "easy", None),
    ("Pipeline Bot", "nightly regression suite is green again",
     "All 1,204 checks passed. The flake in ocr_layout_test did not recur.",
     "Notification", "med",
     "a completed successful job is still Notification, never Receipt"),
    ("Repo Service", "milan-h requested changes on !338",
     "Comments left on 4 files of 'Tighten page-segmentation heuristics'.",
     "Notification", "easy", None),
    ("Repo Service", "!329 was squashed into trunk",
     "Merged by ada-o. The source branch was deleted automatically.",
     "Notification", "easy", None),
    ("Repo Service", "issue 1187 reopened",
     "sofie-a reopened 'Tables lose column order above 40 rows' with new "
     "reproduction steps.", "Notification", "easy", None),
    ("Repo Service", "weekly advisory roundup for your projects",
     "Six advisories touch libraries you depend on. Two have patches available.",
     "Notification", "hard",
     "advisories about your dependencies are not account access -> not Security"),
    ("Artifact Registry", "scan of ocr-api:3.2.1 finished",
     "Two medium findings, none blocking. Full report retained for 90 days.",
     "Notification", "med", None),
    ("Release Bot", "3.2.1 is live in eu-central",
     "Rollout reached 100% of pods with no error-budget burn.",
     "Notification", "easy", None),
    ("Release Bot", "canary halted, traffic returned to 3.2.0",
     "Latency on the canary exceeded the guard for six consecutive minutes.",
     "Notification", "easy", None),
    ("Coverage Bot", "line coverage moved to 81.4% (+0.9)",
     "Your branch adds 240 covered lines and 31 uncovered ones.",
     "Notification", "easy", None),
    ("Static Analysis", "9 findings introduced on branch layout-v3",
     "Five naming, three unchecked errors, one possible slice out of range.",
     "Notification", "easy", None),
    ("Repo Service", "yusuf-d mentioned you in a thread on !341",
     "'Robin wrote the original heuristic, worth a second opinion here.'",
     "Notification", "hard",
     "a person addressing you inside app activity is still Notification"),

    # =====================================================================
    # Notification — group 2: monitoring and incidents (12)
    # =====================================================================
    ("Watchtower", "FIRING — p99 latency above guard on ocr-api",
     "Observed 2.4s against a 900ms objective for eight minutes.",
     "Notification", "easy", None),
    ("Watchtower", "RESOLVED — p99 latency back within guard",
     "The condition cleared on its own after eleven minutes.",
     "Notification", "easy", None),
    ("Watchtower", "error budget for this quarter is 62% consumed",
     "At the current burn rate the budget lasts until 4 November.",
     "Notification", "med", None),
    ("Pager Rota", "you take the primary rota from Monday 08:00",
     "Two weeks, swapping with Diego. Verify your escalation contacts.",
     "Notification", "hard",
     "an on-call rota notice is not an event you accept -> not Calendar"),
    ("Pager Rota", "escalation path for platform was edited",
     "Kaia moved you from third to second position in the chain.",
     "Notification", "easy", None),
    ("Certificate Watch", "wildcard cert renews automatically in 21 days",
     "Nothing to do unless the delegated zone changed since issuance.",
     "Notification", "med", None),
    ("Snapshot Agent", "snapshot of pgdata completed in 22 minutes",
     "412 GiB written. The most recent restore drill passed on 2 August.",
     "Notification", "med",
     "a finished job with no money involved -> Notification, not Receipt"),
    ("Snapshot Agent", "snapshot ABORTED — target volume unreachable",
     "The last usable snapshot is now 31 hours old.",
     "Notification", "easy", None),
    ("Storage Health", "predictive failure raised on drive 4 of array B",
     "Wear indicators crossed the vendor threshold. Consider swapping it out.",
     "Notification", "easy", None),
    ("Edge Network", "two of six origins were pulled from rotation",
     "Health probes failed three times consecutively on ams and fra.",
     "Notification", "easy", None),
    ("Status Desk", "planned maintenance, 22 September 01:00–03:00 UTC",
     "The metadata cluster moves to new hardware. Writes queue briefly.",
     "Notification", "hard",
     "a maintenance window on the PROVIDER's calendar, not the recipient's"),
    ("Queue Monitor", "retry queue for ingest-worker passed 800 messages",
     "The oldest message has been waiting six hours.",
     "Notification", "easy", None),

    # =====================================================================
    # Notification — group 3: hosting and machine lifecycle (12)
    # =====================================================================
    ("Server Provisioning", "your machine LX7 is ready",
     "Provisioning finished and the base image booted. Credentials inside.",
     "Notification", "easy", None),
    ("Server Provisioning", "reboot you asked for has completed",
     "Machine LX7 answered probes 40 seconds after coming back up.",
     "Notification", "easy", None),
    ("Server Provisioning", "rescue mode was left enabled overnight",
     "Machine LX4 has been in rescue for 14 hours. Disable it when finished.",
     "Notification", "med", None),
    ("Datacentre Ops", "power feed B lost redundancy in hall 3",
     "Equipment stayed up on feed A. Engineers are on site.",
     "Notification", "easy", None),
    ("Datacentre Ops", "cross-connect order 88-2210 was patched",
     "The link tested clean at 10G. Ports are listed in the portal.",
     "Notification", "med", None),
    ("Home Gateway", "router firmware moved to 8.02",
     "Applied during the maintenance window you configured. Nothing to do.",
     "Notification", "easy", None),
    ("Home Gateway", "an unfamiliar device joined the guest network",
     "It appeared eleven minutes ago and is still connected.",
     "Notification", "hard",
     "LAN activity, not a sign-in on your account -> not Security"),
    ("Storage Box", "volume two crossed 90% used",
     "Remove old snapshots or extend the volume to avoid write failures.",
     "Notification", "easy", None),
    ("Storage Box", "four package updates are ready to install",
     "Automatic installation is switched off, so these are waiting for you.",
     "Notification", "easy", None),
    ("Print Room", "cyan is down to 6%",
     "Order a cartridge soon to avoid a stoppage mid-job.",
     "Notification", "med", None),
    ("Building System", "side entrance released at 19:04",
     "Opened with the keypad credential labelled 'cleaning'.",
     "Notification", "med", None),
    ("Building System", "moisture detected under the sink",
     "The stopcock closed automatically two minutes after detection.",
     "Notification", "easy", None),

    # =====================================================================
    # Notification — group 4: cloud, SaaS quota and lifecycle (12)
    # =====================================================================
    ("Platform Billing Ops", "trial allowance runs out in nine days",
     "You have 38 units of trial allowance left; paid usage continues after.",
     "Notification", "hard",
     "an allowance balance with nothing owed -> Notification, not Finance"),
    ("Platform Ops", "v1 endpoints retire on 3 December",
     "Your projects still made 41 calls to v1 last week. Move to v2.",
     "Notification", "med", None),
    ("Platform Ops", "your bucket now serves from eu-central",
     "The migration finished overnight. No configuration change is needed.",
     "Notification", "med", None),
    ("Inference Service", "your throughput ceiling was raised",
     "Automatic tier review moved you to 8,000 requests per minute.",
     "Notification", "easy", None),
    ("Inference Service", "you have used 78% of this cycle's allowance",
     "The counter resets on the 1st; beyond it, usage is metered.",
     "Notification", "hard",
     "usage warning where billing is only hypothetical -> not Finance"),
    ("Search Cluster", "reindex of 2.1M documents finished",
     "Elapsed 54 minutes. Nine documents were rejected as malformed.",
     "Notification", "easy", None),
    ("Flag Service", "'stream-layout' went to 100% of traffic",
     "Changed by ada-o twenty minutes ago; it was at 30% before.",
     "Notification", "easy", None),
    ("Product Analytics", "your monthly rollup is ready",
     "Conversions up 6%, median session length unchanged. Open the board.",
     "Notification", "hard",
     "a report about YOUR OWN account -> Notification, not Newsletter"),
    ("Crash Reporter", "new crash group: nil map write in extractor",
     "Seen 27 times since 04:12, affecting 9 installations.",
     "Notification", "easy", None),
    ("Vault Service", "a credential is past its rotation window",
     "The signing key is 94 days old and policy allows 90.",
     "Notification", "hard",
     "credential hygiene on a stored item, not an access event -> not Security"),
    ("Workspace Admin", "an administrator moved you to the Scale plan",
     "Seats went from 5 to 12; the change takes effect next cycle.",
     "Notification", "hard",
     "an admin action on your account, nothing due now -> not Finance"),
    ("Workspace Admin", "your data export is ready to collect",
     "The archive stays available for seven days, then it is purged.",
     "Notification", "easy", None),

    # =====================================================================
    # Notification — group 5: social and professional networks (12)
    # =====================================================================
    ("Professional Network", "you turned up in 22 searches last week",
     "Most searchers work in logistics and document automation.",
     "Notification", "easy", None),
    ("Professional Network", "Sofie Aalto tagged you in a comment",
     "'…which is roughly what Robin argued at the meetup.' Open the thread.",
     "Notification", "easy", None),
    ("Professional Network", "six people looked at your profile",
     "Two of them work at companies you follow.", "Notification", "easy", None),
    ("Professional Network", "your update reached 3,100 readers",
     "That is ahead of 88% of what you have posted before.",
     "Notification", "easy", None),
    ("Professional Network", "digest from the OCR Practitioners group",
     "Eleven new discussions in a group you belong to.",
     "Notification", "hard",
     "group activity tied to YOUR membership -> Notification, not Newsletter"),
    ("Professional Network", "Jonas Werner shared a role you may know someone for",
     "Someone in your network posted an opening for a platform engineer.",
     "Notification", "med", None),
    ("Photo Service", "Lena Fischer left a comment on a shared album",
     "'Great shot of the whole team.' Two other people replied.",
     "Notification", "easy", None),
    ("Photo Service", "three people asked to follow you",
     "Approve or ignore them from the requests tab.",
     "Notification", "easy", None),
    ("Dev Forum", "your answer was marked as accepted",
     "On 'Deskew scanned pages without losing DPI'. Reputation +15.",
     "Notification", "easy", None),
    ("Dev Forum", "activity on questions you follow",
     "Four new answers and one edit since your last visit.",
     "Notification", "hard",
     "a digest of YOUR follows -> Notification, not Newsletter"),
    ("Video Host", "processing finished for 'layout engine walkthrough'",
     "Available in 1440p. Visibility remains set to unlisted.",
     "Notification", "easy", None),
    ("Team Chat", "you were added to #ingest-rewrite",
     "Added by milan-h. There are 61 messages since the channel opened.",
     "Notification", "easy", None),

    # =====================================================================
    # Notification — group 6: documents, tasks and collaboration (12)
    # =====================================================================
    ("Docs Service", "Ada Okoye commented on 'Segmentation design'",
     "'Do we still need the fallback path here?' Answer in the document.",
     "Notification", "hard",
     "a question raised INSIDE a doc is app activity, not mail asking you"),
    ("Docs Service", "'Ingest runbook' was rolled back",
     "Restored to the 9 August revision by sofie-a; three sections differ.",
     "Notification", "easy", None),
    ("Docs Service", "Diego Salas shared 'Q4 capacity model' with you",
     "You have edit access. Comments are enabled for the whole team.",
     "Notification", "easy", None),
    ("Task Tracker", "four items assigned to you are past due",
     "Daily digest: four overdue, six landing this week.",
     "Notification", "med", None),
    ("Task Tracker", "iteration 19 closed automatically",
     "22 of 27 items done; the remainder rolled into iteration 20.",
     "Notification", "easy", None),
    ("Task Tracker", "New activity: Quarterly Planning Session milestone reached",
     "Nadia El-Amin marked the milestone complete on the board.",
     "Notification", "hard",
     "'Session'/'Planning' are decoys; a finished milestone is not an event"),
    ("Knowledge Base", "'Restarting the extractor safely' was edited",
     "A page you watch changed in two sections.", "Notification", "easy", None),
    ("Design Tool", "Kaia Lindberg invited you to a prototype",
     "'Reader layout, third pass' is open for comments.",
     "Notification", "easy", None),
    ("Form Builder", "one new submission on 'Pilot interest'",
     "That brings the total to 187 submissions.", "Notification", "easy", None),
    ("File Drive", "a document you own was shared outside the company",
     "'capacity-model-q4.xlsx' now has an external viewer.",
     "Notification", "hard",
     "sharing activity that reads like a security event -> Notification"),
    ("File Drive", "trash older than 30 days was emptied",
     "That reclaimed 18 GB of your allowance.", "Notification", "easy", None),
    ("Translation Desk", "glossary import completed",
     "512 entries added, seven skipped as exact duplicates.",
     "Notification", "easy", None),

    # =====================================================================
    # Notification — group 7: HR and internal systems (12)
    # =====================================================================
    ("People Portal", "your September payslip is available",
     "Open it in the portal. Net pay is unchanged from August.",
     "Notification", "hard",
     "payslip availability is an account notice, not money owed -> not Finance"),
    ("People Portal", "Tomas Rasmussen asked for leave on 12 September",
     "As their manager you can approve or decline in the portal.",
     "Notification", "med", None),
    ("People Portal", "one compliance module is still outstanding",
     "The data-handling refresher closes on 30 September; it takes 15 minutes.",
     "Notification", "hard",
     "an automated nag mentioning compliance -> not Security, not Calendar"),
    ("People Portal", "probation review for Yusuf Demir opens next week",
     "The form reaches you on Monday and stays open for ten days.",
     "Notification", "med", None),
    ("Expenses", "claim EX-4471 was approved",
     "EUR 186.40 will go out with the next payment run.",
     "Notification", "hard",
     "an approved claim is not yet paid -> Notification, not Receipt"),
    ("Expenses", "claim EX-4488 is missing an attachment",
     "One line has no document, so the claim cannot be submitted.",
     "Notification", "hard", "the word attachment/receipt appears in a workflow notice"),
    ("Recruiting", "feedback is outstanding for two candidates",
     "Both interviewed for the platform engineer opening last week.",
     "Notification", "med", None),
    ("Recruiting", "a candidate stepped out of the process",
     "Kaia Lindberg withdrew from the platform engineer pipeline.",
     "Notification", "easy", None),
    ("IT Desk", "your laptop patches on Friday evening",
     "Patching starts at 18:30 and reboots once. Save open work.",
     "Notification", "med", None),
    ("Asset Register", "inventory reconciliation finished",
     "One device added, one flagged as unaccounted for.",
     "Notification", "easy", None),
    ("Learning Portal", "a course was assigned to you",
     "'Handling scanned identity documents' — no deadline set.",
     "Notification", "med", None),
    ("Workplace App", "desk booking for Thursday was released",
     "You did not check in by 10:00, so the desk returned to the pool.",
     "Notification", "hard",
     "an automated booking release, not an event on your calendar"),

    # =====================================================================
    # Notification — group 8: support and ticketing (12)
    # =====================================================================
    ("Service Desk", "we logged your request as SD-7741",
     "An engineer picks it up within one working day. No reply needed.",
     "Notification", "hard",
     "an automated 'we logged it' confirmation is not Needs reply"),
    ("Service Desk", "SD-7741 is now with Milan Horvat",
     "Your request moved to in-progress. Updates follow here.",
     "Notification", "easy", None),
    ("Service Desk", "SD-7741 was marked solved",
     "It closes on its own in five days unless you reopen it.",
     "Notification", "easy", None),
    ("Service Desk", "rate your experience on SD-7741",
     "One click, five options. It helps us spot weak areas.",
     "Notification", "hard",
     "an automated satisfaction survey is not a person asking you"),
    ("Vendor Support", "case 30219 moved to second line",
     "Our platform team has it now; expect an update inside 24 hours.",
     "Notification", "easy", None),
    ("Vendor Support", "Обращение 55831 закрыто",
     "Здравствуйте! Ваше обращение закрыто. Оценка работы специалиста "
     "доступна в личном кабинете.", "Notification", "med", None),
    ("Vendor Support", "Statusänderung zu Vorgang 9042",
     "Ein Kollege hat einen Kommentar hinterlegt. Eine Rückmeldung ist "
     "nicht notwendig.", "Notification", "med", None),
    ("Vendor Support", "Ihr Wartungsfenster wurde bestätigt",
     "Die Arbeiten laufen am 24. September zwischen 02:00 und 04:00 Uhr.",
     "Notification", "med", None),
    ("Incident Comms", "write-up published for the 19 August degradation",
     "Root cause, timeline and five follow-up items are in the document.",
     "Notification", "easy", None),
    ("Incident Comms", "follow-up item 3 of 5 was closed",
     "'Add a guard on retry depth' shipped with release 3.2.1.",
     "Notification", "easy", None),
    ("Service Desk", "заявка 55902 принята в работу",
     "Уважаемый клиент, статус вашей заявки изменился на 'в работе'.",
     "Notification", "med", None),
    ("Vendor Support", "scheduled call notes were attached to case 30219",
     "The engineer added a summary of yesterday's session to the case.",
     "Notification", "med", None),

    # =====================================================================
    # Notification — group 9: personal devices and services (12)
    # =====================================================================
    ("Cycle Computer", "yesterday's ride synced",
     "58.2 km in 2h11m, added to your September distance.",
     "Notification", "easy", None),
    ("Photo Backup", "1,860 items uploaded overnight",
     "Your library is fully backed up. The next pass runs tonight.",
     "Notification", "easy", None),
    ("Home Server", "container 'indexer' restarted five times",
     "It is looping. Check the logs before it is marked unhealthy.",
     "Notification", "easy", None),
    ("Energy Meter", "consumption was 14% above last week",
     "Weekly household summary; most of the change came from heating.",
     "Notification", "hard",
     "a usage report with no bill attached -> Notification, not Finance"),
    ("Music Service", "your September listening summary",
     "You played 2,940 minutes. Your top three artists are inside.",
     "Notification", "med", None),
    ("Fitness Band", "you closed your movement goal six days running",
     "Weekly summary and a comparison with August are ready.",
     "Notification", "easy", None),
    ("Password Vault", "five saved logins need attention",
     "They appear in known breach corpora and should be replaced.",
     "Notification", "hard",
     "a report on stored items, not an event on your account -> not Security"),
    ("Password Vault", "vault synced across four devices",
     "No conflicts were found during the merge.", "Notification", "easy", None),
    ("Reading App", "312 unread items are waiting",
     "Your weekly nudge. Clear them from the app in one action.",
     "Notification", "hard",
     "a nag about YOUR queue, not publisher content -> not Newsletter"),
    ("Uptime Watch", "September availability was 99.96%",
     "One incident, eighteen minutes of downtime. Full report inside.",
     "Notification", "easy", None),
    ("Parcel Locker", "something is waiting in locker 12",
     "Collect it with the code in the app within 48 hours.",
     "Notification", "med", None),
    ("Utility Provider", "Start van je warmtecontract op 1 oktober",
     "Beste bewoner, vanaf die datum leveren wij warmte op het opgegeven adres. "
     "Je hoeft nu niets te doen.", "Notification", "hard",
     "a service-start confirmation with no payment is not a Receipt"),

    # =====================================================================
    # Notification — group 10: account changes and service announcements (12)
    # =====================================================================
    ("Workspace Admin", "a member left your workspace",
     "milan-h removed Tomas Rasmussen; nine members remain.",
     "Notification", "easy", None),
    ("Workspace Admin", "terms of service change effective 1 November",
     "Continued use after that date counts as acceptance of the new terms.",
     "Notification", "med", None),
    ("Workspace Admin", "single sign-on was switched on for everyone",
     "Members now reach the workspace through the company identity provider.",
     "Notification", "hard",
     "an admin configuration change, not an access event on your account"),
    ("Domain Registrar", "zone file for northwind.example was updated",
     "Six records changed and propagated to all authoritative servers.",
     "Notification", "easy", None),
    ("Domain Registrar", "auto-renewal is switched on for two domains",
     "They renew in November. No action is required now.",
     "Notification", "med", None),
    ("Compliance Desk", "your control dashboard changed this month",
     "Four controls drifted out of policy and two returned to green.",
     "Notification", "hard",
     "'controls' reads as security but this is a status report"),
    ("Compliance Desk", "evidence collection ran overnight",
     "Eighty artefacts were gathered; three sources need reconnecting.",
     "Notification", "med", None),
    ("Analytics Suite", "a scheduled report failed to build",
     "The source table was locked when the job started. It will retry.",
     "Notification", "easy", None),
    ("Backup Vault", "retention policy removed 240 old snapshots",
     "This freed 1.4 TB. The policy keeps daily snapshots for 30 days.",
     "Notification", "easy", None),
    ("Family Site", "a relative added new photographs",
     "Two albums were updated on the family site you follow.",
     "Notification", "med", None),
    ("Marketplace", "someone made an offer on your listing",
     "A buyer offered less than your asking price for the bike stand.",
     "Notification", "easy", None),
    ("Ticket Platform", "your ticket transfer was completed",
     "The seat is now held under the recipient's name.",
     "Notification", "med", None),

    # =====================================================================
    # Needs reply (28) — a person is asking something of the recipient
    # =====================================================================
    ("Ada Okoye", "which extractor for the Hartwell pilot?",
     "We can ship either the fast path or the accurate one, not both by "
     "Friday. Which do you want me to prioritise?", "Needs reply", "easy", None),
    ("Milan Horvat", "coffee before standup?",
     "I'm in at 08:45 tomorrow. Fancy grabbing something first?",
     "Needs reply", "easy", None),
    ("Nadia El-Amin", "slides for Thursday",
     "Can you send me the capacity slides tonight? I want to read them "
     "before the board call.", "Needs reply", "easy", None),
    ("Sofie Aalto", "weekly platform note",
     "Ingest throughput is steady, the layout patch shipped on Tuesday, and "
     "queue depth is flat. Support tickets fell for the third week. One thing "
     "before I file this: are we still required to keep the SOC2 letter for "
     "the Nordic reseller, or did legal drop that?", "Needs reply", "hard",
     "one question buried at the end of a long status report"),
    ("Beatrix Hollander", "two options for the workshop",
     "Would you rather run it as one full day or two half days? We need to "
     "book the trainer this week.", "Needs reply", "easy", None),
    ("Diego Salas", "sharing your notes",
     "Are you comfortable with me passing your architecture notes to the "
     "client's security reviewer?", "Needs reply", "easy", None),
    ("Priyanka Shah", "Re: reported vulnerabilities",
     "Thanks for the detail. Could you give us a target date for the "
     "template renderer remediation?", "Needs reply", "easy", None),
    ("Halvard Nyström", "Re: version requirements",
     "Our reviewers insist on curl 8.21 or above and openssl 3.4. Can you "
     "confirm you can meet that?", "Needs reply", "easy", None),
    ("Grace Mbeki", "policy document request",
     "Would you be able to send over your vulnerability management policy? "
     "Our auditors have asked for a copy.", "Needs reply", "easy", None),
    ("Jonas Werner", "Re: counter-offer for the layout role",
     "Fine, let's go with eight percent from October. What did he mean about "
     "the ceiling on the current track?", "Needs reply", "easy", None),
    ("Tomas Rasmussen", "moving on",
     "I've accepted an offer elsewhere and will be leaving at the end of "
     "October. Happy to talk through handover whenever suits.",
     "Needs reply", "easy", None),
    ("Security Reviewer", "Re: [northwind/infra] advisory triage",
     "Could you prepare a patch release for the item below and tell us when "
     "it lands?", "Needs reply", "easy", None),
    ("Owen Castellanos", "Re: [EXT] pilot kickoff",
     "Team — the operator guide is attached. Sandbox credentials are still "
     "pending on our side. Owen", "Needs reply", "med", None),
    ("Ingrid Solberg", "pilot planning, where are we",
     "We have the workflow documentation now and will by now have looked "
     "at the integration points. Where did you land?", "Needs reply", "med", None),
    ("Rosalind Fahey", "renewal conversation",
     "Hope the autumn is treating you well. As we approach the renewal date, "
     "could we find twenty minutes this week?", "Needs reply", "med", None),
    ("Marcus Whitlow", "third-party access review",
     "Our security group is tightening rules on external integrations. Can we "
     "talk through what that means for you?", "Needs reply", "easy", None),
    ("Delphine Roux", "RE: insurance brokerage services",
     "Circling back in case this slipped past you. Worth a look? Send me the "
     "regions you cover and I'll take it from there.", "Needs reply", "med",
     "cold outreach, but a real person asking a direct question"),
    ("theo", "question about your document pipeline",
     "Saw that you're doing OCR at volume. Something we hear a lot is that "
     "accuracy plateaus around the 90% mark. Open to a conversation?",
     "Needs reply", "med", None),
    ("Gregory Palmer", "checking in on headcount",
     "Hello Robin, reaching out to ask whether you are recruiting at the "
     "moment or expect to later this year.", "Needs reply", "med", None),
    ("Gregory Palmer", "a shortcut for your next opening",
     "Hello Robin, hiring usually stalls on finding people who fit rather "
     "than people who qualify. Shall I send a few profiles?",
     "Needs reply", "hard",
     "same cold sender as the previous case; both are 1:1 outreach, not Newsletter"),
    ("Wesley Kwan", "about that platform role",
     "Robin, noticed the opening went live. Whether it works out tends to "
     "hinge on the first month. Want my onboarding checklist?",
     "Needs reply", "hard", "cold 1:1 outreach that reads like marketing"),
    ("Jeroen Bakker", "Draaien er meerdere agents tegelijk bij jullie?",
     "Beste Robin, een korte vraag: laten jullie meerdere agents tegelijk op "
     "dezelfde codebase los? Dat levert bij anderen vaak conflicten op.",
     "Needs reply", "easy", None),
    ("Femke Visser", "voorstel besparing vaste lasten",
     "Hallo Robin, als wij de vaste lasten van jullie kantoor met een vijfde "
     "omlaag kunnen brengen, mag ik daar dan een voorstel voor opsturen?",
     "Needs reply", "med", None),
    ("Ruben Hofstra", "beschikbare Go-opdrachten in fintech",
     "Goedemorgen Robin, ik zoek op dit moment voor een vaste klant naar "
     "backend-specialisten. Heb je ruimte voor een gesprek?",
     "Needs reply", "med", None),
    ("Daan Meijer", "RE: samenwerking platformteam",
     "Hoi Robin, ik breng mijn eerdere bericht nog even onder je aandacht. "
     "Benieuwd hoe jullie er nu voor staan.", "Needs reply", "med", None),
    ("Ilona Kazlauskas", "let's connect",
     "Ilona, partnerships lead, would like to hear back from you.",
     "Needs reply", "med", None),
    ("Dean Whitfield", "quick note on your campaigns",
     "Following up in case my earlier note arrived at a busy moment. It is "
     "also possible I have misjudged this entirely.", "Needs reply", "med", None),
    ("Card Platform", "Diego needs your sign-off on a top-up",
     "Diego Salas has requested EUR 400 against the shared team card. Approve or "
     "decline.", "Needs reply", "hard",
     "automated, but it blocks on the recipient's own decision"),

    # =====================================================================
    # Newsletter (22) — editorial or general-audience publisher content
    # =====================================================================
    ("Backend Weekly", "iterator helpers, arenas, and a faster JSON path",
     "Issue 214, 12 September. Read it in your browser. Curated for backend "
     "engineers.", "Newsletter", "easy", None),
    ("Systems Digest", "a proposal for arena allocation in the next release",
     "Plus tracing the allocator under load, structured concurrency in "
     "practice, and why interfaces leak.", "Newsletter", "easy", None),
    ("The Weekly Pick", "seven links worth your Sunday",
     "Hand-picked reading across engineering, design and operations. "
     "Unsubscribe whenever you like.", "Newsletter", "easy", None),
    ("Applied ML Letter", "issue 118: small models are eating the edge",
     "This week's research picks, one long read and the jobs board.",
     "Newsletter", "easy", None),
    ("Lowlands Briefing", "your five-minute Netherlands roundup",
     "Housing debate resumes, rail works at Utrecht, and the weekend "
     "forecast.", "Newsletter", "easy", None),
    ("City Gallery", "Het is vandaag de dag van de architectuur",
     "Van grachtenpanden tot naoorlogse bouw: ontdek de collectie online.",
     "Newsletter", "easy", None),
    ("Charting Tool", "what we shipped over the summer",
     "Saved views, a faster canvas, and diagrams that follow your repository.",
     "Newsletter", "med", None),
    ("Notes App Team", "autofill is markedly better this release",
     "We closed the long tail of login-form failures and made second-factor "
     "entry more dependable.", "Newsletter", "med",
     "a product update mentioning second factors concerns stored credentials, not account access"),
    ("Inference Service", "lower prices and a faster tier",
     "From today the mid tier costs less per million tokens and responds "
     "sooner. See the updated pricing.", "Newsletter", "easy", None),
    ("Tracker Company", "what's new for teams working with agents",
     "Planning boards, automation hooks and a rebuilt timeline view.",
     "Newsletter", "easy", None),
    ("Repo Service", "cost controls that stretch your allowance",
     "Assign groups to cost centres, cap spend per team and report on it.",
     "Newsletter", "med", None),
    ("Cloud Vendor", "ship AI-ready services without overspending",
     "Stop paying for idle capacity. Join the session on 8 October to see "
     "how.", "Newsletter", "hard",
     "a public webinar promoted to a mailing list is Newsletter, not Calendar"),
    ("DocuConf", "free registration is open for DocuConf",
     "Every keynote and workshop, streamed live from 21 to 23 October.",
     "Newsletter", "hard",
     "a public conference with dates is still Newsletter, not Calendar"),
    ("Enterprise Suite", "hands-on agent training, places are limited",
     "Join a virtual session and build something practical. Runs through "
     "November.", "Newsletter", "hard",
     "'places are limited' is scarcity marketing, not a personal commitment"),
    ("Compliance Desk", "how many agents run in your estate? live session",
     "Most teams cannot answer that question. We will show one way to find "
     "out, live on 15 October.", "Newsletter", "hard",
     "a vendor webinar promotion, not an invitation to the recipient"),
    ("Hosting Provider", "our autumn infrastructure update",
     "New locations, denser storage nodes and where the roadmap goes next.",
     "Newsletter", "easy", None),
    ("Ride Supply Co", "built for the wet months",
     "Mudguards, lights and layers that hold up when the weather turns.",
     "Newsletter", "easy", None),
    ("Everyday Apparel", "the pieces that carry a whole wardrobe",
     "Plain shirts, honest trousers, and how to combine six items forty ways.",
     "Newsletter", "easy", None),
    ("Outdoor Depot", "the autumn range has landed",
     "Chosen by hikers, trail runners and people who commute in the rain.",
     "Newsletter", "hard",
     "generic seasonal marketing with NO code and no specific offer"),
    ("National Rail", "Ontdek Nederland met de trein",
     "Van duinen tot heuvelland: bestemmingen voor een dag weg zonder auto.",
     "Newsletter", "easy", None),
    ("Ads Academy", "four routes into stronger B2B campaigns",
     "Pick the track that matches your work: measurement, setup, reporting or "
     "strategy.", "Newsletter", "easy", None),
    ("Gateway Shop", "Wat wil je weten over mesh-netwerken?",
     "We hebben de meestgestelde vragen verzameld en beantwoord op het blog.",
     "Newsletter", "easy", None),

    # =====================================================================
    # Discount (14) — a redeemable code, or a specific limited-time offer
    # =====================================================================
    ("Webshop Offers", "a third off, this weekend only",
     "Everything in the autumn range. Enter HARVEST33 at the basket. Closes "
     "Sunday at midnight.", "Discount", "easy", None),
    ("Webshop Basket", "you left something behind",
     "Your basket is still here, and so is 12% off with the code STAY12 for "
     "the next two days.", "Discount", "easy", None),
    ("Airline Extras", "30% off extra baggage on your booking",
     "Add hold luggage to reference QP4T7M at a reduced rate until 28 "
     "September.", "Discount", "easy", None),
    ("Meal Delivery", "two mains for the price of one",
     "Tonight only, with the code PAIR21 at checkout.",
     "Discount", "easy", None),
    ("Phone Retailer", "tot EUR 950 voordeel op geselecteerde toestellen",
     "De actieweek loopt tot en met zondag. Bekijk welke modellen meedoen.",
     "Discount", "easy", None),
    ("Outdoor Depot", "alleen dit weekend 20% op regenkleding",
     "Gebruik de code REGEN20 en profiteer voordat het echt najaar wordt.",
     "Discount", "easy", None),
    ("Cafe Group", "nieuwe aanbiedingen vanaf vandaag",
     "Ontdek de najaarsdeals, geldig tot en met zondag in alle vestigingen.",
     "Discount", "med", None),
    ("Ride Supply Co", "members get first look at the sale",
     "Reductions across last season's kit, open to members a day early.",
     "Discount", "med", None),
    ("Loyalty Scheme", "je eerste spaarcadeau ligt klaar",
     "Haal het op in de app; het blijft veertien dagen geldig.",
     "Discount", "med", None),
    ("Utility Referral", "tot EUR 60 als je iemand doorverwijst",
     "Je vriend bespaart op de vaste lasten en jij krijgt een tegoed.",
     "Discount", "med", None),
    ("Online Retailer", "delivery is on us this week",
     "No minimum spend and no code needed, until Sunday night.",
     "Discount", "med",
     "no code, but a specific limited-time offer -> Discount"),
    ("Foundation Events", "the lower rate closes tomorrow",
     "Register before 23:59 on 30 September to keep the reduced ticket price "
     "for the summit.", "Discount", "hard",
     "a public event, BUT the mail is about a specific expiring price"),
    ("Craft Conference", "bring your hardest engineering problems",
     "Sessions on scaling, on-call and platform design. Early rate ends "
     "Friday, with a further 15% off when two of you book together.",
     "Discount", "hard",
     "event marketing carrying an explicit % off -> Discount, not Newsletter"),
    ("Proxy Vendor", "a free tier bump for existing customers",
     "As a current customer you can move up a tier for three months at no "
     "extra cost.", "Discount", "hard",
     "an offer to an existing customer with no code -> still Discount"),

    # =====================================================================
    # Security (14) — account access, credentials, verification
    # =====================================================================
    ("Repo Service", "sign-in from a device we don't recognise",
     "Someone signed in from Gdansk, Poland. If that was not you, change your "
     "password now.", "Security", "easy", None),
    ("Repo Service", "confirm this device before continuing",
     "The sign-in attempt needs a second check because the device is new.",
     "Security", "easy", None),
    ("Repo Service", "review a completed sign-in",
     "The sign-in succeeded, but it came from a location we have not seen "
     "before.", "Security", "easy", None),
    ("Identity Provider", "unusual sign-in on your account",
     "A session opened on a Linux machine in a new city.",
     "Security", "easy", None),
    ("Team Tools", "someone signed in as you",
     "A new session started an hour ago. Secure the account if that was not "
     "you.", "Security", "easy", None),
    ("Enterprise Suite", "Passwort-Zurücksetzung angefordert",
     "Für ein Konto in Ihrer Organisation wurde eine Zurücksetzung "
     "beantragt. Bitte genehmigen oder ablehnen.", "Security", "easy", None),
    ("Domain Registrar", "you asked to reset your password",
     "The link below works for thirty minutes and once only.",
     "Security", "easy", None),
    ("Notes App", "your sign-in link",
     "Use the link to continue, or type the code shown underneath it.",
     "Security", "med", None),
    ("Payments Provider", "your one-time code",
     "Second-factor code 618402. It lapses in ten minutes. Nobody legitimate "
     "will ask you for it.", "Security", "easy", None),
    ("Government Portal", "Ваш код для входа",
     "Никому не сообщайте этот код. Код для входа: 903418.",
     "Security", "easy", None),
    ("Photo Service", "confirm it was you",
     "There was an attempt to open your profile from an unknown device. Use "
     "the code below if it was you.", "Security", "easy", None),
    ("Wallet Service", "we have paused your account",
     "Activity outside your usual pattern triggered a hold. Verify your "
     "identity to lift it.", "Security", "easy", None),
    ("Breach Watch", "your address turned up in a new dump",
     "It appears in a credential set published this week. Replace any reused "
     "password.", "Security", "easy", None),
    ("Identity Provider", "recovery address was changed",
     "The backup address on your account is different as of today. Undo it if "
     "that was not you.", "Security", "easy", None),

    # =====================================================================
    # Receipt (14) — money already paid, or goods already moving
    # =====================================================================
    ("Indie Store", "thanks for your order",
     "Purchase complete. Reference 4471209833. The library entry is already "
     "unlocked.", "Receipt", "easy", None),
    ("Ride App", "your trip on 12 September",
     "Fare EUR 14.80, taken from the card on file. Route and time inside.",
     "Receipt", "easy", None),
    ("Audio Service", "September charge",
     "EUR 11.99 went out on the 1st for the family plan.",
     "Receipt", "easy", None),
    ("App Marketplace", "payment received, nothing further needed",
     "We have your EUR 149.00. This message confirms the purchase.",
     "Receipt", "easy", None),
    ("Inference Service", "your balance has been topped up",
     "USD 120.00 was taken from the card ending 8814 to add credit.",
     "Receipt", "easy", None),
    ("Proxy Vendor", "payment received for invoice",
     "Receipt for invoice 771402, settled in full on 8 September.",
     "Receipt", "easy", None),
    ("Hosting Billing", "we have your payment (48120366)",
     "Confirming settlement. The full breakdown is in the customer portal.",
     "Receipt", "easy", None),
    ("Electronics Retailer", "order 55913 is confirmed and paid",
     "Total EUR 62.90 on the card ending 3307. Dispatch details follow "
     "separately.", "Receipt", "med", None),
    ("Grocery Chain", "invoice for your shop on 10 September",
     "Total EUR 71.35, settled in full at the till. Keep this for your "
     "records.", "Receipt", "hard",
     "says 'invoice' but the money is already PAID -> Receipt, not Finance"),
    ("Relief Fund", "thank you for your gift",
     "We received EUR 75.00 on 5 September. This note is valid for tax "
     "relief.", "Receipt", "hard",
     "mentions tax, but it is a completed payment -> Receipt, not Finance"),
    ("Parcel Service", "Je pakket is bij ons binnen",
     "Trackingnummer 000009114472. Controleer je bezorgvoorkeuren in de app.",
     "Receipt", "med", "a delivery update counts as Receipt per the prompt"),
    ("Online Retailer", "out for delivery today",
     "Your parcel is on the van and should arrive between 13:00 and 17:00.",
     "Receipt", "med", None),
    ("Outdoor Depot", "je retour is geregistreerd",
     "We hebben je retourzending ontvangen; het bedrag komt binnen vijf "
     "werkdagen terug.", "Receipt", "med", None),
    ("Licensing Desk", "your renewed licence is active",
     "The renewal has been fulfilled. Your contract reference is inside.",
     "Receipt", "hard", "post-payment fulfilment, not a bill -> Receipt"),

    # =====================================================================
    # Finance (14) — money still owed, or account/billing state
    # =====================================================================
    ("Supplier Billing", "invoice NW-2026-4417 is ready",
     "EUR 1,480.00 for September, payable within fourteen days by transfer.",
     "Finance", "easy", None),
    ("Supplier Credit Control", "invoice 6612 is now overdue",
     "EUR 2,950.00 was due on the 1st. Please arrange payment or raise a "
     "dispute.", "Finance", "easy", None),
    ("SaaS Billing", "we could not take your subscription payment",
     "The card was declined for the EUR 39 monthly plan. Update it within "
     "seven days to avoid interruption.", "Finance", "easy", None),
    ("Payments Platform", "a payment on your account was refused",
     "Your bank declined the charge linked to your billing profile. Update "
     "the details to avoid suspension.", "Finance", "easy", None),
    ("Cloud Vendor", "your billing profile needs attention",
     "You are listed as the billing contact. The profile must be completed "
     "before 1 October.", "Finance", "easy", None),
    ("Retail Bank", "your September statement is ready",
     "Closing balance and the full transaction list are available in the app.",
     "Finance", "easy", None),
    ("Card Issuer", "payment due on 27 September",
     "EUR 640.20 is due. Set up a direct debit to avoid late charges.",
     "Finance", "easy", None),
    ("Tax Office", "Btw-aangifte derde kwartaal",
     "De aangifte over het derde kwartaal moet vóór 31 oktober ingediend en "
     "betaald zijn.", "Finance", "med", None),
    ("Payments Provider", "a new tax invoice is waiting",
     "Your invoice covering 1 to 30 September can be viewed in the "
     "dashboard.", "Finance", "med", None),
    ("Direct Debit Bureau", "Wijziging van ons incassonummer",
     "Beste klant, ons incassonummer verandert per 1 november. U hoeft zelf "
     "niets te doen, bewaar deze brief.", "Finance", "med", None),
    ("Uptime Watch", "you have gone past your included checks",
     "Additional check runs are charged per thousand and appear on the next "
     "invoice.", "Finance", "hard",
     "a usage alert that creates a charge -> Finance, not Notification"),
    ("Energy Supplier", "je contract loopt vandaag af",
     "Zonder verlenging schuif je door naar een tarief dat maandelijks kan "
     "wisselen. Verlengen kan in één minuut.", "Finance", "hard",
     "a tariff change affecting what you pay -> Finance, not Discount"),
    ("Insurer", "your premium changes on 1 November",
     "The monthly amount moves from EUR 38 to EUR 44. Cover continues "
     "unchanged.", "Finance", "med", None),
    ("Accountancy Practice", "provisional corporation tax for Q3",
     "The estimate is attached; payment is due by 31 October. Tell me if the "
     "figures look off.", "Finance", "hard",
     "a human sender, but the subject is money you owe -> Finance"),

    # =====================================================================
    # Calendar (12) — a commitment on the recipient's own calendar
    # =====================================================================
    ("Ada Okoye", "Invitation: Robin / Ada @ Tue 16 Sep 11:00",
     "Ada Okoye invites you to an event called Robin / Ada on Tuesday 16 "
     "September.", "Calendar", "easy", None),
    ("Milan Horvat", "Updated invitation: platform sync @ weekly 15:00",
     "You are invited to platform sync, repeating weekly from 15:00 to 15:30.",
     "Calendar", "easy", None),
    ("Nadia El-Amin", "Invitation: audit walkthrough @ Wed 24 - Thu 25 Sep",
     "Nadia El-Amin invites you to an event called audit walkthrough.",
     "Calendar", "easy", None),
    ("Jonas Werner", "Accepted: half-year check-in @ Fri 19 Sep 14:00",
     "Jonas Werner accepted. When: Friday 19 September, 14:00 to 14:45.",
     "Calendar", "med", None),
    ("Sofie Aalto", "Accepted: layout deep dive @ Mon 22 Sep 10:00",
     "Sofie Aalto accepted. When: Monday 22 September, 10:00 to 11:00.",
     "Calendar", "med", None),
    ("Diego Salas", "Declined: capacity review @ Thu 18 Sep 16:00",
     "Diego Salas declined and added: clashes with the reseller call.",
     "Calendar", "med", None),
    ("Booking Link", "a discovery call was scheduled",
     "Beatrix Hollander booked 25 September at 15:30 CEST for thirty minutes.",
     "Calendar", "easy", None),
    ("Meeting Rooms", "iteration review", "Iteration review is set for "
     "Wednesday 17 September, 13:00 to 14:00 CEST. The joining link is "
     "below.", "Calendar", "easy", None),
    ("Tandartspraktijk", "Herinnering: controle op donderdag 09:30",
     "Dit is een herinnering voor uw controle op donderdag 18 september om "
     "09:30. Antwoord met AFZEGGEN om te annuleren.", "Calendar", "easy", None),
    ("Sofie Aalto", "standup shifts on Monday",
     "From Monday the standup runs at 10:15 rather than 09:30, same room. No "
     "reply needed, just move it in your calendar.", "Calendar", "hard",
     "a scheduling change with no RSVP is still Calendar"),
    ("Ingrid Solberg", "notes from this morning's kickoff",
     "Thanks for the session. Minutes and next week's agenda are attached.",
     "Calendar", "hard", "the prompt files minutes and agendas under Calendar"),
    ("Meeting Rooms", "Cancelled: reseller sync @ Fri 19 Sep 09:00",
     "The organiser cancelled this event and it has left your calendar.",
     "Calendar", "med", None),

    # =====================================================================
    # Travel (10)
    # =====================================================================
    ("Airline", "e-ticket: Rotterdam (RTM) to Porto (OPO)",
     "Booking reference QP4T7M. Departs 2 October at 09:15. Thank you for "
     "flying with us.", "Travel", "easy", None),
    ("Airline", "check-in has opened for QP4T7M",
     "Choose a seat and download your boarding pass for the 2 October "
     "departure.", "Travel", "hard",
     "an automated travel notice is Travel, not Notification"),
    ("Airline", "your connection time has changed",
     "The gap in Lisbon is now 1h05 rather than 2h20. Nothing else has "
     "changed.", "Travel", "med", None),
    ("National Rail", "Je e-ticket voor 20 september",
     "Rotterdam Centraal naar Groningen, tweede klas. Toon de QR-code aan de "
     "conducteur.", "Travel", "easy", None),
    ("Coach Operator", "your booking 2284517",
     "This message is not a travel document. Sign in to download the ticket "
     "before departure.", "Travel", "med", None),
    ("Hotel Group", "reservation confirmed for two nights",
     "2 to 4 October, check-in from 14:00. Free cancellation until 30 "
     "September.", "Travel", "easy", None),
    ("Car Hire", "your vehicle is reserved",
     "Collect at the airport desk on 2 October at 11:00, return on the 4th at "
     "18:00.", "Travel", "easy", None),
    ("Airport Services", "your security slot is booked",
     "Arrive at the marked entrance ten minutes before the slot opens on 2 "
     "October.", "Travel", "med", None),
    ("Border Agency", "your travel authorisation expires soon",
     "The authorisation issued on 4 October 2024 lapses within thirty days. "
     "You cannot travel on it after that.", "Travel", "hard",
     "a document expiry that gates travel -> Travel"),
    ("Airline", "your boarding pass is ready",
     "Gate B12, boarding at 08:35. Add it to your wallet.",
     "Travel", "easy", None),

    # =====================================================================
    # "" no tag (24) — nothing to do, own sent mail, closing pleasantries
    #
    # By volume this class is tiny, but it is where "Needs reply" false
    # positives land: a model that tags a thank-you as actionable erodes trust
    # in the tag far faster than a missed newsletter does. These cases exist to
    # measure NEEDS-REPLY PRECISION.
    # =====================================================================
    ("Old Housemate", "long time",
     "Ages since we spoke. Saw the photos from the coast, looked idyllic. "
     "Hope life is good.", "", "easy", None),
    ("Milan Horvat", "Re: the extractor patch",
     "Brilliant, thanks for turning that round so quickly.", "", "easy", None),
    ("Beatrix Hollander", "Re: pilot numbers",
     "Understood, nothing further from me.", "", "easy", None),
    ("Clara Beaumont", "well deserved",
     "Just saw the launch announcement. Congratulations to you and the whole "
     "team.", "", "easy", None),
    ("Nadia El-Amin", "photos from Friday",
     "The album from the team dinner is up. There are some regrettable ones "
     "of Diego with a microphone.", "", "easy", None),
    ("Diego Salas", "worth a read",
     "Came across this piece on document automation in insurance. Nothing "
     "needed from you, just thought it was relevant.", "", "hard",
     "explicitly says nothing is needed -> no tag, not Needs reply"),
    ("Pavel Novotny", "Re: specialist availability",
     "Thank you for coming back to me. Do get in touch if the picture "
     "changes. Enjoy the rest of your week.", "", "hard",
     "a polite close to a finished thread -> no tag, not Needs reply"),
    ("Jonas Werner", "Re: the review outcome",
     "That is good to hear. Thanks for sorting it out and for the candour. "
     "Looking forward to catching up.", "", "hard",
     "a thank-you closing an exchange -> no tag, not Needs reply"),
    ("Milan Horvat", "Re: Re: passing thought",
     "Ha, that is precisely what I was thinking. Anyway, disregard me.",
     "", "hard", None),
    ("Femke Visser", "dank!",
     "Bedankt voor de snelle reactie, keurig geregeld.", "", "med", None),
    ("Andreas Keller", "Re: Terminbestätigung",
     "Alles klar, danke für die Rückmeldung. Bis dann.", "", "med", None),
    ("Idris Bello", "good to have you with us",
     "Just a note to say welcome aboard — no need to reply, we will catch up "
     "properly on Monday.", "", "hard", "explicitly says no reply needed"),
    ("Marta Lindqvist", "for visibility",
     "Passing this along so you have seen it. The ticket is already handled.",
     "", "hard", None),
    ("robin@example.com", "Re: workshop dates",
     "Hi Beatrix, the 11th is confirmed at our end. The invoice will come "
     "separately. Best, Robin", "", "hard",
     "the recipient's OWN sent reply, which reads exactly like Needs reply"),
    ("robin@example.com", "Re: the extractor patch",
     "Merged and out to production. Thanks all.", "", "hard", "own sent mail"),
    ("robin@example.com", "RE: backend specialists",
     "Hi Ruben, good question. We recruit in house, and normally through the "
     "usual job boards.", "", "hard", "own sent reply answering someone else"),
    ("Former Colleague", "spotted you",
     "Saw you on stage at the meetup — went down well from where I was "
     "standing.", "", "easy", None),
    ("Dean Whitfield", "Re: remove me",
     "No trouble at all, you are off the list. All the best.",
     "", "med", None),
    ("Ada Okoye", "Re: handover",
     "I will draft the handover plan and pick it up next week once Tomas is "
     "back.", "", "hard",
     "an informational FYI in a thread, nothing asked of the recipient"),
    ("Sofie Aalto", "no action",
     "Logging this so it is written down somewhere: the flake did not "
     "reappear after the retry guard.", "", "med", None),
    ("Grace Mbeki", "Re: policy document",
     "Received with thanks, that covers what the auditors wanted.",
     "", "med", None),
    ("Neighbour", "parcel",
     "Took in a box for you this afternoon, it is in our porch whenever suits.",
     "", "med", None),
    ("Owen Castellanos", "Re: pilot kickoff",
     "Noted, thanks. Nothing needed from your side at this stage.",
     "", "hard", None),
    ("Ingrid Solberg", "Re: minutes",
     "Perfect, that matches my notes exactly.", "", "easy", None),
]


# High-precision markers only. A real inbox in Europe is not English-only and
# small models degrade noticeably on non-English input, so accuracy is reported
# per language too. Better to label a Dutch case "en" than to mislabel an
# English one, hence short, distinctive word lists rather than broad stopwords.
_NL = (" je ", " jij ", " uw ", " niet ", " voor de ", " geen ", "dankjewel",
       "bedankt", "vriendelijke", "afspraak", "wij ", "onze ", " het ",
       "herinnering", "bevestiging", "korting", "vandaag", "aangekomen",
       "ontdek", "beste ", "hallo ", "goedemorgen", "benieuwd", "voorstel")
_DE = ("vielen dank", "freundlichen", "ihre ", "ihr ticket", "keine",
       "termin", "wurde", "antwort", "alles klar", "bestaetigung",
       "techniker", "erforderlich", "bitte", "danke", "rückmeldung",
       "zurücksetzung", "vorgang")


def language(text):
    """-> 'ru' | 'nl' | 'de' | 'en' for reporting per-language accuracy."""
    if any("Ѐ" <= ch <= "ӿ" for ch in text):
        return "ru"
    low = " " + text.lower() + " "
    nl = sum(1 for m in _NL if m in low)
    de = sum(1 for m in _DE if m in low)
    if nl > de and nl >= 2:
        return "nl"
    if de > nl and de >= 2:
        return "de"
    return "en"


def load():
    """
    -> (cases, meta)

    Each case is a dict with the PRODUCTION item string mailbox builds in
    internal/aiwork/worker.go:
        "From: %s / Subject: %s / %s"
    """
    cases = []
    for i, (who, subj, snip, label, diff, probe) in enumerate(CASES):
        item = f"From: {who} / Subject: {subj} / {snip}"
        cases.append({
            "id": i,
            "item": item,
            "label": label,
            "difficulty": diff,
            "probe": probe,
            "lang": language(item),
        })
    meta = {
        "source": "synthetic (bench/cases.py) -- safe to commit",
        "scored": len(cases),
        "categories": len({c["label"] for c in cases}),
        "probes": sum(1 for c in cases if c["probe"]),
    }
    return cases, meta


if __name__ == "__main__":
    import collections
    import json as _json
    c, m = load()
    print(_json.dumps(m, indent=2))
    print("\nlabel distribution:")
    for k, v in collections.Counter(x["label"] for x in c).most_common():
        print(f"  {k or '(no tag)':<14} {v}")
    print("\ndifficulty:")
    for k, v in collections.Counter(x["difficulty"] for x in c).most_common():
        print(f"  {k:<6} {v}")
    print("\nlanguage:")
    for k, v in collections.Counter(x["lang"] for x in c).most_common():
        print(f"  {k:<6} {v}")
    print(f"\nboundary probes: {m['probes']}")
