package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// Token lifetime — 15 minutes
const accessTokenExpiry = 15 * time.Minute

// GenerateAccessToken creates a signed JWT for the given user.
func GenerateAccessToken(userID uuid.UUID, email string, secret string) (string, error) {
	now := time.Now()

	// Build the claims — these are the fields inside the JWT payload
	claims := jwt.MapClaims{
		"sub":   userID.String(),          // who this token is for
		"email": email,                     // convenience — avoids a DB lookup for display
		"exp":   now.Add(accessTokenExpiry).Unix(), // when it expires (15 min from now)
		"iat":   now.Unix(),                // when it was issued
		"jti":   uuid.New().String(),       // unique token ID — useful for blacklisting
	}

	// Create the token object with HS256 signing method
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// Sign it with the secret key — this produces the final "xxxxx.yyyyy.zzzzz" string
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return signed, nil
}

// ParseToken validates a JWT string and extracts the claims.
// Returns an error if the signature is invalid, the token is expired, or malformed.
func ParseToken(tokenString string, secret string) (*model.Claims, error) {
	// Parse and validate the token in one call.
	// The keyFunc tells the parser which secret to use for verification.
	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		// Security check: make sure the signing method is what we expect.
		// Without this, an attacker could set alg=none and bypass verification.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithExpirationRequired()) // reject tokens without an exp claim

	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	// Extract the claims map from the parsed token
	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	// Parse the "sub" field back into a UUID
	subStr, _ := mapClaims["sub"].(string)
	userID, err := uuid.Parse(subStr)
	if err != nil {
		return nil, fmt.Errorf("invalid sub claim: %w", err)
	}

	// Extract remaining fields — type assertions because MapClaims stores everything as interface{}
	email, _ := mapClaims["email"].(string)
	exp, _ := mapClaims["exp"].(float64) // JSON numbers are float64
	iat, _ := mapClaims["iat"].(float64)
	jti, _ := mapClaims["jti"].(string)

	return &model.Claims{
		Sub:   userID,
		Email: email,
		Exp:   int64(exp),
		Iat:   int64(iat),
		JTI:   jti,
	}, nil
}
