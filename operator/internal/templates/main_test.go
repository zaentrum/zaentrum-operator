package templates

import (
	"os"
	"testing"

	"github.com/zaentrum/zaentrum-operator/operator/internal/offline"
)

// TestMain runs the package's tests offline: see offline.Run.
func TestMain(m *testing.M) { os.Exit(offline.Run(m)) }
