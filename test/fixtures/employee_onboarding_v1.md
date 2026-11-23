---
schema_version: v1
playbook_id: internal-employee-onboarding
version: 1
name: Internal Employee Onboarding
description: Provisions Google Workspace account, collects AI tool preferences, and syncs the Notion onboarding tracker.
owner: role:people_ops
trigger:
  archetype: requestor
  inputs:
    employee_name:
      type: string
      description: Full name of the new hire
    personal_email:
      type: email
      description: Personal email used before company account exists
state:
  desired_prefix:
    type: string
    description: Preferred username prefix before @spacelions.com
  work_email:
    type: email
    description: Provisioned Google Workspace email
  temp_password:
    type: string
    description: One-time temporary Google Workspace password
    sensitive: true
  ai_tools:
    type: string_list
    options:
      - gemini
      - claude
    description: Requested AI coding/productivity tools
  provision_notes:
    type: string
    description: Receipt notes from IT Ops after provisioning AI tools
roles:
  it_ops:
    archetype: fulfiller
    binding: role:it_ops
    description: Internal IT Ops team handling manual AI seat provisioning
  escalation_owner:
    archetype: escalation_owner
    binding: role:it_ops
    description: Receives SLA breach alerts for stalled human steps
connectors:
  - name: google_workspace
    operations:
      - create_user
      - send_email
      - username_available
  - name: notion
    operations:
      - upsert_db_row
    resources:
      - onboarding_tracker
limits:
  max_turns: 30
  max_duration: 168h
required_outcomes:
  state_keys:
    - work_email
  operations:
    - google_workspace.create_user
    - notion.upsert_db_row
---

# Internal Employee Onboarding

## Goal

Onboard a new employee end-to-end: let them pick an available `@spacelions.com` email prefix, provision their Google Workspace account, collect AI tool preferences, coordinate manual IT provisioning if requested, and sync the Notion onboarding tracker.

## Steps

1. **Pick Company Email (`ground_truth_provider`)**
   - Send a Gmail Magic Link to `personal_email` asking for `desired_prefix` with live `google_workspace.username_available` validation.
   - SLA: remind after `24h`, escalate to `escalation_owner` after `48h`.

2. **Provision Google Workspace Account**
   - Call `google_workspace.create_user` using `employee_name`, `desired_prefix`, and `personal_email`.
   - Store `work_email` and sensitive `temp_password` in Case State, then email temporary credentials to `personal_email`.

3. **AI Tools Access Survey (`ground_truth_provider`)**
   - Send a Gmail Magic Link to `work_email` asking which AI tools they need (`gemini`, `claude`).
   - SLA: remind after `24h`, escalate to `escalation_owner` after `48h`.

4. **Manual AI Tool Provisioning (`fulfiller`, conditional)**
   - If the employee requested any AI tools, assign a `fulfiller` task to `it_ops` with a "Mark Done" receipt (`provision_notes`).
   - SLA: remind after `24h`, escalate to `workflow.owner` after `48h`.

5. **Closeout (`observer`)**
   - Upsert a row into Notion resource `onboarding_tracker` with `employee_name`, `work_email`, `ai_tools`, and status `Onboarded`.
   - Send a completion summary email to `run.initiator`.
