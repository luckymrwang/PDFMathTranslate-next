package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

func payHMAC(key, text string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

func pushSignature(parts ...string) string {
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func decodeAESKey(value string) ([]byte, error) {
	if len(value) != 43 {
		return nil, errors.New("VIRTUALPAY_ENCODING_AES_KEY must contain 43 characters")
	}
	key, err := base64.StdEncoding.DecodeString(value + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid message encryption key")
	}
	return key, nil
}

func decryptPush(encoded, keyString, appID string) ([]byte, error) {
	key, err := decodeAESKey(keyString)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid encrypted payload")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, data)
	padding := int(plain[len(plain)-1])
	if padding < 1 || padding > 32 || padding > len(plain) {
		return nil, errors.New("invalid message padding")
	}
	if !bytes.Equal(plain[len(plain)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, errors.New("invalid message padding")
	}
	plain = plain[:len(plain)-padding]
	if len(plain) < 20 {
		return nil, errors.New("invalid message length")
	}
	length := int(binary.BigEndian.Uint32(plain[16:20]))
	if length > len(plain)-20 {
		return nil, errors.New("invalid message length")
	}
	if !hmac.Equal(plain[20+length:], []byte(appID)) {
		return nil, errors.New("message appid mismatch")
	}
	return plain[20 : 20+length], nil
}
