// Package redact applies the security filtering policy to interaction content
// before anything is persisted. It is deliberately independent of retention
// modelling: a classifier is never a redactor.
package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Placeholder prefix used for every removed span.
const placeholderPrefix = "[REDACTED:"

type rule struct {
	name string
	re   *regexp.Regexp
	// group, when >0, limits replacement to that capture group so the key
	// name stays visible ("password=[REDACTED:...]").
	group int
}

var builtinRules = []rule{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`), 0},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"github-token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`), 0},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), 0},
	{"api-key", regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{20,}\b`), 0},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), 0},
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})`), 1},
	{"url-credentials", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`), 1},
	{"secret-assignment", regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[A-Za-z0-9_.-]*["']?\s*[:=]\s*["']?([^\s"',;]{4,})`), 1},
}

// Default denied path globs: matched against every path segment sequence.
var defaultDeniedPaths = []string{
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".npmrc", ".pypirc", ".netrc", ".git-credentials", "credentials", "credentials.json", "secrets.*", ".aws/credentials",
}

// Policy is a compiled filtering policy.
type Policy struct {
	rules  []rule
	denied []string
}

// fileConfig is the optional .curator/policy.json.
type fileConfig struct {
	Patterns    []filePattern `json:"patterns"`
	DeniedPaths []string      `json:"denied_paths"`
}

type filePattern struct {
	Name  string `json:"name"`
	Regex string `json:"regex"`
}

// Default returns the built-in policy.
func Default() *Policy {
	return &Policy{rules: builtinRules, denied: defaultDeniedPaths}
}

// Load returns the built-in policy extended by storeDir/policy.json when it
// exists. An unreadable or invalid policy file is an error: ingestion must
// fail closed rather than persist unfiltered content.
func Load(storeDir string) (*Policy, error) {
	p := Default()
	data, err := os.ReadFile(filepath.Join(storeDir, "policy.json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse policy.json: %w", err)
	}
	p = &Policy{rules: append([]rule(nil), builtinRules...), denied: append([]string(nil), defaultDeniedPaths...)}
	for _, fp := range cfg.Patterns {
		if fp.Name == "" || fp.Regex == "" {
			return nil, errors.New("policy.json: pattern needs name and regex")
		}
		re, err := regexp.Compile(fp.Regex)
		if err != nil {
			return nil, fmt.Errorf("policy.json: pattern %q: %w", fp.Name, err)
		}
		p.rules = append(p.rules, rule{name: fp.Name, re: re})
	}
	for _, g := range cfg.DeniedPaths {
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("policy.json: denied path %q: %w", g, err)
		}
		p.denied = append(p.denied, g)
	}
	return p, nil
}

// Result is the filtered content plus accounting.
type Result struct {
	Content string
	// Redactions counts replaced spans (a denied path counts as one).
	Redactions int
	// Denied is true when the whole content was withheld due to a denied path.
	Denied bool
}

// DeniedPath reports whether p matches a denied path glob, testing the full
// slash-normalized path, its base name, and every trailing sub-path.
func (p *Policy) DeniedPath(pth string) bool {
	norm := strings.TrimPrefix(filepath.ToSlash(pth), "./")
	parts := strings.Split(norm, "/")
	for i := range parts {
		sub := strings.Join(parts[i:], "/")
		for _, g := range p.denied {
			if ok, _ := path.Match(g, sub); ok {
				return true
			}
		}
	}
	return false
}

// Apply filters content. paths are file paths the interaction touched (from
// tool arguments); content that touches a denied path is withheld entirely.
// toolPayload marks tool-call/result text, which is also scanned for denied
// path mentions. The remaining content has secret patterns replaced.
func (p *Policy) Apply(content string, paths []string, toolPayload bool) Result {
	denied := toolPayload && p.MentionsDeniedPath(content)
	for _, pth := range paths {
		denied = denied || p.DeniedPath(pth)
	}
	if denied {
		return Result{Content: placeholderPrefix + "denied-path]", Redactions: 1, Denied: true}
	}
	res := Result{Content: content}
	for _, r := range p.rules {
		res.Content = r.re.ReplaceAllStringFunc(res.Content, func(match string) string {
			if r.group == 0 {
				res.Redactions++
				return placeholderPrefix + r.name + "]"
			}
			sub := r.re.FindStringSubmatchIndex(match)
			if sub == nil || len(sub) <= 2*r.group+1 || sub[2*r.group] < 0 {
				return match
			}
			secret := match[sub[2*r.group]:sub[2*r.group+1]]
			if strings.HasPrefix(secret, placeholderPrefix) {
				return match
			}
			res.Redactions++
			return match[:sub[2*r.group]] + placeholderPrefix + r.name + "]" + match[sub[2*r.group+1]:]
		})
	}
	return res
}

var pathToken = regexp.MustCompile(`[^\s"'<>|;&=()\[\]{},:]+`)

// MentionsDeniedPath reports whether tool payload text names a denied path,
// such as a "cat .env" call. Only tokens that look like paths (contain "/" or
// ".") are tested so ordinary words never trigger it.
func (p *Policy) MentionsDeniedPath(payload string) bool {
	for _, tok := range pathToken.FindAllString(payload, -1) {
		if strings.ContainsAny(tok, "/.") && p.DeniedPath(tok) {
			return true
		}
	}
	return false
}
