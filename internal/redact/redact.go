package redact

import (
	"regexp"
	"strings"
)

type pattern struct {
	re          *regexp.Regexp
	replacement string
}

var patterns = []pattern{
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[REDACTED:AWS_KEY]"},
	{regexp.MustCompile(`(?i)(aws_secret_access_key|aws_secret)\s*[=:]\s*\S+`), "$1=[REDACTED:AWS_SECRET]"},
	{regexp.MustCompile(`(?i)(api[_-]?key|api[_-]?secret|api[_-]?token|access[_-]?token|auth[_-]?token|bearer|secret[_-]?key|client[_-]?secret|private[_-]?key)\s*[=:]\s*["']?([A-Za-z0-9+/=_\-]{20,})["']?`), "${1}=[REDACTED]"},
	{regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)\S+`), "${1}[REDACTED]"},
	{regexp.MustCompile(`\b(ghp_|gho_|ghs_|ghr_)[A-Za-z0-9_]{36,}\b`), "[REDACTED:GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), "[REDACTED:GITHUB_TOKEN]"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{10,}\b`), "[REDACTED:SLACK_TOKEN]"},
	{regexp.MustCompile(`\b[sr]k_(live|test)_[A-Za-z0-9]{20,}\b`), "[REDACTED:STRIPE_KEY]"},
	{regexp.MustCompile(`(?s)-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----.*?-----END (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`), "[REDACTED:PRIVATE_KEY]"},
	{regexp.MustCompile(`(?i)(mongodb(\+srv)?|postgres(ql)?|mysql|redis|amqp)://[^:]+:[^@\s]+@`), "[REDACTED:CONNECTION_STRING]@"},
	{regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[=:]\s*["']?[^\s"',;]{4,}["']?`), "${1}=[REDACTED]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), "[REDACTED:JWT]"},
	{regexp.MustCompile(`(?i)(ssh-(?:rsa|ed25519|dss|ecdsa)\s+)AAAA[A-Za-z0-9+/=]{40,}`), "${1}[REDACTED:SSH_KEY]"},
	{regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`), "[REDACTED:EMAIL]"},
}

var sensitiveKeywords = []string{
	"password", "passwd", "pwd", "secret", "token", "api_key", "apikey",
	"api-key", "access_key", "private_key", "auth", "credential",
	"client_secret", "signing_key", "encryption_key", "db_password",
	"database_password", "smtp_password", "ssh_password",
}

// Clean scrubs security-sensitive information from text before storage.
func Clean(text string) string {
	result := text
	for _, p := range patterns {
		result = p.re.ReplaceAllString(result, p.replacement)
	}
	lines := strings.Split(result, "\n")
	for i, line := range lines {
		lines[i] = redactKeyValueLine(line)
	}
	return strings.Join(lines, "\n")
}

func redactKeyValueLine(line string) string {
	// Work only on ASCII lines to avoid byte-offset mismatches between the
	// ToLower'd copy and the original when multibyte UTF-8 characters are present.
	for _, b := range []byte(line) {
		if b > 0x7e {
			return line
		}
	}
	lower := strings.ToLower(line)
	for _, kw := range sensitiveKeywords {
		idx := strings.Index(lower, kw)
		if idx == -1 {
			continue
		}
		endIdx := idx + len(kw)
		if endIdx > len(line) {
			continue
		}
		rest := line[endIdx:]
		trimmed := strings.TrimLeft(rest, " \t\"'")
		if len(trimmed) == 0 {
			continue
		}
		sep := trimmed[0]
		if sep != '=' && sep != ':' {
			continue
		}
		valStart := strings.TrimLeft(trimmed[1:], " \t\"'")
		if len(valStart) < 4 {
			continue
		}
		if strings.HasPrefix(valStart, "[REDACTED") {
			continue
		}
		valEnd := strings.IndexAny(valStart, " \t\n\"',;")
		var val string
		if valEnd == -1 {
			val = valStart
		} else {
			val = valStart[:valEnd]
		}
		if len(val) >= 4 {
			line = strings.Replace(line, val, "[REDACTED]", 1)
		}
	}
	return line
}
