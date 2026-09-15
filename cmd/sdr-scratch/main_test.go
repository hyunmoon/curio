package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectApprovalAndDiscoveryBypasses(t *testing.T) {
	for _, args := range [][]string{nil, {"apply", "--yes"}, {"apply", "--force"}, {"apply", "--all"}, {"preview", "--sectors", "*"}, {"run", "--yes"}, {"apply", "unexpected"}} {
		require.Error(t, run(args))
	}
}
