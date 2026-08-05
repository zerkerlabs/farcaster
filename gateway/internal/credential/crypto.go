package credential

import (
	"crypto/rand"
	"fmt"
	"io"

	"github.com/zerkerlabs/gateway/gateway/internal/cryptoutil"
)

// dekSize is 32 bytes, giving AES-256.
const dekSize = 32

// generateDEK returns a cryptographically-random 256-bit data encryption key.
func generateDEK() ([]byte, error) {
	key := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	return key, nil
}

func sealGCM(key, plaintext []byte) ([]byte, error) { return cryptoutil.SealGCM(key, plaintext) }
func openGCM(key, data []byte) ([]byte, error)      { return cryptoutil.OpenGCM(key, data) }

// maskedHint returns the last 4 Unicode code points of plaintext prefixed with
// "...". If plaintext has 4 or fewer runes the result is fully masked with '*'
// characters so the full value is never revealed in metadata responses.
func maskedHint(plaintext []byte) string {
	runes := []rune(string(plaintext))
	n := len(runes)
	if n == 0 {
		return ""
	}
	if n <= 4 {
		mask := make([]rune, n)
		for i := range mask {
			mask[i] = '*'
		}
		return string(mask)
	}
	return "..." + string(runes[n-4:])
}
