package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsAreRemovedAndSurroundingTextKept(t *testing.T) {
	secrets := []string{
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_" + strings.Repeat("a1B2", 8),
		"xoxb-123456789012-abcdefghij",
		"sk-" + strings.Repeat("Zz9", 10),
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r",
		"hunter2hunter2",
		"s3cr3tPassw0rd",
		"abcdef0123456789abcdef",
	}
	input := strings.Join([]string{
		"deploy with key " + secrets[0],
		"token " + secrets[1],
		"slack " + secrets[2],
		"openai " + secrets[3],
		"jwt " + secrets[4],
		"DB_PASSWORD=" + secrets[5],
		"curl https://admin:" + secrets[6] + "@host.example/x",
		"Authorization: Bearer " + secrets[7],
		"-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----",
	}, "\n")
	res := Default().Apply(input, nil, false)
	for _, s := range secrets {
		if strings.Contains(res.Content, s) {
			t.Errorf("secret %q survived: %s", s, res.Content)
		}
	}
	if strings.Contains(res.Content, "MIIBOg") {
		t.Error("private key body survived")
	}
	for _, keep := range []string{"deploy with key", "DB_PASSWORD=", "https://admin:", "@host.example/x", "Authorization: Bearer "} {
		if !strings.Contains(res.Content, keep) {
			t.Errorf("redaction removed surrounding text %q: %s", keep, res.Content)
		}
	}
	if res.Redactions != 9 {
		t.Errorf("redactions = %d, want 9", res.Redactions)
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	in := "Fix refresh-token race in src/auth/token.ts; token count is 5 and the password field is validated."
	res := Default().Apply(in, nil, false)
	if res.Content != in || res.Redactions != 0 {
		t.Fatalf("benign text changed: %q (%d)", res.Content, res.Redactions)
	}
}

func TestRedactionIsIdempotent(t *testing.T) {
	once := Default().Apply("API_KEY=supersecretvalue1", nil, false)
	twice := Default().Apply(once.Content, nil, false)
	if twice.Content != once.Content || twice.Redactions != 0 {
		t.Fatalf("second pass changed output: %q", twice.Content)
	}
}

func TestDeniedPaths(t *testing.T) {
	p := Default()
	for _, path := range []string{".env", "app/.env.production", "certs/server.pem", "/home/u/.ssh/id_rsa", "./config/credentials.json", "home/.aws/credentials"} {
		if !p.DeniedPath(path) {
			t.Errorf("%s should be denied", path)
		}
	}
	for _, path := range []string{"src/environment.ts", "README.md", "src/auth/token.ts", "docs/keys.md"} {
		if p.DeniedPath(path) {
			t.Errorf("%s should be allowed", path)
		}
	}
}

func TestDeniedPathWithholdsToolPayloadEntirely(t *testing.T) {
	p := Default()
	byPath := p.Apply("DEBUG=1\nPLAIN=value", []string{"app/.env"}, true)
	byMention := p.Apply(`{"command":"cat .env"}`, nil, true)
	for name, r := range map[string]Result{"path": byPath, "mention": byMention} {
		if !r.Denied || strings.Contains(r.Content, "PLAIN") || strings.Contains(r.Content, "cat") {
			t.Errorf("%s: payload not withheld: %+v", name, r)
		}
	}
	// A user message that merely talks about .env is not a tool payload.
	if r := p.Apply("my .env file is broken", nil, false); r.Denied {
		t.Error("user message withheld")
	}
}

func TestLoadExtendsDefaultsAndFailsClosedOnInvalidPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.json"),
		[]byte(`{"patterns":[{"name":"ticket","regex":"CORP-[0-9]{6}"}],"denied_paths":["vault/*"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Apply("see CORP-123456", nil, false); strings.Contains(r.Content, "123456") {
		t.Errorf("custom pattern ignored: %q", r.Content)
	}
	if !p.DeniedPath("vault/db") || !p.DeniedPath(".env") {
		t.Error("custom or default denied path missing")
	}
	if r := p.Apply("AKIAIOSFODNN7EXAMPLE", nil, false); r.Redactions != 1 {
		t.Error("built-in rule lost after loading policy")
	}

	for _, bad := range []string{`{`, `{"patterns":[{"name":"x","regex":"("}]}`, `{"patterns":[{"name":"","regex":"a"}]}`, `{"denied_paths":["["]}`} {
		if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Errorf("invalid policy %q accepted", bad)
		}
	}
	if p, err := Load(t.TempDir()); err != nil || p == nil {
		t.Fatalf("missing policy file must yield defaults: %v", err)
	}
}
