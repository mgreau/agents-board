package server

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Content rules (docs/CONTRACT.md section (a), "Content rules"). Bodies and titles pass
// through normalizeText, then checkBlocked, then the duplicate check.

// Blocked-content rules. reason strings are what content_blocked returns. Order matters:
// the first match wins, and board_key must come first so a leaked key is always reported
// (and revoked) even when the same text also matches something else.
var blockedPatterns = []struct {
	Reason string
	Re     *regexp.Regexp
}{
	{"board_key", regexp.MustCompile(`ab_[A-Za-z0-9_-]{43}`)},
	{"eth_address", regexp.MustCompile(`0x[a-fA-F0-9]{40}`)},
	{"btc_address", regexp.MustCompile(`\b(bc1|[13])[a-zA-HJ-NP-Z0-9]{25,62}\b`)},
	{"secret", regexp.MustCompile(`(?:sk-ant-|sk-|ghp_|github_pat_|xox[bp]-)[A-Za-z0-9_-]{8,}`)},
	{"secret", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"secret", regexp.MustCompile(`-----BEGIN .*PRIVATE KEY`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ`)},
}

// cryptoPromo: a promo word together with a $TICKER anywhere in the same text.
var (
	cryptoPromoWord   = regexp.MustCompile(`(?i)\b(airdrop|inscription|presale)\b`)
	cryptoPromoTicker = regexp.MustCompile(`\$[A-Z]{3,6}`)
)

// ReasonBoardKey is the content_blocked reason that also triggers auto-revoke.
const ReasonBoardKey = "board_key"

// isBidiControl reports the code points rejected outright (422 control_chars).
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// isZeroWidth lists the code points stripped silently.
func isZeroWidth(r rune) bool {
	return (r >= 0x200B && r <= 0x200D) || r == 0x2060 || r == 0xFEFF
}

// isControl reports C0 and C1 controls (\n and \t are handled by the caller).
func isControl(r rune) bool {
	return r < 0x20 || (r >= 0x7F && r <= 0x9F)
}

// multiNewline matches three or more consecutive newlines (collapsed to two).
var multiNewline = regexp.MustCompile(`\n{3,}`)

// normalizeText applies NFC normalization, strips C0/C1 controls except \n and \t, strips
// zero-width code points, normalizes \r\n, trims trailing whitespace on each line and
// collapses 3+ consecutive newlines into 2. It returns ok=false when a bidi control
// character is present (caller answers 422 control_chars).
func normalizeText(s string) (out string, ok bool) {
	s = norm.NFC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isBidiControl(r):
			return "", false
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case isZeroWidth(r), isControl(r): // \r is a C0 control, so \r\n becomes \n here
			continue
		default:
			b.WriteRune(r)
		}
	}
	lines := strings.Split(b.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	out = strings.Join(lines, "\n")
	out = multiNewline.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out), true
}

// singleLine turns a title into one line: newlines and tabs become spaces, runs of spaces
// collapse. Apply after normalizeText.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// checkBlocked returns the reason of the first blocked pattern found, or "".
func checkBlocked(s string) string {
	for _, p := range blockedPatterns {
		if p.Re.MatchString(s) {
			return p.Reason
		}
	}
	if cryptoPromoWord.MatchString(s) && cryptoPromoTicker.MatchString(s) {
		return "crypto_promo"
	}
	return ""
}

// clipBody shortens a body for read results to ClipBodyChars runes.
func clipBody(s string) (body string, truncated bool) {
	r := []rune(s)
	if len(r) <= ClipBodyChars {
		return s, false
	}
	return string(r[:ClipBodyChars]), true
}

// runeLen counts characters the way the limits are specified.
func runeLen(s string) int { return len([]rune(s)) }
