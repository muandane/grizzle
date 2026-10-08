package postgres

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // MD5 matches PostgreSQL's historical md5 rolpassword format
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// passwordMatchesStored reports whether plaintext matches a pg_authid.rolpassword
// verifier (md5 or SCRAM-SHA-256). The plaintext never leaves process memory.
func passwordMatchesStored(plaintext, rolpassword, rolname string) bool {
	if plaintext == "" {
		return rolpassword == ""
	}
	if rolpassword == "" {
		return false
	}
	switch {
	case strings.HasPrefix(rolpassword, "md5"):
		sum := md5.Sum([]byte(plaintext + rolname)) //nolint:gosec // PostgreSQL md5 password verifier
		return rolpassword == "md5"+hex.EncodeToString(sum[:])
	case strings.HasPrefix(rolpassword, "SCRAM-SHA-256$"):
		return scramSHA256Verify(plaintext, rolpassword)
	default:
		return false
	}
}

func scramSHA256Verify(password, stored string) bool {
	// SCRAM-SHA-256$<iterations>:<salt>$<storedkey>:<serverkey>
	rest := strings.TrimPrefix(stored, "SCRAM-SHA-256$")
	parts := strings.Split(rest, "$")
	if len(parts) != 2 {
		return false
	}
	iterSalt := strings.SplitN(parts[0], ":", 2)
	keys := strings.SplitN(parts[1], ":", 2)
	if len(iterSalt) != 2 || len(keys) != 2 {
		return false
	}
	iterations, err := strconv.Atoi(iterSalt[0])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(iterSalt[1])
	if err != nil {
		return false
	}
	storedKey, err := base64.StdEncoding.DecodeString(keys[0])
	if err != nil {
		return false
	}
	salted := pbkdf2SHA256([]byte(password), salt, iterations, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	computed := sha256.Sum256(clientKey)
	return hmac.Equal(computed[:], storedKey)
}

func hmacSHA256(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}

// pbkdf2SHA256 is a minimal PBKDF2-HMAC-SHA256 (stdlib only).
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	var out []byte
	var block uint32 = 1
	for len(out) < keyLen {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], block)
		u := hmacSHA256(password, append(append([]byte{}, salt...), buf[:]...))
		t := append([]byte{}, u...)
		for i := 1; i < iterations; i++ {
			u = hmacSHA256(password, u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
		block++
	}
	return out[:keyLen]
}

// quoteStringLiteral escapes a PostgreSQL single-quoted string literal.
func quoteStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// GenerateAlterRolePasswordSQL builds ALTER ROLE ... PASSWORD with the real
// plaintext. Callers must not persist the result on Step.SQL.
func GenerateAlterRolePasswordSQL(roleName, password string) string {
	return fmt.Sprintf("ALTER ROLE %s PASSWORD %s;", quoteIdentifier(roleName), quoteStringLiteral(password))
}

// GenerateAlterRolePasswordSQLRedacted is the Step.SQL form of a password change.
func GenerateAlterRolePasswordSQLRedacted(roleName string) string {
	return fmt.Sprintf("ALTER ROLE %s PASSWORD %s;", quoteIdentifier(roleName), quoteStringLiteral("********"))
}
