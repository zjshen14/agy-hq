package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripInvisibleUnicodeAndFindViolations(t *testing.T) {
	vaultDir := t.TempDir()
	priv := filepath.Join(vaultDir, "private")
	if err := os.MkdirAll(priv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "pii-denylist.txt"), []byte("# comment\nSecretCodename\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Zero-width space evasion inside denied term must still be caught after stripping
	evasive := "Status update for Secret\u200bCodename at /home/user/work/file"
	vs := FindViolations(vaultDir, evasive, []string{"q4-calibration"})
	var sawPII, sawHome bool
	for _, v := range vs {
		if v.Kind == "pii_denylist" {
			sawPII = true
		}
		if v.Kind == "home_path" {
			sawHome = true
		}
	}
	if !sawPII || !sawHome {
		t.Fatalf("expected pii_denylist and home_path violations, got %+v", vs)
	}

	// Harness token detection
	hv := FindViolations(vaultDir, "Please approve prop-1795208512-abcdef123456 now", nil)
	if len(hv) == 0 || hv[0].Kind != "harness_token" {
		t.Fatalf("expected harness_token violation, got %+v", hv)
	}

	// ScrubForUntrustedOutput must redact vault path, secrets, and denylist terms
	scrubbed := ScrubForUntrustedOutput(vaultDir, "Vault="+vaultDir+" term=SecretCodename tok=prop-1795208512-abcdef123456")
	if strings.Contains(scrubbed, vaultDir) || strings.Contains(scrubbed, "SecretCodename") || strings.Contains(scrubbed, "prop-1795208512") {
		t.Fatalf("expected sensitive items scrubbed, got %q", scrubbed)
	}
}

func TestHasHighEntropyBlob(t *testing.T) {
	blob := "QmFzZTY0RW5jb2RlZFNlY3JldFBheWxvYWQxMjM0NTY3ODkwQUJDREVGR0hJSktMTU5PUA=="
	if _, ok := HasHighEntropyBlob("Reply: "+blob, nil); !ok {
		t.Fatalf("expected high-entropy base64 blob to be flagged")
	}
	if _, ok := HasHighEntropyBlob("Reply: "+blob, []string{"ci output had " + blob}); ok {
		t.Fatalf("expected blob present verbatim in allowVerbatim to pass")
	}
	if _, ok := HasHighEntropyBlob("Normal build status: billing-v2 nightly green across all 12 suites.", nil); ok {
		t.Fatalf("expected normal English text not to trigger high-entropy detector")
	}
	if _, ok := HasHighEntropyBlob("Reviewed PreCombineKeyMessagesFn/KeyMessagesWithIndexFn and //pkg/service/common/proto_utils:delta_processor_test.", nil); ok {
		t.Fatalf("expected CamelCase code symbols and build target labels not to trigger high-entropy detector")
	}
}

func TestSharedPatternsAndSecretSSNDetection(t *testing.T) {
	patterns := DefaultSecretPatterns()
	if len(patterns) < 7 {
		t.Fatalf("expected >= 7 default secret patterns, got %d", len(patterns))
	}
	if !HomeDirPathPattern().MatchString("/home/user/repo") {
		t.Fatalf("expected HomeDirPathPattern to match home path")
	}
	syntheticSSN := "987-" + "65-4321"
	if !SSNPattern().MatchString("ssn " + syntheticSSN) {
		t.Fatalf("expected SSNPattern to match SSN")
	}

	fakeGH := "ghp_" + strings.Repeat("A", 36)
	vs := FindViolations("", "token="+fakeGH+" ssn="+syntheticSSN, nil)
	var sawSecret, sawSSN bool
	for _, v := range vs {
		if v.Kind == "secret" {
			sawSecret = true
		}
		if v.Kind == "pii_ssn" {
			sawSSN = true
		}
	}
	if !sawSecret || !sawSSN {
		t.Fatalf("expected secret and pii_ssn violations, got %+v", vs)
	}
}
