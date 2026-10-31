package main

import (
	"os"
	"strings"
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

func TestHostInstallSnippetAndScript(t *testing.T) {
	got := installSnippet("server", "clu_123", "tok456")
	if !strings.Contains(got, "install-host.sh | sudo sh -s -- --id clu_123 --token tok456") {
		t.Fatalf("server snippet = %q", got)
	}
	if k8s := installSnippet("kubernetes", "clu_1", "t"); !strings.Contains(k8s, "kubectl apply -f") {
		t.Fatalf("kubernetes snippet = %q", k8s)
	}
	for _, want := range []string{"User=clusterheart", "NoNewPrivileges=yes", "--uninstall", "chmod 0640"} {
		if !strings.Contains(hostInstallScript, want) {
			t.Errorf("install script missing %q", want)
		}
	}
}
