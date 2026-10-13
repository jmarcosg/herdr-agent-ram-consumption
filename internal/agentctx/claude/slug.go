package claude

import (
	"strconv"
	"unicode/utf16"
)

const dirNameMax = 200

// Slug mirrors how Claude Code names a project directory: every UTF-16 unit
// that is not an ASCII letter or digit becomes '-'; names over 200 units are cut
// and suffixed with the base36 form of the JS string hash of the cwd.
func Slug(cwd string) string {
	units := utf16.Encode([]rune(cwd))
	name := make([]byte, len(units))
	for i, u := range units {
		if u < 0x80 && (u >= '0' && u <= '9' || u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z') {
			name[i] = byte(u)
		} else {
			name[i] = '-'
		}
	}
	if len(name) <= dirNameMax {
		return string(name)
	}
	return string(name[:dirNameMax]) + "-" + strconv.FormatUint(uint64(stringHash(units)), 36)
}

// stringHash is JavaScript's `hash = (hash << 5) - hash + unit` over int32, abs'd.
func stringHash(units []uint16) uint32 {
	var h int32
	for _, u := range units {
		h = h<<5 - h + int32(u)
	}
	if h < 0 {
		return uint32(-int64(h))
	}
	return uint32(h)
}
