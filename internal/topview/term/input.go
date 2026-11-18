package term

import "unicode/utf8"

// KeyCode is a decoded key.
type KeyCode uint8

// Keys top uses. Anything else is consumed and dropped.
const (
	Rune KeyCode = iota
	Up
	Down
	Left
	Right
	PgUp
	PgDn
	Home
	End
	Enter
	Esc
	Tab
	Backspace
	CtrlC
)

// Key is one key press. Rune is set for Code == Rune.
type Key struct {
	Code KeyCode
	Rune rune
}

// Decode turns terminal input into keys. rest is an incomplete sequence
// to prepend to the next read. A lone ESC stays in rest: it may start a
// sequence whose remainder has not arrived, so the caller waits briefly
// (50 ms) and then calls Flush.
func Decode(buf []byte) (keys []Key, rest []byte) {
	for len(buf) > 0 {
		k, n, ok := decodeOne(buf)
		if n == 0 {
			return keys, buf
		}
		if ok {
			keys = append(keys, k)
		}
		buf = buf[n:]
	}
	return keys, nil
}

// Flush decodes what Decode left once no more input has followed.
func Flush(rest []byte) []Key {
	if len(rest) > 0 && rest[0] == 0x1b {
		return []Key{{Code: Esc}}
	}
	return nil
}

// decodeOne decodes the first key in buf. n == 0 means buf holds only
// the start of a key; ok == false means n bytes were consumed and
// dropped.
func decodeOne(buf []byte) (Key, int, bool) {
	switch c := buf[0]; {
	case c == 0x1b:
		if len(buf) == 1 {
			return Key{}, 0, false
		}
		switch buf[1] {
		case '[':
			return csi(buf)
		case 'O':
			if len(buf) < 3 {
				return Key{}, 0, false
			}
			k, ok := final(buf[2])
			return k, 3, ok
		}
		return Key{Code: Esc}, 1, true
	case c == '\r' || c == '\n':
		return Key{Code: Enter}, 1, true
	case c == '\t':
		return Key{Code: Tab}, 1, true
	case c == 0x7f || c == 0x08:
		return Key{Code: Backspace}, 1, true
	case c == 0x03:
		return Key{Code: CtrlC}, 1, true
	case c < 0x20:
		return Key{}, 1, false
	case c < utf8.RuneSelf:
		return Key{Code: Rune, Rune: rune(c)}, 1, true
	}
	if !utf8.FullRune(buf) {
		return Key{}, 0, false
	}
	r, n := utf8.DecodeRune(buf)
	if r == utf8.RuneError || (r >= 0x80 && r < 0xa0) {
		return Key{}, n, false
	}
	return Key{Code: Rune, Rune: r}, n, true
}

// csi decodes ESC [ params final. Modifier parameters (ESC [ 1 ; 5 C)
// are ignored: top binds no modified keys.
func csi(buf []byte) (Key, int, bool) {
	for i := 2; i < len(buf); i++ {
		c := buf[i]
		if c >= 0x40 && c <= 0x7e {
			if c == '~' {
				return tilde(buf[2:i]), i + 1, tilde(buf[2:i]).Code != Rune
			}
			k, ok := final(c)
			return k, i + 1, ok
		}
		if c < 0x20 || c > 0x3f {
			return Key{}, i, false // malformed: drop what came before
		}
		if i > 32 {
			return Key{}, i, false // runaway parameters
		}
	}
	return Key{}, 0, false
}

func final(c byte) (Key, bool) {
	switch c {
	case 'A':
		return Key{Code: Up}, true
	case 'B':
		return Key{Code: Down}, true
	case 'C':
		return Key{Code: Right}, true
	case 'D':
		return Key{Code: Left}, true
	case 'H':
		return Key{Code: Home}, true
	case 'F':
		return Key{Code: End}, true
	}
	return Key{}, false
}

func tilde(params []byte) Key {
	n := 0
	for _, c := range params {
		if c == ';' {
			break
		}
		if c < '0' || c > '9' {
			return Key{}
		}
		n = n*10 + int(c-'0')
	}
	switch n {
	case 1, 7:
		return Key{Code: Home}
	case 4, 8:
		return Key{Code: End}
	case 5:
		return Key{Code: PgUp}
	case 6:
		return Key{Code: PgDn}
	}
	return Key{}
}
