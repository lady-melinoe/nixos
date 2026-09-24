package main

import (
	"encoding/base64"
	"fmt"
)

var zeroKey NoisePrivateKey

func keyToBase64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func parseKeyBase64(s string) (NoisePrivateKey, error) {
	var key NoisePrivateKey
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return key, fmt.Errorf("invalid base64 key: %w", err)
	}
	if len(decoded) != NoisePrivateKeySize {
		return key, fmt.Errorf("key must decode to exactly %d bytes, got %d", NoisePrivateKeySize, len(decoded))
	}
	copy(key[:], decoded)
	return key, nil
}

func parsePubKeyBase64(s string) (NoisePublicKey, error) {
	sk, err := parseKeyBase64(s)
	return NoisePublicKey(sk), err
}
