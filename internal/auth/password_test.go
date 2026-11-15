package auth

import "testing"

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("password123")
	if err != nil {
		t.Fatal("HashPassword failed:", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if hash == "password123" {
		t.Fatal("hash should not equal plain text password")
	}
}

func TestHashPasswordDifferentSalts(t *testing.T) {
	hash1, _ := HashPassword("password123")
	hash2, _ := HashPassword("password123")
	if hash1 == hash2 {
		t.Fatal("same password should produce different hashes (different salts)")
	}
}

func TestCheckPasswordCorrect(t *testing.T) {
	hash, _ := HashPassword("password123")
	if err := CheckPassword("password123", hash); err != nil {
		t.Fatal("expected match, got error:", err)
	}
}

func TestCheckPasswordWrong(t *testing.T) {
	hash, _ := HashPassword("password123")
	if err := CheckPassword("wrongpassword", hash); err == nil {
		t.Fatal("expected error for wrong password, got nil")
	}
}
