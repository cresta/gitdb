package httpserver

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cresta/gitdb/internal/testhelp"
	"github.com/dgrijalva/jwt-go"
	"github.com/stretchr/testify/require"
)

func TestJWTSignIn(t *testing.T) {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	j := JWTSignIn{
		Logger: testhelp.ZapTestingLogger(t),
		Auth: func(username string, password string) (bool, error) {
			return username == "user" && password == "pass", nil
		},
		SigningString: func(_ string) *rsa.PrivateKey {
			return pk
		},
	}
	rec := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/url", nil)
	require.NoError(t, err)
	req.SetBasicAuth("user", "pass")
	j.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	tok, err := jwt.Parse(rec.Body.String(), func(_ *jwt.Token) (interface{}, error) {
		return pk.Public(), nil
	})
	require.NoError(t, err)
	require.True(t, tok.Valid)
}
