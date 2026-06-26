package identity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// unverifiedIssuer reads the `iss` claim from a JWT WITHOUT verifying its signature — used only to pick
// which configured provider's verifier to run. The real signature/audience/expiry check happens in
// that verifier (oidc.IDTokenVerifier.Verify); this routing read trusts nothing.
func unverifiedIssuer(rawToken string) (string, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT (want 3 dot-separated segments)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("JWT payload is not valid base64url")
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("JWT payload is not valid JSON")
	}
	if claims.Iss == "" {
		return "", errors.New("JWT has no iss claim")
	}
	return claims.Iss, nil
}
