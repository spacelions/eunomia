---
playbook_id: internal-employee-onboarding
version: 1
name: Internal Employee Onboarding
description: Onboards a new Space Lions employee end-to-end.
allowed_connectors:
  - google_workspace
  - notion
required_tools:
  - google_workspace.create_user
  - notion.upsert_db_row
require_approval: false
max_turns: 30
max_duration: 168h
---

# Internal Employee Onboarding

Inputs provided at launch: `employee_name`, `personal_email`.

1. **Pick Company Email:** Ask the new hire (at `personal_email`) via Gmail Magic Link to pick an available `@spacelions.com` email prefix (validate live with `google_workspace.username_available`). Remind after 24h; escalate to IT Ops after 48h.
2. **Provision Google Account:** Call `google_workspace.create_user` and email temporary credentials to `personal_email`.
3. **AI Tools Survey:** Ask the new hire at their new work email via Gmail Magic Link which AI tools they need (`gemini`, `claude`). Remind after 24h; escalate to IT Ops after 48h.
4. **Provision AI Tools (if requested):** If they requested any AI tools, ask IT Ops to provision them and mark done with notes. Remind after 24h; escalate to People Ops after 48h.
5. **Closeout:** Upsert a row in Notion `onboarding_tracker` with their details and status `Onboarded`, then email a completion summary to the person who initiated this run.
