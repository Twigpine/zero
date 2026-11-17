package tui

import (
	"net/url"
	"strings"
)

// ONE BOUNDARY FOR EVERY CONSUMER OF A COMMAND-LINE ELEMENT.
//
// The failure collector, the Target row and the URL collector each decided
// where a flag ends and its value begins, separately, and each did it by
// cutting at the first "=". That is only the boundary when the "=" comes before
// any whitespace. In "--api-key YWJjZGVmZ2hpag==" and
// "-HX-Workspace-Id: YWJjZGVmZ2hpag==" the first "=" is base64 padding inside
// the credential, so the cut kept "=" as the value and left the recoverable
// body outside the redaction set, while the Target row printed that body
// followed by "=[REDACTED]". Reported by @jatmn.
//
// The boundary is found here, before anything inside the value is interpreted,
// and all three consumers read the same result.

// mcpArgParts is one element split at the boundary between a flag and the value
// it carries in that same element.
type mcpArgParts struct {
	// flag is the flag exactly as written, without its separator, so the display
	// can print it back unchanged.
	flag string
	// separator is what stood between the two: "=", " ", or "" for the attached
	// short header spelling "-HName: value".
	separator string
	// value is everything after the boundary. It is never split again on an "="
	// of its own.
	value string
	// header marks a value that is header text ("Name: value") rather than a
	// bare credential.
	header bool
	// sensitive marks a value whose flag names it as a credential.
	sensitive bool
}

// splitMCPArgBoundary returns the flag, the separator and the remainder of one
// element, choosing whichever of the first "=" and the first whitespace comes
// EARLIER. ok is false when the element carries no value of its own.
func splitMCPArgBoundary(arg string) (flag, separator, rest string, ok bool) {
	trimmed := strings.TrimSpace(arg)
	at := strings.IndexAny(trimmed, "= \t")
	if at <= 0 {
		return "", "", "", false
	}
	rest = strings.TrimSpace(trimmed[at+1:])
	if rest == "" {
		return "", "", "", false
	}
	separator = "="
	if trimmed[at] != '=' {
		separator = " "
	}
	return trimmed[:at], separator, rest, true
}

// mcpArgIsEndpoint reports whether the element IS a URL, as opposed to a flag
// that carries one. In a positional endpoint the first "=" is a query separator
// and not a flag boundary, so every reader that cuts there gets a "flag" of
// "https://u:pw@host/mcp?token", which names a credential: the key=value reader
// masked the query value and printed the userinfo beside it, and the bare-flag
// reader printed the whole element and masked the NEXT one instead.
//
// The decision is made on the text in front of the first "=" or whitespace,
// which is where a flag name would be, and a flag name never holds a scheme.
func mcpArgIsEndpoint(arg string) bool {
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" || strings.HasPrefix(trimmed, "-") {
		return false
	}
	head := trimmed
	if at := strings.IndexAny(trimmed, "= \t"); at >= 0 {
		head = trimmed[:at]
	}
	lower := strings.ToLower(head)
	return strings.Contains(head, "://") ||
		strings.HasPrefix(lower, "http:") ||
		strings.HasPrefix(lower, "https:")
}

// splitMCPArgValue classifies one element that carries its own value. ok is
// false for a bare flag (whose value is the next element), a positional word,
// and a flag whose name says nothing about a credential.
func splitMCPArgValue(arg string) (mcpArgParts, bool) {
	// Header spellings first: they are recognised by their prefix alone, so no
	// character of the header text is consulted to find where it starts.
	if flag, separator, carried, ok := mcpHeaderArgumentParts(arg); ok {
		if carried == "" {
			return mcpArgParts{}, false
		}
		return mcpArgParts{flag: flag, separator: separator, value: carried, header: true, sensitive: true}, true
	}
	if mcpArgIsEndpoint(arg) {
		// The URL paths own this element: redactMCPDisplayURL for the row,
		// mcpArgURLCandidates for the collector.
		return mcpArgParts{}, false
	}
	flag, separator, rest, ok := splitMCPArgBoundary(arg)
	if !ok {
		return mcpArgParts{}, false
	}
	switch separator {
	case "=":
		if !isSensitiveMCPDisplayKey(flag) {
			return mcpArgParts{}, false
		}
	default:
		if !isSensitiveMCPDisplayFlag(flag) {
			return mcpArgParts{}, false
		}
	}
	return mcpArgParts{flag: flag, separator: separator, value: rest, sensitive: true}, true
}

// A KNOWN AUTHENTICATION VALUE KEEPS ITS PROVENANCE WHEN IT IS TAKEN APART.
//
// credentialCandidates offers the tails of a value so that "Bearer <token>"
// also yields <token>, and it applies the eight-byte readability floor to every
// tail. That floor exists for AMBIGUOUS configuration. Applied to a value whose
// key already said "this is a credential", it discarded the credential itself:
// "Authorization": "Bearer s3cr3t" kept the whole value and dropped the six-byte
// token, so a server echoing the bare token reached the panel and the
// transcript. Reported by @jatmn.
//
// Only the supported authentication shapes qualify, "<scheme> <credential>" and
// "<header>: [<scheme>] <credential>": at most two leading name-like words and
// then ONE credential token. A known passphrase of several words is not taken
// apart this way, so its short words never enter the redaction set.
const maxKnownCredentialLeadingWords = 2

func knownCredentialTails(value string) []string {
	remainder := strings.TrimSpace(value)
	if len(remainder) > maxMCPCredentialSeparatorScan {
		// The separators of every supported shape sit in a short prefix; only that
		// prefix is searched, and the tail survives at any length.
		head := remainder[:maxMCPCredentialSeparatorScan]
		if !strings.ContainsAny(head, " :") {
			return nil
		}
	}
	for words := 0; words < maxKnownCredentialLeadingWords; words++ {
		window := remainder
		if len(window) > maxMCPCredentialSeparatorScan {
			window = window[:maxMCPCredentialSeparatorScan]
		}
		index := strings.IndexAny(window, " :")
		if index < 0 {
			break
		}
		if !mcpSchemeLikeWord(remainder[:index]) {
			return nil
		}
		next := strings.TrimSpace(remainder[index+1:])
		if next == "" || next == remainder {
			return nil
		}
		remainder = next
		if !strings.ContainsAny(remainder, " :") {
			// One credential token is left behind the scheme or header name.
			return []string{remainder}
		}
	}
	return nil
}

// withUnpaddedSpellings returns candidates with, beside each one that ends in
// "=", the same value without that padding, provided what is left is at least
// floor bytes long.
//
// Base64 padding is not part of what makes a value secret, and a server that
// normalizes before echoing drops it. Exact-value redaction knows
// "YWJjZGVmZ2hpag==", the message says "YWJjZGVmZ2hpag", nothing matches, and
// the whole recoverable body is displayed for the sake of two characters.
//
// The floor is the caller's: one byte for known credential material, and the
// readability floor for ambiguous values, so an ordinary short string does not
// enter the redaction set by losing its padding. At most one spelling is added
// per candidate, so the bounded expansion stays bounded.
func withUnpaddedSpellings(candidates []string, floor int) []string {
	out := make([]string, 0, len(candidates)+1)
	for _, candidate := range candidates {
		out = append(out, candidate)
		if body := strings.TrimRight(candidate, "="); body != candidate && len(body) >= floor && body != "" {
			out = append(out, body)
		}
	}
	return out
}

// mcpSchemeLikeWord reports whether word reads as a scheme or header name:
// letters, digits, "-" and "_" only. A credential fragment with other
// punctuation in front of a space is not a scheme, and refusing it keeps this
// from walking into the middle of an opaque value.
func mcpSchemeLikeWord(word string) bool {
	if word == "" || len(word) > 64 {
		return false
	}
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// mcpURLFragmentSecretValues collects the credential-bearing values of a URL
// fragment, classified the way redactMCPDisplayRawQuery classifies them for the
// Target row: "key=value" pairs, known when the key names a credential and
// ambiguous otherwise, so the floor still keeps "#v=1" readable.
//
// The fragment is never sent to an HTTP server, but a stdio child receives the
// whole argument, and one that prints its endpoint while failing puts the
// fragment into the stderr this panel renders. Target already masked it; the
// failure reason one row above did not. Reported by @jatmn.
//
// The raw spelling is cut out of the ORIGINAL string, because that is what the
// child was handed and therefore what it echoes; the decoded spelling is added
// beside it. The Target row splits the DECODED fragment, so that one is split
// here as well: an escaped "&" or "=" moves the pair boundaries, and the two
// rows would otherwise disagree about which text is the value.
func mcpURLFragmentSecretValues(rawURL string) (known []string, ambiguous []string) {
	_, rawFragment, found := strings.Cut(rawURL, "#")
	if !found || rawFragment == "" {
		return nil, nil
	}
	collect := func(fragment string, decodeValues bool) {
		for _, pair := range strings.Split(fragment, "&") {
			key, value, hasValue := strings.Cut(pair, "=")
			if !hasValue || value == "" {
				// A bare fragment is an anchor. The Target row prints it as it is,
				// and nothing here makes it a credential.
				continue
			}
			decodedKey := key
			if decodeValues {
				if unescaped, err := url.QueryUnescape(key); err == nil {
					decodedKey = unescaped
				}
			}
			forms := []string{value}
			if decodeValues {
				if decoded, err := url.PathUnescape(value); err == nil && decoded != value {
					forms = append(forms, decoded)
				}
			}
			if isSensitiveMCPDisplayKey(decodedKey) {
				known = append(known, forms...)
				continue
			}
			ambiguous = append(ambiguous, forms...)
		}
	}
	collect(rawFragment, true)
	if decodedFragment, err := url.PathUnescape(rawFragment); err == nil && decodedFragment != rawFragment {
		// Already decoded once. Decoding its values again would invent a third
		// spelling that was never configured and that nothing echoes.
		collect(decodedFragment, false)
	}
	return known, ambiguous
}
