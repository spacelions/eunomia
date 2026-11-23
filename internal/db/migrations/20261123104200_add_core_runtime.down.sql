DROP TABLE IF EXISTS interaction_tasks;

--bun:split

DROP TABLE IF EXISTS case_runs;

--bun:split

DROP TABLE IF EXISTS playbook_drafts;

--bun:split

DROP TRIGGER IF EXISTS trg_playbooks_immutable ON playbooks;

--bun:split

DROP FUNCTION IF EXISTS prevent_playbook_update();

--bun:split

DROP TABLE IF EXISTS playbooks;
