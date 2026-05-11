package paths

import "strconv"

// ProjectPath returns the path identity for a project.
func ProjectPath(projectName string) string {
	return "projects/" + projectName
}

// WorkflowPath returns the path identity for a workflow registered to a project.
func WorkflowPath(projectName, workflowName string) string {
	return ProjectPath(projectName) + "/workflows/" + workflowName
}

// PhasePath returns the path identity for a phase declared in a workflow.
func PhasePath(projectName, workflowName, phaseName string) string {
	return WorkflowPath(projectName, workflowName) + "/phases/" + phaseName
}

// RunPath returns the path identity for a run on a project.
func RunPath(projectName, runID string) string {
	return ProjectPath(projectName) + "/runs/" + runID
}

// AttemptPath returns the path identity for one attempt within a run.
func AttemptPath(projectName, runID string, attemptIndex int) string {
	return RunPath(projectName, runID) + "/attempts/" + strconv.Itoa(attemptIndex)
}

// JobPath returns the path identity for a native k8s_job within an attempt.
func JobPath(projectName, runID string, attemptIndex int, jobID string) string {
	return AttemptPath(projectName, runID, attemptIndex) + "/jobs/" + jobID
}

// StepPath returns the path identity for a step within a job attempt.
func StepPath(
	projectName string,
	runID string,
	attemptIndex int,
	jobID string,
	stepSlug string,
) string {
	return JobPath(projectName, runID, attemptIndex, jobID) + "/steps/" + stepSlug
}
