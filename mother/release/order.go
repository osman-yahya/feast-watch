package release

import "strconv"

// naturalLess orders version strings so v1.9.0 sorts before v1.10.0, which a
// plain string comparison gets backwards. It compares digit runs numerically
// and everything else lexically.
//
// This is an ordering aid for the panel's dropdown, not a semver
// implementation, and notably it does not rank a prerelease against its
// release — it falls through to length, which puts v1.3.0-rc1 above v1.3.0.
// That is tolerable here because the consequence is a dropdown order. The
// panel deliberately does NOT reuse this rule for its downgrade guard, where
// the same answer would wave a rollback through; see the panel's
// src/lib/versionOrder.js.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		an, arest := leadingChunk(a)
		bn, brest := leadingChunk(b)
		if an != bn {
			ai, aerr := strconv.Atoi(an)
			bi, berr := strconv.Atoi(bn)
			if aerr == nil && berr == nil {
				return ai < bi
			}
			return an < bn
		}
		a, b = arest, brest
	}
	return len(a) < len(b)
}

// leadingChunk splits off the leading run of digits, or the leading run of
// non-digits when the string does not start with one.
func leadingChunk(s string) (chunk, rest string) {
	digits := s[0] >= '0' && s[0] <= '9'
	for i := 0; i < len(s); i++ {
		if (s[i] >= '0' && s[i] <= '9') != digits {
			return s[:i], s[i:]
		}
	}
	return s, ""
}
