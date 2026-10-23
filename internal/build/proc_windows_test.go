//go:build windows

package build

import "testing"

// checkGrandchildKilled is not verified on Windows: taskkill /T is used
// and process inspection would need extra dependencies.
func checkGrandchildKilled(t *testing.T, pidFile string) { t.Helper() }
