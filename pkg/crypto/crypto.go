package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"

	"golang.org/x/crypto/scrypt"
)

const (
	ScryptN  = 32768
	Scryptr  = 8
	Scryptp  = 1
	KeyLen   = 32
	SaltLen  = 16
	NonceLen = 12
)

// Encrypt encrypts plaintext using a key derived from the password and a random salt
func Encrypt(password string, plaintext []byte) ([]byte, error) {
	salt := make([]byte, SaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}

	key, err := scrypt.Key([]byte(password), salt, ScryptN, Scryptr, Scryptp, KeyLen)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Result: Salt + Nonce + Ciphertext
	result := make([]byte, SaltLen+NonceLen+len(ciphertext))
	copy(result[0:SaltLen], salt)
	copy(result[SaltLen:SaltLen+NonceLen], nonce)
	copy(result[SaltLen+NonceLen:], ciphertext)

	return result, nil
}

// Decrypt decrypts ciphertext using a key derived from the password and the salt embedded in the data
func Decrypt(password string, data []byte) ([]byte, error) {
	if len(data) < SaltLen+NonceLen {
		return nil, errors.New("ciphertext too short")
	}

	salt := data[:SaltLen]
	nonce := data[SaltLen : SaltLen+NonceLen]
	ciphertext := data[SaltLen+NonceLen:]

	key, err := scrypt.Key([]byte(password), salt, ScryptN, Scryptr, Scryptp, KeyLen)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("decryption failed: invalid password or corrupted data")
	}

	return plaintext, nil
}
