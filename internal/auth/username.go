package auth

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The username rule, and the only place it is written. Every username a User is
// created under is held to it, whoever chose it: an Admin on the /users surface,
// the first Admin at setup, and a Sign-in provider at an identity's first
// sign-in. Usernames already stored are not re-judged.
//
// A username is stored in Unicode NFC, so the one name typed two ways is one
// string; it is 1 to maxUsernameLength characters (code points, counted after
// NFC); and it holds nothing a person reading the Users list could not see: no
// control, format, private-use, surrogate or unassigned code point, no
// default-ignorable one, and none of the fillers that print as a blank.
// Uniqueness ignores case and compatibility forms (store.UsernameKey), because
// the store makes that comparison inside the insert's transaction.
//
// A username an Admin or setup chooses is local, and is trimmed of surrounding
// whitespace first (localUsername); one a provider names is judged as named.
const maxUsernameLength = 64

// UsernameRule is the rule as a person reads it, for the api layer's refusals.
const UsernameRule = "a username is 1 to 64 characters, with no control, format or other invisible characters, and is unique regardless of case"

// localUsername is a username typed here — at setup, by an Admin, or at sign-in —
// without the whitespace around it.
func localUsername(username string) string {
	return strings.TrimSpace(username)
}

// normalizeUsername is username in NFC, and whether that follows the rule.
func normalizeUsername(username string) (string, bool) {
	username = norm.NFC.String(username)
	if username == "" || utf8.RuneCountInString(username) > maxUsernameLength {
		return username, false
	}
	for _, r := range username {
		if isInvisible(r) {
			return username, false
		}
	}
	return username, true
}

// isInvisible reports the code points that print nothing a reader can see, so a
// name holding one looks like a name that does not: every control (Cc), format
// (Cf), private-use (Co), surrogate (Cs) and unassigned (Cn) code point — all of
// general category C, so everything outside L, M, N, P, S and Z — every
// Default_Ignorable_Code_Point, and the fillers that render as a blank. The Cf
// part of Default_Ignorable_Code_Point is already refused as Cf; the rest is
// Other_Default_Ignorable_Code_Point and the variation selectors. "Unassigned"
// is as of the Unicode version Go's tables carry.
func isInvisible(r rune) bool {
	switch {
	case !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z),
		unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector):
		return true
	}
	switch r {
	case '\u115f', // Hangul choseong filler
		'\u1160', // Hangul jungseong filler
		'\u3164', // Hangul filler
		'\uffa0', // halfwidth Hangul filler
		'\u2800': // Braille pattern blank
		return true
	}
	return false
}
