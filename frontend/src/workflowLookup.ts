type WorkflowLookupItem = {
  project: string;
  name: string;
};

export function resolveProjectWorkflow<T extends WorkflowLookupItem>(
  workflows: T[],
  project: string,
  candidateNames: Array<string | null | undefined>,
): T | null {
  const projectWorkflows = workflows.filter((workflow) => workflow.project === project);
  for (const name of candidateNames) {
    if (!name) continue;
    const exact = projectWorkflows.find((workflow) => workflow.name === name);
    if (exact) return exact;
  }
  if (projectWorkflows.length === 1) {
    return projectWorkflows[0];
  }
  return null;
}
