// Fixture data for the Glimmung dashboard UI kit.
window.GlimmungData = {
  projects: [
    { name: "spirelens", github_repo: "nelsong6/spirelens" },
    { name: "ambience", github_repo: "nelsong6/ambience" },
    { name: "tank-operator", github_repo: "nelsong6/tank-operator" },
  ],
  workflows: [
    { project: "spirelens", name: "issue-agent", workflow_filename: "issue-agent.yml", workflow_ref: "main", trigger_label: "agent-run", default_requirements: { runtime: "x11", gpu: true } },
    { project: "spirelens", name: "agent-retry", workflow_filename: "agent-retry.yml", workflow_ref: "main", trigger_label: "", default_requirements: { runtime: "x11", gpu: true } },
    { project: "ambience", name: "issue-agent", workflow_filename: "issue-agent.yml", workflow_ref: "main", trigger_label: "agent-run", default_requirements: { runtime: "linux" } },
    { project: "tank-operator", name: "deploy-agent", workflow_filename: "deploy.yml", workflow_ref: "main", trigger_label: "deploy", default_requirements: { runtime: "linux", privileged: true } },
  ],
  hosts: [
    { name: "x11-gpu-01", capabilities: { runtime: "x11", gpu: true, ram_gb: 32 }, current_lease_id: null, drained: false, last_heartbeat: "2026-05-01T12:00:00Z", last_used_at: "2026-05-01T11:57:00Z" },
    { name: "x11-gpu-02", capabilities: { runtime: "x11", gpu: true, ram_gb: 32 }, current_lease_id: "01HKQX2A7C3F9D6E4B5G2H1J8K", drained: false, last_heartbeat: "2026-05-01T12:00:30Z", last_used_at: "2026-05-01T12:00:30Z" },
    { name: "linux-04", capabilities: { runtime: "linux", ram_gb: 16 }, current_lease_id: "01HKQX2B8D4G0E7F5C6H3J2K9L", drained: false, last_heartbeat: "2026-05-01T12:00:25Z", last_used_at: "2026-05-01T12:00:25Z" },
    { name: "linux-05", capabilities: { runtime: "linux", ram_gb: 16 }, current_lease_id: null, drained: true, last_heartbeat: "2026-04-29T08:00:00Z", last_used_at: "2026-04-29T08:00:00Z" },
  ],
  pending_leases: [
    { id: "01HKQX3C9E5H1F8G6D7J4K3L0M", project: "spirelens", workflow: "issue-agent", state: "pending", requirements: { runtime: "x11", gpu: true }, metadata: { issue: 42 }, requested_at: "2026-05-01T11:59:30Z", host: null, assigned_at: null },
  ],
  active_leases: [
    { id: "01HKQX2A7C3F9D6E4B5G2H1J8K", project: "spirelens", workflow: "issue-agent", state: "active", host: "x11-gpu-02", requirements: { runtime: "x11", gpu: true }, metadata: { issue: 41 }, requested_at: "2026-05-01T11:55:00Z", assigned_at: "2026-05-01T11:55:02Z" },
    { id: "01HKQX2B8D4G0E7F5C6H3J2K9L", project: "ambience", workflow: "issue-agent", state: "active", host: "linux-04", requirements: { runtime: "linux" }, metadata: { issue: 17 }, requested_at: "2026-05-01T11:50:00Z", assigned_at: "2026-05-01T11:50:01Z" },
  ],
  issues: [
    { id: "i_001", project: "spirelens", repo: "nelsong6/spirelens", number: 42, title: "agent loops on rare card prediction", labels: ["agent-run", "bug"], last_run_state: "in_progress", issue_lock_held: true },
    { id: "i_002", project: "spirelens", repo: "nelsong6/spirelens", number: 41, title: "verifier crashes on empty deck", labels: ["agent-run"], last_run_state: "passed", issue_lock_held: false },
    { id: "i_003", project: "ambience", repo: "nelsong6/ambience", number: 17, title: "fade transitions skip frame on resize", labels: ["agent-run", "agent-budget:5x50"], last_run_state: "in_progress", issue_lock_held: true },
    { id: "i_004", project: "tank-operator", repo: "nelsong6/tank-operator", number: 8, title: "deploy hangs when secret rotation overlaps", labels: [], last_run_state: null, issue_lock_held: false },
    { id: "i_005", project: "spirelens", repo: "nelsong6/spirelens", number: 40, title: "support multi-elite encounters in plan", labels: [], last_run_state: "aborted", issue_lock_held: false },
  ],
  prs: [
    { id: "p_001", project: "spirelens", repo: "nelsong6/spirelens", pr_number: 124, title: "fix verifier crash on empty deck", run_state: "passed", run_attempts: 1, run_cumulative_cost_usd: 1.84, issue_number: 41, pr_lock_held: false },
    { id: "p_002", project: "ambience", repo: "nelsong6/ambience", pr_number: 56, title: "rework fade timing on resize", run_state: "in_progress", run_attempts: 2, run_cumulative_cost_usd: 4.20, issue_number: 17, pr_lock_held: true },
    { id: "p_003", project: "tank-operator", repo: "nelsong6/tank-operator", pr_number: 19, title: "manual: bump pinned image", run_state: null, run_attempts: 0, run_cumulative_cost_usd: 0, issue_number: null, pr_lock_held: false },
  ],
};
