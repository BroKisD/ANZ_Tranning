package helper

func IsValidEmail(email string) bool {
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	at := false
	for _, c := range email {
		if c == '@' {
			at = true
			break
		}
	}
	return at
}
