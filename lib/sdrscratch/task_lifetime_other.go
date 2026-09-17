//go:build !linux

package sdrscratch

func taskExecutionIdentity(*personalSession) (string, error)      { return "", nil }
func taskExecutionStopped(*personalSession, string) (bool, error) { return false, nil }
