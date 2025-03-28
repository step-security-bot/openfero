package utils

import (
	"math/rand"
	"strings"
)

// Charset for random string generation
const Charset = "abcdefghijklmnopqrstuvwxyz0123456789"

// StringWithCharset generates a random string with the specified length and charset
func StringWithCharset(length int, charset string) string {
	randombytes := make([]byte, length)
	for i := range randombytes {
		num := rand.Intn(len(charset))
		randombytes[i] = charset[num]
	}

	return string(randombytes)
}

// SanitizeInput sanitizes input strings
func SanitizeInput(input string) string {
	input = strings.ReplaceAll(input, "\n", "")
	input = strings.ReplaceAll(input, "\r", "")
	return input
}
