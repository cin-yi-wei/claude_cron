package channelagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func HashJSON(v any) (string, error) {
	payload, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func HashSource(source SourceMessage) (string, error) {
	return HashJSON(source)
}

func HashOutput(output OutputJob) (string, error) {
	return HashJSON(output)
}
