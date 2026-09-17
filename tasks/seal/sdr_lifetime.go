package seal

import "github.com/filecoin-project/curio/lib/sdrscratch"

func (*SDRTask) TaskExecutionIdentity() (string, error) {
	return sdrscratch.TaskExecutionIdentity()
}

func (*SDRTask) TaskExecutionStopped(identity string) (bool, error) {
	return sdrscratch.TaskExecutionStopped(identity)
}
