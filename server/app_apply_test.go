package main

import (
	"os"
	"testing"
)

// The served manifest must match the one in deploy/ (the source of truth).
func TestRemediationManifestInSync(t *testing.T) {
	want, err := os.ReadFile("../deploy/k8s/remediation/remediation.yaml")
	if err != nil {
		t.Skip("deploy manifest not available:", err)
	}
	if string(want) != remediationManifest {
		t.Fatal("server/remediation.yaml is out of sync with deploy/k8s/remediation/remediation.yaml; copy it over")
	}
}
