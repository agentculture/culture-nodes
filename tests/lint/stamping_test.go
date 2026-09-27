package testslint

import "testing"

func TestStampingModuleIsIdenticalInEveryAdapter(t *testing.T) {
	assertModuleIsIdenticalEverywhere(t, "stamping.py")
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, "stamping.py") {
			t.Errorf("%s lacks stamping.py", pkg.adapter)
		}
	}
}
