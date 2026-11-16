package updates_test

import (
	"os"
	"testing"

	"github.com/zaentrum/zaentrum-operator/operator/internal/offline"
)

// TestMain runs the package's tests offline: see offline.Run. It is in the
// external test package, as offline imports this package for the URL its
// stand-in answers.
func TestMain(m *testing.M) { os.Exit(offline.Run(m)) }
