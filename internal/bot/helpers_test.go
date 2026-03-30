package bot

import (
	"testing"

	"go.uber.org/zap"
)

func nopLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zap.NewNop()
}
