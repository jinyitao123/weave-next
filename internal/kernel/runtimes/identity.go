package runtimes

// RuntimeWorkerID identifies one runtime's task lease within its workspace.
func RuntimeWorkerID(workspaceID, runtimeID string) string {
	return "runtime:" + workspaceID + ":" + runtimeID
}
