package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testApp(t *testing.T) (*AppCredentials, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return &AppCredentials{AppID: 123, PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), WebhookSecret: "s"}, key
}

func TestSignature(t *testing.T) {
	body := []byte(`{"action":"queued"}`)
	sig := Sign("secret", body)
	if !VerifySignature("secret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("other", body, sig) || VerifySignature("secret", append(body, ' '), sig) || VerifySignature("", body, sig) || VerifySignature("secret", body, "sha1=abc") {
		t.Fatal("invalid signature accepted")
	}
}

func TestAppJWT(t *testing.T) {
	app, key := testApp(t)
	c, err := New(app)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.AppJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %s", tok)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("bad signature: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(payload, &claims)
	if claims.Iss != "123" || claims.Exp-claims.Iat > 600 || claims.Iat > time.Now().Unix() {
		t.Fatalf("bad claims %+v", claims)
	}
}

func TestInstallationTokenCachingAndJIT(t *testing.T) {
	app, _ := testApp(t)
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/9/access_tokens":
			tokenCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Errorf("token request must use the app JWT")
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_abc", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/o/r/actions/runners/generate-jitconfig":
			if r.Header.Get("Authorization") != "token ghs_abc" {
				t.Errorf("JIT request must use the installation token")
			}
			var body struct {
				Name   string   `json:"name"`
				Labels []string `json:"labels"`
				Group  int      `json:"runner_group_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Name != "n" || len(body.Labels) != 1 || body.Group != 1 {
				t.Errorf("bad JIT body %+v", body)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"runner": map[string]any{"id": 5, "name": "n"}, "encoded_jit_config": "abc"})
		case r.URL.Path == "/repos/o/r/contents/.github/truner.yml":
			http.Error(w, `{"message":"Not Found"}`, 404)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 500)
		}
	}))
	defer srv.Close()
	c, _ := New(app)
	c.BaseURL = srv.URL
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		jit, err := c.GenerateJITConfig(ctx, 9, "o", "r", "n", []string{"truner"})
		if err != nil || jit.Runner.ID != 5 || jit.EncodedJITConfig != "abc" {
			t.Fatalf("jit: %+v %v", jit, err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("installation token should be cached, fetched %d times", tokenCalls.Load())
	}
	data, err := c.GetFile(ctx, 9, "o", "r", ".github/truner.yml", "")
	if err != nil || data != nil {
		t.Fatalf("missing file must be (nil, nil): %v %v", data, err)
	}
}
