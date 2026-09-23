package service

const base62Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// EncodeBase62 encodes a non-negative id using the digits 0-9, then a-z, then A-Z.
func EncodeBase62(id int64) string {
	if id < 0 {
		panic("EncodeBase62: negative id")
	}
	if id == 0 {
		return "0"
	}

	var buf [11]byte // 62^11 > math.MaxInt64
	i := len(buf)
	for id > 0 {
		i--
		buf[i] = base62Alphabet[id%62]
		id /= 62
	}
	return string(buf[i:])
}
