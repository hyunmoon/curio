package sdrscratch

// These APIs attest process-subtree lifetime only. They neither inspect nor
// delete scratch files, and are not a task/sector success or liveness oracle.
func TaskExecutionIdentity() (string, error) {
	s := personalSessionPtr.Load()
	if s == nil {
		return "", nil // Unmanaged/old workers have no termination evidence.
	}
	return taskExecutionIdentity(s)
}

func TaskExecutionStopped(identity string) (bool, error) {
	s := personalSessionPtr.Load()
	if s == nil || identity == "" {
		return false, nil
	}
	return taskExecutionStopped(s, identity)
}
