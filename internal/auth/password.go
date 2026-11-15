package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

func HashPassword(password string) (string, error) {
	hashb, err := bcrypt.GenerateFromPassword([]byte(password),12)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w",err)
	}
	return string(hashb),nil
}

func CheckPassword(password, hash string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash),[]byte(password))
	if err != nil {
		return fmt.Errorf("failed password check: %w",err)
	}
	return nil
}